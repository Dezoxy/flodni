package torr

import (
	"sync"
	"time"

	"flodni/internal/gostorm/settings"
)

// Opt-in connection boost at cold start (engine_cold_boost), adapted from upstream Tiramisu
// 2b0df97. A cold torrent mostly waits for a peer to unchoke it, and more open connections are
// more chances. While the configured per-torrent limit is below coldStartConns, the limit is
// raised to it until coldBoostEndBytes of useful data have arrived or coldBoostMaxAge has
// passed, then the configured limit comes back. With the switch off nothing here is reached.
const (
	coldStartConns    = 35
	coldBoostEndBytes = 1 << 20
	coldBoostMaxAge   = 30 * time.Second
)

// EngineColdBoost turns the boost on. The daemon sets it once at start from engine_cold_boost,
// before the first torrent wakes; it is not read again. False is the engine's behaviour
// without the boost.
var EngineColdBoost bool

// coldBoost is one wrapper's boost: it begins at most once and restores at most once. It
// never sleeps and starts no goroutine: Torrent.UpdateStats ticks it every second and
// Torrent.Close cancels it. mu is held across the engine setter so a restore cannot run
// before the raise it undoes; the engine never calls back into it.
type coldBoost struct {
	mu     sync.Mutex
	state  coldBoostState
	base   int64
	start  time.Time
	setter func(int)
}

type coldBoostState int

const (
	coldBoostIdle coldBoostState = iota
	coldBoostActive
	coldBoostDone
)

// begin raises the limit to coldStartConns when configured is below it. It reports whether
// it did: a boost that is active or already finished does not begin again.
func (b *coldBoost) begin(configured int, base int64, now time.Time, set func(int)) bool {
	if configured >= coldStartConns || set == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != coldBoostIdle {
		return false
	}
	b.state = coldBoostActive
	b.base, b.start, b.setter = base, now, set
	set(coldStartConns)
	return true
}

// tick restores the limit, to limitNow (the limit configured at that moment), once the boost
// has done its job or run out of time. useful is the useful bytes the torrent has read so
// far, or negative when that is not known yet (only the time then counts).
func (b *coldBoost) tick(useful int64, now time.Time, limitNow int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != coldBoostActive {
		return
	}
	if !(useful >= 0 && useful-b.base >= coldBoostEndBytes) && now.Sub(b.start) < coldBoostMaxAge {
		return
	}
	b.restoreLocked(limitNow)
}

// cancel restores the limit when the torrent closes during the boost.
func (b *coldBoost) cancel(limitNow int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == coldBoostActive {
		b.restoreLocked(limitNow)
	}
}

func (b *coldBoost) restoreLocked(limitNow int) {
	b.state = coldBoostDone
	set := b.setter
	b.setter = nil
	set(limitNow)
}

func configuredConns() int {
	if settings.BTsets == nil {
		return 0
	}
	return settings.BTsets.ConnectionsLimit
}

// setConns is the engine's per-torrent setter, behind the wrapper's lock: it does nothing
// once the wrapper has dropped its torrent.
func (t *Torrent) setConns(n int) {
	if tor := t.UnderlyingTorrent(); tor != nil {
		tor.SetMaxEstablishedConns(n)
	}
}

// BoostColdStart starts the boost when a player starts reading the torrent; the daemon decides
// when (internal/daemon/coldboost_handle.go). It does nothing unless
// engine_cold_boost is on, and nothing the second time for the same wrapper.
func (t *Torrent) BoostColdStart() {
	if !EngineColdBoost || t == nil {
		return
	}
	tor := t.UnderlyingTorrent()
	if tor == nil {
		return
	}
	st := tor.Stats()
	t.startColdBoost(st.BytesReadUsefulData.Int64(), time.Now(), t.setConns)
}

func (t *Torrent) startColdBoost(base int64, now time.Time, set func(int)) bool {
	if !EngineColdBoost || settings.BTsets == nil {
		return false
	}
	return t.coldBoost.begin(settings.BTsets.ConnectionsLimit, base, now, set)
}
