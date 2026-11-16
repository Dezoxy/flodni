package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flodni/internal/apiauth"
)

func runAuth(args ...string) (code int, out, errOut string) {
	var o, e bytes.Buffer
	code = authMain(args, &o, &e)
	return code, o.String(), e.String()
}

func TestAuthNewTokenOutputAndRoundTrip(t *testing.T) {
	code, out, errOut := runAuth("new-token", "--name", "bridge", "--scopes", "upload,engine")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout must be the token line and the entry line, got %d lines: %q", len(lines), lines)
	}
	token := lines[0]
	if !strings.HasPrefix(token, "fl_") || len(token) != 46 {
		t.Fatalf("token shape: %d chars", len(token))
	}
	// The token goes to stdout only, once.
	if strings.Contains(errOut, token) || strings.Count(out, token) != 1 {
		t.Fatal("the token must be printed exactly once, on stdout only")
	}
	// The entry parses, is the one to add, and holds the hash of the printed token.
	var entry apiauth.FileEntry
	dec := json.NewDecoder(strings.NewReader(lines[1]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entry); err != nil {
		t.Fatal(err)
	}
	if entry.Name != "bridge" || strings.Join(entry.Scopes, ",") != "engine,upload" {
		t.Fatalf("entry %+v", entry)
	}
	// Round trip: the file holding that entry loads and the printed token finds it.
	file := filepath.Join(t.TempDir(), "tokens.json")
	if err := os.WriteFile(file, []byte(`{"tokens":[`+lines[1]+`]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tok, err := apiauth.Load(apiauth.LoadOptions{TokensFile: file, GoStormURL: "http://127.0.0.1:8090"})
	if err != nil {
		t.Fatal(err)
	}
	name, sc, ok := tok.Lookup(token)
	if !ok || name != "bridge" || !sc.Has(apiauth.ScopeUpload) || !sc.Has(apiauth.ScopeEngine) || sc.Has(apiauth.ScopeRead) {
		t.Fatalf("round trip: %q %v %v", name, sc.Names(), ok)
	}
	// The hint for the owner is on stderr and holds no secret.
	if errOut == "" {
		t.Error("expected a hint on stderr")
	}
}

func TestAuthNewTokenIsDifferentEachTime(t *testing.T) {
	_, a, _ := runAuth("new-token", "--name", "a", "--scopes", "read")
	_, b, _ := runAuth("new-token", "--name", "a", "--scopes", "read")
	if strings.Split(a, "\n")[0] == strings.Split(b, "\n")[0] {
		t.Fatal("two runs printed the same token")
	}
}

func TestAuthNewTokenRefusals(t *testing.T) {
	cases := map[string][]string{
		"no subcommand":      {},
		"unknown subcommand": {"rotate"},
		"no name":            {"new-token", "--scopes", "read"},
		"no scopes":          {"new-token", "--name", "a"},
		"bad name":           {"new-token", "--name", "Bad Name", "--scopes", "read"},
		"reserved name":      {"new-token", "--name", "internal", "--scopes", "read"},
		"unknown scope":      {"new-token", "--name", "a", "--scopes", "root"},
		"empty scope item":   {"new-token", "--name", "a", "--scopes", "read,"},
		"extra argument":     {"new-token", "--name", "a", "--scopes", "read", "extra"},
		"unknown flag":       {"new-token", "--name", "a", "--scopes", "read", "--file", "x"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, errOut := runAuth(args...)
			if code != 2 {
				t.Fatalf("exit %d, want 2", code)
			}
			if out != "" {
				t.Fatalf("a refusal must print no token: %q", out)
			}
			if !strings.Contains(errOut, "usage: flodni auth new-token") {
				t.Fatalf("stderr %q", errOut)
			}
		})
	}
}

func fakeAuthToken(c byte) string { return "fl_" + strings.Repeat(string(c), 43) }

func authHash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
