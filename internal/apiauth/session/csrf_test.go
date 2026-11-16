package session

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func post(host string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/control/login", nil)
	r.Host = host
	r.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		if v == "" {
			r.Header.Del(k)
		} else {
			r.Header.Set(k, v)
		}
	}
	return r
}

// The ambient-credential rules of ADR 0004 for a non-GET request: an Origin whose
// host and port equal Host, Sec-Fetch-Site same-origin when present, and a JSON
// content type.
func TestCheckAmbientPost(t *testing.T) {
	type c struct {
		name  string
		host  string
		hdr   map[string]string
		https bool // Options.PanelHTTPS
		tls   bool
		ok    bool
	}
	for _, tc := range []c{
		{name: "same host and port", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080"}, ok: true},
		{name: "host case differs", host: "Panel.LAN:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080"}, ok: true},
		{name: "another port of the same host", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:8080"}},
		{name: "another host", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://evil.example:9080"}},
		{name: "no Origin", host: "panel.lan:9080", hdr: map[string]string{}},
		{name: "null Origin", host: "panel.lan:9080", hdr: map[string]string{"Origin": "null"}},
		{name: "unparsable Origin", host: "panel.lan:9080", hdr: map[string]string{"Origin": "://"}},
		{name: "Origin with a path", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080/x"}},
		{name: "text/plain", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080", "Content-Type": "text/plain"}},
		{name: "form post", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080", "Content-Type": "application/x-www-form-urlencoded"}},
		{name: "no content type", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080", "Content-Type": ""}},
		{name: "json with a charset", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080", "Content-Type": "application/json; charset=utf-8"}, ok: true},
		{name: "json look-alike", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080", "Content-Type": "application/jsonp"}},
		{name: "json suffix type", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080", "Content-Type": "application/vnd.x+json"}},
		{name: "same-origin fetch metadata", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080", "Sec-Fetch-Site": "same-origin"}, ok: true},
		{name: "same-site fetch metadata", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080", "Sec-Fetch-Site": "same-site"}},
		{name: "cross-site fetch metadata", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080", "Sec-Fetch-Site": "cross-site"}},
		{name: "none fetch metadata", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080", "Sec-Fetch-Site": "none"}},
		{name: "empty fetch metadata", host: "panel.lan:9080", hdr: map[string]string{"Origin": "http://panel.lan:9080", "Sec-Fetch-Site": "bogus"}},
		// The TLS tunnel: Flodni sees plain http with the original Host.
		{name: "tunnel, no port on either side", host: "panel.example", hdr: map[string]string{"Origin": "https://panel.example"}, ok: true},
		{name: "tunnel, explicit matching port", host: "panel.example:8443", hdr: map[string]string{"Origin": "https://panel.example:8443"}, ok: true},
		{name: "tunnel, Host without a port, Origin on 8443", host: "panel.example", hdr: map[string]string{"Origin": "https://panel.example:8443"}},
		{name: "tunnel, Host on 8443, Origin without a port", host: "panel.example:8443", hdr: map[string]string{"Origin": "https://panel.example"}},
		{name: "default port spelt out on the Host", host: "panel.example:443", hdr: map[string]string{"Origin": "https://panel.example"}, ok: true},
		{name: "http default port spelt out", host: "panel.example:80", hdr: map[string]string{"Origin": "http://panel.example"}, ok: true},
		{name: "http host on 80, https origin", host: "panel.example:80", hdr: map[string]string{"Origin": "https://panel.example"}},
		// The scheme is not compared, so the other scheme on default ports is only
		// separated by fetch metadata.
		{name: "http origin, no ports, no fetch metadata", host: "panel.example", hdr: map[string]string{"Origin": "http://panel.example"}, ok: true},
		{name: "http origin, no ports, same-site", host: "panel.example", hdr: map[string]string{"Origin": "http://panel.example", "Sec-Fetch-Site": "same-site"}},
		// Known https: the Origin scheme must be https too.
		{name: "panel known https by config, http origin", host: "panel.example", hdr: map[string]string{"Origin": "http://panel.example"}, https: true},
		{name: "panel known https by config, https origin", host: "panel.example", hdr: map[string]string{"Origin": "https://panel.example"}, https: true, ok: true},
		{name: "known https by header, http origin", host: "panel.example", hdr: map[string]string{"Origin": "http://panel.example", "X-Forwarded-Proto": "https"}},
		{name: "known https by header, https origin", host: "panel.example", hdr: map[string]string{"Origin": "https://panel.example", "X-Forwarded-Proto": "https"}, ok: true},
		{name: "known https by TLS, http origin", host: "panel.example", hdr: map[string]string{"Origin": "http://panel.example"}, tls: true},
		{name: "known https by TLS, https origin", host: "panel.example", hdr: map[string]string{"Origin": "https://panel.example"}, tls: true, ok: true},
		{name: "ipv6 host", host: "[::1]:9080", hdr: map[string]string{"Origin": "http://[::1]:9080"}, ok: true},
		{name: "ipv6 other port", host: "[::1]:9080", hdr: map[string]string{"Origin": "http://[::1]:9081"}},
		{name: "empty host", host: "", hdr: map[string]string{"Origin": "http://panel.lan"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _ := newMgr(t, Options{PanelHTTPS: tc.https})
			r := post(tc.host, tc.hdr)
			if tc.tls {
				r.TLS = tlsState()
			}
			err := m.CheckAmbient(r)
			if tc.ok && err != nil {
				t.Errorf("refused: %v", err)
			}
			if !tc.ok && err == nil {
				t.Error("accepted, want refused")
			}
		})
	}
}

func TestCheckAmbientRepeatedHeaders(t *testing.T) {
	m, _ := newMgr(t, Options{})
	r := post("panel.lan:9080", map[string]string{"Origin": "http://panel.lan:9080"})
	r.Header.Add("Origin", "http://panel.lan:9080")
	if m.CheckAmbient(r) == nil {
		t.Error("two Origin headers accepted")
	}
	r = post("panel.lan:9080", map[string]string{"Origin": "http://panel.lan:9080"})
	r.Header.Add("Content-Type", "text/plain")
	if m.CheckAmbient(r) == nil {
		t.Error("a second, text/plain Content-Type accepted")
	}
}

// A GET with an ambient credential keeps the existing cross-site refusal only.
func TestCheckAmbientGet(t *testing.T) {
	m, _ := newMgr(t, Options{})
	for site, ok := range map[string]bool{"": true, "same-origin": true, "none": true, "same-site": true, "cross-site": false} {
		r := httptest.NewRequest(http.MethodGet, "/api/sessions", nil)
		if site != "" {
			r.Header.Set("Sec-Fetch-Site", site)
		}
		if got := m.CheckAmbient(r) == nil; got != ok {
			t.Errorf("GET with Sec-Fetch-Site %q: accepted=%v, want %v", site, got, ok)
		}
	}
}
