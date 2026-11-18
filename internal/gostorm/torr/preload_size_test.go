package torr

import (
	"testing"

	sets "flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr/storage/torrstor"
)

// The preload is a share of the cache the torrent has, not of the live CacheSize: after a
// hot CacheSize raise a loaded torrent still has its old cache.
func TestPreloadSizeUsesTheCacheTheTorrentHas(t *testing.T) {
	old := sets.BTsets
	defer func() { sets.BTsets = old }()
	sets.BTsets = &sets.BTSets{CacheSize: 512 * mib, PreloadCache: 50}

	c := torrstor.NewCache(128*mib, nil)
	defer c.Close()
	if got := preloadSize(&Torrent{cache: c}); got != 64*mib {
		t.Errorf("preload = %d, want 64 MiB (50%% of the open 128 MiB)", got)
	}
	if got := preloadSize(&Torrent{}); got != 256*mib {
		t.Errorf("preload with no cache = %d, want 256 MiB (50%% of the live size)", got)
	}
	sets.BTsets.PreloadCache = 0
	if got := preloadSize(&Torrent{cache: c}); got != 0 {
		t.Errorf("preload 0%% = %d", got)
	}
}
