package daemon

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"flodni/internal/config"

	"github.com/hanwen/go-fuse/v2/fuse"
)

func dirAttr(t *testing.T, dir string) fuse.Attr {
	t.Helper()
	prev := globalConfig.Load()
	globalConfig.Store(&config.Config{FuseBlockSize: 1 << 20})
	t.Cleanup(func() { globalConfig.Store(prev) })
	var st syscall.Stat_t
	if err := syscall.Stat(dir, &st); err != nil {
		t.Fatal(err)
	}
	var a fuse.Attr
	fillAttrFromStat(&st, &a)
	return a
}

// A directory's times come from the physical directory: adding a stub moves them,
// an unrelated stat does not, and two stats in a row agree. time.Now() made every
// stat look like a change.
func TestDirectoryTimesComeFromThePhysicalDirectory(t *testing.T) {
	dir := t.TempDir()
	old := time.Now().Add(-2 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}

	a1 := dirAttr(t, dir)
	time.Sleep(1100 * time.Millisecond)
	a2 := dirAttr(t, dir)
	if a1.Mtime != a2.Mtime || a1.Ctime != a2.Ctime || a1.Atime != a2.Atime {
		t.Errorf("two stats differ: %+v vs %+v", a1, a2)
	}
	if a1.Mtime != uint64(old.Unix()) {
		t.Errorf("mtime = %d, want the directory's %d", a1.Mtime, old.Unix())
	}

	if err := os.WriteFile(filepath.Join(dir, "new.mkv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	a3 := dirAttr(t, dir)
	if a3.Mtime == a1.Mtime {
		t.Error("adding a stub did not change the directory mtime")
	}
}
