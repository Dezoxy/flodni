package settings

import (
	"reflect"
	"testing"
)

// A new BTSets field must be classified on purpose: Diff would otherwise treat it as
// Client, which is safe but restarts the engine for what may be a read-at-use field.
func TestEveryFieldIsClassified(t *testing.T) {
	typ := reflect.TypeOf(BTSets{})
	for i := 0; i < typ.NumField(); i++ {
		if _, ok := fieldClass[typ.Field(i).Name]; !ok {
			t.Errorf("BTSets.%s has no entry in fieldClass", typ.Field(i).Name)
		}
	}
	for name := range fieldClass {
		if _, ok := typ.FieldByName(name); !ok {
			t.Errorf("fieldClass names %s, which is not a BTSets field", name)
		}
	}
}

func TestDiffSplitsByClass(t *testing.T) {
	old := *DefaultBTSets()
	next := old
	next.CacheSize *= 2
	next.ReaderReadAHead = 40
	next.PeersListenPort = 6881
	next.DisableDHT = !old.DisableDHT
	hot, client := Diff(old, next)
	if !reflect.DeepEqual(hot, []string{"CacheSize", "ReaderReadAHead"}) {
		t.Errorf("hot = %v", hot)
	}
	if !reflect.DeepEqual(client, []string{"DisableDHT", "PeersListenPort"}) {
		t.Errorf("client = %v", client)
	}
	if h, c := Diff(old, old); len(h)+len(c) != 0 {
		t.Errorf("identical settings differ: %v %v", h, c)
	}
}

// Normalize is what SetBTSets applies, so a request compared after it reads the same as
// what would be stored.
func TestNormalizeMatchesWhatSetBTSetsClamps(t *testing.T) {
	s := BTSets{CacheSize: -1, ConnectionsLimit: 0, ReaderReadAHead: 500, PreloadCache: -3}
	Normalize(&s)
	if s.CacheSize != 128<<20 || s.ConnectionsLimit != 25 || s.TorrentDisconnectTimeout != 30 ||
		s.ReaderReadAHead != 100 || s.PreloadCache != 0 {
		t.Errorf("normalized = %+v", s)
	}
}
