package daemon

import (
	"flodni/internal/gostorm/torr"
	"flodni/internal/warmup"
)

// wantsColdBoost reports whether this read starts the engine's cold-start connection boost
// (engine_cold_boost): the first read of the primary handle past the header region, which is
// the first sign of a player and not a scan. Scans (isScanRead), probes (shadowsActivePlayer,
// #237), handles that are not the primary, header reads (the SSD head window) and the tail
// probe zone never start it. It is true at most once per handle; with the switch off it is
// false without touching the handle.
func (h *MkvHandle) wantsColdBoost(off int64, probe bool) bool {
	if !torr.EngineColdBoost || h.hash == "" || probe || !h.isPrimaryHandle.Load() || isScanRead(h) {
		return false
	}
	if off < warmup.FileSize || off >= h.size-tailProbeZoneSize(h.size) {
		return false
	}
	return h.coldBoostTried.CompareAndSwap(false, true)
}

// boostColdStart starts the boost off the read path: the engine's setter takes its client lock.
func (h *MkvHandle) boostColdStart(off int64, probe bool) {
	if !h.wantsColdBoost(off, probe) {
		return
	}
	hash := h.hash
	go func() {
		if tr := torr.PeekTorrent(hash); tr != nil {
			tr.BoostColdStart()
		}
	}()
}
