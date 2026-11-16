package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const unchangedHash = "0123456789abcdef0123456789abcdef01234567"

// An identical rewrite must not touch the file: a new inode and a new mtime make a
// scan and a media server see a change that never happened.
func TestWriteStubSkipsAnIdenticalRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "x.mkv")
	if err := WriteStub(path, "http://e/stream?link=h&index=1&play", 10, unchangedHash, "tt1"); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteStub(path, "http://e/stream?link=h&index=1&play", 10, unchangedHash, "tt1"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("an identical rewrite replaced the file (inode changed)")
	}
	if !after.ModTime().Equal(old) {
		t.Errorf("an identical rewrite changed mtime: %v -> %v", old, after.ModTime())
	}
}

func TestWriteStubReplacesAChangedStubAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.mkv")
	if err := WriteStub(path, "http://e/stream?link=h&index=1&play", 10, unchangedHash, "tt1"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	if err := WriteStub(path, "http://e/stream?link=h&index=2&play", 10, unchangedHash, "tt1"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(path)
	if os.SameFile(before, after) {
		t.Error("a changed stub was written in place, not replaced")
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "index=2") {
		t.Errorf("stub not updated: %s", got)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temp file left behind: %v", entries)
	}
}
