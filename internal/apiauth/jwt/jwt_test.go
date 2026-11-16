package jwt

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"flodni/internal/config"
)

const (
	testIss = "https://idp.example"
	testAud = "app-aud"
	testHdr = "X-Test-Assertion"
)

var epoch = time.Unix(1_800_000_000, 0)

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jsonEnc(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return enc(b)
}

// signer is a local key pair; kid names it in the JWKS.
type signer struct {
	kid string
	rsa *rsa.PrivateKey
	ec  *ecdsa.PrivateKey
}

func newRSA(t *testing.T, kid string, bits int) *signer {
	k, err := rsa.GenerateKey(rand.Reader, bits)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{kid: kid, rsa: k}
}

func newEC(t *testing.T, kid string) *signer {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{kid: kid, ec: k}
}

func (s *signer) jwk() map[string]any {
	if s.rsa != nil {
		return map[string]any{"kty": "RSA", "kid": s.kid, "use": "sig", "alg": "RS256",
			"n": enc(s.rsa.N.Bytes()), "e": enc(big.NewInt(int64(s.rsa.E)).Bytes())}
	}
	return map[string]any{"kty": "EC", "kid": s.kid, "use": "sig", "alg": "ES256", "crv": "P-256",
		"x": enc(s.ec.X.FillBytes(make([]byte, 32))), "y": enc(s.ec.Y.FillBytes(make([]byte, 32)))}
}

func (s *signer) sign(t *testing.T, input string) []byte {
	h := sha256.Sum256([]byte(input))
	if s.rsa != nil {
		sig, err := rsa.SignPKCS1v15(rand.Reader, s.rsa, crypto.SHA256, h[:])
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}
	r, ss, err := ecdsa.Sign(rand.Reader, s.ec, h[:])
	if err != nil {
		t.Fatal(err)
	}
	return append(r.FillBytes(make([]byte, 32)), ss.FillBytes(make([]byte, 32))...)
}

func goodClaims() map[string]any {
	return map[string]any{"iss": testIss, "aud": testAud, "sub": "user-1", "email": "a@example.test",
		"exp": epoch.Add(time.Hour).Unix(), "nbf": epoch.Add(-time.Minute).Unix()}
}

// mint builds a token; edit may change the header and claims, sig the
// signature bytes. alg and kid default to the signer's.
func mint(t *testing.T, s *signer, edit func(h, c map[string]any), sig func([]byte) []byte) string {
	alg := "RS256"
	if s.ec != nil {
		alg = "ES256"
	}
	h := map[string]any{"alg": alg, "kid": s.kid, "typ": "JWT"}
	c := goodClaims()
	if edit != nil {
		edit(h, c)
	}
	in := jsonEnc(t, h) + "." + jsonEnc(t, c)
	sg := s.sign(t, in)
	if sig != nil {
		sg = sig(sg)
	}
	return in + "." + enc(sg)
}

// fakeJWKS serves a key set over TLS and counts fetches.
type fakeJWKS struct {
	srv   *httptest.Server
	mu    sync.Mutex
	keys  []map[string]any
	mode  string // "", "down", "redirect", "huge", "raw"
	raw   string
	hits  atomic.Int32
	other atomic.Int32 // hits on the redirect target
	delay time.Duration
}

func newJWKS(t *testing.T, keys ...*signer) *fakeJWKS {
	f := &fakeJWKS{}
	f.setKeys(keys...)
	mux := http.NewServeMux()
	mux.HandleFunc("/other", func(http.ResponseWriter, *http.Request) { f.other.Add(1) })
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		f.mu.Lock()
		mode, raw, keys, delay := f.mode, f.raw, f.keys, f.delay
		f.mu.Unlock()
		time.Sleep(delay)
		switch mode {
		case "down":
			http.Error(w, "down", http.StatusServiceUnavailable)
		case "redirect":
			http.Redirect(w, r, "/other", http.StatusFound)
		case "huge":
			w.Write([]byte(`{"keys":[],"pad":"` + strings.Repeat("a", 70<<10) + `"}`))
		case "raw":
			w.Write([]byte(raw))
		default:
			json.NewEncoder(w).Encode(map[string]any{"keys": keys})
		}
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeJWKS) setKeys(ss ...*signer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.keys = nil
	for _, s := range ss {
		f.keys = append(f.keys, s.jwk())
	}
}

func (f *fakeJWKS) setMode(m string) { f.mu.Lock(); f.mode = m; f.mu.Unlock() }

func (f *fakeJWKS) setRaw(s string) { f.mu.Lock(); f.mode, f.raw = "raw", s; f.mu.Unlock() }

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

func newVerifier(t *testing.T, f *fakeJWKS, edit func(*config.AuthJWT)) (*Verifier, *clock) {
	cfg := config.AuthJWT{Issuer: testIss, Audience: testAud, JWKSURL: f.srv.URL + "/jwks", Header: testHdr}
	if edit != nil {
		edit(&cfg)
	}
	v, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	clk := &clock{t: epoch}
	v.now = clk.now
	v.keys.client.Transport = f.srv.Client().Transport // trust the test CA
	return v, clk
}

func req(tokens ...string) *http.Request {
	r := httptest.NewRequest("GET", "/api/x", nil)
	for _, tk := range tokens {
		r.Header.Add(testHdr, tk)
	}
	return r
}

func wantReason(t *testing.T, v *Verifier, r *http.Request, want string) {
	t.Helper()
	id, err := v.Verify(r)
	if got := Reason(err); got != want {
		t.Fatalf("reason = %q (err %v, id %+v), want %q", got, err, id, want)
	}
	if want != "" && id != (Identity{}) {
		t.Fatalf("a refusal returned an identity: %+v", id)
	}
}

func TestValidTokens(t *testing.T) {
	rs, ec := newRSA(t, "rs", 2048), newEC(t, "ec")
	v, _ := newVerifier(t, newJWKS(t, rs, ec), nil)
	for _, s := range []*signer{rs, ec} {
		id, err := v.Verify(req(mint(t, s, nil, nil)))
		if err != nil || id != (Identity{Subject: "user-1", Email: "a@example.test"}) {
			t.Fatalf("%s: id %+v err %v", s.kid, id, err)
		}
	}
	// aud as an array
	tk := mint(t, rs, func(h, c map[string]any) { c["aud"] = []string{"other", testAud} }, nil)
	wantReason(t, v, req(tk), "")
}

func TestNoHeaderIsMissing(t *testing.T) {
	v, _ := newVerifier(t, newJWKS(t, newEC(t, "ec")), nil)
	wantReason(t, v, req(), ReasonMissing)
}

func TestClaimFailures(t *testing.T) {
	ec := newEC(t, "ec")
	f := newJWKS(t, ec)
	v, _ := newVerifier(t, f, nil)
	cases := []struct {
		name string
		edit func(h, c map[string]any)
		want string
	}{
		{"wrong audience", func(h, c map[string]any) { c["aud"] = "nope" }, ReasonAudience},
		{"wrong audience array", func(h, c map[string]any) { c["aud"] = []string{"a", "b"} }, ReasonAudience},
		{"wrong issuer", func(h, c map[string]any) { c["iss"] = "https://evil.example" }, ReasonIssuer},
		{"expired", func(h, c map[string]any) { c["exp"] = epoch.Add(-2 * time.Minute).Unix() }, ReasonExpired},
		{"expired inside skew", func(h, c map[string]any) { c["exp"] = epoch.Add(-30 * time.Second).Unix() }, ""},
		{"nbf in the future", func(h, c map[string]any) { c["nbf"] = epoch.Add(2 * time.Minute).Unix() }, ReasonNotYetValid},
		{"nbf inside skew", func(h, c map[string]any) { c["nbf"] = epoch.Add(30 * time.Second).Unix() }, ""},
		{"no exp", func(h, c map[string]any) { delete(c, "exp") }, ReasonClaims},
		{"string exp", func(h, c map[string]any) { c["exp"] = "soon" }, ReasonClaims},
		{"no iss", func(h, c map[string]any) { delete(c, "iss") }, ReasonClaims},
		{"no aud", func(h, c map[string]any) { delete(c, "aud") }, ReasonClaims},
		{"no sub", func(h, c map[string]any) { delete(c, "sub") }, ReasonClaims},
		{"numeric email", func(h, c map[string]any) { c["email"] = 5 }, ReasonClaims},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { wantReason(t, v, req(mint(t, ec, tc.edit, nil)), tc.want) })
	}
}

func TestHeaderAndSignatureFailures(t *testing.T) {
	rs, ec := newRSA(t, "rs", 2048), newEC(t, "ec")
	v, _ := newVerifier(t, newJWKS(t, rs, ec), nil)
	good := mint(t, ec, nil, nil)
	parts := strings.Split(good, ".")
	flip := func(sig []byte) []byte { sig[0] ^= 1; return sig }

	cases := []struct {
		name  string
		token string
		want  string
	}{
		{"bad signature", mint(t, ec, nil, flip), ReasonSignature},
		{"tampered payload", parts[0] + "." + jsonEnc(t, map[string]any{"iss": testIss, "aud": testAud, "sub": "root", "exp": epoch.Add(time.Hour).Unix()}) + "." + parts[2], ReasonSignature},
		{"alg none, empty signature", jsonEnc(t, map[string]any{"alg": "none", "kid": "ec"}) + "." + parts[1] + ".", ReasonMalformed},
		{"alg none with sig", mint(t, ec, func(h, c map[string]any) { h["alg"] = "none" }, nil), ReasonAlg},
		{"alg HS256", mint(t, rs, func(h, c map[string]any) { h["alg"] = "HS256" }, nil), ReasonAlg},
		{"alg ES384", mint(t, ec, func(h, c map[string]any) { h["alg"] = "ES384" }, nil), ReasonAlg},
		{"alg PS256", mint(t, rs, func(h, c map[string]any) { h["alg"] = "PS256" }, nil), ReasonAlg},
		{"no alg", mint(t, ec, func(h, c map[string]any) { delete(h, "alg") }, nil), ReasonAlg},
		{"RS256 header over an EC key", mint(t, ec, func(h, c map[string]any) { h["alg"] = "RS256" }, nil), ReasonAlg},
		{"ES256 header over an RSA key", mint(t, rs, func(h, c map[string]any) { h["alg"] = "ES256" }, nil), ReasonAlg},
		{"crit", mint(t, ec, func(h, c map[string]any) { h["crit"] = []string{"exp"} }, nil), ReasonMalformed},
		{"no kid", mint(t, ec, func(h, c map[string]any) { delete(h, "kid") }, nil), ReasonUnknownKid},
		{"two segments", parts[0] + "." + parts[1], ReasonMalformed},
		{"four segments", good + ".x", ReasonMalformed},
		{"padded base64", parts[0] + "=." + parts[1] + "." + parts[2], ReasonMalformed},
		{"standard alphabet", strings.Replace(parts[0], parts[0][:1], "+", 1) + "." + parts[1] + "." + parts[2], ReasonMalformed},
		{"whitespace", parts[0] + "\n." + parts[1] + "." + parts[2], ReasonMalformed},
		{"header not an object", enc([]byte("[]")) + "." + parts[1] + "." + parts[2], ReasonMalformed},
		{"63-byte ES256 signature", mint(t, ec, nil, func(s []byte) []byte { return s[:63] }), ReasonSignature},
		{"65-byte ES256 signature", mint(t, ec, nil, func(s []byte) []byte { return append(s, 0) }), ReasonSignature},
		{"jwk jku x5c x5u ignored", mint(t, ec, func(h, c map[string]any) {
			h["jwk"], h["jku"], h["x5c"], h["x5u"] = map[string]any{"kty": "oct"}, "https://evil.example", []string{"AA"}, "https://evil.example"
		}, nil), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wantReason(t, v, req(tc.token), tc.want)
		})
	}
}

func TestHS256SignedWithThePublicKeyIsRefused(t *testing.T) {
	rs := newRSA(t, "rs", 2048)
	v, _ := newVerifier(t, newJWKS(t, rs), nil)
	in := jsonEnc(t, map[string]any{"alg": "HS256", "kid": "rs"}) + "." + jsonEnc(t, goodClaims())
	wantReason(t, v, req(in+"."+enc([]byte("hmac-with-public-key-bytes-32-long"))), ReasonAlg)
}

func TestTwoHeaderValuesAreMalformed(t *testing.T) {
	ec := newEC(t, "ec")
	v, _ := newVerifier(t, newJWKS(t, ec), nil)
	tk := mint(t, ec, nil, nil)
	wantReason(t, v, req(tk, tk), ReasonMalformed)
}

func TestAllowedEmails(t *testing.T) {
	ec := newEC(t, "ec")
	v, _ := newVerifier(t, newJWKS(t, ec), func(c *config.AuthJWT) { c.AllowedEmails = []string{" A@Example.test "} })
	wantReason(t, v, req(mint(t, ec, nil, nil)), "")
	wantReason(t, v, req(mint(t, ec, func(h, c map[string]any) { c["email"] = "b@example.test" }, nil)), ReasonClaims)
	wantReason(t, v, req(mint(t, ec, func(h, c map[string]any) { delete(c, "email") }, nil)), ReasonClaims)
	// off by default: no email needed
	v2, _ := newVerifier(t, newJWKS(t, ec), nil)
	wantReason(t, v2, req(mint(t, ec, func(h, c map[string]any) { delete(c, "email") }, nil)), "")
}

func TestKeyVetting(t *testing.T) {
	good := newEC(t, "good")
	weak := newRSA(t, "weak", 1024)
	badUse := newEC(t, "enc")
	badUse2 := badUse.jwk()
	badUse2["use"] = "enc"
	badAlg := newRSA(t, "algmismatch", 2048)
	badAlgJWK := badAlg.jwk()
	badAlgJWK["alg"] = "RS512"
	ecBadAlg := newEC(t, "ecalg")
	ecBadAlgJWK := ecBadAlg.jwk()
	ecBadAlgJWK["alg"] = "RS256"
	off := newEC(t, "off")
	offJWK := off.jwk()
	offJWK["y"] = enc(new(big.Int).Add(off.ec.Y, big.NewInt(1)).FillBytes(make([]byte, 32)))
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	p384JWK := map[string]any{"kty": "EC", "kid": "p384", "crv": "P-384", "x": enc(p384.X.Bytes()), "y": enc(p384.Y.Bytes())}

	f := newJWKS(t, good, weak)
	f.mu.Lock()
	f.keys = append(f.keys, badUse2, badAlgJWK, ecBadAlgJWK, offJWK, p384JWK)
	f.mu.Unlock()
	v, clk := newVerifier(t, f, nil)

	wantReason(t, v, req(mint(t, good, nil, nil)), "")
	for _, s := range []*signer{weak, badUse, badAlg, ecBadAlg, off} {
		clk.add(2 * time.Minute) // allow the refresh each unknown kid triggers
		wantReason(t, v, req(mint(t, s, nil, nil)), ReasonUnknownKid)
	}
}

func TestKeyCountRules(t *testing.T) {
	ec := newEC(t, "ec")
	noKid := ec.jwk()
	delete(noKid, "kid")
	for name, keys := range map[string][]map[string]any{
		"no kid":      {ec.jwk(), noKid},
		"repeated id": {ec.jwk(), ec.jwk()},
	} {
		f := newJWKS(t)
		f.mu.Lock()
		f.keys = keys
		f.mu.Unlock()
		v, _ := newVerifier(t, f, nil)
		t.Run(name, func(t *testing.T) { wantReason(t, v, req(mint(t, ec, nil, nil)), ReasonKeysUnavailable) })
	}
}

func TestEmptyAndBrokenJWKSAreFailedFetches(t *testing.T) {
	ec := newEC(t, "ec")
	f := newJWKS(t) // empty set
	v, _ := newVerifier(t, f, nil)
	wantReason(t, v, req(mint(t, ec, nil, nil)), ReasonKeysUnavailable)
	for _, mode := range []string{"huge", "down", "redirect"} {
		f.setMode(mode)
		v, _ = newVerifier(t, f, nil)
		wantReason(t, v, req(mint(t, ec, nil, nil)), ReasonKeysUnavailable)
	}
	f.setRaw("not json")
	v, _ = newVerifier(t, f, nil)
	wantReason(t, v, req(mint(t, ec, nil, nil)), ReasonKeysUnavailable)
}

func TestEmptySetCannotWipeTheCache(t *testing.T) {
	ec := newEC(t, "ec")
	f := newJWKS(t, ec)
	v, clk := newVerifier(t, f, nil)
	wantReason(t, v, req(mint(t, ec, nil, nil)), "")
	f.setKeys()
	clk.add(2 * time.Minute)
	wantReason(t, v, req(mint(t, newEC(t, "new"), nil, nil)), ReasonKeysUnavailable) // refresh fails
	wantReason(t, v, req(mint(t, ec, nil, nil)), "")                                 // old set kept
}

func TestJWKSRedirectIsNotFollowed(t *testing.T) {
	ec := newEC(t, "ec")
	f := newJWKS(t, ec)
	f.setMode("redirect")
	v, _ := newVerifier(t, f, nil)
	wantReason(t, v, req(mint(t, ec, nil, nil)), ReasonKeysUnavailable)
	if f.other.Load() != 0 {
		t.Fatal("the redirect target was fetched")
	}
}

func TestUnknownKidRefreshesOnceAMinute(t *testing.T) {
	a, b := newEC(t, "a"), newEC(t, "b")
	f := newJWKS(t, a)
	v, clk := newVerifier(t, f, nil)
	wantReason(t, v, req(mint(t, a, nil, nil)), "")
	if f.hits.Load() != 1 {
		t.Fatalf("hits = %d, want 1", f.hits.Load())
	}
	// the provider rotated in b; a is cached and costs no fetch
	f.setKeys(a, b)
	wantReason(t, v, req(mint(t, a, nil, nil)), "")
	if f.hits.Load() != 1 {
		t.Fatal("a cached kid fetched")
	}
	// within the minute: no refresh, unknown
	clk.add(30 * time.Second)
	wantReason(t, v, req(mint(t, b, nil, nil)), ReasonUnknownKid)
	if f.hits.Load() != 1 {
		t.Fatal("refreshed twice inside a minute")
	}
	// after the minute: one refresh finds b
	clk.add(31 * time.Second)
	wantReason(t, v, req(mint(t, b, nil, nil)), "")
	if f.hits.Load() != 2 {
		t.Fatalf("hits = %d, want 2", f.hits.Load())
	}
	// a random kid: one refresh, then refused; the next one is rationed
	clk.add(2 * time.Minute)
	z := newEC(t, "zzz")
	wantReason(t, v, req(mint(t, z, nil, nil)), ReasonUnknownKid)
	wantReason(t, v, req(mint(t, z, nil, nil)), ReasonUnknownKid)
	if f.hits.Load() != 3 {
		t.Fatalf("hits = %d, want 3", f.hits.Load())
	}
}

func TestSingleRefreshInFlight(t *testing.T) {
	ec := newEC(t, "ec")
	f := newJWKS(t, ec)
	f.delay = 150 * time.Millisecond
	v, _ := newVerifier(t, f, nil)
	tk := mint(t, ec, nil, nil)
	var wg sync.WaitGroup
	var bad atomic.Int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := v.Verify(req(tk)); err != nil {
				bad.Add(1)
			}
		}()
	}
	wg.Wait()
	if bad.Load() != 0 || f.hits.Load() != 1 {
		t.Fatalf("failures %d, fetches %d; want 0 and 1", bad.Load(), f.hits.Load())
	}
}

func TestOutageKeepsTheLastGoodSetFor24Hours(t *testing.T) {
	ec := newEC(t, "ec")
	f := newJWKS(t, ec)
	v, clk := newVerifier(t, f, nil)
	wantReason(t, v, req(mint(t, ec, nil, nil)), "")

	f.setMode("down")
	clk.add(23 * time.Hour)
	// a token from a later epoch is fine: move its claims with the clock
	late := func() string {
		return mint(t, ec, func(h, c map[string]any) { c["exp"] = clk.now().Add(time.Hour).Unix() }, nil)
	}
	wantReason(t, v, req(late()), "")
	// an unknown kid during the outage: the refresh fails, the old set stays
	wantReason(t, v, req(mint(t, newEC(t, "other"), nil, nil)), ReasonKeysUnavailable)
	wantReason(t, v, req(late()), "")

	clk.add(2 * time.Hour) // 25 h since the last good fetch
	wantReason(t, v, req(late()), ReasonKeysUnavailable)

	f.setMode("")
	clk.add(15 * time.Minute) // past any backoff
	wantReason(t, v, req(late()), "")
}

func TestBackoffAfterFailures(t *testing.T) {
	ec := newEC(t, "ec")
	f := newJWKS(t, ec)
	f.setMode("down")
	v, clk := newVerifier(t, f, nil)
	tk := mint(t, ec, nil, nil)
	hits := func(want int32) {
		t.Helper()
		if f.hits.Load() != want {
			t.Fatalf("hits = %d, want %d", f.hits.Load(), want)
		}
	}
	wantReason(t, v, req(tk), ReasonKeysUnavailable)
	hits(1)
	clk.add(30 * time.Second)
	wantReason(t, v, req(tk), ReasonKeysUnavailable)
	hits(1)
	clk.add(31 * time.Second) // 1 min after the first failure
	wantReason(t, v, req(tk), ReasonKeysUnavailable)
	hits(2)
	clk.add(61 * time.Second) // the next wait is 2 min
	wantReason(t, v, req(tk), ReasonKeysUnavailable)
	hits(2)
	clk.add(60 * time.Second)
	wantReason(t, v, req(tk), ReasonKeysUnavailable)
	hits(3)
}

func TestConfigRefusals(t *testing.T) {
	ok := config.AuthJWT{Issuer: testIss, Audience: testAud, JWKSURL: "https://idp.example/certs", Header: "Cf-Access-Jwt-Assertion"}
	if _, err := New(ok); err != nil {
		t.Fatal(err)
	}
	for name, edit := range map[string]func(*config.AuthJWT){
		"http jwks":          func(c *config.AuthJWT) { c.JWKSURL = "http://idp.example/certs" },
		"no scheme":          func(c *config.AuthJWT) { c.JWKSURL = "idp.example/certs" },
		"userinfo":           func(c *config.AuthJWT) { c.JWKSURL = "https://u:p@idp.example/certs" },
		"Authorization":      func(c *config.AuthJWT) { c.Header = "Authorization" },
		"authorization case": func(c *config.AuthJWT) { c.Header = "aUTHORIZATION" },
		"no issuer":          func(c *config.AuthJWT) { c.Issuer = "" },
		"no audience":        func(c *config.AuthJWT) { c.Audience = "" },
		"no jwks":            func(c *config.AuthJWT) { c.JWKSURL = "" },
		"no header":          func(c *config.AuthJWT) { c.Header = "" },
		"empty email":        func(c *config.AuthJWT) { c.AllowedEmails = []string{""} },
	} {
		c := ok
		edit(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConfigBlockParses(t *testing.T) {
	var c config.Config
	raw := `{"auth_jwt":{"issuer":"i","audience":"a","jwks_url":"https://x.example/k","header":"H","allowed_emails":["e@x.test"]}}`
	if err := json.Unmarshal([]byte(raw), &c); err != nil || c.AuthJWT == nil || c.AuthJWT.AllowedEmails[0] != "e@x.test" {
		t.Fatalf("%v %+v", err, c.AuthJWT)
	}
	var off config.Config
	if json.Unmarshal([]byte(`{}`), &off); off.AuthJWT != nil {
		t.Fatal("the mode is on without a block")
	}
}

func TestErrorTextHoldsNoToken(t *testing.T) {
	ec := newEC(t, "ec")
	v, _ := newVerifier(t, newJWKS(t, ec), nil)
	tk := mint(t, ec, func(h, c map[string]any) { c["aud"] = "x" }, nil)
	_, err := v.Verify(req(tk))
	if strings.Contains(err.Error(), tk[:20]) || err.Error() != "jwt: jwt_audience" {
		t.Fatal(err)
	}
}

// Without the 64-byte rule, r||0x00||s (65 bytes) would split at 32 and verify
// as the valid (r, s), so only the length check refuses it.
func TestES256SignatureLengthRuleIsWhatRefusesAPaddedSignature(t *testing.T) {
	ec := newEC(t, "ec")
	v, _ := newVerifier(t, newJWKS(t, ec), nil)
	padded := func(s []byte) []byte { return append(append(append([]byte{}, s[:32]...), 0), s[32:]...) }
	wantReason(t, v, req(mint(t, ec, nil, padded)), ReasonSignature)
	wantReason(t, v, req(mint(t, ec, nil, func(s []byte) []byte { return s[:63] })), ReasonSignature)
	// the same signature at its real length verifies, so the padding is the only difference
	wantReason(t, v, req(mint(t, ec, nil, nil)), "")

	h := sha256.Sum256([]byte("x"))
	r, s, _ := ecdsa.Sign(rand.Reader, ec.ec, h[:])
	good := append(r.FillBytes(make([]byte, 32)), s.FillBytes(make([]byte, 32))...)
	k := key{alg: "ES256", ec: &ec.ec.PublicKey}
	if !k.verify([]byte("x"), good) {
		t.Fatal("a 64-byte signature must verify")
	}
	if k.verify([]byte("x"), append(append(append([]byte{}, good[:32]...), 0), good[32:]...)) {
		t.Fatal("a 65-byte signature must not verify")
	}
}

func TestEvenRSAExponentIsDropped(t *testing.T) {
	rs := newRSA(t, "rs", 2048)
	for _, e := range []int64{2, 65536} {
		j := rs.jwk()
		j["e"] = enc(big.NewInt(e).Bytes())
		raw, _ := json.Marshal(map[string]any{"keys": []any{j, newEC(t, "other").jwk()}})
		set, err := parseJWKS(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := set["rs"]; ok || len(set) != 1 {
			t.Fatalf("e=%d: key kept: %v", e, set)
		}
		// a token signed by the matching private key finds no usable key
		f := newJWKS(t)
		f.setRaw(string(raw))
		v, _ := newVerifier(t, f, nil)
		wantReason(t, v, req(mint(t, rs, nil, nil)), ReasonUnknownKid)
	}
	// the odd exponent of the same key is accepted
	if _, ok := rs.jwkKey(); !ok {
		t.Fatal("the real key was dropped")
	}
}

func (s *signer) jwkKey() (key, bool) {
	b, _ := json.Marshal(s.jwk())
	var j jwk
	_ = json.Unmarshal(b, &j)
	return j.key()
}
