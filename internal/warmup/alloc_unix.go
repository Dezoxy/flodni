//go:build linux || darwin

package warmup

import (
	"os"
	"syscall"
)

// diskBytes is what a file holds on disk: st_blocks in 512-byte units. A sparse file
// counts its written blocks, not its length. A filesystem that reports no blocks for a
// non-empty file falls back to the length, so the quota never counts it as free.
func diskBytes(fi os.FileInfo) int64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		if st.Blocks == 0 && fi.Size() > 0 {
			return fi.Size()
		}
		return int64(st.Blocks) * 512
	}
	return fi.Size()
}
