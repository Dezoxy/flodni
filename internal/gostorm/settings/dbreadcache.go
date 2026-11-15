package settings

import (
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
	if ReadOnly {
		if IsDebug() {
			log.TLogln("DBReadCache.Set: Read-only DB mode!", name)
		}
		return
	}
	if v.closed.Load() {
		log.TLogln("DBReadCache.Set: DB is closed, cannot set", name)
		return
	}

	// The cache holds committed data only: a failed commit leaves it as it was, so the
	// next identical save differs from it and commits again.
	if !v.commit(func(c committer) error { return c.trySet(xPath, name, value) }, func(db GoStormDB) { db.Set(xPath, name, value) }) {
		return
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
}

// commit runs a write on the database under the cache and reports whether it committed.
// A database that cannot report (not a committer) is assumed to have committed.
func (v *DBReadCache) commit(try func(committer) error, plain func(GoStormDB)) bool {
	if c, ok := v.db.(committer); ok {
		return try(c) == nil
	}
	plain(v.db)
	return true
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
	if ReadOnly {
		if IsDebug() {
			log.TLogln("DBReadCache.Rem: Read-only DB mode!", name)
		}
		return
	}
	if v.closed.Load() {
		log.TLogln("DBReadCache.Rem: DB is closed, cannot remove", name)
		return
	}

	if !v.commit(func(c committer) error { return c.tryRem(xPath, name) }, func(db GoStormDB) { db.Rem(xPath, name) }) {
		return
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
}

func (v *DBReadCache) Clear(xPath string) {
	if ReadOnly {
		if IsDebug() {
			log.TLogln("DBReadCache.Clear: Read-only DB mode!", xPath)
		}
		return
	}

	if v.closed.Load() {
		log.TLogln("DBReadCache.Clear: DB is closed, cannot clear", xPath)
		return
	}
	// Clear the underlying DB first; the cache follows only if that committed.
	if !v.commit(func(c committer) error { return c.tryClear(xPath) }, func(db GoStormDB) { db.Clear(xPath) }) {
		return
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
}

func (v *DBReadCache) makeDataCacheKey(xPath, name string) [2]string {
	return [2]string{xPath, name}
}
