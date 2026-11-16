package redact

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

const key = "abcdef0123456789abcdef0123456789"

// assertClean fails when out still holds a secret.
func assertClean(t *testing.T, name, out string, secrets ...string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(out, s) {
			t.Errorf("%s: %q still contains %q", name, out, s)
		}
	}
}

func TestStringURLs(t *testing.T) {
	cases := []struct {
		name, in, want string
		secrets        []string
	}{
		{"path passkey", "add https://tracker.example/" + key + "/announce failed", "add https://tracker.example/<redacted> failed", []string{key}},
		{"announce then key", "https://tracker.example/announce/" + key, "https://tracker.example/<redacted>", []string{key}},
		{"query passkey", "https://tracker.example:8443/announce?passkey=" + key + "&info_hash=zz", "https://tracker.example:8443/<redacted>", []string{key, "info_hash"}},
		{"userinfo", "https://alice:" + key + "@tracker.example/announce", "https://tracker.example/<redacted>", []string{key, "alice"}},
		{"userinfo only", "https://" + key + "@tracker.example", "https://tracker.example/<redacted>", []string{key}},
		{"udp", "announcing to \"udp://tracker.example:6969/" + key + "\": x", "announcing to \"udp://tracker.example:6969/<redacted>\": x", []string{key}},
		{"ws", "wss://tracker.example/" + key + "/announce", "wss://tracker.example/<redacted>", []string{key}},
		{"trailing colon", "dial https://tracker.example/" + key + ": refused", "dial https://tracker.example/<redacted>: refused", []string{key}},
		{"bare host untouched", "https://tracker.example/ and http://tracker.example", "https://tracker.example/ and http://tracker.example", nil},
		{"no url untouched", "nothing to hide, dial tcp 10.0.0.1:80", "nothing to hide, dial tcp 10.0.0.1:80", nil},
		{"escaped quote", `msg="announcing to \"https://tracker.example/` + key + `/announce\": x"`, `msg="announcing to \"https://tracker.example/<redacted>\": x"`, []string{key}},
		{"torrs token", "add torrs://" + key + "x done", "add torrs://<redacted> done", []string{key}},
		{"stray key", "retry uk=" + key + " now", "retry uk=*** now", []string{key}},
	}
	for _, c := range cases {
		got := String(c.in)
		if got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		assertClean(t, c.name, got, c.secrets...)
	}
}

func TestStringMagnet(t *testing.T) {
	tr1 := url.QueryEscape("https://tracker.example/" + key + "/announce")
	tr2 := url.QueryEscape("udp://other.example:1337/announce?passkey=" + key)
	xs := url.QueryEscape("https://files.example/dl/" + key + ".torrent")
	in := "add magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&dn=Name&tr=" + tr1 + "&tr=" + tr2 + "&xs=" + xs + " ok"
	got := String(in)
	assertClean(t, "magnet", got, key)
	for _, keep := range []string{"xt=urn:btih:0123456789abcdef0123456789abcdef01234567", "dn=Name", "tracker.example", "other.example:1337", "files.example", " ok"} {
		if !strings.Contains(got, keep) {
			t.Errorf("magnet: %q lost %q", got, keep)
		}
	}
	// a raw (unescaped) tracker value is redacted too, and the rest survives
	raw := String("magnet:?xt=urn:btih:aa&tr=https://t.example/" + key + "/announce&dn=X")
	assertClean(t, "raw magnet", raw, key)
	if !strings.Contains(raw, "dn=X") || !strings.Contains(raw, "t.example") {
		t.Errorf("raw magnet lost data: %q", raw)
	}
	// an unparsable tr value is dropped whole
	odd := String("magnet:?xt=urn:btih:aa&tr=" + key)
	assertClean(t, "odd tr", odd, key)
}

// The error a real http.Client returns embeds the full URL; the redactor has to cope
// with that text as it comes, not with a URL it parsed itself.
func TestStringHTTPClientError(t *testing.T) {
	target := "https://tracker.example/" + key + "/announce?passkey=" + key
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Dial a closed local port whatever the host says: the URL stays in the error.
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialClosed(ctx, network)
		},
	}}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("request to an unreachable address succeeded")
	}
	var ue *url.Error
	if !errors.As(err, &ue) || !strings.Contains(err.Error(), key) {
		t.Fatalf("test premise: the client error should embed the URL, got %q", err)
	}
	for name, got := range map[string]string{"String": String(err.Error()), "Err": Err(err)} {
		assertClean(t, name, got, key, "passkey")
		if !strings.Contains(got, "https://tracker.example/<redacted>") {
			t.Errorf("%s: %q lost the host", name, got)
		}
	}
	if Err(nil) != "" {
		t.Error("Err(nil) should be empty")
	}
}

// dialClosed connects to a local port nobody listens on.
func dialClosed(ctx context.Context, network string) (net.Conn, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	addr := l.Addr().String()
	_ = l.Close()
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

func TestStringQueryToken(t *testing.T) {
	for _, in := range []string{
		"GET /stream?link=x&token=SECRET&index=1",
		`400 | 1.2.3.4 | GET "/webhook?token=SECRET"`,
		`line with \"/webhook?token=SECRET\" quoted`,
		"token=SECRET",
		"/x?access_token=SECRET#frag",
		"/x?token%3DSECRET",
		"/x?TOKEN=SECRET&token=SECRET",
		`Get "http://host:8090/stream?token=SECRET": dial tcp: refused`,
	} {
		out := String(in)
		assertClean(t, in, out, "SECRET")
	}
	if got, want := String("/p?a=1&token=SECRET&b=2"), "/p?a=1&token=<redacted>&b=2"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := String("/p?token=A&token=B"), "/p?token=<redacted>&token=<redacted>"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	// Unrelated text and words that merely end in "token" are left alone.
	for _, in := range []string{"no secrets here", "/p?csrftoken=1", "tokens=3", "token missing"} {
		if got := String(in); got != in {
			t.Errorf("%q changed to %q", in, got)
		}
	}
	// Idempotent.
	once := String("/p?token=SECRET")
	if String(once) != once {
		t.Errorf("not idempotent: %q", String(once))
	}
}

func TestStringHeaders(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"Authorization: Bearer SECRET", "Authorization: Bearer <redacted>"},
		{"authorization:bearer SECRET", "authorization:bearer <redacted>"},
		{"Authorization: Basic SECRET", "Authorization: Basic <redacted>"},
		{"Authorization: SECRET", "Authorization: <redacted>"},
		{"Cookie: flodni_session=SECRET; other=SECRET", "Cookie: <redacted>"},
		{"Set-Cookie: s=SECRET; HttpOnly", "Set-Cookie: <redacted>"},
		{"GET / HTTP/1.1\r\nCookie: a=SECRET\r\nHost: x", "GET / HTTP/1.1\r\nCookie: <redacted>\r\nHost: x"},
	} {
		got := String(tc.in)
		assertClean(t, tc.in, got, "SECRET")
		if got != tc.want {
			t.Errorf("%q: got %q, want %q", tc.in, got, tc.want)
		}
	}
}
