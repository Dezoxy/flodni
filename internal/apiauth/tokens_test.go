package apiauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A valid-shaped token, built so no scanner reads it as a secret: "fl_" plus 43 characters.
func fakeToken(c byte) string { return "fl_" + strings.Repeat(string(c), 43) }

func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func entryJSON(name, scopes, hash string) string {
	return `{"name":"` + name + `","scopes":[` + scopes + `],"sha256":"` + hash + `"}`
}

func writeTokensFile(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadFile(t *testing.T, body string) (*Tokens, error) {
	t.Helper()
	return Load(LoadOptions{TokensFile: writeTokensFile(t, body), GoStormURL: "http://127.0.0.1:8090"})
}

const okEntry = `{"name":"bridge","scopes":["engine","upload"],"sha256":"` +
	"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" + `"}`

func TestLoadRefusals(t *testing.T) {
	h1, h2 := hashHex(fakeToken('a')), hashHex(fakeToken('b'))
	emptyHash := hashHex("")
	many := make([]string, 65)
	for i := range many {
		many[i] = entryJSON("t"+strings.Repeat("a", i/26)+string(rune('a'+i%26)), `"read"`, hashHex(string(rune('A'+i))+"x"))
	}
	cases := []struct{ name, body, want string }{
		{"not json", `not json`, "parse"},
		{"empty file", ``, "parse"},
		{"unknown field", `{"tokens":[` + entryJSON("a", `"read"`, h1) + `],"extra":1}`, "extra"},
		{"unknown entry field", `{"tokens":[{"name":"a","scopes":["read"],"sha256":"` + h1 + `","admin":true}]}`, "admin"},
		{"trailing data", `{"tokens":[` + entryJSON("a", `"read"`, h1) + `]} {}`, "trailing"},
		{"trailing garbage", `{"tokens":[` + entryJSON("a", `"read"`, h1) + `]}x`, "trailing"},
		{"zero tokens", `{"tokens":[]}`, "no tokens"},
		{"no tokens key", `{}`, "no tokens"},
		{"null tokens", `{"tokens":null}`, "no tokens"},
		{"unknown scope", `{"tokens":[` + entryJSON("a", `"read","root"`, h1) + `]}`, "unknown scope"},
		{"no scopes", `{"tokens":[` + entryJSON("a", ``, h1) + `]}`, "no scopes"},
		{"duplicate hash", `{"tokens":[` + entryJSON("a", `"read"`, h1) + `,` + entryJSON("b", `"read"`, h1) + `]}`, "repeats"},
		{"empty-string hash", `{"tokens":[` + entryJSON("a", `"read"`, emptyHash) + `]}`, "empty"},
		{"short hash", `{"tokens":[` + entryJSON("a", `"read"`, "abc") + `]}`, "64"},
		{"upper-case hash", `{"tokens":[` + entryJSON("a", `"read"`, strings.ToUpper(h1)) + `]}`, "64"},
		{"non-hex hash", `{"tokens":[` + entryJSON("a", `"read"`, strings.Repeat("z", 64)) + `]}`, "64"},
		{"reserved name", `{"tokens":[` + entryJSON("internal", `"read"`, h1) + `]}`, "reserved"},
		{"empty name", `{"tokens":[` + entryJSON("", `"read"`, h1) + `]}`, "name"},
		{"upper-case name", `{"tokens":[` + entryJSON("Bridge", `"read"`, h1) + `]}`, "name"},
		{"leading dash name", `{"tokens":[` + entryJSON("-a", `"read"`, h1) + `]}`, "name"},
		{"space in name", `{"tokens":[` + entryJSON("a b", `"read"`, h1) + `]}`, "name"},
		{"newline in name", `{"tokens":[` + entryJSON(`a\nb`, `"read"`, h1) + `]}`, "name"},
		{"name too long", `{"tokens":[` + entryJSON(strings.Repeat("a", 33), `"read"`, h1) + `]}`, "name"},
		{"duplicate name", `{"tokens":[` + entryJSON("a", `"read"`, h1) + `,` + entryJSON("a", `"read"`, h2) + `]}`, "repeats"},
		{"too many tokens", `{"tokens":[` + strings.Join(many, ",") + `]}`, "64"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tok, err := loadFile(t, c.body)
			if err == nil {
				t.Fatalf("loaded; want a refusal containing %q (mode %q)", c.want, tok.Mode())
			}
			if tok != nil {
				t.Fatalf("a refusal must return no Tokens, got mode %q", tok.Mode())
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
		})
	}
}

func TestLoadRefusesAnUnreadableFile(t *testing.T) {
	_, err := Load(LoadOptions{TokensFile: filepath.Join(t.TempDir(), "absent.json"), GoStormURL: "http://127.0.0.1:8090"})
	if err == nil {
		t.Fatal("a missing tokens file must refuse, not open the port")
	}
	dir := t.TempDir()
	if _, err := Load(LoadOptions{TokensFile: dir, GoStormURL: "http://127.0.0.1:8090"}); err == nil {
		t.Fatal("a directory must refuse")
	}
}

func TestLoadRefusesAFileOver64KiB(t *testing.T) {
	// Valid JSON, valid entry, padded with whitespace past the cap.
	body := `{"tokens":[` + entryJSON("a", `"read"`, hashHex(fakeToken('a'))) + `]}` + strings.Repeat(" ", maxFileBytes)
	if _, err := loadFile(t, body); err == nil || !strings.Contains(err.Error(), "64 KiB") {
		t.Fatalf("err = %v, want the size refusal", err)
	}
	// Exactly at the cap is fine.
	ok := `{"tokens":[` + entryJSON("a", `"read"`, hashHex(fakeToken('a'))) + `]}`
	ok += strings.Repeat(" ", maxFileBytes-len(ok))
	if _, err := loadFile(t, ok); err != nil {
		t.Fatalf("a file of exactly 64 KiB must load: %v", err)
	}
}

func TestLoadNamesTheLineOfASyntaxError(t *testing.T) {
	_, err := loadFile(t, "{\n  \"tokens\": [\n    oops\n  ]\n}")
	if err == nil || !strings.Contains(err.Error(), "line 3") {
		t.Fatalf("err = %v, want it to name line 3", err)
	}
}

func TestLoadCombinationRefusals(t *testing.T) {
	file := writeTokensFile(t, `{"tokens":[`+entryJSON("a", `"read"`, hashHex(fakeToken('a')))+`]}`)
	loop := "http://127.0.0.1:8090"
	cases := []struct {
		name string
		o    LoadOptions
		want []string
	}{
		{"api_token and a tokens file", LoadOptions{APIToken: "x", TokensFile: file, GoStormURL: loop}, []string{"api_token", "auth_tokens_file"}},
		{"auth_jwt without a tokens file", LoadOptions{JWTConfigured: true, GoStormURL: loop}, []string{"auth_jwt", "auth_tokens_file"}},
		{"auth_jwt with api_token only", LoadOptions{APIToken: "x", JWTConfigured: true, GoStormURL: loop}, []string{"auth_jwt"}},
		{"remote engine in scoped mode", LoadOptions{TokensFile: file, GoStormURL: "http://10.0.0.5:8090"}, []string{"gostorm_url", "loopback"}},
		{"remote hostname engine", LoadOptions{TokensFile: file, GoStormURL: "http://engine.example:8090"}, []string{"loopback"}},
		{"empty engine url", LoadOptions{TokensFile: file, GoStormURL: ""}, []string{"loopback"}},
		{"unparsable engine url", LoadOptions{TokensFile: file, GoStormURL: "://nope"}, []string{"gostorm_url"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tok, err := Load(c.o)
			if err == nil || tok != nil {
				t.Fatalf("loaded %v; want a refusal", tok)
			}
			for _, w := range c.want {
				if !strings.Contains(err.Error(), w) {
					t.Fatalf("error %q does not name %q", err, w)
				}
			}
		})
	}
}

func TestLoadAcceptsLoopbackEngines(t *testing.T) {
	file := writeTokensFile(t, `{"tokens":[`+entryJSON("a", `"read"`, hashHex(fakeToken('a')))+`]}`)
	for _, u := range []string{"http://127.0.0.1:8090", "http://localhost:8090", "http://[::1]:8090", "https://127.9.9.9"} {
		if _, err := Load(LoadOptions{TokensFile: file, GoStormURL: u}); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

// A non-loopback engine is only a problem in scoped mode.
func TestLoadRemoteEngineIsFineOutsideScopedMode(t *testing.T) {
	for _, o := range []LoadOptions{{GoStormURL: "http://10.0.0.5:8090"}, {APIToken: "x", GoStormURL: "http://10.0.0.5:8090"}} {
		if _, err := Load(o); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
}

func TestLoadModes(t *testing.T) {
	open, err := Load(LoadOptions{})
	if err != nil || open.Mode() != ModeOpen || open.Len() != 0 {
		t.Fatalf("open: %v %v", open, err)
	}
	if _, _, ok := open.Lookup(fakeToken('a')); ok {
		t.Fatal("open mode has nothing to look up")
	}

	leg, err := Load(LoadOptions{APIToken: "hunter2"})
	if err != nil || leg.Mode() != ModeLegacyToken || leg.Len() != 1 {
		t.Fatalf("legacy: %v %v", leg, err)
	}
	// The legacy token is hashed at load and goes through the same Lookup; it is exempt
	// from the shape rule and holds every scope.
	name, sc, ok := leg.Lookup("hunter2")
	if !ok || name == "" {
		t.Fatalf("legacy token not found: %q %v", name, ok)
	}
	for _, s := range AllScopes {
		if !sc.Has(s) {
			t.Errorf("legacy token lacks %s", s)
		}
	}
	if _, _, ok := leg.Lookup("hunter3"); ok {
		t.Fatal("wrong legacy token accepted")
	}
	if _, _, ok := leg.Lookup(""); ok {
		t.Fatal("empty bearer accepted in legacy mode")
	}

	file := writeTokensFile(t, `{"tokens":[`+entryJSON("a", `"read"`, hashHex(fakeToken('a')))+`]}`)
	sco, err := Load(LoadOptions{TokensFile: file, GoStormURL: "http://127.0.0.1:8090"})
	if err != nil || sco.Mode() != ModeScoped || sco.Len() != 1 {
		t.Fatalf("scoped: %v %v", sco, err)
	}
}

func TestLoadWarnsOnlyForAWritableFile(t *testing.T) {
	body := `{"tokens":[` + entryJSON("a", `"read"`, hashHex(fakeToken('a'))) + `]}`
	run := func(mode os.FileMode) (string, error) {
		p := writeTokensFile(t, body)
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		_, err := Load(LoadOptions{TokensFile: p, GoStormURL: "http://127.0.0.1:8090", Logger: slog.New(slog.NewTextHandler(&buf, nil))})
		return buf.String(), err
	}
	for _, m := range []os.FileMode{0o400, 0o600, 0o640, 0o644, 0o444} {
		if out, err := run(m); err != nil || out != "" {
			t.Errorf("mode %v: err %v log %q, want a quiet load", m, err, out)
		}
	}
	for _, m := range []os.FileMode{0o660, 0o620, 0o602, 0o666} {
		out, err := run(m)
		if err != nil {
			t.Errorf("mode %v refused: %v (a warning only)", m, err)
		}
		if !strings.Contains(out, "writable") {
			t.Errorf("mode %v: log %q, want a writable warning", m, out)
		}
	}
}

func TestLookup(t *testing.T) {
	a, b, c := fakeToken('a'), fakeToken('b'), fakeToken('c')
	tok, err := loadFile(t, `{"tokens":[`+
		entryJSON("one", `"read"`, hashHex(a))+`,`+
		entryJSON("two", `"engine","upload"`, hashHex(b))+`,`+
		entryJSON("three", `"admin"`, hashHex(c))+`]}`)
	if err != nil {
		t.Fatal(err)
	}
	name, sc, ok := tok.Lookup(b)
	if !ok || name != "two" || !sc.Has(ScopeEngine) || !sc.Has(ScopeUpload) || sc.Has(ScopeRead) || sc.Has(ScopeAdmin) {
		t.Fatalf("two: %q %v %v", name, sc.Names(), ok)
	}
	name, sc, ok = tok.Lookup(c)
	if !ok || name != "three" || !sc.Has(ScopeAdmin) {
		t.Fatalf("three: %q %v", name, ok)
	}
	// admin implies every other scope; nothing else implies anything.
	for _, s := range AllScopes {
		if !sc.Has(s) {
			t.Errorf("admin lacks %s", s)
		}
	}
	if name, _, ok = tok.Lookup(a); !ok || name != "one" {
		t.Fatalf("one: %q %v", name, ok)
	}

	for _, bad := range []string{
		"",                              // the empty bearer
		fakeToken('d'),                  // right shape, unknown
		strings.TrimPrefix(a, "fl_"),    // no prefix
		a + "x",                         // one too long
		a[:len(a)-1],                    // one too short
		"FL_" + a[3:],                   // wrong prefix case
		"fl_" + strings.Repeat("!", 43), // outside the alphabet
		"fl_" + strings.Repeat("+", 43), // standard base64, not url-safe
		" " + a,                         // padding
		a + "\n",                        // trailing newline
		"Bearer " + a,
	} {
		if n, s, ok := tok.Lookup(bad); ok || n != "" || s != 0 {
			t.Errorf("Lookup(%q) = %q %v %v, want no match", bad, n, s.Names(), ok)
		}
	}
}

// The hash of the empty string must never be reachable: the loader refuses it, and an
// empty or mis-shaped bearer is turned away before any hashing.
func TestLookupEmptyBearerNeverMatchesEvenIfTheHashWereStored(t *testing.T) {
	tok := &Tokens{mode: ModeScoped, cmp: cmpCounter(new(int))}
	var h [sha256.Size]byte = sha256.Sum256(nil)
	tok.entries = []Entry{{Name: "x", Scopes: scopeSetOf(ScopeAdmin), hash: h}}
	if _, _, ok := tok.Lookup(""); ok {
		t.Fatal("empty bearer matched the empty-string hash")
	}
}

func cmpCounter(n *int) func(a, b []byte) int {
	return func(a, b []byte) int {
		*n++
		return constantTimeCompare(a, b)
	}
}

// Structural constant-time check: every entry is compared for every well-shaped
// lookup, whichever entry matches or whether none does; a wrongly shaped value is
// refused before any comparison.
func TestLookupVisitsEveryEntry(t *testing.T) {
	var lines []string
	var toks []string
	for i := 0; i < 10; i++ {
		tk := fakeToken(byte('a' + i))
		toks = append(toks, tk)
		lines = append(lines, entryJSON("t"+string(rune('a'+i)), `"read"`, hashHex(tk)))
	}
	tok, err := loadFile(t, `{"tokens":[`+strings.Join(lines, ",")+`]}`)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	tok.cmp = cmpCounter(&n)
	for _, presented := range append(append([]string{}, toks...), fakeToken('z')) {
		n = 0
		tok.Lookup(presented)
		if n != len(toks) {
			t.Errorf("lookup compared %d entries, want all %d", n, len(toks))
		}
	}
	n = 0
	tok.Lookup("not-a-token")
	if n != 0 {
		t.Errorf("a wrongly shaped value reached %d comparisons, want 0", n)
	}

	// Same loop in api_token mode: the one entry is compared.
	leg, _ := Load(LoadOptions{APIToken: "x"})
	var m int
	leg.cmp = cmpCounter(&m)
	leg.Lookup("y")
	if m != 1 {
		t.Errorf("legacy lookup compared %d entries, want 1", m)
	}
}

func TestReport(t *testing.T) {
	var nilTokens *Tokens
	if r := nilTokens.Report(); r.Mode != "open" || r.Tokens != 0 || r.Scopes == nil || len(r.Scopes) != 0 || r.JWT {
		t.Fatalf("nil: %+v", r)
	}
	open, _ := Load(LoadOptions{})
	if r := open.Report(); r.Mode != "open" || r.Tokens != 0 || len(r.Scopes) != 0 || r.Scopes == nil {
		t.Fatalf("open: %+v", r)
	}
	leg, _ := Load(LoadOptions{APIToken: "x"})
	r := leg.Report()
	if r.Mode != "legacy_token" || r.Tokens != 1 || len(r.Scopes) != len(AllScopes) {
		t.Fatalf("legacy: %+v", r)
	}
	sco, err := loadFile(t, `{"tokens":[`+
		entryJSON("a", `"webhook"`, hashHex(fakeToken('a')))+`,`+
		entryJSON("b", `"read","engine"`, hashHex(fakeToken('b')))+`,`+
		entryJSON("c", `"read"`, hashHex(fakeToken('c')))+`]}`)
	if err != nil {
		t.Fatal(err)
	}
	r = sco.Report()
	if r.Mode != "scoped" || r.Tokens != 3 || strings.Join(r.Scopes, ",") != "engine,read,webhook" || r.JWT {
		t.Fatalf("scoped: %+v", r)
	}
}

func TestParseScopes(t *testing.T) {
	sc, err := ParseScopes("webhook, read,read")
	if err != nil || strings.Join(sc.Names(), ",") != "read,webhook" {
		t.Fatalf("%v %v", sc.Names(), err)
	}
	for _, bad := range []string{"", ",", "root", "read,,", "READ"} {
		if _, err := ParseScopes(bad); err == nil {
			t.Errorf("ParseScopes(%q) accepted", bad)
		}
	}
}

func TestGenerateTokenRoundTrip(t *testing.T) {
	tk, h, err := GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if !validShape(tk) || len(tk) != 46 {
		t.Fatalf("shape: %d chars", len(tk))
	}
	if h != hashHex(tk) {
		t.Fatal("the returned hash is not the SHA-256 of the token")
	}
	tk2, _, _ := GenerateToken()
	if tk == tk2 {
		t.Fatal("two tokens are equal")
	}
}
