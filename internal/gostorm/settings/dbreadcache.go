package settings

import (
	"errors"
	"sync"
	"sync/atomic"

	"flodni/internal/gostorm/log"
)

type DBReadCache struct {
	db             GoStormDB
	listCache      map[string][]string
	listCacheMutex sync.RWMutex
	dataCache      *dataLRU
	dataCacheMutex sync.Mutex
	closed         atomic.Bool
}

func NewDBReadCache(db GoStormDB) GoStormDB {
	cdb := &DBReadCache{
		db:        db,
		listCache: map[string][]string{},
		dataCache: newDataLRU(DataCacheLimit),
	}
	return cdb
}

// CloseDB closes the database under the cache. Nothing is nilled: a call that races the
// close still finds a valid object. After it, reads return nothing and writes are refused,
// so the cache can neither serve a record the closed database does not hold nor keep one
// it never stored.
func (v *DBReadCache) CloseDB() {
	v.closed.Store(true)
	v.db.CloseDB()
}

func (v *DBReadCache) Get(xPath, name string) []byte {
	if v.closed.Load() {
		return nil
	}
	cacheKey := v.makeDataCacheKey(xPath, name)

	// Re-check under the lock: CloseDB nils the cache, and unlike the bare map it replaced,
	// calling into a nil *dataLRU dereferences.
	v.dataCacheMutex.Lock()
	if v.dataCache != nil {
		if data, ok := v.dataCache.get(cacheKey); ok {
			defer v.dataCacheMutex.Unlock()
			return data
		}
	}
	v.dataCacheMutex.Unlock()

	data := v.db.Get(xPath, name)

	v.dataCacheMutex.Lock()
	if v.dataCache != nil { // Двойная проверка
		v.dataCache.put(cacheKey, data)
	}
	v.dataCacheMutex.Unlock()

	return data
}

func (v *DBReadCache) Set(xPath, name string, value []byte) {
	_ = v.trySet(xPath, name, value)
}

// errDBClosed is the error of a write that arrives after CloseDB: it is lost, and counted.
var errDBClosed = errors.New("settings: database is closed")

// trySet is Set that reports a failed commit. The cache holds committed data only: a
// failed commit leaves it as it was, so the next identical save differs from it and
// commits again.
func (v *DBReadCache) trySet(xPath, name string, value []byte) error {
	if ReadOnly {
		if IsDebug() {
			log.TLogln("DBReadCache.Set: Read-only DB mode!", name)
		}
		return nil
	}
	if v.closed.Load() {
		log.TLogln("DBReadCache.Set: DB is closed, cannot set", name)
		countWriteError(opSet)
		return errDBClosed
	}

	if err := tryWrite(v.db, func(c committer) error { return c.trySet(xPath, name, value) }, func(db GoStormDB) { db.Set(xPath, name, value) }); err != nil {
		countWriteError(opSet)
		return err
	}

	cacheKey := v.makeDataCacheKey(xPath, name)

	v.dataCacheMutex.Lock()
	if v.dataCache != nil { // Двойная проверка
		v.dataCache.put(cacheKey, value)
	}
	v.dataCacheMutex.Unlock()

	v.listCacheMutex.Lock()
	if v.listCache != nil {
		delete(v.listCache, xPath)
	}
	v.listCacheMutex.Unlock()
	return nil
}

// tryWrite runs a write on a database and returns its commit error. A database that
// cannot report (not a committer) is assumed to have committed.
func tryWrite(db GoStormDB, try func(committer) error, plain func(GoStormDB)) error {
	if err := commitFault(); err != nil {
		return err
	}
	if c, ok := db.(committer); ok {
		return try(c)
	}
	plain(db)
	return nil
}

func (v *DBReadCache) List(xPath string) []string {
	if v.closed.Load() {
		return nil
	}

	v.listCacheMutex.RLock()
	if names, ok := v.listCache[xPath]; ok {
		defer v.listCacheMutex.RUnlock()
		return names
	}
	v.listCacheMutex.RUnlock()

	names := v.db.List(xPath)

	v.listCacheMutex.Lock()
	if v.listCache != nil { // Двойная проверка
		v.listCache[xPath] = names
	}
	v.listCacheMutex.Unlock()

	return names
}

func (v *DBReadCache) Rem(xPath, name string) {
	_ = v.tryRem(xPath, name)
}

func (v *DBReadCache) tryRem(xPath, name string) error {
	if ReadOnly {
		if IsDebug() {
			log.TLogln("DBReadCache.Rem: Read-only DB mode!", name)
		}
		return nil
	}
	if v.closed.Load() {
		log.TLogln("DBReadCache.Rem: DB is closed, cannot remove", name)
		countWriteError(opRem)
		return errDBClosed
	}

	if err := tryWrite(v.db, func(c committer) error { return c.tryRem(xPath, name) }, func(db GoStormDB) { db.Rem(xPath, name) }); err != nil {
		countWriteError(opRem)
		return err
	}

	cacheKey := v.makeDataCacheKey(xPath, name)

	v.dataCacheMutex.Lock()
	if v.dataCache != nil {
		v.dataCache.remove(cacheKey)
	}
	v.dataCacheMutex.Unlock()

	v.listCacheMutex.Lock()
	if v.listCache != nil {
		delete(v.listCache, xPath)
	}
	v.listCacheMutex.Unlock()
	return nil
}

func (v *DBReadCache) Clear(xPath string) {
	_ = v.tryClear(xPath)
}

func (v *DBReadCache) tryClear(xPath string) error {
	if ReadOnly {
		if IsDebug() {
			log.TLogln("DBReadCache.Clear: Read-only DB mode!", xPath)
		}
		return nil
	}

	if v.closed.Load() {
		log.TLogln("DBReadCache.Clear: DB is closed, cannot clear", xPath)
		countWriteError(opClear)
		return errDBClosed
	}
	// Clear the underlying DB first; the cache follows only if that committed.
	if err := tryWrite(v.db, func(c committer) error { return c.tryClear(xPath) }, func(db GoStormDB) { db.Clear(xPath) }); err != nil {
		countWriteError(opClear)
		return err
	}

	// Clear cache
	v.listCacheMutex.Lock()
	delete(v.listCache, xPath)
	v.listCacheMutex.Unlock()

	// Clear data cache entries for this xPath
	v.dataCacheMutex.Lock()
	if v.dataCache != nil {
		v.dataCache.removePath(xPath)
	}
	v.dataCacheMutex.Unlock()
	return nil
}

func (v *DBReadCache) makeDataCacheKey(xPath, name string) [2]string {
	return [2]string{xPath, name}
}
