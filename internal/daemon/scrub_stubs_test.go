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
		{[]string{"--allow-root", "--dry-run", src}, "scanned=1 rewritten=1 skipped=0 errors=0 (dry run"},
		{[]string{"--allow-root", src}, "scanned=1 rewritten=1 skipped=0 errors=0\n"},
		{[]string{"--allow-root", src}, "scanned=1 rewritten=0 skipped=1 errors=0\n"},
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

func TestScrubStubsCommandRefusesBadInvocations(t *testing.T) {
	src := t.TempDir()
	p := filepath.Join(src, "tv", "S", "a.mkv")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	old := `{"imdb":"","magnet":"magnet:?xt=urn:btih:` + scrubTestHash + `&tr=http%3A%2F%2Fseeder.example%2Fannounce","size":9,"url":"http://engine/stream?link=` + scrubTestHash + `"}`
	old = strings.ReplaceAll(old, "&", "\\"+"u0026") // the way encoding/json writes it
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	empty := t.TempDir()
	if err := os.MkdirAll(filepath.Join(empty, "movies"), 0o755); err != nil {
		t.Fatal(err)
	}
	asRoot := func() int { return 0 }
	for _, tc := range []struct {
		name string
		args []string
		euid func() int
	}{
		{"missing source", []string{"--allow-root", filepath.Join(t.TempDir(), "nope")}, nil},
		{"no source", []string{"--allow-root"}, nil},
		{"trailing flag", []string{"--allow-root", src, "--dry-run"}, nil},
		{"two sources", []string{"--allow-root", src, src}, nil},
		{"no movies or tv", []string{"--allow-root", t.TempDir()}, nil},
		{"nothing scanned", []string{"--allow-root", empty}, nil},
		{"root without --allow-root", []string{src}, asRoot},
	} {
		if tc.euid != nil {
			old := scrubEuid
			scrubEuid = tc.euid
			t.Cleanup(func() { scrubEuid = old })
		}
		var out, errOut bytes.Buffer
		if code := scrubStubsMain(tc.args, &out, &errOut); code != 2 {
			t.Errorf("%s: exit %d, want 2 (out %q err %q)", tc.name, code, out.String(), errOut.String())
		}
		if got, _ := os.ReadFile(p); string(got) != old {
			t.Errorf("%s: the stub was written", tc.name)
		}
	}
}
