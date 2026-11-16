//go:build !linux

package daemon

import "errors"

// statBDI is Linux-only; the daemon runs in the Linux image.
func statBDI(string) (string, error) {
	return "", errors.New("backing device info is only available on Linux")
}
