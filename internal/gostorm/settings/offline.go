package settings

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	bolt "go.etcd.io/bbolt"
)

// ErrDBInUse is returned by OpenOffline when another process holds config.db.
var ErrDBInUse = errors.New("config.db is in use")

// OpenOffline opens <dir>/config.db for the offline owner commands, without
// touching the package's global database. The daemon holds the file lock while it
// runs, so a held lock within timeout is ErrDBInUse. A missing file is an error,
// never created: a wrong directory must not leave an empty database behind. A
// read-only open takes the shared lock and writes nothing, not even a chmod. The
// caller closes it with CloseDB.
func OpenOffline(dir string, timeout time.Duration, readOnly bool) (*TDB, error) {
	path := filepath.Join(dir, "config.db")
	if fi, err := os.Stat(path); err != nil || !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("no config.db in the state directory")
	}
	var db *bolt.DB
	var err error
	if readOnly {
		db, err = bolt.Open(path, 0o600, &bolt.Options{Timeout: timeout, ReadOnly: true})
	} else {
		db, err = openBoltTimeout(path, timeout)
	}
	if err != nil {
		if errors.Is(err, bolt.ErrTimeout) {
			return nil, ErrDBInUse
		}
		return nil, fmt.Errorf("cannot open config.db: %w", err)
	}
	return &TDB{Path: dir, db: db}, nil
}

// Put is Set that reports a failed commit: one transaction, fsynced.
func (v *TDB) Put(xpath, name string, value []byte) error {
	return v.trySet(xpath, name, value)
}
