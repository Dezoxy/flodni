package torrstor

import (
	"testing"

	"flodni/internal/gostorm/settings"

	"github.com/anacrolix/torrent/metainfo"
)

const liveMiB = 1 << 20

// A capacity change reaches caches opened after it. A cache opened before keeps its size,
// and so does the capacity the engine's request strategy reads for it.
func TestSetCapacityAppliesToNextOpenOnly(t *testing.T) {
	s := NewStorage(128 * liveMiB)
	info := &metainfo.Info{PieceLength: 1 << 20, Length: 8 << 20, Name: "x", Pieces: make([]byte, 20*8)}
	first, err := s.OpenTorrent(info, metainfo.Hash{1})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	s.SetCapacity(512 * liveMiB)

	second, err := s.OpenTorrent(info, metainfo.Hash{2})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	if got, _ := (*first.Capacity)(); got != 128*liveMiB {
		t.Errorf("already-open torrent capacity = %d, want 128 MiB", got)
	}
	if got, _ := (*second.Capacity)(); got != 512*liveMiB {
		t.Errorf("next torrent capacity = %d, want 512 MiB", got)
	}
	if got := s.GetCache(metainfo.Hash{1}).capacity; got != 128*liveMiB {
		t.Errorf("already-open cache resized to %d", got)
	}
	if got := s.GetCache(metainfo.Hash{2}).capacity; got != 512*liveMiB {
		t.Errorf("next cache capacity = %d, want 512 MiB", got)
	}
}

// A loaded cache's read-ahead follows the percentage live but is derived from the size
// it was opened with, so raising CacheSize cannot push it past that cache.
func TestCacheReadaheadFollowsPercentNotLaterCacheSize(t *testing.T) {
	old := settings.BTsets
	defer func() { settings.BTsets = old }()
	settings.BTsets = &settings.BTSets{CacheSize: 128 * liveMiB, ReaderReadAHead: 50}
	c := NewCache(128*liveMiB, nil)
	defer c.Close()

	if got := c.Readahead(); got != 64*liveMiB {
		t.Fatalf("readahead = %d, want 64 MiB", got)
	}
	settings.BTsets = &settings.BTSets{CacheSize: 1024 * liveMiB, ReaderReadAHead: 100}
	if got := c.Readahead(); got != 128*liveMiB {
		t.Errorf("after CacheSize 1024 MiB and 100%%: readahead = %d, want the open size's 128 MiB", got)
	}
	if got := CacheReadahead(); got != 1024*liveMiB {
		t.Errorf("CacheReadahead() = %d, want the live size's 1024 MiB", got)
	}
}
