package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	auditHashA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	auditHashB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func auditStub(t *testing.T, e *charEnv, dir, name, hash string, index int, size int64) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteStub(p, e.m.streamURL(hash, index), size, "m", ""); err != nil {
		t.Fatal(err)
	}
	return p
}

// snapshot is every file under root with its bytes, to prove the audit writes nothing.
func auditSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, rerr := os.ReadFile(p)
		out[p] = string(b) + info.ModTime().String()
		return rerr
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAuditReportsMismatchesCountsNoFileListAndWritesNothing(t *testing.T) {
	e := newCharEnv(t)
	records := map[string][]TorrentFile{
		auditHashA: {{Index: 3, Path: "a.mkv", Length: 100}, {Index: 7, Path: "b.mkv", Length: 200}},
	}
	e.m.cfg.StoredFiles = func(h string) []TorrentFile { return records[h] }

	good := auditStub(t, e, e.movies, "good.mkv", auditHashA, 7, 200)
	wrongLen := auditStub(t, e, e.movies, "wrong.mkv", auditHashA, 3, 200) // index 3 is 100 bytes
	noFile := auditStub(t, e, e.tv, "nofile.mkv", auditHashA, 5, 200)      // no file 5
	_ = auditStub(t, e, e.tv, "nolist.mkv", auditHashB, 1, 10)             // record has no list
	_ = auditStub(t, e, e.tv, "nolist2.mkv", auditHashB, 2, 10)

	before := auditSnapshot(t, filepath.Dir(e.movies))
	rep, err := e.m.Audit()
	if err != nil {
		t.Fatal(err)
	}
	if after := auditSnapshot(t, filepath.Dir(e.movies)); len(after) != len(before) {
		t.Fatalf("files changed: %d -> %d", len(before), len(after))
	} else {
		for k, v := range before {
			if after[k] != v {
				t.Fatalf("%s changed", k)
			}
		}
	}

	if rep.Checked != 3 {
		t.Errorf("checked = %d, want 3 (stubs with no file list are counted apart)", rep.Checked)
	}
	if rep.MismatchedTotal != 2 || len(rep.Mismatched) != 2 {
		t.Fatalf("mismatched = %+v", rep.Mismatched)
	}
	by := map[string]AuditMismatch{}
	for _, m := range rep.Mismatched {
		by[m.Path] = m
	}
	if _, ok := by[good]; ok {
		t.Error("the correct stub was reported")
	}
	w := by[wrongLen]
	if w.Reason != AuditLengthDiffers || w.EngineLength == nil || *w.EngineLength != 100 ||
		w.Hash != auditHashA || w.Index != 3 || w.StubSize != 200 {
		t.Errorf("wrong-length entry = %+v", w)
	}
	n := by[noFile]
	if n.Reason != AuditNoSuchFile || n.EngineLength != nil {
		t.Errorf("no-file entry = %+v", n)
	}
	if rep.NoFileList.Stubs != 2 || rep.NoFileList.Torrents != 1 || len(rep.NoFileList.Hashes) != 1 || rep.NoFileList.Hashes[0] != auditHashB {
		t.Errorf("no_file_list = %+v", rep.NoFileList)
	}
}

func TestAuditCapsTheArraysAndKeepsTheTotals(t *testing.T) {
	e := newCharEnv(t)
	e.m.cfg.StoredFiles = func(string) []TorrentFile { return []TorrentFile{{Index: 1, Length: 1}} }
	for i := 0; i < auditCap+5; i++ {
		auditStub(t, e, e.movies, strings.Repeat("x", 1)+string(rune('a'+i%26))+string(rune('a'+i/26))+".mkv", auditHashA, 1, 2)
	}
	rep, err := e.m.Audit()
	if err != nil {
		t.Fatal(err)
	}
	if rep.MismatchedTotal != auditCap+5 || len(rep.Mismatched) != auditCap || !rep.Truncated {
		t.Fatalf("total %d, listed %d, truncated %v", rep.MismatchedTotal, len(rep.Mismatched), rep.Truncated)
	}
}

func TestAuditWithoutAStoredRecordSourceIsUnavailable(t *testing.T) {
	e := newCharEnv(t)
	if _, err := e.m.Audit(); err == nil {
		t.Fatal("want an error when no record source is wired")
	}
}
