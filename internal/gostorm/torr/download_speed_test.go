package torr

import (
	"testing"
	"time"
)

// download_speed is the rate of useful data; download_speed_raw is the wire rate, which
// also counts protocol overhead, duplicates and waste. Both come from one sample.
func TestSpeedSampleSeparatesUsefulFromRaw(t *testing.T) {
	// Useful data grows 2.4 MB in a second while the wire counter did not move: the
	// useful rate must read 2.4 MB/s, not 0.
	useful, raw := speedSample(0, 2_400_000, 1)
	if useful != 2_400_000 || raw != 0 {
		t.Errorf("useful=%v raw=%v, want 2.4e6 and 0", useful, raw)
	}
	// Wire bytes include waste: raw is larger than useful.
	useful, raw = speedSample(3_000_000, 2_400_000, 1)
	if useful != 2_400_000 || raw != 3_000_000 {
		t.Errorf("useful=%v raw=%v, want 2.4e6 and 3e6", useful, raw)
	}
}

func TestSpeedSampleNeverNegativeNorInfinite(t *testing.T) {
	if u, r := speedSample(-5, -9, 1); u != 0 || r != 0 {
		t.Errorf("counters that went back read %v / %v, want 0", u, r)
	}
	if u, r := speedSample(100, 100, 0); u != 0 || r != 0 {
		t.Errorf("a window that did not advance reads %v / %v, want 0", u, r)
	}
}

// UpdateStats takes its counters from the engine and hands them to sampleCounters: the
// useful-data counter feeds download_speed and the wire counter download_speed_raw.
// Feeding the wire delta to download_speed fails this.
func TestSampleCountersFollowsUsefulForSpeedAndWireForRaw(t *testing.T) {
	tr := &Torrent{}
	tr.lastTimeSpeed = time.Now().Add(-time.Second)
	tr.sampleCounters(3_000_000, 2_400_000, 500_000, tr.lastTimeSpeed.Add(time.Second))
	if tr.DownloadSpeed != 2_400_000 || tr.DownloadSpeedRaw != 3_000_000 || tr.UploadSpeed != 500_000 {
		t.Errorf("speed=%v raw=%v up=%v, want 2.4e6 3e6 5e5", tr.DownloadSpeed, tr.DownloadSpeedRaw, tr.UploadSpeed)
	}
	// the next window differences against the last sample
	tr.sampleCounters(3_500_000, 2_900_000, 500_000, tr.lastTimeSpeed.Add(time.Second))
	if tr.DownloadSpeed != 500_000 || tr.DownloadSpeedRaw != 500_000 || tr.UploadSpeed != 0 {
		t.Errorf("second window: speed=%v raw=%v up=%v", tr.DownloadSpeed, tr.DownloadSpeedRaw, tr.UploadSpeed)
	}
}
