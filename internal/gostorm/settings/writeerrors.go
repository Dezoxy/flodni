package settings

import (
	"sync"
	"sync/atomic"
)

// Write-error counters, one per operation. Counted where the cache meets the database,
// so every write through tdb is covered, a write after CloseDB included.
const (
	opSet = iota
	opRem
	opClear
)

var writeErrors [3]atomic.Int64

func countWriteError(op int) { writeErrors[op].Add(1) }

// DBWriteErrors returns the commits that failed since start, by operation: the engine
// database did not take the write, so the record is not saved (or not removed).
func DBWriteErrors() (set, rem, clear int64) {
	return writeErrors[opSet].Load(), writeErrors[opRem].Load(), writeErrors[opClear].Load()
}

// InjectCommitFault makes every commit fail with the error f returns (nil lets it
// through), until the returned restore runs. A seam for tests of packages that cannot
// reach the database underneath.
func InjectCommitFault(f func() error) (restore func()) {
	faultMu.Lock()
	prev := fault
	fault = f
	faultMu.Unlock()
	return func() {
		faultMu.Lock()
		fault = prev
		faultMu.Unlock()
	}
}

var (
	faultMu sync.Mutex
	fault   func() error
)

func commitFault() error {
	faultMu.Lock()
	f := fault
	faultMu.Unlock()
	if f == nil {
		return nil
	}
	return f()
}

// trySetOn, tryRemOn: a write on db that returns the commit error when db can report it.
func trySetOn(db GoStormDB, xPath, name string, value []byte) error {
	if c, ok := db.(committer); ok {
		return c.trySet(xPath, name, value)
	}
	db.Set(xPath, name, value)
	return nil
}

func tryRemOn(db GoStormDB, xPath, name string) error {
	if c, ok := db.(committer); ok {
		return c.tryRem(xPath, name)
	}
	db.Rem(xPath, name)
	return nil
}
