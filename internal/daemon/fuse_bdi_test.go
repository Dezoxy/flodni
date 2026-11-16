package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"flodni/internal/promtext"
)

// fakeBDI builds a fake sysfs bdi directory holding read_ahead_kb.
func fakeBDI(t *testing.T, bdi, content string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, bdi), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, bdi, "read_ahead_kb"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestBDIName(t *testing.T) {
	// Linux st_dev layout: major 0, minor 281 is a FUSE anonymous device.
	// minor 281 = 0x119: low 8 bits 0x19, the rest from bit 20 of dev.
	dev := uint64(0x19) | (uint64(0x1) << 20)
	cases := []struct {
		dev  uint64
		want string
	}{
		{dev, "0:281"},
		{(8 << 8) | 1, "8:1"},
		{0, "0:0"},
		// major above 0xfff lives from bit 44 of dev: 0x1034 = 4148
		{(uint64(0x1) << 44) | (uint64(0x34) << 8), "4148:0"},
	}
	for _, c := range cases {
		if got := bdiName(c.dev); got != c.want {
			t.Errorf("bdiName(%#x) = %q, want %q", c.dev, got, c.want)
		}
	}
}

func TestReadAheadKBFromFakeSysfs(t *testing.T) {
	root := fakeBDI(t, "0:281", "128\n")
	got, err := readAheadKB(root, "0:281")
	if err != nil || got != 128 {
		t.Fatalf("readAheadKB = %d, %v; want 128, nil", got, err)
	}
	if _, err := readAheadKB(root, "0:999"); err == nil {
		t.Error("a missing bdi must be an error")
	}
	bad := fakeBDI(t, "0:281", "lots\n")
	if _, err := readAheadKB(bad, "0:281"); err == nil {
		t.Error("an unparsable value must be an error")
	}
}

func TestReadAheadGauge(t *testing.T) {
	root := fakeBDI(t, "0:281", "4096\n")
	oldRoot := sysBDIRoot
	sysBDIRoot = root
	t.Cleanup(func() { sysBDIRoot = oldRoot; mountBDI.Store(nil) })

	scrape := func() string {
		var pw promtext.Writer
		collectProm(&pw)
		if err := pw.Err(); err != nil {
			t.Fatal(err)
		}
		return string(pw.Bytes())
	}

	mountBDI.Store(nil)
	if strings.Contains(scrape(), "flodni_fuse_read_ahead_kb") {
		t.Error("no series before the mount's bdi is known")
	}

	b := "0:281"
	mountBDI.Store(&b)
	if out := scrape(); !strings.Contains(out, "flodni_fuse_read_ahead_kb 4096\n") {
		t.Errorf("gauge missing or wrong:\n%s", grepLines(out, "read_ahead"))
	}

	// The host tunes it; the next scrape sees the new value, with no restart.
	if err := os.WriteFile(filepath.Join(root, "0:281", "read_ahead_kb"), []byte("128\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out := scrape(); !strings.Contains(out, "flodni_fuse_read_ahead_kb 128\n") {
		t.Errorf("gauge did not follow the file:\n%s", grepLines(out, "read_ahead"))
	}

	// Unreadable: absent, not zero.
	if err := os.Remove(filepath.Join(root, "0:281", "read_ahead_kb")); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(scrape(), "flodni_fuse_read_ahead_kb ") {
		t.Error("an unreadable file must leave the series absent")
	}
}

func TestMountObservedLine(t *testing.T) {
	root := fakeBDI(t, "0:281", "128\n")
	if got, want := mountObservedLine(root, "0:281"), "[FUSE] mounted, bdi 0:281, read_ahead_kb 128"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
	if got, want := mountObservedLine(root, "0:9"), "[FUSE] mounted, bdi 0:9, read_ahead_kb unreadable"; got != want {
		t.Errorf("line = %q, want %q", got, want)
	}
}

func grepLines(s, sub string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}
