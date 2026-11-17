package daemon

import (
	"context"
	"flodni/internal/gostorm/torr"
	torrstor "flodni/internal/gostorm/torr/storage/torrstor"
	"flodni/internal/warmup"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"time"
)

// tailFillTarget carries what EnsureTail needs, resolved at Open and read on media.stop.
type tailFillTarget struct {
	hash   string
	fileID int
	size   int64
}

// NativePumpState tracks a shared pump across multiple handles for the same file.
type NativePumpState struct {
	cancel           context.CancelFunc
	reader           streamReader
	path             string
	refCount         int32
	playerOff        int64       // last known player position, saved on handle release
	interruptPending atomic.Bool // prevents cascade: only the first handle per seek fires Interrupt()
}

// attachToExistingPump links the handle to a running pump exactly once. The caller holds
// pumpCreationMu, which serializes attaches on this path, and the hasSlot re-check under
// h.mu keeps the state assignment and the refCount increment together: two reads racing on
// the same handle would otherwise both attach, and Release could never bring refCount back
// to zero, pinning the pump and its slot until the reader idle timeout.
func (h *MkvHandle) attachToExistingPump(ps *NativePumpState) bool {
	h.mu.Lock()
	if h.hasSlot.Load() {
		h.mu.Unlock()
		return false
	}
	h.hasSlot.Store(true)
	h.refDropped.Store(false)
	h.pumpState = ps
	h.isWatching = true
	h.nativeReader = ps.reader
	h.pumpCancel = ps.cancel
	h.mu.Unlock()
	atomic.AddInt32(&ps.refCount, 1)
	globalOpenTracker.Inc(h.hash, h.path)
	return true
}

// terminateOrphanPump cancels and forgets any pump registered for path. The cleanup sweep
// uses it when pruning a zombie registry entry: without it the pump keeps its slot and keeps
// fetching until the reader idle timeout (V262, up to 2h with an open handle).
func terminateOrphanPump(path string) bool {
	val, ok := app.activePumps.Load(path)
	if !ok {
		return false
	}
	if ps, ok := val.(*NativePumpState); ok && ps.cancel != nil {
		ps.cancel()
	}
	app.activePumps.Delete(path)
	return true
}

// startNativePump acquires a slot and starts the background pump.
// Called from Open (proactive) or Read (rescue for late resolution).
// scanSlotLimit is how many pump slots a not-yet-confirmed playback may find taken before it
// is denied one. capacity is the semaphore's, fixed at make(): reading the live config here
// would apply half of a limit change, since the channel itself cannot be resized.
// The floor of 1 matters because a request cannot reach IsHealthy without pumping first.
func scanSlotLimit(capacity int, anyHealthyPlayback bool) int {
	if anyHealthyPlayback {
		return 5
	}
	if n := capacity - 5; n > 1 {
		return n
	}
	return 1
}

func (h *MkvHandle) startNativePump(finalHash string, fileIdx int) {
	// 1. Verify we don't already have a slot or an active pump
	if h.hasSlot.Load() {
		return
	}

	isHealthy := false
	if val, ok := app.playbackRegistry.Load(h.path); ok {
		if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
			isHealthy = true
		}
	}

	// StrategicReserve: limit background scan slots.
	// If any confirmed playback is active (IsHealthy), tighten the limit to 5 slots
	// so the scan cannot consume memory that the active stream needs.
	// Without active playback, allow up to MasterConcurrencyLimit-5 (default 20).
	canTakeSlot := true
	if !isHealthy {
		anyHealthyPlayback := false
		app.playbackRegistry.Range(func(_, v interface{}) bool {
			if ps, ok := v.(*PlaybackState); ok && ps.GetStatus() {
				anyHealthyPlayback = true
				return false
			}
			return true
		})
		scanLimit := scanSlotLimit(cap(masterDataSemaphore), anyHealthyPlayback)
		if len(masterDataSemaphore) >= scanLimit {
			canTakeSlot = false
			logger.Info(fmt.Sprintf("[StrategicReserve] Denying pump slot to background scan (Saturation: %d/%d, healthyPlayback=%v): %s", len(masterDataSemaphore), gc().MasterConcurrencyLimit, anyHealthyPlayback, filepath.Base(h.path)), "any_healthy_playback", anyHealthyPlayback, "file", filepath.Base(h.path))
		}
	}

	if !canTakeSlot {
		return
	}

	pumpCreationMu.Lock()

	if val, ok := app.activePumps.Load(h.path); ok {
		ps := val.(*NativePumpState)
		newRefs := atomic.AddInt32(&ps.refCount, 1)
		h.mu.Lock()
		h.hasSlot.Store(true)
		h.refDropped.Store(false)
		h.pumpState = ps
		h.isWatching = true
		h.nativeReader = ps.reader
		h.pumpCancel = ps.cancel
		h.mu.Unlock()
		globalOpenTracker.Inc(h.hash, h.path)
		// Primary reconnect (refCount 0→1): inherit player position.
		// Secondary handles (Plex probes at arbitrary offsets): no inheritance.
		if newRefs == 1 && !h.shadowsActivePlayer() {
			becomePrimary(h)
			if curPos := atomic.LoadInt64(&ps.playerOff); curPos > 0 {
				atomic.StoreInt64(&h.lastOff, curPos)
			}
		}
		logger.Info(fmt.Sprintf("[V264] Attached to existing pump (Refs: %d, primary=%v): %s", newRefs, h.isPrimaryHandle.Load(), filepath.Base(h.path)), "new_refs", newRefs, "file", filepath.Base(h.path))
		pumpCreationMu.Unlock()
		return
	}

	// Release mutex before blocking on semaphore to avoid holding it during I/O.
	pumpCreationMu.Unlock()

	select {
	case masterDataSemaphore <- struct{}{}:
		// Double-check activePumps after acquiring semaphore (another goroutine may have created it).
		pumpCreationMu.Lock()
		if val, ok := app.activePumps.Load(h.path); ok {
			<-masterDataSemaphore
			ps := val.(*NativePumpState)
			newRefs := atomic.AddInt32(&ps.refCount, 1)
			h.mu.Lock()
			h.hasSlot.Store(true)
			h.refDropped.Store(false)
			h.pumpState = ps
			h.isWatching = true
			h.nativeReader = ps.reader
			h.pumpCancel = ps.cancel
			h.mu.Unlock()
			globalOpenTracker.Inc(h.hash, h.path)
			if newRefs == 1 && !h.shadowsActivePlayer() {
				becomePrimary(h)
				if curPos := atomic.LoadInt64(&ps.playerOff); curPos > 0 {
					atomic.StoreInt64(&h.lastOff, curPos)
				}
			}
			pumpCreationMu.Unlock()
			return
		}

		h.hasSlot.Store(true)
		h.refDropped.Store(false)
		// The pump's creator is its primary, unless a player is watching through another handle
		// (a probe that opens while no pump is registered must not take the primary this way).
		if !h.shadowsActivePlayer() {
			becomePrimary(h)
		}
		// Capture the reader now and pass it to the pump goroutine: re-reading h.nativeReader
		// inside the goroutine raced with Release setting it to nil, and the resulting early
		// return (before the teardown defer) leaked the master slot.
		pumpReader := nativeBridge.NewStreamReader(finalHash, fileIdx, h.size)
		h.nativeReader = pumpReader
		if tr := torr.PeekTorrent(finalHash); tr != nil && tr.Torrent != nil {
			if info := tr.Torrent.Info(); info != nil && info.PieceLength > 0 {
				pl := int64(info.PieceLength)
				pumpReader.SetPieceLen(pl)
				raCache.SetPieceLen(h.path, pl)
			}
		}

		// Register in activePumps BEFORE releasing lock, but BEFORE doing I/O
		pumpCtx, pumpCancel := context.WithCancel(context.Background())
		h.pumpCancel = pumpCancel
		sharedState := &NativePumpState{
			cancel:   pumpCancel,
			reader:   pumpReader,
			path:     h.path,
			refCount: 1,
		}
		app.activePumps.Store(h.path, sharedState)
		h.pumpState = sharedState
		globalOpenTracker.Inc(h.hash, h.path)
		pumpCreationMu.Unlock() // SUCCESS: Shared state registered, global lock released

		logger.Info(fmt.Sprintf("[V264] Native Pump Started (Slot Acquired): %s", filepath.Base(h.path)), "file", filepath.Base(h.path))

		// Seed h.lastOff from the persisted resume position (V750 playback_states.last_seek_off)
		// before any of the anchoring logic below runs, so a cold pump-start (h.lastOff still -1,
		// no live signal yet) for a file resumed deep into its runtime is treated exactly like an
		// inherited primary-reconnect position (see newRefs==1 above) instead of starting near
		// MaxCachedOffset/0 - without this, resuming at e.g. 19GB left the pump buffering from
		// scratch while the player was served entirely by FetchBlock until something else caught
		// up. >2MB matches the existing real-playback-vs-scanner threshold (main.go IsInferredPlayback).
		if atomic.LoadInt64(&h.lastOff) < 0 {
			seeded := false
			if val, ok := app.playbackRegistry.Load(h.path); ok {
				if pbs, ok := val.(*PlaybackState); ok {
					pbs.mu.RLock()
					lastSeekOff := pbs.LastSeekOff
					pbs.mu.RUnlock()
					seeded = h.seedResumeAnchor(pbs, lastSeekOff)
				}
			}
			// V751: registry miss or fresh entry (a clean stop before boot leaves
			// IsStopped=true, which restorePlaybackStates filters out) → fall back to the
			// DB so a mid-file resume still anchors the pump at the real position instead
			// of starting from 0 while the player is served entirely by FetchBlock.
			if !seeded && stateDB != nil {
				if rec, err := stateDB.LoadPlaybackStateByPath(h.path); err == nil && rec != nil && rec.LastSeekOff > 0 {
					if h.seedResumeAnchor(nil, rec.LastSeekOff) {
						logger.Info(fmt.Sprintf("[ResumeAnchor] Seeded pump start from DB: %.1fMB", float64(rec.LastSeekOff)/(1<<20)))
					}
				}
			}
		}

		// Start background pump — resume from last cached position
		resumeOffset := raCache.MaxCachedOffset(h.path)

		// Start pump near end of warmup zone so it buffers past 64MB before SSD handover.
		if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" {
			diskOffset := warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID)
			if diskOffset > 16*1024*1024 {
				safetyMargin := int64(16 * 1024 * 1024)
				skipOffset := diskOffset - safetyMargin
				if skipOffset > resumeOffset {
					resumeOffset = skipOffset
					logger.Info(fmt.Sprintf("[DiskWarmup] PUMP SKIP: Starting from %.1fMB to bridge SSD handover", float64(resumeOffset)/(1<<20)))
				}
			}
		}

		// Anchor pump to player position when MaxCachedOffset is stale-high to prevent EOF loops.
		if playerOff := atomic.LoadInt64(&h.lastOff); playerOff > 0 {
			chunkSize := raCache.ChunkSize(h.path)
			if resumeOffset > playerOff+chunkSize*2 {
				aligned := (playerOff / chunkSize) * chunkSize
				logger.Info(fmt.Sprintf("[V700] Pump anchored to player: %.1fMB (MaxCached was %.1fMB)", float64(aligned)/(1<<20), float64(resumeOffset)/(1<<20)))
				resumeOffset = aligned
			}
		} else if playerOff < 0 && resumeOffset > 0 {
			// New handle: reset stale MaxCachedOffset unless warmup is active and covers the range.
			// If resumeOffset >= warmup.FileSize, pump skip cannot fire → dead zone in raCache.
			warmupCoverage := int64(0)
			if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" {
				warmupCoverage = warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID)
			}
			if warmupCoverage == 0 || resumeOffset >= warmup.FileSize {
				logger.Info(fmt.Sprintf("[V700] New handle: reset stale MaxCachedOffset %.1fMB → 0", float64(resumeOffset)/(1<<20)))
				resumeOffset = 0
			}
		}

		// Anchor pump to player position on resume to eliminate anacrolix priority competition.
		{
			chunkSize := raCache.ChunkSize(h.path)
			if raV310 := atomic.LoadInt64(&h.lastOff); raV310 > 0 && resumeOffset+chunkSize < raV310 {
				pumpStartV310 := (raV310 / chunkSize) * chunkSize
				logger.Info(fmt.Sprintf("[V310] Resume anchor: pump start → %dMB (player at %dMB)", pumpStartV310/(1024*1024), raV310/(1024*1024)))
				resumeOffset = pumpStartV310
			}
		}

		if resumeOffset > 0 {
			atomic.StoreInt64(&h.lastOff, resumeOffset)
		}

		pumpStart := resumeOffset
		capturedState := sharedState
		safeGo(func() {
			h.nativePump(pumpCtx, pumpReader, pumpStart, capturedState)
		})
	default:
		// If slots are full, it will fall back to per-request slots in Read
		logger.Info(fmt.Sprintf("[MasterSemaphore] Limit reached, %s will use Fallback mode", filepath.Base(h.path)), "file", filepath.Base(h.path))
	}
}

// nativePump reads continuously from the Native pipe and fills raCache.
// sharedState guards against orphan-delete in defer. pumpReader is captured by the caller
// so a concurrent Release cannot nil it out from under the goroutine.
func (h *MkvHandle) nativePump(ctx context.Context, pumpReader streamReader, startOffset int64, sharedState *NativePumpState) {
	// The master slot must be returned on every exit, including the early return below.
	// It used to be released only by the teardown defer, which the nil-reader return path
	// never reached, leaking one slot per event.
	defer func() {
		if h.hasSlot.Load() {
			select {
			case <-masterDataSemaphore:
			default:
			}
			h.hasSlot.Store(false)
		}
	}()

	if pumpReader == nil {
		logger.Info(fmt.Sprintf("[Pump] reader is nil at startup for %s", filepath.Base(h.path)), "file", filepath.Base(h.path))
		return
	}

	// ctx is checked only at the top of the loop; without this a cancelled pump blocked in
	// ReadAt keeps its slot and keeps downloading until the stream times out.
	// Adapted from upstream Tiramisu 2c6300e.
	stopInterruptOnCancel := context.AfterFunc(ctx, pumpReader.Interrupt)
	defer stopInterruptOnCancel()

	if h.hash == "" {
		// Late hash resolution for handles where Open() didn't complete it.
		if hash, fileID, err := resolveTargetFile(h.url, h.size, h.path); err == nil {
			h.mu.Lock()
			h.hash = hash
			h.fileID = fileID
			h.mu.Unlock()
			logger.Info(fmt.Sprintf("[Pump] Late resolution success: %s", hash[:8]), "hash", hash[:8])
		} else {
			logger.Warn(fmt.Sprintf("[Pump] Warning: hash empty for %s, warmup disabled", filepath.Base(h.path)), "file", filepath.Base(h.path))
		}
	}
	defer func() {
		// V306: Check if pump exited during confirmed healthy playback — this is
		// unexpected and usually indicates an I/O error or cancellation. Log for diagnosis.
		pumpExitedHealthy := false
		if val, ok := app.playbackRegistry.Load(h.path); ok {
			if ps, ok := val.(*PlaybackState); ok && ps.GetStatus() {
				pumpExitedHealthy = true
			}
		}

		h.mu.Lock()
		// Only delete if our sharedState is still the registered one (prevents pump A's defer from deleting pump B).
		if val, ok := app.activePumps.Load(h.path); ok && val == sharedState {
			app.activePumps.Delete(h.path)
			// Purge the adaptive chunk size set at this pump's start (SetPieceLen, main.go:1075):
			// otherwise pieceLens keeps growing with entries for paths no pump is streaming
			// anymore, and a future pump for the same path with a different piece length would
			// see the stale entry via ChunkSize() until a fresh SetPieceLen call overwrites it.
			raCache.ClearPieceLen(h.path)
		}

		if pumpExitedHealthy {
			logger.Warn(fmt.Sprintf("[V306] WARNING: pump goroutine exited during healthy playback for %s — priority preserved via IsHealthy", filepath.Base(h.path)), "file", filepath.Base(h.path))
		}

		// The master slot is released by the defer installed at the top of nativePump.
		_ = pumpReader.Close() // read-only reader
		h.mu.Unlock()
		logger.Info(fmt.Sprintf("[V239] Native Pump Goroutine Ended: %s", filepath.Base(h.path)), "file", filepath.Base(h.path))
	}()

	// V752: starvation watchdog state — sliding 10s window + last revival time.
	var watchSamples []watchSample
	lastRevival := time.Now().Add(-time.Hour)

	chunkSize := raCache.ChunkSize(h.path)

	// Track bytes pumped in this session for the Grace Period Boost
	pumpedBytes := int64(0)
	// Align startOffset to chunk boundary
	offset := (startOffset / chunkSize) * chunkSize

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		// Re-read chunkSize each iteration for consistency with raCache's own lookups - not
		// because it can change mid-pump. SetPieceLen is called exactly once, at pump start
		// (main.go:1084), only if metadata is already resolved at that moment; if not, this
		// stays at the ReadAheadBase fallback for the pump's entire lifetime. No late-binding path
		// exists today - if one is ever added, chunkKey's offset/chunkSize indexing means a
		// mid-pump size change must purge that path's existing raCache entries first, or stale
		// keys orphan and double-cache.
		chunkSize = raCache.ChunkSize(h.path)

		// Release idle slot: confirmed playback gets 2h, background scans get 45s.
		// Check all handles for this path, not just the pump creator.
		lastAct := time.Time{}
		app.activeHandles.Range(func(key, value interface{}) bool {
			handle := key.(*MkvHandle)
			if handle.path == h.path {
				handle.mu.Lock()
				if handle.lastActivityTime.After(lastAct) {
					lastAct = handle.lastActivityTime
				}
				handle.mu.Unlock()
			}
			return true
		})
		// Fallback to pump creator's activity if no active handles found
		if lastAct.IsZero() {
			h.mu.Lock()
			lastAct = h.lastActivityTime
			h.mu.Unlock()
		}

		timeoutLimit := 45 * time.Second
		playbackConfirmedOrInferred := false
		if val, ok := app.playbackRegistry.Load(h.path); ok {
			if ps, ok := val.(*PlaybackState); ok {
				if ps.GetStatus() {
					timeoutLimit = 2 * time.Hour
					playbackConfirmedOrInferred = true
				} else if ps.IsInferredPlayback() {
					timeoutLimit = 10 * time.Minute // V750: Inferred playback
					playbackConfirmedOrInferred = true
				}
			}
		}

		if time.Since(lastAct) > timeoutLimit {
			logger.Info(fmt.Sprintf("[V262] Idle timeout (%v) for %s - yielding slot", timeoutLimit, filepath.Base(h.path)), "timeout_limit", timeoutLimit, "file", filepath.Base(h.path))
			return
		}

		// Reader budget: park, never cancel, while the next chunk is refused. The engine reader
		// stays open and unread (the state of the lead-limit park below); a confirmation
		// resumes the pump with no recovery, and the exits above and the context still run.
		if budgetBlocksFetch(h.path, offset, chunkSize, h.size) {
			watchSamples = watchSamples[:0]
			time.Sleep(100 * time.Millisecond)
			continue
		}

		// Sync to primary handles only; secondary metadata probes cause false 10GB+ jumps.
		now := time.Now()
		playerOff := int64(0)
		app.activeHandles.Range(func(key, value interface{}) bool {
			handle := key.(*MkvHandle)
			if handle.path == h.path && handle.isPrimaryHandle.Load() {
				if off := atomic.LoadInt64(&handle.lastOff); off > playerOff {
					playerOff = off
				}
			}
			return true
		})

		// Snap pump to player position when seek gap exceeds budget, aligned to chunk boundary.
		jumpThreshold := int64(gc().ReadAheadBudget)
		if playerOff > offset+jumpThreshold {
			jumpTo := (playerOff / chunkSize) * chunkSize
			if jumpTo < 0 {
				jumpTo = 0
			}
			logger.Info(fmt.Sprintf("[V284] Pump jump: %dMB → %dMB (player at %dMB, gap %dMB)", offset/(1024*1024), jumpTo/(1024*1024), playerOff/(1024*1024), (playerOff-offset)/(1024*1024)))
			offset = jumpTo
			pumpedBytes = 0                 // reset grace period so throttle doesn't fire immediately
			watchSamples = watchSamples[:0] // V752: fresh window after jump — rates across a jump are meaningless
		} else if pumpStrandedAhead(offset, playerOff, jumpThreshold) {
			// V754: the snap above only fires forward, so a pump anchored past the player could
			// never rejoin it — nativePumpChunk's hard limit parked it in a 100ms sleep for the
			// whole session while every read fell through to blocking FetchBlock.
			jumpTo := (playerOff / chunkSize) * chunkSize
			if jumpTo < 0 {
				jumpTo = 0
			}
			logger.Info(fmt.Sprintf("[V754] Pump re-anchor (stranded ahead): %dMB → %dMB (player at %dMB, lead %dMB)", offset/(1024*1024), jumpTo/(1024*1024), playerOff/(1024*1024), (offset-playerOff)/(1024*1024)))
			offset = jumpTo
			pumpedBytes = 0
			watchSamples = watchSamples[:0]
		}

		// V752: starvation watchdog — revive a pump that crawls below the player's
		// consumption rate while its lead is thin, without waiting for a player seek.
		// A blocked read inside nativePumpChunk cannot evaluate here; fetch timeouts
		// (p95/hedge scale) return control to the loop and the next iteration fires.
		watchSamples = append(watchSamples, watchSample{t: now, offset: offset, playerOff: playerOff})
		if len(watchSamples) > 16 {
			watchSamples = watchSamples[len(watchSamples)-16:] // V752: size-bounded ring — sparse crawls must survive in the window
		}
		if len(watchSamples) >= 2 {
			first, lastS := watchSamples[0], watchSamples[len(watchSamples)-1]
			elapsed := lastS.t.Sub(first.t).Seconds()
			if elapsed >= 9 {
				pumpRate := float64(lastS.offset-first.offset) / elapsed
				playerRate := float64(lastS.playerOff-first.playerOff) / elapsed
				if shouldFireStarvationRevival(playbackConfirmedOrInferred, offset-playerOff, pumpRate, playerRate, now.Sub(lastRevival)) {
					// Same core as V286b: Interrupt unblocks the in-flight chunk read and
					// the retry path re-issues it with fresh peer selection; ResetShield
					// clears adaptive state. interruptPending prevents cascade with seeks.
					if ps := sharedState; !ps.interruptPending.Swap(true) {
						// pumpReader, not h.nativeReader: Release nils the field while the pump
						// keeps its captured copy alive, so the field can be nil right here.
						pumpReader.Interrupt()
						torrstor.ResetShield()
						lastRevival = now
						watchSamples = watchSamples[:0]
						logger.Info(fmt.Sprintf("[StarvationWatchdog] Revival: pump %dMB vs player %dMB (rates %.1f/%.1f MB/s) — interrupt+shield reset", offset/(1<<20), playerOff/(1<<20), pumpRate/(1<<20), playerRate/(1<<20)))
					}
				}
			}
		}

		// Throttle background pump after 64MB grace period.
		if pumpedBytes > 64*1024*1024 {
			isHealthy := false
			if val, ok := app.playbackRegistry.Load(h.path); ok {
				if ps, ok := val.(*PlaybackState); ok && (ps.GetStatus() || ps.IsInferredPlayback()) {
					isHealthy = true
				}
			}

			if !isHealthy {
				// Throttle background tasks: 150ms delay between 16MB chunks.
				time.Sleep(150 * time.Millisecond)
			}
		}

		stop, nextOffset := h.nativePumpChunk(pumpReader, offset, chunkSize, playerOff)
		if stop {
			// Transient errors (seek interrupt, reconnect, piece timeout): retry until genuine EOF.
			if offset < h.size {
				time.Sleep(200 * time.Millisecond)
				// V286b: reset after each interrupt so subsequent seeks can fire Interrupt()
				// and ResetShield() again. Without this, interruptPending stays true for the
				// pump's entire lifetime after the first seek.
				sharedState.interruptPending.Store(false)
				continue
			}
			return // genuine EOF
		}

		pumpedBytes += (nextOffset - offset)
		offset = nextOffset
	}
}

// nativePumpChunk reads a single chunk from the Native pipe into raCache.
// pumpLeadLimit caps the pump's lead so the player stays inside the raCache window. The cache
// holds the last `budget` bytes the pump wrote, i.e. [pumpOff-budget, pumpOff]; the pump stops
// only once diff EXCEEDS the limit, so its settled lead is limit+chunkSize. With limit == budget
// the player sits a whole chunk below the window and every read on that edge misses (Lucky
// S01E04, 2026-08-22: pump-off pinned at 272MB against a 256MB cache, 40 consecutive stalls).
// Reserving three chunks leaves two of margin after the overshoot. Budget and chunkSize are both
// user-configurable, so the margin is derived, never hardcoded; a budget too small to hold it
// keeps the old behaviour rather than starving the lead.
func pumpLeadLimit(budget, chunkSize int64) int64 {
	if budget <= 0 || chunkSize <= 0 {
		return budget
	}
	slack := 3 * chunkSize
	if slack > budget/2 {
		slack = budget / 2
	}
	lead := budget - slack
	if lead < chunkSize {
		lead = chunkSize
	}
	if lead > budget {
		lead = budget
	}
	return lead
}

func (h *MkvHandle) nativePumpChunk(r streamReader, offset, chunkSize, playerOff int64) (stop bool, nextOffset int64) {
	// Don't pump beyond file size
	if offset >= h.size {
		return true, offset
	}

	budget := gc().ReadAheadBudget
	leadLimit := pumpLeadLimit(budget, chunkSize)
	diff := offset - playerOff

	if diff > leadLimit {
		// Hard limit reached: Wait for player to advance.
		time.Sleep(100 * time.Millisecond)
		return false, offset
	} else if diff > (leadLimit * 7 / 10) {
		// Soft limit (70%): Slow down gradually.
		sleepMs := (diff - (leadLimit * 7 / 10)) / (1024 * 1024)
		if sleepMs > 50 {
			sleepMs = 50
		}
		if sleepMs > 0 {
			time.Sleep(time.Duration(sleepMs) * time.Millisecond)
		}
	}

	// Playback pressure: the pump's lead over the player has worn thin (opposite end of the same
	// diff/budget signal used to throttle above). Extends tail-hedging into steady-state playback
	// via the same mechanism already used for warmup - see SetPlaybackPressure. Called on every
	// chunk like SetWarmupActive already is; both just store atomics, cheap either way.
	if h.hash != "" {
		if tr := torr.PeekTorrent(h.hash); tr != nil && tr.Torrent != nil {
			tr.Torrent.SetPlaybackPressure(diff < budget*20/100, offset)
		}
	}

	end := offset + chunkSize
	if end > h.size {
		end = h.size
	}

	// Coverage-aware skip-check: must cover the WHOLE [offset, end-1] range, not just probe the
	// first byte. A single-byte probe would still hit after a partial Put (see the drift fix
	// below), letting the pump skip a chunk that's only partially cached and leave a hole.
	// Covered() does the check without copying; this is the common path in steady-state
	// playback, so only pay for Get()'s up-to-16MB defensive copy when DiskWarmup actually
	// needs the bytes.
	if raCache.Covered(h.path, offset, end-1) {
		if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" && offset <= warmup.FileSize {
			if data := raCache.Get(h.path, offset, end-1); data != nil {
				warmup.DiskWarmup.WriteChunk(h.hash, h.fileID, data, offset)
			}
		}
		return false, offset + chunkSize
	}

	// Skip warmup zone during initial play (SSD serves 0-80MB); pump jumps ahead to pre-fill raCache.
	// Gated on stateWarmup to avoid skip on resume/seek.
	if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" && h.state.Load() == stateWarmup {
		warmupCoverage := warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID)
		if warmupCoverage >= offset+chunkSize {
			return false, offset + chunkSize
		}
	}

	// Use buffer from pool to reduce allocations
	bufPtr := readBufferPool.Get().(*[]byte)
	defer readBufferPool.Put(bufPtr)

	n, err := r.ReadAt((*bufPtr)[:end-offset], offset)
	if n > 0 {
		raCache.Put(h.path, offset, offset+int64(n)-1, (*bufPtr)[:n])
		noteFillAttributed(h.path, offset, int64(n), h.size)
		if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" && offset <= warmup.FileSize {
			warmup.DiskWarmup.WriteChunk(h.hash, h.fileID, (*bufPtr)[:n], offset)
		}
	}

	if err != nil {
		return true, offset + int64(n)
	}

	// A short read (fewer bytes than requested, err==nil) happens when a stream hiccup gets
	// silently converted to (n, nil) upstream (see native.go ReadAt's ErrUnexpectedEOF handling).
	// Continuing from offset+n would leave the pump mid-chunk permanently: every Put/skip-check
	// keys off ChunkSize-aligned offsets, so an unaligned continuation drifts off the chunk grid
	// for the rest of the pump's lifetime, turning every future chunk boundary into a cache miss.
	// Snap forward to the next chunk boundary instead; the small gap this skips is filled on demand
	// by FUSE reads landing in it (native.go's ReadAt handles both small and large forward jumps).
	got := offset + int64(n)
	if int64(n) < end-offset {
		return false, (got/chunkSize + 1) * chunkSize
	}
	return false, got
}

// shouldInterruptForSeek returns true for genuine seeks beyond budget.
// Ignores: new handles (prevOff<=0), Samba header probes (off==0), sequential reads.
func shouldInterruptForSeek(prevOff, off, budget int64) bool {
	if prevOff <= 0 || off == 0 {
		return false
	}
	return off > prevOff+budget || prevOff > off+budget
}

// tailProbeZoneSize returns the adaptive tail-probe zone size for a file of the given size:
// 5%, clamped to [64MB, 2GB]. Shared by the LastSeekOff
// write-site guard, and the ResumeAnchor seed-site guard - a single scanner probe reading MKV
// Cues near the end of a file must never look like a real playback position to any of them.
// promoteIfStreaming marks a handle primary once it has proven it is the player: several
// consecutive sequential reads of streaming size. The pump syncs to primary handles only, and
// during steady playback the real reader is usually an attached (secondary) handle - the
// nominal primary can be an idle probe holding the first reference, so the pump follows the
// wrong offset and the player is served by FetchBlock (Soulm8te, 2026-08-19: 29 attaches, all
// primary=false, only a seek recovered it by forcing a refCount 0->1 reconnect).
//
// Promotion is monotonic and never recomputed per tick, unlike the freshness heuristic reverted
// in 6ae20c9: the pump takes the MAX of the primaries' offsets, so a promotion can only raise
// that value, never drag it backwards - which is what produced the jump/re-anchor thrashing.
// Tail-zone reads never count: an EOF probe must not be able to pull the pump to the end.
// advanceProvesStreaming reports whether one read looks like the player moving forward through
// the file: streaming-sized, sequential from the previous read, and outside the tail probe zone.
// The size floor is a quarter of StreamingThreshold so Synology's 64KB CIFS reads still count.
func advanceProvesStreaming(prevOff, off, readLen, size, tolerance, streamingThreshold int64) bool {
	if prevOff < 0 || off <= prevOff || readLen <= 0 {
		return false
	}
	if off-prevOff > readLen+tolerance {
		return false
	}
	if readLen < streamingThreshold/4 {
		return false
	}
	return off < size-tailProbeZoneSize(size)
}

const primaryProofReads = 3 // consecutive sequential reads before a handle drives the pump

func (h *MkvHandle) promoteIfStreaming(prevOff, off int64, readLen int) {
	advance := prevOff >= 0 && advanceProvesStreaming(prevOff, off, int64(readLen), h.size, gc().SequentialTolerance, gc().StreamingThreshold)
	if !advance {
		h.seqRun.Store(0)
	} else if h.seqRun.Add(1) >= primaryProofReads {
		h.streamedOnce.Store(true)
	}
	if h.isPrimaryHandle.Load() || h.hash == "" || prevOff < 0 {
		return
	}
	if !advance {
		h.seqAdvances.Store(0)
		return
	}
	if h.seqAdvances.Add(1) < primaryProofFor(off, primaryOffsetFor(h.path, h), gc().ReadAheadBudget) {
		return
	}
	// Only during real playback: a background scan reads sequentially too, and outside a
	// confirmed session there is no player for the pump to follow anyway.
	val, ok := app.playbackRegistry.Load(h.path)
	if !ok {
		return
	}
	ps, ok := val.(*PlaybackState)
	if !ok || !(ps.GetStatus() || ps.IsInferredPlayback()) {
		return
	}
	// A second sequential reader (a media analysis, a thumbnail pass) must not take the pump
	// from a player that is watching. Adapted from upstream Tiramisu 49533ee.
	if inc := activeStreamingPrimary(h.path, h); inc != nil && h.defersTo(inc, ps.sessionStartedAt()) {
		return
	}
	if becomePrimary(h) {
		logger.Info(fmt.Sprintf("[V284] Handle promoted to primary after %d sequential reads at %dMB: %s", h.seqAdvances.Load(), off/(1024*1024), filepath.Base(h.path)), "file", filepath.Base(h.path))
	}
}

func tailProbeZoneSize(size int64) int64 {
	zone := size / 20 // 5%
	if zone < 64*1024*1024 {
		zone = 64 * 1024 * 1024
	}
	if zone > 2*1024*1024*1024 {
		zone = 2 * 1024 * 1024 * 1024
	}
	return zone
}

// shouldServeTailFromSSD reports whether a read at offset off (file size size) can be
// served from the tail warmup file (the last tailWarmupSize bytes). Pure gate for the
// Read() path: ReadTail itself is range-safe (returns 0 outside coverage), so this
// only decides whether the SSD tail is even a candidate. It deliberately does not
// depend on handle state - any first read with off >= warmup.FileSize transitions
// WARMUP→STREAMING (seek/resume detection) before a state-gated tail check could run.
// A resume in the last 16MB was therefore served by network FetchBlock (~16s TTFF on
// a 36.6GB remux, 2026-08-06) although the tail file was complete on SSD.
func shouldServeTailFromSSD(off, size, tailWarmupSize int64) bool {
	if size <= 0 || tailWarmupSize <= 0 {
		return false
	}
	return off >= warmup.TailStartFor(size, tailWarmupSize)
}

// shouldRecordResumeOff reports whether a read may update the persisted resume position.
// PlaybackState is per-path, so before the single-primary invariant any handle touching the file
// could write here, and the monotonic guard it used to carry made the value a high-water mark
// that never came back down: a probe reading at 6431MB became the "resume position" for a player
// sitting at 1300MB, and the next session anchored the pump 5GB off (Lucky S01E04, 2026-08-22).
// Gating on the primary handle and dropping the monotonic guard records where the player
// actually is. The 2MB floor and the tail-probe ceiling still exclude header and Cues reads.
func shouldRecordResumeOff(healthy, isPrimary bool, off, size int64) bool {
	if !healthy || !isPrimary || size <= 0 {
		return false
	}
	return off > 2*1024*1024 && off < size-tailProbeZoneSize(size)
}

// resumeAnchorFromPersisted applies the tail-probe guard to a persisted resume
// position. Returns (anchor, zeroed): anchor is a valid pump start (0 when unusable),
// zeroed=true when the value lies in the tail-probe zone (or beyond EOF) and must be
// cleared - a single scanner probe reading MKV Cues near the end of a file must never
// look like a real playback position. Shared by the registry seeding and the DB
// fallback so both paths apply identical semantics.
func resumeAnchorFromPersisted(lastSeekOff, size int64) (anchor int64, zeroed bool) {
	if lastSeekOff > 2*1024*1024 && lastSeekOff < size-tailProbeZoneSize(size) {
		return lastSeekOff, false
	}
	if lastSeekOff >= size-tailProbeZoneSize(size) {
		return 0, true
	}
	return 0, false
}

// shouldInterruptPump decides whether a read should interrupt the pump. Reads served
// from the SSD tail warmup (shouldServeTailFromSSD) never touch the pump - Interrupt()
// + ResetShield() would only stall a pump the read does not need. The old isTailProbe
// gate was unreachable dead state (any read in the tail zone first transitions
// WARMUP→STREAMING), so a live scrub into the last 16MB interrupted the pump even
// though the SSD served the read instantly.
func shouldInterruptPump(prevOff, off, budget int64, tailServed bool) bool {
	if tailServed {
		return false
	}
	return shouldInterruptForSeek(prevOff, off, budget)
}

// primaryActiveWindow is how recently a primary that has streamed must have read to count as a
// player that is watching: the configured warm_start_idle_seconds, the idle time after which
// the read path already treats a handle as starting afresh. A buffered player goes quiet for
// seconds between bursts; once it is quiet for longer nothing watches through its primary and
// it is up for taking, which is what a reopen or a seek through another handle needs.
func primaryActiveWindow() time.Duration {
	if n := gc().WarmStartIdleSeconds; n > 0 {
		return time.Duration(n) * time.Second
	}
	return 6 * time.Second
}

// activeStreamingPrimary returns the primary handle on path, other than except, that has
// streamed like a player and read within primaryActiveWindow, or nil. A stale probe primary
// (header and Cues only) never streamed, so it never blocks the real player. It takes no lock:
// the read path calls it for every read of a non-primary handle.
func activeStreamingPrimary(path string, except *MkvHandle) *MkvHandle {
	var found *MkvHandle
	now := time.Now().UnixNano()
	app.activeHandles.Range(func(key, _ interface{}) bool {
		other, ok := key.(*MkvHandle)
		if !ok || other == except || other.path != path || !other.isPrimaryHandle.Load() || !other.streamedOnce.Load() {
			return true
		}
		if now-other.lastRead.Load() <= int64(primaryActiveWindow()) {
			found = other
			return false
		}
		return true
	})
	return found
}

// mayTakeOverStreamingPrimary reports whether h is the successor of inc, a primary that is
// watching: the player of the session, opened after inc and no later than the play webhook
// that started the session (sessionStart, set by media.play only; resume, pause and stop leave
// it alone). An analysis started during a session opens after that play; when the analysis ran
// first, the player opens after it and before the webhook.
//
// Anything else waits for inc to go quiet (activeStreamingPrimary is nil then) or to be
// released. A reopen that leaves the old handle open is served as a probe until then: a
// buffered player goes silent between bursts all the time, and a probe opening in such a lull
// must never be able to write the resume position.
func mayTakeOverStreamingPrimary(h, inc *MkvHandle, sessionStart time.Time) bool {
	return !h.openedAt.IsZero() && h.openedAt.After(inc.openedAt) && !h.openedAt.After(sessionStart)
}

// defersTo reports whether h must leave the primary to inc, a primary that is watching. An
// incumbent that opened after h never wins this way: h is the older handle, and if it reads
// again it is the player returning (after a pause, say) and retakes the primary on the normal
// rules, a sequential proof or a seek.
func (h *MkvHandle) defersTo(inc *MkvHandle, sessionStart time.Time) bool {
	if inc.openedAt.After(h.openedAt) {
		return false
	}
	return !mayTakeOverStreamingPrimary(h, inc, sessionStart)
}

func (h *MkvHandle) sessionStart() time.Time {
	if val, ok := app.playbackRegistry.Load(h.path); ok {
		if ps, ok := val.(*PlaybackState); ok {
			return ps.sessionStartedAt()
		}
	}
	return time.Time{}
}

// shadowsActivePlayer reports whether h is a second handle on a file whose primary is watching
// and h is not the player's successor (a probe). Such a handle never takes the primary, never
// interrupts or moves the pump and never fills the shared read-ahead cache (which would evict
// the primary's next chunks, oldest first).
func (h *MkvHandle) shadowsActivePlayer() bool {
	if h.isPrimaryHandle.Load() {
		return false
	}
	inc := activeStreamingPrimary(h.path, h)
	if inc == nil {
		return false
	}
	return h.defersTo(inc, h.sessionStart())
}

// primaryProofFor returns how many consecutive sequential reads a challenger must show before it
// may take primary. One landing within the read-ahead budget of the incumbent is the player moving
// normally (its own reopen lands right where it left off) and needs the base proof. One landing
// further away would drag the pump across the file, so it must sustain the streak far longer than
// a short-lived probe can: Lucky S01E04, 2026-08-22 saw handles attach at 5578/6069/6430MB, earn
// primary within 2-3s and be released immediately, each yanking the pump away from a player
// reading 2GB behind. A genuine seek by an open handle promotes through Read()'s seek path
// instead, so it never pays this. The extended proof is also the escape hatch that stops a wrong
// incumbent from wedging primary forever.
func primaryProofFor(challengerOff, incumbentOff, budget int64) int32 {
	if incumbentOff <= 0 || budget <= 0 {
		return primaryProofReads // no incumbent to displace
	}
	d := challengerOff - incumbentOff
	if d < 0 {
		d = -d
	}
	if d <= budget {
		return primaryProofReads
	}
	return primaryProofReads * 4
}

// primaryOffsetFor returns the offset of the current primary handle on path, or 0 if there is none.
func primaryOffsetFor(path string, except *MkvHandle) int64 {
	var off int64
	app.activeHandles.Range(func(key, _ interface{}) bool {
		other, ok := key.(*MkvHandle)
		if ok && other != except && other.path == path && other.isPrimaryHandle.Load() {
			if o := atomic.LoadInt64(&other.lastOff); o > off {
				off = o
			}
		}
		return true
	})
	return off
}

// becomePrimary makes h the only primary handle for its path, returning false if it already was.
// The pump follows the MAX of the primary offsets, so a probe that earned primary and then went
// idle kept dragging it to a frozen offset (Soulm8te, 2026-08-22: a stale PRI@8697MB anchored the
// pump 7.8GB past a player reading at 1120MB). Demotion is edge-triggered here at promotion time,
// never recomputed per pump tick: re-evaluating relevance every tick is what made the freshness
// heuristic in 6ae20c9 thrash (6517 jump pairs in 19h). Demoted handles lose their sequential
// streak too, so reclaiming primary costs a fresh primaryProofReads run and a frozen handle,
// which by definition stops reading, can never reclaim it at all.
func becomePrimary(h *MkvHandle) bool {
	if !h.isPrimaryHandle.CompareAndSwap(false, true) {
		return false
	}
	app.activeHandles.Range(func(key, _ interface{}) bool {
		other, ok := key.(*MkvHandle)
		if ok && other != h && other.path == h.path && other.isPrimaryHandle.CompareAndSwap(true, false) {
			other.seqAdvances.Store(0)
		}
		return true
	})
	return true
}

// pumpStrandedAhead reports whether the pump sits so far past the player that it can no longer
// serve it. nativePumpChunk's hard limit already parks a healthy pump at ~budget, so only a lead
// beyond 2x budget means a bad anchor rather than throttling. playerOff == 0 means no primary
// handle is registered right now (handle handoff), and must never drag the pump back to 0.
func pumpStrandedAhead(offset, playerOff, budget int64) bool {
	if playerOff <= 0 || budget <= 0 {
		return false
	}
	return offset > playerOff+2*budget
}

// shouldFireStarvationRevival decides whether the pump loop must self-revive: the
// pump is confirmed playback, its lead over the player is thin (< 16MB), and it is
// advancing below 75% of the player's consumption rate for the observation window.
// This replicates what a manual player seek does via V286b (Interrupt + ResetShield +
// fresh peer selection) without moving the player. Rates are bytes/second averaged
// over the 10s window by the caller. The lead gate protects the healthy hard-limit
// idle (lead = ReadAheadBudget): a pump with a thick lead is deliberately stopped,
// not starved (coincidentally the same 16MB as warmup.TailWarmupSize, but a
// different purpose: the watchdog lead, not the SSD tail coverage). Cooldown
// prevents revival storms.
func shouldFireStarvationRevival(confirmed bool, leadBytes int64, pumpRate, playerRate float64, cooldownAge time.Duration) bool {
	if !confirmed || cooldownAge < 60*time.Second {
		return false
	}
	if playerRate <= 0 || leadBytes >= 16*1024*1024 {
		return false
	}
	return pumpRate < playerRate*0.75
}

// seedResumeAnchor applies the tail-probe guard to a persisted resume position and,
// when valid, anchors the pump start there. ps is optional (nil on the DB fallback
// path). Returns true when the pump start was anchored. Zeroing a poisoned tail-probe
// value is a targeted UPDATE - it must never clobber the previous session's flags.
func (h *MkvHandle) seedResumeAnchor(ps *PlaybackState, lastSeekOff int64) bool {
	if lastSeekOff <= 2*1024*1024 {
		return false
	}
	anchor, zeroed := resumeAnchorFromPersisted(lastSeekOff, h.size)
	if anchor > 0 {
		atomic.StoreInt64(&h.lastOff, anchor)
		logger.Info(fmt.Sprintf("[ResumeAnchor] Seeding pump start from persisted resume position: %.1fMB", float64(anchor)/(1<<20)))
		return true
	}
	if zeroed {
		// Self-heal: a value in the tail zone (or beyond EOF) is a poisoned tail probe,
		// never a real resume position (a genuine near-end resume is served by the SSD
		// tail warmup, see shouldServeTailFromSSD, so it needs no pump anchor). Zero it
		// in memory + DB so it stops poisoning future pump starts.
		if ps != nil {
			ps.mu.Lock()
			ps.LastSeekOff = 0
			ps.mu.Unlock()
		}
		if stateDB != nil {
			if err := stateDB.UpdatePlaybackSeekOff(h.path, 0); err != nil {
				logger.Info(fmt.Sprintf("[ResumeAnchor] Failed to zero poisoned last_seek_off for %s: %v", filepath.Base(h.path), err), "file", filepath.Base(h.path), "err", err)
			} else {
				logger.Info(fmt.Sprintf("[ResumeAnchor] Zeroed poisoned last_seek_off (%.1fMB in tail zone) for %s", float64(lastSeekOff)/(1<<20), filepath.Base(h.path)), "file", filepath.Base(h.path))
			}
		}
	}
	return false
}

// forceTorrentWarmupActive marks hash/fileID as warmup-active unconditionally. Used at Open() for
// genuine cold starts (!headReady), before any WriteChunk has happened yet (and thus before
// DiskWarmup's writeWorker has had a chance to record the STARTING state itself), so aggressive
// PEX churn (Task 3) and the hedge watchdog (Task 4) catch the torrent's initial peer-connection
// burst instead of missing it. warmup.OnWarmupStateChange (wired in main() at startup) owns
// turning this back to false once the writeWorker actually observes STARTING/COMPLETED -
// synchronously, from inside processWrite itself, not by polling IsWarmingUp() from here (which
// would race ahead of the async write queue - WriteChunk only enqueues and returns immediately).
func forceTorrentWarmupActive(hash string, fileID int) {
	if warmup.DiskWarmup == nil {
		return
	}
	if tr := torr.PeekTorrent(hash); tr != nil && tr.Torrent != nil {
		tr.Torrent.SetWarmupActive(true, fileID)
	}
}
