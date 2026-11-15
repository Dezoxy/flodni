//go:build !linux

package library

import (
	"errors"
	"os"
	"time"
)

func statOwnerTimes(fi os.FileInfo) ownerTimes { return ownerTimes{atime: fi.ModTime()} }

func restoreOwnerTimes(path string, o ownerTimes, mtime time.Time) error {
	if err := os.Chtimes(path, o.atime, mtime); err != nil {
		return errors.New(ScrubTimesFailed)
	}
	return nil
}
