//go:build linux

package daemon

import (
	"syscall"
	"time"
)

// statTimes returns the modification and change times recorded in st.
func statTimes(st *syscall.Stat_t) (mtime, ctime time.Time) {
	return time.Unix(int64(st.Mtim.Sec), int64(st.Mtim.Nsec)), time.Unix(int64(st.Ctim.Sec), int64(st.Ctim.Nsec))
}
