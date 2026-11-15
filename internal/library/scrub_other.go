//go:build !linux

package library

import "os"

func statOwnerTimes(fi os.FileInfo) ownerTimes { return ownerTimes{atime: fi.ModTime()} }

func chownFile(*os.File, ownerTimes) error { return nil }
