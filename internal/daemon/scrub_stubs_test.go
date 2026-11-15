package daemon

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const scrubTestHash = "1111111122222222333333334444444455555555"

func TestScrubStubsCommandSummarisesWithoutALeak(t *testing.T) {
	src := t.TempDir()
	p := filepath.Join(src, "movies", "A (2020)", "a.mkv")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"imdb":"tt0000001","magnet":"magnet:?xt=urn:btih:` + scrubTestHash +
		`&tr=http%3A%2F%2Fseeder.example%2Fannounce%3Fpasskey%3D0123456789abcdef","size":9,"url":"http://engine/stream?link=` + scrubTestHash + `&index=1&play"}`
	// encoding/json spells & as a backslash-u0026 escape; so do the stubs on disk.
	old = strings.ReplaceAll(old, "&", "\\"+"u0026")
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--dry-run", src}, "scanned=1 rewritten=1 skipped=0 errors=0 (dry run"},
		{[]string{src}, "scanned=1 rewritten=1 skipped=0 errors=0\n"},
		{[]string{src}, "scanned=1 rewritten=0 skipped=1 errors=0\n"},
	} {
		var out, errOut bytes.Buffer
		if code := scrubStubsMain(tc.args, &out, &errOut); code != 0 {
			t.Fatalf("%v: exit %d: %s", tc.args, code, errOut.String())
		}
		if !strings.Contains(out.String(), tc.want) {
			t.Errorf("%v: output %q lacks %q", tc.args, out.String(), tc.want)
		}
		for _, s := range []string{out.String(), errOut.String()} {
			for _, bad := range []string{"://", "magnet", "passkey", "seeder", scrubTestHash} {
				if strings.Contains(s, bad) {
					t.Errorf("%v: output carries %q: %s", tc.args, bad, s)
				}
			}
		}
		if len(strings.Split(strings.TrimSpace(out.String()), "\n")) != 1 {
			t.Errorf("want one summary line, got %q", out.String())
		}
	}
	got, _ := os.ReadFile(p)
	if want := "magnet:?xt=urn:btih:" + scrubTestHash; !strings.Contains(string(got), want) || strings.Contains(string(got), "passkey") {
		t.Errorf("stub after the run: %s", got)
	}
	if fi, _ := os.Stat(p); !fi.ModTime().Equal(mtime) {
		t.Errorf("mtime = %v, want %v", fi.ModTime(), mtime)
	}
}

func TestScrubStubsCommandRefusesAMissingSource(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := scrubStubsMain([]string{filepath.Join(t.TempDir(), "nope")}, &out, &errOut); code != 2 {
		t.Fatalf("exit %d, want 2", code)
	}
}
