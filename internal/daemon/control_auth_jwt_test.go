package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"flodni/internal/config"
)

// One key per line: the config loader's comment stripper cuts a line at a second "//".
const jwtBlock = "{\n\"issuer\":\"https://idp.example\",\n\"audience\":\"a\",\n\"jwks_url\":\"https://idp.example/k\",\n\"header\":\"H\"}"
const jwtBlockEdited = "{\n\"issuer\":\"https://idp.example\",\n\"audience\":\"edited\",\n\"jwks_url\":\"https://idp.example/k\",\n\"header\":\"H\"}"
const jwtBlockEvil = "{\n\"issuer\":\"https://evil.example\",\n\"audience\":\"a\",\n\"jwks_url\":\"https://evil.example/k\",\n\"header\":\"H\"}"

func fileAuthJWT(t *testing.T, path string) *config.AuthJWT {
	t.Helper()
	data, _ := os.ReadFile(path)
	var c config.Config
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	return c.AuthJWT
}

// auth_jwt is startup-only (ADR 0004): a save never plants or changes it, in either mode.
func TestSaveRefusesChangingAuthJWT(t *testing.T) {
	for _, tc := range []struct{ name, start string }{{"open port", ""}, {"token mode", "tok"}} {
		t.Run(tc.name, func(t *testing.T) {
			h, path, _ := controlAPIServer(t, tc.start, tc.start)
			// file without a block: planting one is refused
			before, _ := os.ReadFile(path)
			code, resp := doControl(h, "/api/config", `{"log_level":"debug","auth_jwt":`+jwtBlockEvil+`}`, tc.start)
			if code != http.StatusForbidden || !json.Valid([]byte(resp)) || !strings.Contains(resp, "auth_jwt is startup-only") {
				t.Fatalf("plant: status %d body %q, want 403 JSON naming auth_jwt", code, resp)
			}
			if after, _ := os.ReadFile(path); string(before) != string(after) {
				t.Fatalf("file changed on refusal:\n%s\n->\n%s", before, after)
			}
			// file with a block: changing it is refused, the file stays
			body := `{"log_level":"info","auth_jwt":` + jwtBlock + `}`
			if tc.start != "" {
				body = `{"log_level":"info","api_token":"tok","auth_jwt":` + jwtBlock + `}`
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			before, _ = os.ReadFile(path)
			code, _ = doControl(h, "/api/config", `{"auth_jwt":`+jwtBlockEvil+`}`, tc.start)
			if code != http.StatusForbidden {
				t.Fatalf("change: status %d, want 403", code)
			}
			if after, _ := os.ReadFile(path); string(before) != string(after) {
				t.Fatalf("file changed on refusal:\n%s\n->\n%s", before, after)
			}
		})
	}
}

func TestSaveKeepsAuthJWTWhenOmittedOrEqual(t *testing.T) {
	for _, tc := range []struct{ name, start string }{{"open port", ""}, {"token mode", "tok"}} {
		t.Run(tc.name, func(t *testing.T) {
			h, path, _ := controlAPIServer(t, tc.start, tc.start)
			body := `{"log_level":"info","auth_jwt":` + jwtBlock + `}`
			if tc.start != "" {
				body = `{"log_level":"info","api_token":"tok","auth_jwt":` + jwtBlock + `}`
			}
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			for _, post := range []string{
				`{"log_level":"debug"}`,
				`{"log_level":"debug","auth_jwt":` + jwtBlock + `}`,
			} {
				if code, resp := doControl(h, "/api/config", post, tc.start); code != http.StatusOK {
					t.Fatalf("%s: status %d (%s), want 200", post, code, resp)
				}
				got := fileAuthJWT(t, path)
				if got == nil || got.Issuer != "https://idp.example" {
					t.Fatalf("after %s the file holds %+v, want the original block", post, got)
				}
			}
		})
	}
}

// A hand edit of the block that has not been picked up yet must not make every panel
// save fail: the panel echoes the running block, which differs from the file's.
func TestSaveAfterAHandEditEchoingTheRunningBlockKeepsTheEdit(t *testing.T) {
	h, path, _ := controlAPIServer(t, "", "")
	if err := os.WriteFile(path, []byte(`{"log_level":"info","auth_jwt":`+jwtBlock+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	globalConfig.Store(&cfg) // the running block is jwtBlock
	if err := os.WriteFile(path, []byte(`{"log_level":"info","auth_jwt":`+jwtBlockEdited+`}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, resp := doControl(h, "/api/config", `{"log_level":"debug","auth_jwt":`+jwtBlock+`}`, ""); code != http.StatusOK {
		t.Fatalf("echoing the running block: status %d (%s), want 200", code, resp)
	}
	if got := fileAuthJWT(t, path); got == nil || got.Audience != "edited" {
		t.Fatalf("the hand edit was lost: %+v", got)
	}
	if code, _ := doControl(h, "/api/config", `{"auth_jwt":`+jwtBlockEvil+`}`, ""); code != http.StatusForbidden {
		t.Fatalf("a third block: status %d, want 403", code)
	}
}
