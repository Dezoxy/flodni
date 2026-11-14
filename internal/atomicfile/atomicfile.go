// Package atomicfile replaces a file so a crash never leaves it truncated.
package atomicfile

import (
	"os"
	"path/filepath"
)

// WriteFile writes data to path through a temporary file in the same
// directory: write, fsync, rename over path, then fsync the directory. A reader
// sees the old content or the new content, never a partial file.
//
// The temporary name is ".<name>.tmp-<random>". It does not end in ".mkv", so
// the FUSE layer and the library walks (isStubPath in main.go) never take it
// for a stub. The temporary file is removed on any error. The directory fsync
// is best effort: the rename has happened by then.
func WriteFile(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if err != nil {
			// The write error is the one to report; cleanup is best effort.
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		return err
	}
	// CreateTemp makes the file 0600; the target keeps the caller's mode.
	if err = tmp.Chmod(perm); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmpName, path); err != nil {
		return err
	}
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync() // best effort, see above
		_ = d.Close()
	}
	return nil
}

// WriteFileKeepMode is WriteFile for a file that may hold secrets: when path
// exists its permission bits are kept, otherwise the file is created with
// newPerm. Ownership is not touched.
func WriteFileKeepMode(path string, data []byte, newPerm os.FileMode) error {
	perm := newPerm
	if fi, err := os.Stat(path); err == nil {
		perm = fi.Mode().Perm()
	}
	return WriteFile(path, data, perm)
}
