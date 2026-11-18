package torrstor

import (
	"sync"

	"flodni/internal/gostorm/torr/storage"

	"github.com/anacrolix/torrent/metainfo"
	ts "github.com/anacrolix/torrent/storage"
)

type Storage struct {
	storage.Storage

	caches   map[metainfo.Hash]*Cache
	capacity int64
	mu       sync.Mutex
}

func NewStorage(capacity int64) *Storage {
	stor := new(Storage)
	stor.capacity = capacity
	stor.caches = make(map[metainfo.Hash]*Cache)
	return stor
}

// SetCapacity changes the size caches opened from now on get. A cache that is already
// open keeps the size it was opened with: its eviction, window and prefetch maths read
// the size without a lock and derive values from it at Init.
func (s *Storage) SetCapacity(capacity int64) {
	s.mu.Lock()
	s.capacity = capacity
	s.mu.Unlock()
}

// Capacity is the size caches opened from now on get.
func (s *Storage) Capacity() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.capacity
}

func (s *Storage) OpenTorrent(info *metainfo.Info, infoHash metainfo.Hash) (ts.TorrentImpl, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	capacity := s.capacity
	ch := NewCache(capacity, s)
	ch.Init(info, infoHash)
	s.caches[infoHash] = ch

	// The size this cache was opened with, so the engine's request strategy and the cache
	// agree after a SetCapacity.
	capFunc := func() (int64, bool) {
		return capacity, true
	}

	return ts.TorrentImpl{
		Piece:    ch.Piece,
		Close:    ch.closeNoWait, // see Cache.Close
		Capacity: &capFunc,
	}, nil
}

func (s *Storage) CloseHash(hash metainfo.Hash) {
	if s.caches == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.caches[hash]; ok {
		_ = ch.Close() // Cache.Close only returns nil
		delete(s.caches, hash)
	}
}

func (s *Storage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.caches {
		_ = ch.Close() // Cache.Close only returns nil
	}
	s.caches = make(map[metainfo.Hash]*Cache)
	return nil
}

func (s *Storage) GetCache(hash metainfo.Hash) *Cache {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cache, ok := s.caches[hash]; ok {
		return cache
	}
	return nil
}
