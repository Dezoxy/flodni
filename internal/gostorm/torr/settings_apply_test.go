package torr

import (
	"errors"
	"reflect"
	"testing"

	sets "flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr/storage/torrstor"
)

const mib = 1 << 20

// settingsFixture installs seams that record what a settings change did, with a stored
// baseline and a storage the engine would hold.
type settingsFixture struct {
	drops, disconnects, connects int
	playing                      int
	storage                      *torrstor.Storage
}

func newSettingsFixture(t *testing.T) *settingsFixture {
	t.Helper()
	f := &settingsFixture{storage: torrstor.NewStorage(128 * mib)}
	base := sets.DefaultBTSets()
	oldBts, oldSets := bts, sets.BTsets
	oldStore, oldDef := storeSettings, storeDefaults
	oldDrop, oldDisc, oldConn, oldWait := dropAllTorrentFn, disconnectFn, connectFn, settleWait
	oldProbe := playbackProbe.Load()

	sets.BTsets = base
	bts = NewBTS()
	bts.storage = f.storage
	storeSettings = func(s *sets.BTSets) { sets.Normalize(s); sets.BTsets = s }
	storeDefaults = func() { sets.BTsets = sets.DefaultBTSets() }
	dropAllTorrentFn = func() { f.drops++ }
	disconnectFn = func() { f.disconnects++ }
	connectFn = func() error { f.connects++; return nil }
	settleWait = func() {}
	SetPlaybackProbe(func() int { return f.playing })
	t.Cleanup(func() {
		bts, sets.BTsets = oldBts, oldSets
		storeSettings, storeDefaults = oldStore, oldDef
		dropAllTorrentFn, disconnectFn, connectFn, settleWait = oldDrop, oldDisc, oldConn, oldWait
		playbackProbe.Store(oldProbe)
	})
	return f
}

func changed(mut func(*sets.BTSets)) *sets.BTSets {
	n := sets.Snapshot()
	mut(&n)
	return &n
}

func (f *settingsFixture) rebuilt() bool { return f.drops+f.disconnects+f.connects > 0 }

func TestHotOnlySetDoesNotRebuildAndAppliesToNextActivation(t *testing.T) {
	f := newSettingsFixture(t)
	err := SetSettings(changed(func(s *sets.BTSets) {
		s.CacheSize = 512 * mib
		s.ReaderReadAHead = 40
	}), false)
	if err != nil {
		t.Fatal(err)
	}
	if f.rebuilt() {
		t.Fatalf("a HOT-only change rebuilt the engine: drops=%d disconnects=%d connects=%d", f.drops, f.disconnects, f.connects)
	}
	if got := sets.BTsets.CacheSize; got != 512*mib {
		t.Errorf("stored CacheSize = %d", got)
	}
	if got := f.storage.Capacity(); got != 512*mib {
		t.Errorf("the next activation's cache capacity = %d, want %d", got, 512*mib)
	}
}

func TestClientSetRebuildsAsBefore(t *testing.T) {
	f := newSettingsFixture(t)
	if err := SetSettings(changed(func(s *sets.BTSets) { s.DisableDHT = true }), false); err != nil {
		t.Fatal(err)
	}
	if f.drops != 1 || f.disconnects != 1 || f.connects != 1 {
		t.Fatalf("drops=%d disconnects=%d connects=%d, want one of each", f.drops, f.disconnects, f.connects)
	}
	if !sets.BTsets.DisableDHT {
		t.Error("the change was not stored")
	}
}

func TestRebuildUnderPlaybackIsRefusedAndNothingChanges(t *testing.T) {
	f := newSettingsFixture(t)
	f.playing = 2
	err := SetSettings(changed(func(s *sets.BTSets) {
		s.PeersListenPort = 51413
		s.DisableDHT = true
		s.CacheSize = 256 * mib // a HOT field riding along is not stored either
	}), false)
	var refused *RestartRefused
	if !errors.As(err, &refused) {
		t.Fatalf("err = %v, want *RestartRefused", err)
	}
	if refused.Sessions != 2 || !reflect.DeepEqual(refused.Fields, []string{"DisableDHT", "PeersListenPort"}) {
		t.Errorf("refusal = %+v", refused)
	}
	if f.rebuilt() {
		t.Error("a refused change rebuilt the engine")
	}
	if sets.BTsets.DisableDHT || sets.BTsets.CacheSize != 128*mib {
		t.Error("a refused change was stored")
	}
}

func TestForcedRebuildUnderPlaybackProceeds(t *testing.T) {
	f := newSettingsFixture(t)
	f.playing = 1
	if err := SetSettings(changed(func(s *sets.BTSets) { s.DisableDHT = true }), true); err != nil {
		t.Fatal(err)
	}
	if f.drops != 1 || f.disconnects != 1 || f.connects != 1 || !sets.BTsets.DisableDHT {
		t.Fatalf("forced change did not rebuild: drops=%d disc=%d conn=%d", f.drops, f.disconnects, f.connects)
	}
}

func TestHotOnlyUnderPlaybackIsAppliedWithoutRefusal(t *testing.T) {
	f := newSettingsFixture(t)
	f.playing = 3
	if err := SetSettings(changed(func(s *sets.BTSets) { s.CacheSize = 256 * mib }), false); err != nil {
		t.Fatalf("a HOT-only change was refused: %v", err)
	}
	if f.rebuilt() || sets.BTsets.CacheSize != 256*mib {
		t.Error("HOT-only change under playback was not applied in place")
	}
}

// The request is normalized before it is diffed: a blank or 0 ConnectionsLimit becomes 25
// and, against a stored 25, is no change at all (no refusal, no rebuild).
func TestBlankConnectionsLimitIsNotAClientChange(t *testing.T) {
	f := newSettingsFixture(t)
	f.playing = 2
	if err := SetSettings(changed(func(s *sets.BTSets) { s.ConnectionsLimit = 0 }), false); err != nil {
		t.Fatalf("a blank ConnectionsLimit was refused: %v", err)
	}
	if f.rebuilt() {
		t.Error("a blank ConnectionsLimit rebuilt the engine")
	}
	if sets.BTsets.ConnectionsLimit != 25 {
		t.Errorf("stored ConnectionsLimit = %d, want 25", sets.BTsets.ConnectionsLimit)
	}
}

// A hot CacheSize change re-aligns the reader's read-ahead ceiling with the new size.
func TestHotCacheSizeMovesTheReadaheadCeiling(t *testing.T) {
	newSettingsFixture(t)
	var got []int64
	old := setMaxReadahead
	setMaxReadahead = func(n int64) { got = append(got, n) }
	defer func() { setMaxReadahead = old }()
	if err := SetSettings(changed(func(s *sets.BTSets) { s.CacheSize = 256 * mib }), false); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != 256*mib {
		t.Errorf("SetMaxReadahead calls = %v, want [256 MiB]", got)
	}
}

func TestDefaultsRebuildOnlyWhenAClientFieldDiffers(t *testing.T) {
	f := newSettingsFixture(t)
	f.playing = 1
	// the fixture baseline is the defaults: resetting changes nothing
	if err := SetDefSettings(false); err != nil || f.rebuilt() {
		t.Fatalf("reset of default settings: err=%v rebuilt=%v", err, f.rebuilt())
	}
	sets.BTsets = changed(func(s *sets.BTSets) { s.DisableTCP = true })
	err := SetDefSettings(false)
	var refused *RestartRefused
	if !errors.As(err, &refused) || !reflect.DeepEqual(refused.Fields, []string{"DisableTCP"}) {
		t.Fatalf("err = %v, want refusal naming DisableTCP", err)
	}
	if err := SetDefSettings(true); err != nil || f.connects != 1 {
		t.Fatalf("forced reset: err=%v connects=%d", err, f.connects)
	}
}
