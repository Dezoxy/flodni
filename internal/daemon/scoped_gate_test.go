package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"flodni/internal/apiauth"
	"flodni/internal/apiauth/authtest"
	"flodni/internal/config"
	"flodni/internal/flog"
	server "flodni/internal/gostorm"
	"flodni/internal/gostorm/web"
	"flodni/internal/promtext"
)

// scopedFixture is the control server as main builds it (the real registrar, the
// real gate constructor), in scoped mode, over a config.json that names the tokens
// file. Only the handlers other than config and restart are stubs.
type scopedFixture struct {
	fx      *authtest.Fixture
	h       http.Handler
	cfgPath string
	logs    *bytes.Buffer
	reached *[]string
}

func newScopedFixture(t *testing.T, extraConfig string) *scopedFixture {
	t.Helper()
	fx := authtest.New(t)
	cfgPath := filepath.Join(t.TempDir(), "config.json")
	body := `{"log_level":"info","gostorm_url":"http://127.0.0.1:8090","auth_tokens_file":` + jsonString(fx.Path) + extraConfig + `}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MKV_PROXY_CONFIG_PATH", cfgPath)
	t.Setenv("FLODNI_API_TOKEN", "")
	t.Setenv("FLODNI_AUTH_TOKENS_FILE", "")
	a := newTestApp(t)
	a.auth = fx.Tokens
	prevCfg, prevLog, prevRestart := globalConfig.Load(), logger, restartSelf
	cfg := loadCfg(t)
	globalConfig.Store(&cfg)
	logs := &bytes.Buffer{}
	logger = flog.NewText(logs)
	restartSelf = func() {}
	t.Cleanup(func() { globalConfig.Store(prevCfg); logger, restartSelf = prevLog, prevRestart })

	var reached []string
	var h controlHandlers
	v := reflect.ValueOf(&h).Elem()
	stub := func(w http.ResponseWriter, r *http.Request) {
		reached = append(reached, r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}
	for i := 0; i < v.NumField(); i++ {
		v.Field(i).Set(reflect.ValueOf(stub))
	}
	h.Config, h.Restart = handleConfig, handleRestart
	routes := apiauth.NewRegistrar()
	registerControlRoutes(routes, h)
	return &scopedFixture{fx: fx, h: newControlGate(routes, a.auth, nil, gc()), cfgPath: cfgPath, logs: logs, reached: &reached}
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func (s *scopedFixture) do(method, target, bearer, body string) (int, string) {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = "panel.example:9080"
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	s.h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// The real control mux behind the real gate, every row of the golden copy, against
// each scope alone, a two-scope token, nothing and a wrong token.
func TestControlMuxEnforcesTheScopesInScopedMode(t *testing.T) {
	s := newScopedFixture(t, "")
	for _, row := range authtest.Golden(t) {
		if row.Server != "control" {
			continue
		}
		for _, cred := range []string{"", "wrong", "admin", "engine", "library", "metrics", "read", "scrape", "upload", "webhook", "readlib"} {
			bearer := s.fx.Token[cred]
			if cred == "wrong" {
				bearer = s.fx.Unknown()
			}
			before := len(*s.reached)
			code, _ := s.do(row.Method, authtest.Path(row.Pattern), bearer, "")
			reached := len(*s.reached) > before
			switch {
			case row.Scope == "open":
				if !reached {
					t.Errorf("%s %s as %q: open route not served (%d)", row.Method, row.Pattern, cred, code)
				}
			case cred == "" || cred == "wrong":
				if reached || code != 401 {
					t.Errorf("%s %s as %q: %d reached=%v, want 401", row.Method, row.Pattern, cred, code, reached)
				}
			case s.fx.Allows(cred, row.Scope):
				// config and restart are the real handlers; the rest are stubs.
				if row.Pattern == "/api/config" || row.Pattern == "/api/restart" {
					if code == 401 || code == 403 && row.Pattern == "/api/restart" {
						t.Errorf("%s %s as %q: refused %d", row.Method, row.Pattern, cred, code)
					}
				} else if !reached {
					t.Errorf("%s %s as %q: %d, want served", row.Method, row.Pattern, cred, code)
				}
			default:
				if reached || code != 403 {
					t.Errorf("%s %s as %q: %d reached=%v, want 403", row.Method, row.Pattern, cred, code, reached)
				}
			}
		}
	}
}

// Scoped mode is "auth enforced": the routes that fail closed without a token (restart,
// api_token and plex writes, confirm) are open to an authorised caller.
func TestScopedModeCountsAsAuthEnforced(t *testing.T) {
	s := newScopedFixture(t, "")
	if !apiTokenSet() {
		t.Error("apiTokenSet (the confirm gate) is false in scoped mode")
	}
	if code, body := s.do("POST", "/api/restart", s.fx.Token["admin"], ""); code != 200 {
		t.Errorf("restart as admin: %d %s, want 200", code, body)
	}
}

// ---- startup-only settings ----

func readCfgFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestScopedSaveRefusesStartupOnlySettings(t *testing.T) {
	s := newScopedFixture(t, "")
	admin := s.fx.Token["admin"]
	before := readCfgFile(t, s.cfgPath)
	for name, body := range map[string]string{
		"api_token":              `{"log_level":"debug","api_token":"` + strings.Repeat("x", 12) + `"}`,
		"another tokens file":    `{"auth_tokens_file":"/elsewhere/tokens.json"}`,
		"auth_jwt block":         `{"auth_jwt":{"issuer":"https://i.example","audience":"a","jwks_url":"https://i.example/k","header":"H"}}`,
		"auth_panel_https":       `{"auth_panel_https":true}`,
		"non-loopback engine":    `{"gostorm_url":"http://192.0.2.5:8090"}`,
		"engine on a name":       `{"gostorm_url":"http://engine.example:8090"}`,
		"engine at 0.0.0.0":      `{"gostorm_url":"http://0.0.0.0:8090"}`,
		"engine userinfo trick":  `{"gostorm_url":"http://127.0.0.1@evil.example:8090"}`,
		"engine on an ipv6 lan":  `{"gostorm_url":"http://[fd00::1]:8090"}`,
		"engine prefix of local": `{"gostorm_url":"http://localhost.evil.example:8090"}`,
	} {
		code, resp := s.do("POST", "/api/config", admin, body)
		if code != http.StatusForbidden || !json.Valid([]byte(resp)) {
			t.Errorf("%s: status %d body %q, want a 403 JSON refusal", name, code, resp)
		}
		if after := readCfgFile(t, s.cfgPath); after != before {
			t.Errorf("%s: the file changed on a refusal:\n%s\n->\n%s", name, before, after)
		}
	}
}

const scopedJWTBlock = `{"issuer":"https://i.example","audience":"a","jwks_url":"https://i.example/k","header":"H","allowed_emails":%s}`

// echoWith is what the panel posts: the running settings, with one real change.
func echoWith(t *testing.T, mutate func(map[string]any)) string {
	t.Helper()
	running, _ := json.Marshal(gc().Redacted())
	var post map[string]any
	_ = json.Unmarshal(running, &post)
	mutate(post)
	body, _ := json.Marshal(post)
	return string(body)
}

func savedFile(t *testing.T, path string) map[string]any {
	t.Helper()
	var saved map[string]any
	if err := json.Unmarshal([]byte(readCfgFile(t, path)), &saved); err != nil {
		t.Fatal(err)
	}
	return saved
}

// A scoped save writes back the file's current value of the startup-only settings, as
// the other modes do: an owner who tightened auth_jwt.allowed_emails by hand and has not
// restarted must not have a panel save restore the looser list.
func TestScopedSaveKeepsAHandEditedAuthJWT(t *testing.T) {
	s := newScopedFixture(t, `,"auth_jwt":`+fmt.Sprintf(scopedJWTBlock, `["a@x.example","b@x.example"]`))
	edited := strings.Replace(readCfgFile(t, s.cfgPath), `["a@x.example","b@x.example"]`, `["a@x.example"]`, 1)
	if err := os.WriteFile(s.cfgPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	// The echoed running block differs from the file's now, and is accepted.
	body := echoWith(t, func(m map[string]any) { m["log_level"] = "debug" })
	if code, resp := s.do("POST", "/api/config", s.fx.Token["admin"], body); code != http.StatusOK {
		t.Fatalf("save with the running block: %d %s", code, resp)
	}
	saved := savedFile(t, s.cfgPath)
	jwt, _ := saved["auth_jwt"].(map[string]any)
	if emails, _ := jwt["allowed_emails"].([]any); len(emails) != 1 {
		t.Errorf("allowed_emails = %v, want the hand-edited list of one", jwt["allowed_emails"])
	}
	// The block the file holds now is accepted too.
	body = echoWith(t, func(m map[string]any) {
		var blk any
		_ = json.Unmarshal([]byte(fmt.Sprintf(scopedJWTBlock, `["a@x.example"]`)), &blk)
		m["auth_jwt"] = blk
	})
	if code, resp := s.do("POST", "/api/config", s.fx.Token["admin"], body); code != http.StatusOK {
		t.Errorf("save with the file's block: %d %s", code, resp)
	}
}

func TestScopedSaveKeepsAHandEditedPanelHTTPS(t *testing.T) {
	s := newScopedFixture(t, "")
	edited := strings.TrimSuffix(strings.TrimSpace(readCfgFile(t, s.cfgPath)), "}") + `,"auth_panel_https":true}`
	if err := os.WriteFile(s.cfgPath, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	body := echoWith(t, func(m map[string]any) { m["log_level"] = "debug" })
	if code, resp := s.do("POST", "/api/config", s.fx.Token["admin"], body); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, resp)
	}
	saved := savedFile(t, s.cfgPath)
	if saved["auth_panel_https"] != true {
		t.Errorf("auth_panel_https = %v, want the hand-edited true", saved["auth_panel_https"])
	}
	if saved["log_level"] != "debug" {
		t.Error("the real change was not saved")
	}
}

// A value that is neither the running one nor the file's current one is still refused,
// and a hand-planted api_token never rides along.
func TestScopedSaveRefusesAThirdValueAndKeepsNoPlantedToken(t *testing.T) {
	s := newScopedFixture(t, `,"auth_jwt":`+fmt.Sprintf(scopedJWTBlock, `["a@x.example"]`))
	admin := s.fx.Token["admin"]
	before := readCfgFile(t, s.cfgPath)
	for name, mutate := range map[string]func(map[string]any){
		"auth_jwt": func(m map[string]any) {
			var blk any
			_ = json.Unmarshal([]byte(fmt.Sprintf(scopedJWTBlock, `["a@x.example","evil@x.example"]`)), &blk)
			m["auth_jwt"] = blk
		},
		"auth_panel_https": func(m map[string]any) { m["auth_panel_https"] = true },
		"auth_tokens_file": func(m map[string]any) { m["auth_tokens_file"] = "/elsewhere/tokens.json" },
	} {
		if code, _ := s.do("POST", "/api/config", admin, echoWith(t, mutate)); code != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", name, code)
		}
		if after := readCfgFile(t, s.cfgPath); after != before {
			t.Errorf("%s: the file changed on a refusal", name)
		}
	}
	planted := strings.TrimSuffix(strings.TrimSpace(before), "}") + `,"api_token":"planted-token"}`
	if err := os.WriteFile(s.cfgPath, []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	body := echoWith(t, func(m map[string]any) { m["log_level"] = "debug" })
	if code, resp := s.do("POST", "/api/config", admin, body); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, resp)
	}
	if v, ok := savedFile(t, s.cfgPath)["api_token"]; ok && v != "" {
		t.Errorf("api_token = %v: a hand-planted token survived the save", v)
	}
}

func TestScopedSaveOverABrokenFileIs409(t *testing.T) {
	s := newScopedFixture(t, "")
	if err := os.WriteFile(s.cfgPath, []byte(`{"log_level": `), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _ := s.do("POST", "/api/config", s.fx.Token["admin"], `{"log_level":"debug"}`); code != http.StatusConflict {
		t.Errorf("status %d, want 409", code)
	}
}

func TestScopedSaveAcceptsALoopbackEngine(t *testing.T) {
	s := newScopedFixture(t, "")
	for _, u := range []string{"http://127.0.0.1:8090", "http://localhost:8090", "http://[::1]:8090", "http://127.0.0.2:9"} {
		running, _ := json.Marshal(gc().Redacted())
		var post map[string]any
		_ = json.Unmarshal(running, &post)
		post["gostorm_url"] = u
		body, _ := json.Marshal(post)
		if code, resp := s.do("POST", "/api/config", s.fx.Token["admin"], string(body)); code != http.StatusOK {
			t.Errorf("%s: %d %s", u, code, resp)
		}
	}
}

// ---- the refusal metric ----

func TestRefusalMetricIsEmittedWithBoundedLabels(t *testing.T) {
	var prev apiauth.Refusals
	prev.Observe("/api/sessions", apiauth.ScopeRead, "scope")
	authRefusals.Observe("/api/restart", apiauth.ScopeAdmin, "missing")
	authRefusals.Observe("pre_route", "", "invalid")
	authRefusals.Observe("/some/raw/path/a1b2c3", "x", "made-up")
	var w promtext.Writer
	collectAuthRefusalsProm(&w)
	out := string(w.Bytes())
	if err := promtext.Lint(w.Bytes()); err != nil {
		t.Fatalf("lint: %v\n%s", err, out)
	}
	for _, want := range []string{
		"# TYPE flodni_auth_refusals_total counter",
		`flodni_auth_refusals_total{route="/api/restart",scope="admin",reason="missing"}`,
		`flodni_auth_refusals_total{route="pre_route",scope="",reason="invalid"}`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	for _, bad := range []string{"a1b2c3", "made-up", `scope="x"`} {
		if strings.Contains(out, bad) {
			t.Errorf("an unbounded label reached the metric: %q", bad)
		}
	}
	// The scrape route carries it.
	var all promtext.Writer
	collectProm(&all)
	if !strings.Contains(string(all.Bytes()), "flodni_auth_refusals_total") {
		t.Error("collectProm does not emit the refusal family")
	}
}

// ---- the engine self-calls ----

// In scoped mode Flodni's own engine calls go in-process: a changed gostorm_url, or a
// redirect, cannot send a request (or a credential) anywhere.
func TestSelfCallsInScopedModeNeverDial(t *testing.T) {
	hit := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit <- r.Header.Get("Authorization") + "|" + r.URL.Path
	}))
	t.Cleanup(srv.Close)
	s := newScopedFixture(t, "")
	cfg := *gc()
	cfg.GoStormBaseURL = srv.URL
	globalConfig.Store(&cfg)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/gostorm/settings", strings.NewReader(`{"action":"get"}`))
	gostormSettingsProxy(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("the proxy answered %d with no engine up; want 502, and no dial", rec.Code)
	}
	select {
	case got := <-hit:
		t.Fatalf("the proxy dialled gostorm_url: %q", got)
	default:
	}
	_ = s
}

func TestEngineClientIsInProcessOnlyInScopedMode(t *testing.T) {
	if got := engineHTTPClient(apiauth.ModeScoped, 0); got.Transport == nil {
		t.Error("scoped mode must use the in-process transport")
	}
	for _, m := range []apiauth.Mode{apiauth.ModeOpen, apiauth.ModeLegacyToken} {
		if engineHTTPClient(m, 0).Transport != nil {
			t.Errorf("%s must keep the default transport: a remote engine still works there", m)
		}
	}
}

// An admin who posts only the setting they change must not leave gostorm_url empty in
// the file: the next start would refuse it. Empty means "keep the running value".
func TestScopedSavePartialBodyKeepsTheEngineURL(t *testing.T) {
	s := newScopedFixture(t, "")
	if code, resp := s.do("POST", "/api/config", s.fx.Token["admin"], `{"log_level":"debug"}`); code != http.StatusOK {
		t.Fatalf("save: %d %s", code, resp)
	}
	var saved map[string]any
	if err := json.Unmarshal([]byte(readCfgFile(t, s.cfgPath)), &saved); err != nil {
		t.Fatal(err)
	}
	if saved["gostorm_url"] != "http://127.0.0.1:8090" {
		t.Errorf("gostorm_url = %v, want the running value kept", saved["gostorm_url"])
	}
	cfg, err := config.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := apiauth.RequireLoopback(cfg.GoStormBaseURL); err != nil {
		t.Errorf("the saved file would not start: %v", err)
	}
}

// 8090 enforces the same tokens as 9080 through the wiring Run uses (configureEngineAuth),
// and a missing store closes the engine instead of opening it.
func TestEngineAuthWiringEnforcesTheSameTokensAsTheControlPort(t *testing.T) {
	s := newScopedFixture(t, "")
	prevAuth, prevClosed, prevTok := web.Auth, web.AuthClosed, web.APIToken
	t.Cleanup(func() { web.Auth, web.AuthClosed, web.APIToken = prevAuth, prevClosed, prevTok })
	configureEngineAuth(app.auth, gc(), logger)
	eng := server.Handler()
	engineCode := func(method, target, bearer, body string) int {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		eng.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, cred := range []string{"", "wrong", "webhook", "metrics", "read", "library", "scrape"} {
		bearer := s.fx.Token[cred]
		if cred == "wrong" {
			bearer = s.fx.Unknown()
		}
		want := 403
		if cred == "" || cred == "wrong" {
			want = 401
		}
		if got := engineCode("POST", "/torrents", bearer, `{"action":"list"}`); got != want {
			t.Errorf("engine /torrents as %q: %d, want %d", cred, got, want)
		}
		if got := engineCode("POST", "/shutdown", bearer, ""); got != want {
			t.Errorf("engine /shutdown as %q: %d, want %d", cred, got, want)
		}
		// The control port refuses the same credential on a route of the same class.
		if code, _ := s.do("POST", "/api/restart", bearer, ""); code != want {
			t.Errorf("control /api/restart as %q: %d, want %d", cred, code, want)
		}
	}
	// An engine token passes the gate (400: the cache route wants a hash), and /echo is open.
	if got := engineCode("POST", "/cache", s.fx.Token["engine"], `{"action":"get"}`); got != http.StatusBadRequest {
		t.Errorf("engine token on /cache: %d, want it past the gate", got)
	}
	if got := engineCode("GET", "/echo", "", ""); got != http.StatusOK {
		t.Errorf("/echo: %d", got)
	}
	// No store: nothing is answered.
	configureEngineAuth(nil, gc(), logger)
	eng = server.Handler()
	if got := engineCode("GET", "/echo", "", ""); got != http.StatusServiceUnavailable {
		t.Errorf("no store, /echo: %d, want 503", got)
	}
}
