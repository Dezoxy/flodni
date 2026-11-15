package library

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flodni/internal/atomicfile"
)

var scrubOldTime = time.Date(2024, 3, 4, 5, 6, 7, 123456789, time.UTC)

// oldStub writes a stub the way older versions did: the request's magnet, trackers
// and all, with a fixed mtime and atime so a rewrite is easy to spot.
func oldStub(t *testing.T, path, hash, imdb string, mode os.FileMode) {
	t.Helper()
	data, err := marshalStub(map[string]interface{}{
		"url":    "http://engine/stream?link=" + hash + "&index=2&play",
		"size":   int64(1234567890123),
		"magnet": trackerMagnet(hash),
		"imdb":   imdb,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, scrubOldTime.Add(time.Hour), scrubOldTime); err != nil {
		t.Fatal(err)
	}
}

func TestScrubRewritesTrackersAwayAndKeepsTheRest(t *testing.T) {
	root := t.TempDir()
	movies, tv := filepath.Join(root, "movies"), filepath.Join(root, "tv")
	a := filepath.Join(movies, "A (2020)", "a.mkv")
	b := filepath.Join(tv, "Show", "Season.01", "b.mkv")
	oldStub(t, a, charHash, "tt0000001", 0o644)
	oldStub(t, b, charOtherHash, "", 0o640)
	before := map[string]os.FileInfo{}
	for _, p := range []string{a, b} {
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		before[p] = fi
	}

	res := ScrubStubs([]string{movies, tv, filepath.Join(root, "missing")}, false)
	if res.Scanned != 2 || res.Rewritten != 2 || res.Skipped != 0 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	afters := map[string]os.FileInfo{}
	for _, p := range []string{a, b} { // before anything below reads the files
		fi, err := os.Lstat(p)
		if err != nil {
			t.Fatal(err)
		}
		afters[p] = fi
	}
	for p, hash := range map[string]string{a: charHash, b: charOtherHash} {
		assertCleanStub(t, p, hash)
		// Same bytes as a fresh WriteStub of the same stub: only the magnet changed.
		imdb := map[string]string{a: "tt0000001", b: ""}[p]
		ref := filepath.Join(t.TempDir(), "ref.mkv")
		if err := WriteStub(ref, "http://engine/stream?link="+hash+"&index=2&play", 1234567890123, hash, imdb); err != nil {
			t.Fatal(err)
		}
		if got, want := readFile(t, p), readFile(t, ref); got != want {
			t.Errorf("%s\n got %s\nwant %s", filepath.Base(p), got, want)
		}
		after := afters[p]
		if !after.ModTime().Equal(before[p].ModTime()) || !after.ModTime().Equal(scrubOldTime) {
			t.Errorf("%s: mtime %v -> %v", filepath.Base(p), before[p].ModTime(), after.ModTime())
		}
		if got, want := statOwnerTimes(after).atime, statOwnerTimes(before[p]).atime; !got.Equal(want) {
			t.Errorf("%s: atime %v -> %v", filepath.Base(p), want, got)
		}
		if after.Mode() != before[p].Mode() {
			t.Errorf("%s: mode %v -> %v", filepath.Base(p), before[p].Mode(), after.Mode())
		}
		if os.SameFile(after, before[p]) {
			t.Errorf("%s: not rewritten through a new file", filepath.Base(p))
		}
	}
	// A second run finds nothing to do.
	if res := ScrubStubs([]string{movies, tv}, false); res.Rewritten != 0 || res.Skipped != 2 {
		t.Errorf("second run = %+v", res)
	}
}

func TestScrubDryRunChangesNothing(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.mkv")
	oldStub(t, p, charHash, "tt0000001", 0o644)
	want := readFile(t, p)
	fi, _ := os.Lstat(p)

	res := ScrubStubs([]string{root}, true)
	if res.Scanned != 1 || res.Rewritten != 1 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	after, _ := os.Lstat(p)
	if readFile(t, p) != want || !os.SameFile(fi, after) || !after.ModTime().Equal(scrubOldTime) {
		t.Error("a dry run touched the stub")
	}
}

func TestScrubLeavesACleanStubAndNonStubsUntouched(t *testing.T) {
	root := t.TempDir()
	clean := filepath.Join(root, "clean.mkv")
	if err := WriteStub(clean, "http://engine/stream?link="+charHash+"&index=1&play", 5, charHash, ""); err != nil {
		t.Fatal(err)
	}
	empty := filepath.Join(root, "empty-magnet.mkv")
	if err := os.WriteFile(empty, []byte(`{"imdb":"","magnet":"","size":5,"url":"http://e/s"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	notStub := map[string][]byte{
		"video.mkv":    append([]byte{0x1a, 0x45, 0xdf, 0xa3}, bytes.Repeat([]byte("x"), 100)...),
		"big.mkv":      bytes.Repeat([]byte("x"), maxStubBytes+1),
		"json.mkv":     []byte(`{"magnet":"magnet:?xt=urn:btih:` + charHash + `&tr=http%3A%2F%2Fa","other":1}`),
		"notes.txt":    []byte(`{"url":"http://e/s","size":1,"magnet":"magnet:?xt=urn:btih:` + charHash + `&tr=x"}`),
		".hidden.tmp-": []byte("x"),
	}
	paths := []string{clean, empty}
	for name, data := range notStub {
		p := filepath.Join(root, name)
		if err := os.WriteFile(p, data, 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	var infos []os.FileInfo
	var contents []string
	for _, p := range paths {
		if err := os.Chtimes(p, scrubOldTime, scrubOldTime); err != nil {
			t.Fatal(err)
		}
		fi, _ := os.Lstat(p)
		infos = append(infos, fi)
		contents = append(contents, readFile(t, p))
	}

	res := ScrubStubs([]string{root}, false)
	if res.Rewritten != 0 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v, want nothing rewritten", res)
	}
	for i, p := range paths {
		fi, err := os.Lstat(p)
		if err != nil || !os.SameFile(fi, infos[i]) || !fi.ModTime().Equal(scrubOldTime) || readFile(t, p) != contents[i] {
			t.Errorf("%s was touched", filepath.Base(p))
		}
	}
}

func TestScrubRewritesALegacyLineStub(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "legacy.mkv")
	old := "http://engine/stream?link=" + charHash + "&index=3\n4321\n" + trackerMagnet(charHash) + "\ntt7654321"
	if err := os.WriteFile(p, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, scrubOldTime, scrubOldTime); err != nil {
		t.Fatal(err)
	}
	if res := ScrubStubs([]string{root}, false); res.Rewritten != 1 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	want := "http://engine/stream?link=" + charHash + "&index=3\n4321\nmagnet:?xt=urn:btih:" + charHash + "\ntt7654321"
	if got := readFile(t, p); got != want {
		t.Errorf("legacy stub\n got %q\nwant %q", got, want)
	}
	if fi, _ := os.Lstat(p); !fi.ModTime().Equal(scrubOldTime) {
		t.Errorf("mtime = %v", fi.ModTime())
	}
	if st, err := readStub(p); err != nil || st.Hash != charHash || st.Size != 4321 || st.IMDB != "tt7654321" {
		t.Errorf("readStub = %+v, %v", st, err)
	}
}

// A stub the encoder would not reproduce byte for byte is left alone and reported,
// with a fixed reason that carries nothing from the file.
func TestScrubLeavesAnUnexpectedLayoutAlone(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "odd.mkv")
	odd := `{"url": "http://e/s?link=` + charHash + `", "size": 1, "magnet": "` + trackerMagnet(charHash) + `"}`
	if err := os.WriteFile(p, []byte(odd), 0o644); err != nil {
		t.Fatal(err)
	}
	res := ScrubStubs([]string{root}, false)
	if res.Rewritten != 0 || len(res.Errors) != 1 || res.Errors[0].Reason != ScrubLayout {
		t.Fatalf("result = %+v", res)
	}
	if readFile(t, p) != odd {
		t.Error("the stub was changed")
	}
	if b, _ := json.Marshal(res.Errors); strings.Contains(string(b), "passkey") {
		t.Error("an error carries file content")
	}
}

// stubFields writes a stub in the written layout with the given magnet value.
func stubFields(t *testing.T, path string, fields map[string]interface{}) {
	t.Helper()
	data, err := marshalStub(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, scrubOldTime, scrubOldTime); err != nil {
		t.Fatal(err)
	}
}

func withBeforeRename(t *testing.T, f func(path string)) {
	t.Helper()
	old := scrubBeforeRename
	scrubBeforeRename = f
	t.Cleanup(func() { scrubBeforeRename = old })
}

// The new file already has the old times when it is renamed over the stub: a mount
// that looks the stub up right after the rename never sees another mtime.
func TestScrubSetsTheTimesBeforeTheRename(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.mkv")
	oldStub(t, p, charHash, "tt0000001", 0o644)
	checked := false
	withBeforeRename(t, func(path string) {
		ents, _ := os.ReadDir(root)
		for _, e := range ents {
			if strings.Contains(e.Name(), ".tmp-") {
				fi, err := os.Lstat(filepath.Join(root, e.Name()))
				if err != nil || !fi.ModTime().Equal(scrubOldTime) {
					t.Errorf("temp file mtime = %v (%v), want the stub's", fi.ModTime(), err)
				}
				checked = true
			}
		}
		if strings.Contains(readFile(t, path), "passkey") != true {
			t.Error("the stub changed before the rename")
		}
	})
	if res := ScrubStubs([]string{root}, false); res.Rewritten != 1 {
		t.Fatalf("result = %+v", res)
	}
	if !checked {
		t.Fatal("no temp file seen before the rename")
	}
	if fi, _ := os.Lstat(p); !fi.ModTime().Equal(scrubOldTime) {
		t.Errorf("mtime = %v", fi.ModTime())
	}
}

func TestScrubDoesNotRecreateAStubRemovedBeforeTheRename(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.mkv")
	oldStub(t, p, charHash, "", 0o644)
	withBeforeRename(t, func(path string) { _ = os.Remove(path) })
	res := ScrubStubs([]string{root}, false)
	if res.Rewritten != 0 || res.Skipped != 1 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Errorf("directory holds %d entries, want none (no stub, no temp file)", len(ents))
	}
}

func TestScrubDoesNotOverwriteAReplacedStub(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "a.mkv")
	oldStub(t, p, charHash, "", 0o644)
	var daemonWrote string
	withBeforeRename(t, func(path string) {
		// The daemon's write: a new file, a new inode, the same size and mtime.
		data := []byte(readFile(t, path))
		if err := atomicfile.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, scrubOldTime, scrubOldTime); err != nil {
			t.Fatal(err)
		}
		daemonWrote = readFile(t, path)
	})
	res := ScrubStubs([]string{root}, false)
	if res.Rewritten != 0 || res.Skipped != 1 {
		t.Fatalf("result = %+v", res)
	}
	if readFile(t, p) != daemonWrote {
		t.Error("the replaced stub was overwritten")
	}
	if ents, _ := os.ReadDir(root); len(ents) != 1 {
		t.Errorf("directory holds %d entries, want only the stub", len(ents))
	}
}

func TestScrubRestoresTheOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to give a file to another owner")
	}
	root := t.TempDir()
	p := filepath.Join(root, "a.mkv")
	oldStub(t, p, charHash, "", 0o644)
	if err := os.Chown(p, 12345, 12346); err != nil {
		t.Fatal(err)
	}
	if res := ScrubStubs([]string{root}, false); res.Rewritten != 1 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	o := statOwnerTimes(mustLstat(t, p))
	if o.uid != 12345 || o.gid != 12346 {
		t.Errorf("owner = %d:%d, want 12345:12346", o.uid, o.gid)
	}
}

func mustLstat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

func TestScrubNeverFollowsASymlink(t *testing.T) {
	outside, root := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "t.mkv")
	oldStub(t, target, charHash, "", 0o644)
	want := readFile(t, target)
	link := filepath.Join(root, "link.mkv")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	res := ScrubStubs([]string{root}, false)
	if res.Scanned != 0 || res.Rewritten != 0 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if readFile(t, target) != want || mustLstat(t, link).Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink or its target was touched")
	}
}

func TestScrubReportsWhatItCannotRebuild(t *testing.T) {
	root := t.TempDir()
	cases := map[string]struct {
		fields map[string]interface{}
		reason string
	}{
		"nohash.mkv": {map[string]interface{}{"url": "http://e/s", "size": 1, "imdb": "", "magnet": "magnet:?tr=http%3A%2F%2Fx"}, ScrubNoHash},
		"number.mkv": {map[string]interface{}{"url": "http://e/s?link=" + charHash, "size": 1, "imdb": "", "magnet": 5}, ScrubMagnetFormat},
	}
	for name, c := range cases {
		stubFields(t, filepath.Join(root, name), c.fields)
	}
	legacy := filepath.Join(root, "legacy.mkv")
	if err := os.WriteFile(legacy, []byte("http://e/s?link="+charHash+"\n10\nnot a magnet at all\ntt1234567"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := ScrubStubs([]string{root}, false)
	got := map[string]string{}
	for _, e := range res.Errors {
		got[filepath.Base(e.Path)] = e.Reason
	}
	want := map[string]string{"nohash.mkv": ScrubNoHash, "number.mkv": ScrubMagnetFormat, "legacy.mkv": ScrubNotMagnet}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: reason %q, want %q (all: %v)", k, got[k], v, got)
		}
	}
	if res.Rewritten != 0 {
		t.Errorf("rewritten = %d", res.Rewritten)
	}
}

// A stub with no imdb field is matched for an id by a regex over its whole content,
// the magnet included: its magnet is left alone so that id is not lost.
func TestScrubLeavesTheMagnetOfAStubWhoseImdbComesFromIt(t *testing.T) {
	root := t.TempDir()
	keep := filepath.Join(root, "keep.mkv")
	stubFields(t, keep, map[string]interface{}{"url": "http://e/s?link=" + charHash, "size": 1, "imdb": "",
		"magnet": "magnet:?xt=urn:btih:" + charHash + "&dn=Title.tt1234567&tr=http%3A%2F%2Fx"})
	rewrite := filepath.Join(root, "rewrite.mkv")
	stubFields(t, rewrite, map[string]interface{}{"url": "http://e/s?link=" + charHash, "size": 1, "imdb": "tt7654321",
		"magnet": "magnet:?xt=urn:btih:" + charHash + "&dn=Title.tt1234567&tr=http%3A%2F%2Fx"})
	want := readFile(t, keep)
	res := ScrubStubs([]string{root}, false)
	if res.Rewritten != 1 || res.Skipped != 1 || len(res.Errors) != 0 {
		t.Fatalf("result = %+v", res)
	}
	if readFile(t, keep) != want {
		t.Error("the stub whose id lives in its magnet was rewritten")
	}
	assertCleanStub(t, rewrite, charHash)
}
