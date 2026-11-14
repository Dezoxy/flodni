//go:build linux || darwin

package warmup

import (
	"os"
	"syscall"
	"testing"
	"time"
)

type fakeInfo struct {
	size   int64
	blocks int64
}

func (f fakeInfo) Name() string       { return "x" }
func (f fakeInfo) Size() int64        { return f.size }
func (f fakeInfo) Mode() os.FileMode  { return 0 }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return false }
func (f fakeInfo) Sys() any           { return &syscall.Stat_t{Blocks: f.blocks} }

// A filesystem that reports no blocks for a non-empty file must not count it as free.
func TestDiskBytesFallsBackWhenBlocksAreZero(t *testing.T) {
	if got := diskBytes(fakeInfo{size: 5000}); got != 5000 {
		t.Fatalf("no blocks, 5000 bytes: counted %d, want 5000", got)
	}
	if got := diskBytes(fakeInfo{size: 0}); got != 0 {
		t.Fatalf("empty file counted %d, want 0", got)
	}
	if got := diskBytes(fakeInfo{size: 1 << 30, blocks: 8}); got != 4096 {
		t.Fatalf("sparse file counted %d, want 4096", got)
	}
}
