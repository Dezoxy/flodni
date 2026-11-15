//go:build linux

package library

import (
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

// chownFile gives the temp file the stub's owner (fchown, by descriptor) when it
// differs from the file's current one, which is the caller's.
func chownFile(f *os.File, o ownerTimes) error {
	if !o.hasOwner {
		return nil
	}
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) == o.uid && int(st.Gid) == o.gid {
		return nil
	}
	return f.Chown(o.uid, o.gid)
}
