package session

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newMgr(t *testing.T, o Options) (*Manager, *clock) {
	t.Helper()
	c := &clock{t: time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)}
	o.Now = c.now
	if o.Active == nil {
		o.Active = func(string) bool { return true }
	}
	m, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return m, c
}

func issue(t *testing.T, m *Manager, r *http.Request, name string) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Issue(rec, r, name)
	cs := rec.Result().Cookies()
	if len(cs) != 1 || cs[0].Name != CookieName {
		t.Fatalf("Issue set %d cookies, want one %s", len(cs), CookieName)
	}
	return cs[0]
}

func plainReq() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://panel.lan:9080/control/login", nil)
	r.Header.Set("Origin", "http://panel.lan:9080")
	return r
}

func withCookie(c *http.Cookie) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
	if c != nil {
		r.AddCookie(c)
	}
	return r
}

// The cookie carries the ADR's attributes: HttpOnly, SameSite=Strict, Path=/, an
// absolute 12 hours, no Domain, and Secure only by the rules below.
func TestIssueAttributes(t *testing.T) {
	m, c := newMgr(t, Options{})
	ck := issue(t, m, plainReq(), "owner")
	if !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode || ck.Path != "/" || ck.Domain != "" {
		t.Errorf("attributes: httponly=%v samesite=%v path=%q domain=%q", ck.HttpOnly, ck.SameSite, ck.Path, ck.Domain)
	}
	if ck.MaxAge != 12*3600 {
		t.Errorf("Max-Age %d, want 43200", ck.MaxAge)
	}
	if want := c.now().Add(12 * time.Hour); !ck.Expires.Equal(want) {
		t.Errorf("Expires %v, want %v", ck.Expires, want)
	}
	if ck.Secure {
		t.Error("a plain-http login must not set Secure")
	}
	if ck.Value == "" || strings.ContainsAny(ck.Value, " ;,\"") {
		t.Errorf("cookie value %q is empty or not cookie-safe", ck.Value)
	}
}

func TestSecureCases(t *testing.T) {
	for _, tc := range []struct {
		name  string
		https bool // Options.PanelHTTPS
		mod   func(*http.Request)
		want  bool
	}{
		{"plain http, http origin", false, func(r *http.Request) {}, false},
		{"no origin, plain", false, func(r *http.Request) { r.Header.Del("Origin") }, false},
		{"request over TLS", false, func(r *http.Request) { r.TLS = tlsState() }, true},
		{"https origin behind a proxy that omits the proto header", false, func(r *http.Request) { r.Header.Set("Origin", "https://panel.lan:9080") }, true},
		{"X-Forwarded-Proto https", false, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "https") }, true},
		{"X-Forwarded-Proto HTTPS", false, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "HTTPS") }, true},
		{"X-Forwarded-Proto http", false, func(r *http.Request) { r.Header.Set("X-Forwarded-Proto", "http") }, false},
		{"auth_panel_https", true, func(r *http.Request) {}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newMgr(t, Options{PanelHTTPS: tc.https})
			r := plainReq()
			tc.mod(r)
			if got := issue(t, m, r, "owner").Secure; got != tc.want {
				t.Errorf("Secure = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestVerifyRoundTrip(t *testing.T) {
	m, _ := newMgr(t, Options{})
	ck := issue(t, m, plainReq(), "owner")
	name, present, err := m.Verify(withCookie(ck))
	if name != "owner" || !present || err != nil {
		t.Fatalf("Verify = %q %v %v, want owner true nil", name, present, err)
	}
}

func TestVerifyNoCookieIsAbsent(t *testing.T) {
	m, _ := newMgr(t, Options{})
	for _, r := range []*http.Request{
		withCookie(nil),
		withCookie(&http.Cookie{Name: "other", Value: "x"}),
		withCookie(&http.Cookie{Name: CookieName, Value: ""}), // a cleared cookie
	} {
		if name, present, err := m.Verify(r); present || err != nil || name != "" {
			t.Errorf("Verify = %q %v %v, want absent", name, present, err)
		}
	}
}

// An absolute 12 hours, no sliding window: Verify never extends it.
func TestExpiry(t *testing.T) {
	m, c := newMgr(t, Options{})
	ck := issue(t, m, plainReq(), "owner")
	c.add(12*time.Hour - time.Second)
	if _, _, err := m.Verify(withCookie(ck)); err != nil {
		t.Fatalf("one second before expiry: %v", err)
	}
	if _, _, err := m.Verify(withCookie(ck)); err != nil { // a use does not slide the window
		t.Fatal(err)
	}
	c.add(time.Second)
	if _, present, err := m.Verify(withCookie(ck)); !present || !errors.Is(err, ErrSession) {
		t.Fatalf("at expiry: present=%v err=%v, want a session refusal", present, err)
	}
	c.add(time.Hour)
	if _, present, err := m.Verify(withCookie(ck)); !present || !errors.Is(err, ErrSession) {
		t.Fatalf("after expiry: present=%v err=%v", present, err)
	}
}

func flip(s string, i int) string {
	b := []byte(s)
	if b[i] == 'A' {
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	return string(b)
}

// Every alteration is refused as present-but-invalid: the gate must not fall through
// to another credential on it.
func TestTamperIsRefused(t *testing.T) {
	m, _ := newMgr(t, Options{})
	good := issue(t, m, plainReq(), "owner").Value
	dot := strings.IndexByte(good, '.')
	if dot < 1 {
		t.Fatalf("value %q has no payload.tag shape", good)
	}
	other := issue(t, m, plainReq(), "guest").Value
	otherPayload := other[:strings.IndexByte(other, '.')]
	cases := map[string]string{
		"flipped payload byte":      flip(good, 0),
		"flipped tag byte":          flip(good, len(good)-2),
		"truncated tag":             good[:len(good)-4],
		"no tag":                    good[:dot],
		"no payload":                good[dot:],
		"no separator":              strings.Replace(good, ".", "", 1),
		"extra segment":             good + ".AAAA",
		"another payload, tag kept": otherPayload + good[dot:],
		"padded base64":             good + "==",
		"not base64":                "!!!!.????",
		"whitespace":                " " + good,
		"standard alphabet":         strings.ReplaceAll(good, "-", "+") + "+/",
		"huge":                      strings.Repeat("A", 100000) + "." + strings.Repeat("A", 100000),
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			got, present, err := m.Verify(withCookie(&http.Cookie{Name: CookieName, Value: v}))
			if !present || !errors.Is(err, ErrSession) || got != "" {
				t.Errorf("Verify = %q %v %v, want present with ErrSession", got, present, err)
			}
		})
	}
}

// Two cookies of the name are ambiguous: refused, never "the first one wins".
func TestDuplicateCookiesRefused(t *testing.T) {
	m, _ := newMgr(t, Options{})
	ck := issue(t, m, plainReq(), "owner")
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(ck)
	r.AddCookie(&http.Cookie{Name: CookieName, Value: "junk"})
	if _, present, err := m.Verify(r); !present || !errors.Is(err, ErrSession) {
		t.Errorf("duplicate: present=%v err=%v", present, err)
	}
}

// The HMAC key is random at each start and never read from anywhere: a second
// manager (a restart) refuses what the first issued.
func TestRestartInvalidates(t *testing.T) {
	m1, _ := newMgr(t, Options{})
	ck := issue(t, m1, plainReq(), "owner")
	m2, _ := newMgr(t, Options{})
	if _, present, err := m2.Verify(withCookie(ck)); !present || !errors.Is(err, ErrSession) {
		t.Fatalf("after restart: present=%v err=%v, want refused", present, err)
	}
	// the new process issues its own, which verifies there and not on the old one
	ck2 := issue(t, m2, plainReq(), "owner")
	if _, _, err := m2.Verify(withCookie(ck2)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m1.Verify(withCookie(ck2)); err == nil {
		t.Fatal("the old process accepted the new process's cookie")
	}
}

// Logout bumps the process-wide generation: every cookie issued before it stops
// verifying, a stolen copy included; one issued after works.
func TestLogoutRevokesEveryCookie(t *testing.T) {
	m, _ := newMgr(t, Options{})
	a := issue(t, m, plainReq(), "owner")
	stolen := *a
	b := issue(t, m, plainReq(), "guest")
	rec := httptest.NewRecorder()
	m.Revoke(rec)
	for _, ck := range []*http.Cookie{a, &stolen, b} {
		if _, present, err := m.Verify(withCookie(ck)); !present || !errors.Is(err, ErrSession) {
			t.Errorf("a cookie from before logout: present=%v err=%v", present, err)
		}
	}
	cs := rec.Result().Cookies()
	if len(cs) != 1 || cs[0].Name != CookieName || cs[0].Value != "" || cs[0].MaxAge >= 0 || !cs[0].HttpOnly || cs[0].Path != "/" {
		t.Errorf("logout must clear the cookie, got %+v", cs)
	}
	c := issue(t, m, plainReq(), "owner")
	if _, _, err := m.Verify(withCookie(c)); err != nil {
		t.Errorf("a cookie issued after logout: %v", err)
	}
}

// The cookie names its token entry, which is re-checked against the store on each
// request: a removed or demoted entry's cookie dies at once.
func TestCookieIsRecheckedAgainstTheStore(t *testing.T) {
	active := true
	m, _ := newMgr(t, Options{Active: func(name string) bool { return name == "owner" && active }})
	ck := issue(t, m, plainReq(), "owner")
	if _, _, err := m.Verify(withCookie(ck)); err != nil {
		t.Fatal(err)
	}
	active = false
	if _, present, err := m.Verify(withCookie(ck)); !present || !errors.Is(err, ErrSession) {
		t.Errorf("entry gone: present=%v err=%v", present, err)
	}
	if _, err := New(Options{}); err == nil {
		t.Error("New without a store check must refuse: a cookie nobody re-checks is a bug")
	}
}

func TestKeysDifferPerManager(t *testing.T) {
	a, _ := newMgr(t, Options{})
	b, _ := newMgr(t, Options{})
	if string(a.key) == string(b.key) || len(a.key) != 32 {
		t.Errorf("keys: len %d, equal %v", len(a.key), string(a.key) == string(b.key))
	}
}

func TestConcurrentIssueVerifyRevoke(t *testing.T) {
	m, _ := newMgr(t, Options{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				ck := issue(t, m, plainReq(), "owner")
				m.Verify(withCookie(ck))
				if j%50 == 0 {
					m.Revoke(httptest.NewRecorder())
				}
			}
		}()
	}
	wg.Wait()
}
