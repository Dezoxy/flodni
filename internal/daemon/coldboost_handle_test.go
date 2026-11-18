package daemon

import (
	"testing"

	"flodni/internal/config"
	"flodni/internal/gostorm/torr"
	"flodni/internal/warmup"
)

// engine_cold_boost starts at real playback only: the primary handle's first read past the
// header region. Scans, probes and header reads never start it, and a later seek does not
// start it again on the same handle.

func coldBoostOn(t *testing.T) {
	t.Helper()
	prev := torr.EngineColdBoost
	torr.EngineColdBoost = true
	t.Cleanup(func() { torr.EngineColdBoost = prev })
}

func coldBoostPlayer(path string) *MkvHandle {
	h := newTestHandle(path)
	h.hasSlot.Store(true)
	h.isPrimaryHandle.Store(true)
	return h
}

func TestColdBoostHeaderOnlyScanReadDoesNotBoost(t *testing.T) {
	withConfig(t, func(c *config.Config) { c.PumpOnOpen = false })
	coldBoostOn(t)
	h := newTestHandle("/test/cb-scan.mkv") // no slot, no playback: a scan
	if h.wantsColdBoost(0, false) || h.wantsColdBoost(warmup.FileSize-1, false) {
		t.Fatal("a header read of a scan boosted")
	}
	if h.wantsColdBoost(warmup.FileSize+1, false) {
		t.Fatal("a scan's read past the header boosted")
	}
	h.isPrimaryHandle.Store(true) // even as primary, without a slot or playback it is a scan
	if h.wantsColdBoost(warmup.FileSize+1, false) {
		t.Fatal("a scan handle boosted")
	}
}

func TestColdBoostProbeHandleReadingFarDoesNotBoost(t *testing.T) {
	withConfig(t, func(c *config.Config) { c.PumpOnOpen = false })
	coldBoostOn(t)
	h := newTestHandle("/test/cb-probe.mkv")
	h.hasSlot.Store(true) // not a scan, but a probe next to a watched file, and not the primary
	if h.wantsColdBoost(5*gb, true) {
		t.Fatal("a probe handle boosted")
	}
	if h.wantsColdBoost(5*gb, false) {
		t.Fatal("a handle that is not the primary boosted")
	}
}

func TestColdBoostPrimaryFirstReadPastHeaderBoostsOnce(t *testing.T) {
	withConfig(t, func(c *config.Config) { c.PumpOnOpen = false })
	coldBoostOn(t)
	h := coldBoostPlayer("/test/cb-primary.mkv")
	if h.wantsColdBoost(warmup.FileSize-1, false) {
		t.Fatal("the primary's header read boosted")
	}
	if !h.wantsColdBoost(warmup.FileSize, false) {
		t.Fatal("the primary's first read past the header did not boost")
	}
	if h.wantsColdBoost(warmup.FileSize+4096, false) {
		t.Fatal("the boost began twice")
	}
}

func TestColdBoostLaterSeekDoesNotBoostAgain(t *testing.T) {
	withConfig(t, func(c *config.Config) { c.PumpOnOpen = false })
	coldBoostOn(t)
	h := coldBoostPlayer("/test/cb-seek.mkv")
	if !h.wantsColdBoost(warmup.FileSize+1, false) {
		t.Fatal("no boost on the first read past the header")
	}
	if h.wantsColdBoost(5*gb, false) || h.wantsColdBoost(warmup.FileSize+1, false) {
		t.Fatal("a later seek boosted again on the same activation")
	}
}

func TestColdBoostTailProbeDoesNotBoost(t *testing.T) {
	withConfig(t, func(c *config.Config) { c.PumpOnOpen = false })
	coldBoostOn(t)
	h := coldBoostPlayer("/test/cb-tail.mkv")
	if h.wantsColdBoost(h.size-1024, false) {
		t.Fatal("a read in the tail probe zone boosted")
	}
	if !h.wantsColdBoost(warmup.FileSize+1, false) {
		t.Fatal("the tail probe used up the boost")
	}
}

func TestColdBoostFlagOffNeverBoosts(t *testing.T) {
	withConfig(t, func(c *config.Config) { c.PumpOnOpen = false })
	prev := torr.EngineColdBoost
	torr.EngineColdBoost = false
	t.Cleanup(func() { torr.EngineColdBoost = prev })
	h := coldBoostPlayer("/test/cb-off.mkv")
	if h.wantsColdBoost(warmup.FileSize+1, false) {
		t.Fatal("boosted with engine_cold_boost off")
	}
}
