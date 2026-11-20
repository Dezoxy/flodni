package torrstor

import (
	"testing"

	"github.com/anacrolix/torrent"

	"flodni/internal/gostorm/settings"
)

// raRecorder stands in for the anacrolix reader and records the read-ahead it was given.
type raRecorder struct {
	torrent.Reader
	set  []int64
	fn   torrent.ReadaheadFunc
	seen bool
}

func (f *raRecorder) SetReadahead(n int64) { f.set = append(f.set, n); f.seen = true }
func (f *raRecorder) SetReadaheadFunc(fn torrent.ReadaheadFunc) {
	f.fn = fn
	f.seen = true
}

// A new reader must hand the engine the read-ahead the stats tick would set (the cache
// read-ahead split across the open-ended readers) at once, not leave the engine's 48 MiB
// default in place until the first tick.
func TestNewReaderSetsEngineReadaheadImmediately(t *testing.T) {
	oldSets, oldBounded := settings.BTsets, BoundedReadahead
	defer func() { settings.BTsets, BoundedReadahead = oldSets, oldBounded }()

	const mib = 1 << 20
	cases := []struct {
		name     string
		pct      int
		existing int32
		want     int64
	}{
		{"first reader, 75% of 128 MiB, times 0.85", 75, 0, 85563801},
		{"second reader splits it", 100, 1, 57042534},
		{"floor of 8 MiB", 5, 3, 8 * mib},
	}
	for _, bounded := range []bool{false, true} {
		for _, tc := range cases {
			settings.BTsets = &settings.BTSets{CacheSize: 128 * mib, ReaderReadAHead: tc.pct}
			BoundedReadahead = bounded
			fake := &raRecorder{}
			cache := &Cache{readers: map[*Reader]struct{}{}, openSize: 128 * mib}
			cache.nActiveOpen.Store(tc.existing)
			r := &Reader{Reader: fake}
			r.attach(cache)

			if !fake.seen {
				t.Fatalf("%s bounded=%v: the engine reader's read-ahead was never set", tc.name, bounded)
			}
			got := int64(-1)
			if bounded {
				got = fake.fn(torrent.ReadaheadContext{})
			} else if len(fake.set) == 1 {
				got = fake.set[0]
			}
			if got != tc.want {
				t.Errorf("%s bounded=%v: engine read-ahead = %d, want %d", tc.name, bounded, got, tc.want)
			}
			if r.Readahead() != tc.want {
				t.Errorf("%s bounded=%v: Readahead() = %d, want %d", tc.name, bounded, r.Readahead(), tc.want)
			}
		}
	}
}
