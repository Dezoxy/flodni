package daemon

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flodni/internal/apiauth"
	"flodni/internal/flog"
)

// controlAPIServer serves the real /api/config and /api/restart handlers behind the
// real bearer middleware, with the control token fixed at start-up the way main fixes
// it, over a config.json holding fileToken. restarts counts the stubbed restart.
func controlAPIServer(t *testing.T, startToken, fileToken string) (h http.Handler, cfgPath string, restarts *int) {
	t.Helper()
	cfgPath = filepath.Join(t.TempDir(), "config.json")
	body := `{"log_level":"info"}`
	if fileToken != "" {
		body = `{"log_level":"info","api_token":"` + fileToken + `"}`
	}
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MKV_PROXY_CONFIG_PATH", cfgPath)
	t.Setenv("FLODNI_API_TOKEN", "")

	newTestApp(t).controlToken = startToken
	prevCfg, prevLog, prevRestart := globalConfig.Load(), logger, restartSelf
	cfg := loadCfg(t)
	globalConfig.Store(&cfg)
	logger = flog.NewText(io.Discard)
	n := 0
	restarts = &n
	restartSelf = func() { n++ }
	t.Cleanup(func() {
		globalConfig.Store(prevCfg)
		logger, restartSelf = prevLog, prevRestart
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/api/config", handleConfig)
	mux.HandleFunc("/api/restart", handleRestart)
	return apiauth.Middleware(app.controlToken, nil, mux), cfgPath, restarts
}

func doControl(h http.Handler, path, body, bearer string) (int, string) {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func fileToken(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		APIToken string `json:"api_token"`
	}
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c.APIToken
}

func TestOpenPortRefusesSettingToken(t *testing.T) {
	h, path, _ := controlAPIServer(t, "", "")
	before, _ := os.ReadFile(path)
	for _, body := range []string{
		`{"log_level":"debug","api_token":"attacker"}`,
		`{"api_token":"attacker"}`,
	} {
		code, resp := doControl(h, "/api/config", body, "")
		if code != http.StatusForbidden {
			t.Fatalf("open port, token in body %s: status %d, want 403", body, code)
		}
		if !strings.Contains(resp, "FLODNI_API_TOKEN") || !strings.Contains(resp, "config.json") || !json.Valid([]byte(resp)) {
			t.Fatalf("refusal must be JSON naming FLODNI_API_TOKEN and config.json, got %q", resp)
		}
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatalf("file changed on refusal:\n%s\n->\n%s", before, after)
	}
	if got := gc().ResolveAPIToken(""); got != "" {
		t.Fatalf("in-memory token changed: %q", got)
	}
}

func TestOpenPortAcceptsSaveWithoutNewToken(t *testing.T) {
	h, path, _ := controlAPIServer(t, "", "")
	for _, body := range []string{
		`{"log_level":"debug"}`,                // absent
		`{"log_level":"debug","api_token":""}`, // echoes the empty value
	} {
		if code, resp := doControl(h, "/api/config", body, ""); code != http.StatusOK {
			t.Fatalf("open port save %s: status %d (%s), want 200", body, code, resp)
		}
	}
	if tok := fileToken(t, path); tok != "" {
		t.Fatalf("token written by an open save: %q", tok)
	}
}

func TestOpenPortRefusesRestart(t *testing.T) {
	h, _, restarts := controlAPIServer(t, "", "")
	code, resp := doControl(h, "/api/restart", "", "")
	if code != http.StatusForbidden {
		t.Fatalf("open port restart: status %d, want 403", code)
	}
	if !json.Valid([]byte(resp)) || !strings.Contains(resp, "FLODNI_API_TOKEN") {
		t.Fatalf("refusal must be JSON naming FLODNI_API_TOKEN, got %q", resp)
	}
	if *restarts != 0 {
		t.Fatal("restart action ran on a refused request")
	}
}

func TestTokenModeAuthenticatedTokenChange(t *testing.T) {
	h, path, _ := controlAPIServer(t, "old-token", "old-token")
	if code, _ := doControl(h, "/api/config", `{"api_token":"new-token"}`, ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated change: status %d, want 401", code)
	}
	if code, resp := doControl(h, "/api/config", `{"api_token":"new-token"}`, "old-token"); code != http.StatusOK {
		t.Fatalf("authenticated change: status %d (%s), want 200", code, resp)
	}
	if tok := fileToken(t, path); tok != "new-token" {
		t.Fatalf("file token = %q, want new-token", tok)
	}
}

func TestTokenModeAuthenticatedRestart(t *testing.T) {
	h, _, restarts := controlAPIServer(t, "tok", "tok")
	if code, _ := doControl(h, "/api/restart", "", ""); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated restart: status %d, want 401", code)
	}
	if *restarts != 0 {
		t.Fatal("restart ran without a token")
	}
	if code, _ := doControl(h, "/api/restart", "", "tok"); code != http.StatusOK {
		t.Fatalf("authenticated restart: status %d, want 200", code)
	}
	if *restarts != 1 {
		t.Fatalf("restart action ran %d times, want 1", *restarts)
	}
}

// The 403 tells the owner to edit config.json and restart. Until that restart the
// memory holds the start-up token (""), so a check against memory would let any open
// save write "" over the hand edit and keep the port open. The save keeps the file's.
func TestOpenPortSaveKeepsAHandEditedToken(t *testing.T) {
	h, path, _ := controlAPIServer(t, "", "")
	if err := os.WriteFile(path, []byte(`{"log_level":"info","api_token":"owner-edit"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, resp := doControl(h, "/api/config", `{"log_level":"debug"}`, ""); code != http.StatusOK {
		t.Fatalf("open save after a hand edit: status %d (%s), want 200", code, resp)
	}
	if tok := fileToken(t, path); tok != "owner-edit" {
		t.Fatalf("open save replaced the hand-edited token with %q", tok)
	}
	// Any posted token is refused, the pending one included: 200 for a match and 403
	// otherwise would answer guesses about it.
	if code, _ := doControl(h, "/api/config", `{"api_token":"owner-edit"}`, ""); code != http.StatusForbidden {
		t.Fatalf("open save naming the pending token: status %d, want 403", code)
	}
}

func TestOpenPortRefusalChecksTheDecodedTokenAndLeavesLiveConfig(t *testing.T) {
	h, path, _ := controlAPIServer(t, "", "")
	before, _ := os.ReadFile(path)
	level := gc().LogLevel
	for _, body := range []string{
		`{"API_TOKEN":"attacker"}`,                     // encoding/json matches keys case-insensitively
		`{"api_token":"","api_token":"attacker"}`,      // the last duplicate wins
		`{"log_level":"debug","Api_Token":"attacker"}`, // a refused save changes nothing live
	} {
		if code, _ := doControl(h, "/api/config", body, ""); code != http.StatusForbidden {
			t.Fatalf("open port %s: status %d, want 403", body, code)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Fatalf("file changed on refusal: %s", after)
	}
	if gc().LogLevel != level {
		t.Fatalf("live log_level changed on a refused save: %q -> %q", level, gc().LogLevel)
	}
}

// A file the daemon cannot parse (an owner mid-edit) would be replaced by defaults,
// dropping a token typed into it. With no token enforced, the save is refused.
func TestOpenPortRefusesSaveOverAnUnparsableFile(t *testing.T) {
	h, path, _ := controlAPIServer(t, "", "")
	broken := []byte(`{"log_level":"info","api_token":"owner-edit",`)
	if err := os.WriteFile(path, broken, 0o600); err != nil {
		t.Fatal(err)
	}
	code, resp := doControl(h, "/api/config", `{"log_level":"debug"}`, "")
	if code != http.StatusConflict || !json.Valid([]byte(resp)) {
		t.Fatalf("open save over an unparsable file: status %d (%q), want 409 with a JSON error", code, resp)
	}
	if after, _ := os.ReadFile(path); string(after) != string(broken) {
		t.Fatalf("unparsable file overwritten: %s", after)
	}
}

// allowed_hosts is like api_token: on an open port any client could widen or clear
// the list the next start enforces, so a changed value is refused. The file is read
// fresh, so a hand edit is compared, not the start-up value.
func TestOpenPortRefusesChangingAllowedHosts(t *testing.T) {
	h, path, _ := controlAPIServer(t, "", "")
	if err := os.WriteFile(path, []byte(`{"log_level":"info","allowed_hosts":["x.example"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadCfg(t) // the running config holds the list too
	globalConfig.Store(&cfg)
	before, _ := os.ReadFile(path)
	for _, body := range []string{
		`{"allowed_hosts":["evil.example"]}`,
		`{"allowed_hosts":["x.example","evil.example"]}`,
		`{"allowed_hosts":[]}`,
		`{"log_level":"debug","allowed_hosts":["evil.example"]}`,
	} {
		code, resp := doControl(h, "/api/config", body, "")
		if code != http.StatusForbidden || !json.Valid([]byte(resp)) || !strings.Contains(resp, "allowed_hosts") {
			t.Fatalf("open port, %s: status %d body %q, want 403 JSON naming allowed_hosts", body, code, resp)
		}
	}
	if after, _ := os.ReadFile(path); string(before) != string(after) {
		t.Fatalf("file changed on refusal:\n%s\n->\n%s", before, after)
	}
}

func TestOpenPortAcceptsSameOrAbsentAllowedHosts(t *testing.T) {
	h, path, _ := controlAPIServer(t, "", "")
	if err := os.WriteFile(path, []byte(`{"log_level":"info","allowed_hosts":["x.example"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"log_level":"debug"}`,
		`{"log_level":"debug","allowed_hosts":["x.example"]}`,
		`{"log_level":"debug","allowed_hosts":["X.Example:9080"]}`,
	} {
		if code, resp := doControl(h, "/api/config", body, ""); code != http.StatusOK {
			t.Fatalf("open port save %s: status %d (%s), want 200", body, code, resp)
		}
		data, _ := os.ReadFile(path)
		var c struct {
			AllowedHosts []string `json:"allowed_hosts"`
		}
		if err := json.Unmarshal(data, &c); err != nil || len(c.AllowedHosts) != 1 || c.AllowedHosts[0] != "x.example" {
			t.Fatalf("after %s the file holds %s, want allowed_hosts [x.example]", body, data)
		}
	}
}

func TestTokenModeAllowsChangingAllowedHosts(t *testing.T) {
	h, path, _ := controlAPIServer(t, "tok", "tok")
	if code, resp := doControl(h, "/api/config", `{"allowed_hosts":["new.example"]}`, "tok"); code != http.StatusOK {
		t.Fatalf("authenticated change: status %d (%s), want 200", code, resp)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `"new.example"`) {
		t.Fatalf("change not written: %s", data)
	}
}

// gostorm_url feeds the engine allowlist, so on an open port a changed host there is
// a changed list: the derived value is gated, not each field.
func TestOpenPortRefusesGostormURLThatWidensTheList(t *testing.T) {
	h, path, _ := controlAPIServer(t, "", "")
	if err := os.WriteFile(path, []byte(`{"gostorm_url":"http://127.0.0.1:8090","allowed_hosts":["x.example"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	code, resp := doControl(h, "/api/config", `{"gostorm_url":"http://attacker.example:8090","allowed_hosts":["x.example"]}`, "")
	if code != http.StatusForbidden || !json.Valid([]byte(resp)) {
		t.Fatalf("changed gostorm_url host: status %d body %q, want 403 JSON", code, resp)
	}
	if after, _ := os.ReadFile(path); string(before) != string(after) {
		t.Fatalf("file changed on refusal:\n%s\n->\n%s", before, after)
	}
	for _, u := range []string{"http://127.0.0.1:9999", "http://10.0.0.5:8090", "http://localhost:8090", "http://127.0.0.1:8090"} {
		body := `{"log_level":"debug","gostorm_url":"` + u + `","allowed_hosts":["x.example"]}`
		if code, resp := doControl(h, "/api/config", body, ""); code != http.StatusOK {
			t.Fatalf("gostorm_url %s leaves the effective set unchanged: status %d (%s), want 200", u, code, resp)
		}
	}
}

// A post equal to the running list is unchanged, so a panel save does not 403 and does
// not overwrite a hand edit that has not been restarted into effect.
func TestOpenPortSaveEqualToRunningListKeepsTheHandEdit(t *testing.T) {
	h, path, _ := controlAPIServer(t, "", "")
	if err := os.WriteFile(path, []byte(`{"allowed_hosts":["old.example"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := loadCfg(t) // the running config: old.example
	globalConfig.Store(&cfg)
	if err := os.WriteFile(path, []byte(`{"allowed_hosts":["old.example","hand.example"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, resp := doControl(h, "/api/config", `{"log_level":"debug","allowed_hosts":["old.example"]}`, ""); code != http.StatusOK {
		t.Fatalf("save equal to the running list: status %d (%s), want 200", code, resp)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "hand.example") {
		t.Fatalf("hand edit lost: %s", data)
	}
}

func TestInvalidPostedAllowedHostsIs400(t *testing.T) {
	for _, tok := range []string{"", "tok"} {
		h, path, _ := controlAPIServer(t, tok, tok)
		before, _ := os.ReadFile(path)
		for _, body := range []string{`{"allowed_hosts":["https://x.example"]}`, `{"allowed_hosts":["ok.example","*.example"]}`, `{"allowed_hosts":[".example.com"]}`} {
			if code, _ := doControl(h, "/api/config", body, tok); code != http.StatusBadRequest {
				t.Fatalf("token %q, %s: status %d, want 400", tok, body, code)
			}
		}
		if after, _ := os.ReadFile(path); string(before) != string(after) {
			t.Fatalf("file changed on a 400:\n%s\n->\n%s", before, after)
		}
	}
}

// A type error in another key makes the file fail to load. Since ADR 0004 a save
// over it is refused (409) in every mode, so the list it holds is never overwritten
// and the file is left as the owner wrote it.
func TestSaveOverATypeErrorFileIs409AndLeavesTheList(t *testing.T) {
	h, path, _ := controlAPIServer(t, "tok", "tok")
	broken := `{"api_token":"tok","allowed_hosts":["x.example"],"metrics_port":"abc"}`
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, resp := doControl(h, "/api/config", `{"log_level":"debug"}`, "tok"); code != http.StatusConflict {
		t.Fatalf("save: status %d (%s), want 409", code, resp)
	}
	if data, _ := os.ReadFile(path); string(data) != broken {
		t.Fatalf("the save changed the file: %s", data)
	}
}
