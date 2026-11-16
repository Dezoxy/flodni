//go:build darwin

package daemon

import (
	"syscall"
	"time"
)

// statTimes returns the modification and change times recorded in st.
func statTimes(st *syscall.Stat_t) (mtime, ctime time.Time) {
	return time.Unix(st.Mtimespec.Sec, st.Mtimespec.Nsec), time.Unix(st.Ctimespec.Sec, st.Ctimespec.Nsec)
}
