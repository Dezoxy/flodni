package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
)

// Flodni only observes the mount's kernel read-ahead. It never writes sysfs: the
// host may tune /sys/class/bdi/<id>/read_ahead_kb, and the gauge and the one start
// line show what is in effect.

// sysBDIRoot is where the kernel lists backing devices. A variable so a test can
// point it at a fake tree.
var sysBDIRoot = "/sys/class/bdi"

// mountBDI is the BDI id ("major:minor") of the FUSE mount, set once at start. Nil
// until known and on a platform without sysfs.
var mountBDI atomic.Pointer[string]

// bdiName turns a st_dev into the "major:minor" name sysfs uses, with the Linux
// encoding (glibc major/minor).
func bdiName(dev uint64) string {
	major := (dev>>8)&0xfff | (dev>>32)&0xfffff000
	minor := dev&0xff | (dev>>12)&0xffffff00
	return fmt.Sprintf("%d:%d", major, minor)
}

// readAheadKB reads the kernel read-ahead of one backing device, in KiB.
func readAheadKB(root, bdi string) (int64, error) {
	b, err := os.ReadFile(filepath.Join(root, bdi, "read_ahead_kb"))
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}

// mountObservedLine is the one line logged when the mount is up.
func mountObservedLine(root, bdi string) string {
	ra := "unreadable"
	if kb, err := readAheadKB(root, bdi); err == nil {
		ra = strconv.FormatInt(kb, 10)
	}
	return fmt.Sprintf("[FUSE] mounted, bdi %s, read_ahead_kb %s", bdi, ra)
}

// observeMountBDI finds the mount's BDI, remembers it for the gauge and logs it
// once. Quiet where the platform has none.
func observeMountBDI(mountPath string) {
	bdi, err := statBDI(mountPath)
	if err != nil {
		return
	}
	mountBDI.Store(&bdi)
	logger.Info(mountObservedLine(sysBDIRoot, bdi), "bdi", bdi)
}
