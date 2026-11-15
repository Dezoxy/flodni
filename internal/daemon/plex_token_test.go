package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"flodni/internal/config"
)

// The media server token is a credential for another system. GET /api/config must
// never carry it, the way it never carries api_token, and a panel save must not lose it.

const plexSecret = "plex-secret-value"

func clearPlexEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"FLODNI_PLEX_TOKEN", "TIRAMISU_PLEX_TOKEN", "GOSTREAM_PLEX_TOKEN", "PLEX_TOKEN"} {
		t.Setenv(k, "")
	}
}

func writeCfg(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func filePlexToken(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Plex struct {
			Token string `json:"token"`
		} `json:"plex"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c.Plex.Token
}

func getConfig(h http.Handler, bearer string) (int, string) {
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestGetConfigNeverReturnsThePlexToken(t *testing.T) {
	clearPlexEnv(t)
	for _, tokenMode := range []bool{false, true} {
		start, bearer := "", ""
		if tokenMode {
			start, bearer = "tok", "tok"
		}
		h, path, _ := controlAPIServer(t, start, "")
		writeCfg(t, path, `{"log_level":"info","api_token":"`+start+`","plex":{"url":"http://x","token":"`+plexSecret+`"}}`)
		cfg := config.LoadConfig()
		globalConfig.Store(&cfg)
		code, body := getConfig(h, bearer)
		if code != http.StatusOK {
			t.Fatalf("token mode %v: GET status %d", tokenMode, code)
		}
		if strings.Contains(body, plexSecret) {
			t.Fatalf("token mode %v: GET /api/config leaks the Plex token: %s", tokenMode, body)
		}
		var got struct {
			Plex struct {
				Token string `json:"token"`
			} `json:"plex"`
			Source string `json:"plex_token_source"`
		}
		if err := json.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if got.Plex.Token != "" || got.Source != "file" {
			t.Fatalf("token mode %v: plex.token=%q plex_token_source=%q, want empty and file", tokenMode, got.Plex.Token, got.Source)
		}
	}
}

func TestGetConfigReportsAnEnvironmentPlexToken(t *testing.T) {
	clearPlexEnv(t)
	t.Setenv("FLODNI_PLEX_TOKEN", "env-plex-secret")
	h, _, _ := controlAPIServer(t, "", "")
	_, body := getConfig(h, "")
	if strings.Contains(body, "env-plex-secret") {
		t.Fatalf("GET leaks the environment token: %s", body)
	}
	if !strings.Contains(body, `"plex_token_source":"env"`) {
		t.Fatalf("GET must say the token comes from the environment: %s", body)
	}
}

func TestSaveWithAnEmptyPlexTokenKeepsTheFilesToken(t *testing.T) {
	clearPlexEnv(t)
	for _, body := range []string{
		`{"log_level":"debug"}`,                                      // plex absent
		`{"log_level":"debug","plex":{"url":"http://y"}}`,            // token absent
		`{"log_level":"debug","plex":{"url":"http://y","token":""}}`, // what the panel sends untouched
	} {
		h, path, _ := controlAPIServer(t, "", "")
		writeCfg(t, path, `{"log_level":"info","plex":{"url":"http://x","token":"`+plexSecret+`"}}`)
		if code, resp := doControl(h, "/api/config", body, ""); code != http.StatusOK {
			t.Fatalf("%s: status %d (%s)", body, code, resp)
		}
		if tok := filePlexToken(t, path); tok != plexSecret {
			t.Fatalf("%s: file token = %q, want it kept", body, tok)
		}
	}
}

func TestSaveWithANewPlexTokenWritesIt(t *testing.T) {
	clearPlexEnv(t)
	h, path, _ := controlAPIServer(t, "", "")
	writeCfg(t, path, `{"log_level":"info","plex":{"token":"`+plexSecret+`"}}`)
	if code, resp := doControl(h, "/api/config", `{"plex":{"token":"replacement"}}`, ""); code != http.StatusOK {
		t.Fatalf("status %d (%s)", code, resp)
	}
	if tok := filePlexToken(t, path); tok != "replacement" {
		t.Fatalf("file token = %q, want replacement", tok)
	}
}

func TestSaveNeverWritesAnEnvironmentPlexTokenToTheFile(t *testing.T) {
	clearPlexEnv(t)
	t.Setenv("FLODNI_PLEX_TOKEN", "env-plex-secret")
	h, path, _ := controlAPIServer(t, "", "")
	for _, body := range []string{`{"log_level":"debug"}`, `{"plex":{"url":"http://y","token":""}}`} {
		if code, resp := doControl(h, "/api/config", body, ""); code != http.StatusOK {
			t.Fatalf("%s: status %d (%s)", body, code, resp)
		}
		if data, _ := os.ReadFile(path); strings.Contains(string(data), "env-plex-secret") {
			t.Fatalf("%s: the environment token reached config.json: %s", body, data)
		}
	}
	// A file token under an environment one stays on disk.
	writeCfg(t, path, `{"plex":{"token":"`+plexSecret+`"}}`)
	if code, _ := doControl(h, "/api/config", `{"log_level":"debug"}`, ""); code != http.StatusOK {
		t.Fatal("save failed")
	}
	if tok := filePlexToken(t, path); tok != plexSecret {
		t.Fatalf("file token = %q, want it kept under an env token", tok)
	}
}

// The owner edits config.json by hand and does not restart; the panel save must keep
// the file's token, not the one memory read at start.
func TestSaveKeepsAHandEditedPlexToken(t *testing.T) {
	clearPlexEnv(t)
	for _, tokenMode := range []bool{false, true} {
		start, bearer := "", ""
		if tokenMode {
			start, bearer = "tok", "tok"
		}
		h, path, _ := controlAPIServer(t, start, start)
		writeCfg(t, path, `{"log_level":"info","api_token":"`+start+`","plex":{"token":"owner-edit"}}`)
		if code, resp := doControl(h, "/api/config", `{"log_level":"debug"}`, bearer); code != http.StatusOK {
			t.Fatalf("token mode %v: status %d (%s)", tokenMode, code, resp)
		}
		if tok := filePlexToken(t, path); tok != "owner-edit" {
			t.Fatalf("token mode %v: save replaced the hand-edited token with %q", tokenMode, tok)
		}
	}
}

// The panel page is static, so its contract is pinned on the served bytes: the token
// field starts empty, is a password field, and the save sends what was typed, never a
// stored value (there is none to send).
func TestPanelNeverHoldsOrResendsThePlexToken(t *testing.T) {
	html := string(settingsHTML)
	if !strings.Contains(html, `<input type="password" name="plex_token"`) {
		t.Error("the Plex token field must be a password input")
	}
	for _, bad := range []string{
		`.value = p.token`,
		`oldPlex.token`,
		`oldPlex.Token`,
		`p.Token`,
	} {
		if strings.Contains(html, bad) {
			t.Errorf("panel still reads or falls back to a stored Plex token: %q", bad)
		}
	}
	if !strings.Contains(html, `token: formData.get('plex_token') || ""`) {
		t.Error("the save must send only what was typed (empty keeps the stored token)")
	}
	if !strings.Contains(html, "plex_token_source") {
		t.Error("the panel must show where the token comes from")
	}
}
