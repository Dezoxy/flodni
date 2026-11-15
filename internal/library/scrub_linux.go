//go:build linux

package library

import (
	"errors"
	"os"
	"syscall"
	"time"
)

func statOwnerTimes(fi os.FileInfo) ownerTimes {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return ownerTimes{atime: fi.ModTime()}
	}
	return ownerTimes{atime: time.Unix(int64(st.Atim.Sec), int64(st.Atim.Nsec)), uid: int(st.Uid), gid: int(st.Gid), hasOwner: true}
}

// restoreOwnerTimes puts the old owner, atime and mtime back on the file the atomic
// write just made.
func restoreOwnerTimes(path string, o ownerTimes, mtime time.Time) error {
	var ownerErr error
	if o.hasOwner {
		if fi, err := os.Lstat(path); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && (int(st.Uid) != o.uid || int(st.Gid) != o.gid) {
				if os.Chown(path, o.uid, o.gid) != nil {
					ownerErr = errors.New(ScrubOwnerFailed)
				}
			}
		}
	}
	if err := os.Chtimes(path, o.atime, mtime); err != nil {
		return errors.New(ScrubTimesFailed)
	}
	return ownerErr
}
