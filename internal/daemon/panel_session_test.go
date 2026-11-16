package daemon

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"flodni/internal/apiauth"
)

// The panel never sends an empty bearer (ADR 0004): the header goes out only when a
// token is held, so a cookie request is not refused as "a wrong credential".
func TestPanelSendsNoEmptyBearer(t *testing.T) {
	html := string(settingsHTML)
	if strings.Contains(html, "|| '')") && strings.Contains(html, "'Bearer ' +") {
		t.Error("api() still builds `Bearer ` + (token || '')")
	}
	if !regexp.MustCompile(`if \(token\)[^\n]*Authorization`).MatchString(html) {
		t.Error("api() must set Authorization only inside `if (token)`")
	}
}

// A 401 that carries Flodni-Auth: login shows the sign-in form that posts to
// /control/login; without it the bearer prompt stays (api_token-only mode).
func TestPanelHasASignInPrompt(t *testing.T) {
	html := string(settingsHTML)
	for _, want := range []string{
		"Flodni-Auth",
		"'/control/login'",
		"'/control/logout'",
		`type="password"`,
		"This flodni requires an API token", // the bearer prompt is kept for api_token-only mode
	} {
		if !strings.Contains(html, want) {
			t.Errorf("settings.html lacks %q", want)
		}
	}
	// the sign-in posts JSON with the headers the CSRF rules need, and never keeps
	// the token it was given
	i := strings.Index(html, "function signIn()")
	j := strings.Index(html, "async function signOut")
	if i < 0 || j < i {
		t.Fatal("signIn() not found")
	}
	body := html[i:j]
	if !strings.Contains(body, "'Content-Type': 'application/json'") {
		t.Error("signIn() must post application/json")
	}
	if strings.Contains(body, "localStorage") {
		t.Error("the sign-in must not store the token in local storage")
	}
}

// Every non-GET the panel makes through api() is JSON, as the cookie's CSRF rule
// requires; the body-less restart POST included.
func TestPanelPostsAreJSON(t *testing.T) {
	html := string(settingsHTML)
	i := strings.Index(html, "async function api(")
	j := strings.Index(html, "async function restartFlodni")
	if i < 0 || j < i {
		t.Fatal("api() not found")
	}
	if body := html[i:j]; !strings.Contains(body, "'Content-Type'") || !strings.Contains(body, "application/json") {
		t.Error("api() must default Content-Type to application/json for a non-GET")
	}
}

// The sign-in dialog is single-flight, sign-out is a plain fetch, and a stale stored
// bearer is dropped and retried once before the dialog opens.
func TestPanelSignInIsSingleFlightAndSignOutIsPlain(t *testing.T) {
	html := string(settingsHTML)
	i := strings.Index(html, "function signIn()")
	j := strings.Index(html, "function askAndSignIn()")
	if i < 0 || j < i {
		t.Fatal("signIn() or askAndSignIn() not found")
	}
	if body := html[i:j]; !strings.Contains(body, "signInInFlight") || !strings.Contains(body, "askAndSignIn()") || strings.Contains(body, "showModal") {
		t.Error("signIn() must hand every caller the one in-flight promise and not open a dialog itself")
	}
	k := strings.Index(html, "async function signOut")
	l := strings.Index(html, "async function api(")
	if k < 0 || l < k {
		t.Fatal("signOut() not found")
	}
	out := html[k:l]
	if !strings.Contains(out, "fetch('/control/logout'") || strings.Contains(out, "await api(") {
		t.Error("signOut() must use a plain fetch, not api()")
	}
	body := html[l:]
	m := strings.Index(body, "Flodni-Auth")
	if m < 0 {
		t.Fatal("api() does not read Flodni-Auth")
	}
	seg := body[m:]
	rm := strings.Index(seg, "localStorage.removeItem(TOKEN_KEY)")
	retry := strings.Index(seg, "await send()")
	ask := strings.Index(seg, "await signIn()")
	if rm < 0 || retry < rm || ask < retry {
		t.Error("api() must drop a stale bearer and retry once without it before it asks to sign in")
	}
}

func scopedAuthForTest(t *testing.T) (*apiauth.Tokens, string) {
	t.Helper()
	tok, hash, err := apiauth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "tokens.json")
	body := `{"tokens":[{"name":"owner","scopes":["admin"],"sha256":"` + hash + `"}]}`
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := apiauth.Load(apiauth.LoadOptions{TokensFile: p, GoStormURL: "http://127.0.0.1:8090"})
	if err != nil {
		t.Fatal(err)
	}
	return a, tok
}

// The real login and logout handlers are on the real control mux through the
// registrar, in scoped mode.
func TestControlMuxServesLoginAndLogout(t *testing.T) {
	a, tok := scopedAuthForTest(t)
	var buf bytes.Buffer
	sess := newControlSession(a, slog.New(slog.NewTextHandler(&buf, nil)), false)
	var h controlHandlers
	stub := func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) }
	h = fillStubs(h, stub)
	h.Login, h.Logout = sess.Login, sess.Logout
	routes := apiauth.NewRegistrar()
	registerControlRoutes(routes, h)

	req := httptest.NewRequest(http.MethodPost, "/control/login", strings.NewReader(`{"token":"`+tok+`"}`))
	req.Host = "panel.lan:9080"
	req.Header.Set("Origin", "http://panel.lan:9080")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	routes.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || len(rec.Result().Cookies()) != 1 {
		t.Fatalf("login through the mux: %d", rec.Code)
	}
	if strings.Contains(buf.String(), tok) {
		t.Error("the token is in the log")
	}
	if got := routes.Pattern(req); got != "/control/login" {
		t.Errorf("the router names %q", got)
	}
	// the page stays GET-only open: /control is the exact pattern, /control/login is not a prefix of it
	if p := routes.Pattern(httptest.NewRequest(http.MethodGet, "/control", nil)); p != "/control" {
		t.Errorf("/control routes to %q", p)
	}
}

// fillStubs sets every handler field of h to stub, by reflection, as stubControl does.
func fillStubs(h controlHandlers, stub func(http.ResponseWriter, *http.Request)) controlHandlers {
	v := reflect.ValueOf(&h).Elem()
	for i := 0; i < v.NumField(); i++ {
		v.Field(i).Set(reflect.ValueOf(stub))
	}
	return h
}
