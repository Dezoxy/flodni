package daemon

import (
	"fmt"
	"strings"
	"testing"

	"flodni/internal/gostorm/torr"
	"flodni/internal/gostorm/torr/storage/torrstor"
)

// The engine-side counters are exposed with every label combination from the start, and the
// existing refetch total keeps its name next to the split.
func TestEngineCacheSeriesFromStart(t *testing.T) {
	body := scrape(t).Body.String()
	var want []string
	for _, z := range torrstor.EvictionZoneNames {
		for _, r := range torrstor.EvictionReaderBuckets {
			want = append(want, fmt.Sprintf(`flodni_engine_cache_evictions_total{zone=%q,readers=%q}`, z, r))
		}
	}
	for _, r := range torr.StreamEndNames {
		want = append(want, fmt.Sprintf(`flodni_engine_stream_ends_total{reason=%q}`, r))
	}
	for _, r := range torrstor.RefetchReaderNames {
		want = append(want, fmt.Sprintf(`flodni_engine_refetched_bytes_total{reader=%q}`, r))
	}
	want = append(want, `flodni_readahead_refetched_bytes_total`)
	for _, w := range want {
		if !strings.Contains(body, w+" ") {
			t.Errorf("series missing: %s", w)
		}
	}
	for fam, words := range map[string][]string{
		"flodni_engine_cache_evictions_total": {"at or after", "current offset", "unread"},
		"flodni_engine_refetched_bytes_total": {"open-ended reader", "any"},
		"flodni_engine_stream_ends_total":     {"Open-ended"},
	} {
		found := false
		for _, line := range strings.Split(body, "\n") {
			if !strings.HasPrefix(line, "# HELP "+fam+" ") {
				continue
			}
			found = true
			for _, w := range words {
				if !strings.Contains(line, w) {
					t.Errorf("HELP of %s lacks %q: %s", fam, w, line)
				}
			}
		}
		if !found {
			t.Errorf("no HELP for %s", fam)
		}
	}
}
