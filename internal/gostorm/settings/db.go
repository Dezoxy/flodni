package settings

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"flodni/internal/gostorm/log"

	bolt "go.etcd.io/bbolt"
)

type TDB struct {
	Path string
	db   *bolt.DB
}

var globalBboltDB GoStormDB

func NewTDB() GoStormDB {
	if globalBboltDB != nil {
		return globalBboltDB // Return existing instance
	}
	db, err := openBolt(filepath.Join(Path, "config.db"))
	if err != nil {
		log.TLogln(err)
		return nil
	}

	tdb := new(TDB)
	tdb.db = db
	tdb.Path = Path
	globalBboltDB = tdb
	return globalBboltDB
}

// openBolt opens the engine database with every commit fsynced: since the strict wake,
// the stored record is the only copy of a torrent's metainfo. The file is owner-only; an
// older, wider one is tightened on the open descriptor, so a swap of the path cannot
// redirect the chmod.
func openBolt(path string) (*bolt.DB, error) {
	return openBoltTimeout(path, 5*time.Second)
}

func openBoltTimeout(path string, timeout time.Duration) (*bolt.DB, error) {
	return bolt.Open(path, 0o600, &bolt.Options{
		Timeout: timeout,
		OpenFile: func(name string, flag int, mode os.FileMode) (*os.File, error) {
			f, err := os.OpenFile(name, flag, mode)
			if err != nil {
				return nil, err
			}
			if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&0o077 != 0 {
				if err := f.Chmod(0o600); err != nil {
					log.TLogln("Cannot tighten", name, "to 0600:", err)
				} else {
					log.TLogln("Tightened", name, "from", fi.Mode().Perm().String(), "to -rw-------")
				}
			}
			return f, nil
		},
	})
}

// CloseDB closes the database. Every commit was already synced; Close adds no fsync of
// its own, it releases the file lock. The handle stays set: a write that
// arrives afterwards gets bbolt's "database not open" error and is logged, not a nil
// dereference. Closing twice is harmless.
func (v *TDB) CloseDB() {
	if v.db != nil {
		if err := v.db.Close(); err != nil {
			log.TLogln("Error close db:", err)
		}
	}
}

func (v *TDB) Get(xpath, name string) []byte {
	spath := strings.Split(xpath, "/")
	if len(spath) == 0 {
		return nil
	}
	var ret []byte
	err := v.db.View(func(tx *bolt.Tx) error {
		buckt := tx.Bucket([]byte(spath[0]))
		if buckt == nil {
			return nil
		}

		for i, p := range spath {
			if i == 0 {
				continue
			}
			buckt = buckt.Bucket([]byte(p))
			if buckt == nil {
				return nil
			}
		}

		data := buckt.Get([]byte(name))
		if data != nil {
			// CRITICAL: Copy the data before returning
			ret = make([]byte, len(data))
			copy(ret, data)
		}
		return nil
	})
	if err != nil {
		log.TLogln("Error get sets", xpath+"/"+name, ", error:", err)
	}

	return ret
}

func (v *TDB) Set(xpath, name string, value []byte) {
	_ = v.trySet(xpath, name, value)
}

// committer is what DBReadCache needs from a database to keep its cache to committed data
// only: the writes report whether the commit succeeded.
type committer interface {
	trySet(xPath, name string, value []byte) error
	tryRem(xPath, name string) error
	tryClear(xPath string) error
}

// trySet is Set that reports a failed commit (after logging it).
func (v *TDB) trySet(xpath, name string, value []byte) error {
	spath := strings.Split(xpath, "/")
	if len(spath) == 0 {
		return nil
	}
	err := v.db.Update(func(tx *bolt.Tx) error {
		buckt, err := tx.CreateBucketIfNotExists([]byte(spath[0]))
		if err != nil {
			return err
		}

		for i, p := range spath {
			if i == 0 {
				continue
			}
			buckt, err = buckt.CreateBucketIfNotExists([]byte(p))
			if err != nil {
				return err
			}
		}

		return buckt.Put([]byte(name), value)
	})
	if err != nil {
		log.TLogln("Error put sets", xpath+"/"+name, ", error:", err)
		// Never the value: a record carries tracker URLs with passkeys.
		log.TLogln("value bytes:", len(value))
	}
	return err
}

func (v *TDB) List(xpath string) []string {
	spath := strings.Split(xpath, "/")
	if len(spath) == 0 {
		return nil
	}
	var ret []string
	err := v.db.View(func(tx *bolt.Tx) error {
		buckt := tx.Bucket([]byte(spath[0]))
		if buckt == nil {
			return nil
		}

		for i, p := range spath {
			if i == 0 {
				continue
			}
			buckt = buckt.Bucket([]byte(p))
			if buckt == nil {
				return nil
			}
		}

		return buckt.ForEach(func(k, _ []byte) error {
			if len(k) > 0 {
				ret = append(ret, string(k))
			}
			return nil
		})
	})
	if err != nil {
		log.TLogln("Error list sets", xpath, ", error:", err)
	}

	return ret
}

func (v *TDB) Rem(xpath, name string) {
	_ = v.tryRem(xpath, name)
}

func (v *TDB) tryRem(xpath, name string) error {
	spath := strings.Split(xpath, "/")
	if len(spath) == 0 {
		return nil
	}
	err := v.db.Update(func(tx *bolt.Tx) error {
		buckt := tx.Bucket([]byte(spath[0]))
		if buckt == nil {
			return nil
		}

		for i, p := range spath {
			if i == 0 {
				continue
			}
			buckt = buckt.Bucket([]byte(p))
			if buckt == nil {
				return nil
			}
		}

		return buckt.Delete([]byte(name))
	})
	if err != nil {
		log.TLogln("Error rem sets", xpath+"/"+name, ", error:", err)
	}
	return err
}

func (v *TDB) Clear(xPath string) {
	_ = v.tryClear(xPath)
}

func (v *TDB) tryClear(xPath string) error {
	spath := strings.Split(xPath, "/")
	if len(spath) == 0 {
		return nil
	}

	err := v.db.Update(func(tx *bolt.Tx) error {
		buckt := tx.Bucket([]byte(spath[0]))
		if buckt == nil {
			return nil
		}

		for i, p := range spath {
			if i == 0 {
				continue
			}
			buckt = buckt.Bucket([]byte(p))
			if buckt == nil {
				return nil
			}
		}

		// Delete all entries in this bucket
		return buckt.ForEach(func(k, _ []byte) error {
			return buckt.Delete(k)
		})
	})

	if err != nil {
		log.TLogln("Error clear xPath", xPath, ", error:", err)
	}
	return err
}
