package settings

import "testing"

type memDB struct{ buf []byte }

func (m *memDB) CloseDB()                  {}
func (m *memDB) Get(_, _ string) []byte    { return m.buf }
func (m *memDB) Set(_, _ string, v []byte) { m.buf = v }
func (m *memDB) List(string) []string      { return nil }
func (m *memDB) Rem(_, _ string)           {}
func (m *memDB) Clear(string)              {}

// A legacy or hand-edited store with out-of-range values loads normalized, so the first
// save is not a CLIENT diff made of defaults alone.
func TestLoadBTSetsNormalizes(t *testing.T) {
	oldDB, oldSets := tdb, BTsets
	defer func() { tdb, BTsets = oldDB, oldSets }()
	tdb = &memDB{buf: []byte(`{"CacheSize":0,"ConnectionsLimit":0,"TorrentDisconnectTimeout":-1,"ReaderReadAHead":900,"PreloadCache":-4}`)}
	BTsets = nil
	loadBTSets()
	if BTsets == nil || BTsets.ConnectionsLimit != 25 || BTsets.CacheSize != 128<<20 ||
		BTsets.TorrentDisconnectTimeout != 30 || BTsets.ReaderReadAHead != 100 || BTsets.PreloadCache != 0 {
		t.Errorf("loaded = %+v", BTsets)
	}
}
