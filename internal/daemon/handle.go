package daemon

import (
	"context"
	"errors"
	"flodni/internal/gostorm/native"
	"flodni/internal/gostorm/torr"
	torrstor "flodni/internal/gostorm/torr/storage/torrstor"
	"flodni/internal/gostorm/web"
	"flodni/internal/vfs"
	"flodni/internal/warmup"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// handleState values for MkvHandle.state (atomic.Uint32).
// A handle transitions one-way: stateWarmup → stateStreaming.
const (
	stateWarmup    uint32 = 0 // Initial: SSD warmup eligible (TTFF phase)
	stateStreaming uint32 = 1 // Pump-only streaming; no SSD warmup
)

// watchSample is a V752 starvation-watchdog observation: pump offset and player
// offset at time t, used to compute both rates over a sliding 10s window.
type watchSample struct {
	t         time.Time
	offset    int64
	playerOff int64
}

func stateName(s uint32) string {
	switch s {
	case stateWarmup:
		return "WARMUP"
	case stateStreaming:
		return "STREAMING"
	default:
		return "UNKNOWN"
	}
}

type MkvHandle struct {
	url, path        string
	size             int64
	lastOff          int64
	lastLen          int
	lastTime         time.Time
	lastActivityTime time.Time
	monitorStarted   bool
	lastGlobalUpdate time.Time

	nativeReader streamReader
	hash         string
	magnet       string
	fileID       int
	wake         func(context.Context, string, int) error // re-activates an idle-closed torrent
	mu           sync.Mutex
	pumpCancel   context.CancelFunc
	hasSlot      atomic.Bool
	// pumpState is the state this handle incremented. activePumps is keyed by path
	// and the state is replaced wholesale, so Release must not decrement a pump it
	// never counted.
	pumpState       *NativePumpState
	isWatching      bool
	hasWarmup       bool          // true if both head+tail warmup available at Open time
	usesWarmup      bool          // true for every handle: all remaining files use the SSD warmup
	state           atomic.Uint32 // handleState: stateWarmup | stateStreaming
	pumpOnce        sync.Once
	isPrimaryHandle atomic.Bool  // pump creator, primary reconnects (refCount 0→1), proven readers
	seqAdvances     atomic.Int32 // consecutive sequential streaming reads, feeds the promotion below
	budgetHold      budgetHold   // reader budget: this handle's wait for a webhook (reader_budget.go)
	wakeRefused     atomic.Bool  // the engine refused to wake this handle's torrent: every read answers EIO
	refDropped      atomic.Bool  // this handle's pump reference and open count are dropped (Release or the sweep); an attach clears it
	released        atomic.Bool  // Release ran: a second call (kernel and orphan sweep) must change nothing
	openedAt        time.Time    // set once at Open; who opened first decides a takeover (mayTakeOverStreamingPrimary)
	lastRead        atomic.Int64 // UnixNano of the last read, primary or not; read without a lock by activeStreamingPrimary
	seqRun          atomic.Int32 // consecutive streaming advances, primary or not (feeds streamedOnce)
	streamedOnce    atomic.Bool  // read sequentially like a player at least once
	coldBoostTried  atomic.Bool  // engine_cold_boost was asked for at this handle's first player read
	tailHold        atomic.Int64 // >0: the last offset before a jump into the tail zone; the pump keeps following it (noteTailHold)
	wasPrimary      atomic.Bool  // recorded a resume position as the primary: an older handle may retake the primary from a later incumbent only then (defersTo)
	openCounted     atomic.Bool  // this handle's count in globalOpenTracker is outstanding (countOpen, uncountOpen)
	tailRun         atomic.Int32 // sequential streaming reads in the tail zone since that jump
}

// Compile-time interface checks for MkvHandle
var _ fs.FileReader = (*MkvHandle)(nil)
var _ fs.FileReleaser = (*MkvHandle)(nil)

// shortAwayFromEOF reports whether a read of n bytes into a want-sized buffer at off leaves a
// gap before end of file. Short AT eof is normal; short before it is what the kernel turns
// into a cached zero-filled page.
func shortAwayFromEOF(n, want int, off, size int64) bool {
	return n < want && off+int64(n) < size
}

// readInnerCall is a test seam: Read's short-read repair is driven by a fake in tests.
var readInnerCall = (*MkvHandle).readInner

// Read never hands the kernel a short read away from EOF. FUSE here runs on the kernel page
// cache (no direct_io), and Linux zero-fills the tail of a short read and marks the page
// uptodate — that region would then serve zeros for the life of the mount. Missing bytes are
// refetched; if they still cannot be produced, EIO is returned, which the kernel does not
// cache, so the read is retried instead of a hole being made permanent.
func (h *MkvHandle) Read(fuseCtx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	defer noteReadInFlight(h.path)()
	res, errno := readInnerCall(h, fuseCtx, dest, off)
	if errno == errnoBudgetRefused || errno == errnoWakeRefused {
		return nil, syscall.EIO // before the failure accounting below: not the swarm failing
	}
	if errno != 0 || res == nil {
		return res, errno
	}

	total := res.Size()
	if !shortAwayFromEOF(total, len(dest), off, h.size) {
		return res, 0
	}

	// Short away from EOF: every readInner return site writes into dest, so the bytes
	// already gathered stay valid and only the tail needs filling.
	fuseShortReadCount.Add(1)
	deadline := time.Now().Add(shortReadRefillBudget)

	for attempt := 0; attempt < shortReadRefillAttempts && shortAwayFromEOF(total, len(dest), off, h.size); attempt++ {
		if fuseCtx.Err() != nil {
			return nil, syscall.EINTR
		}
		if time.Now().After(deadline) {
			break // a blocked FUSE read puts smbd in D-state; bail out rather than hang
		}
		sub, subErrno := readInnerCall(h, fuseCtx, dest[total:], off+int64(total))
		if subErrno == errnoBudgetRefused || subErrno == errnoWakeRefused {
			return nil, syscall.EIO // a refused sub-read is not an unfillable gap: no repair accounting
		}
		if subErrno == syscall.EINTR && fuseCtx.Err() != nil {
			return nil, syscall.EINTR // the caller left (a hold lasts up to 15 s): not a failed read
		}
		if subErrno != 0 || sub == nil {
			break
		}
		m := sub.Size()
		if m == 0 {
			break
		}
		total += m
	}

	if shortAwayFromEOF(total, len(dest), off, h.size) {
		fuseShortReadFailed.Add(1)
		logger.Info(fmt.Sprintf("[ShortRead] Unfillable %d/%d bytes at offset %d for %s - EIO (a partial read would cache zeros)", total, len(dest), off, filepath.Base(h.path)), "total", total, "off", off, "file", filepath.Base(h.path))
		ttffReadFailed(h.path)
		return nil, syscall.EIO
	}

	fuseShortReadRepaired.Add(1)
	return fuse.ReadResultData(dest[:total]), 0
}

// writeWarmupFromFetch mirrors a fetched block into the SSD warmup files: the head window
// always, the tail only until playback is confirmed (the frozen tail is the discovery snapshot).
// Runs off the read path, from FetchAhead's completion callback.
func writeWarmupFromFetch(h *MkvHandle, off int64, data []byte) {
	if !h.usesWarmup || warmup.DiskWarmup == nil || h.hash == "" || len(data) == 0 {
		return
	}
	if off <= warmup.FileSize {
		warmup.DiskWarmup.WriteChunk(h.hash, h.fileID, data, off)
		return
	}
	if h.size <= warmup.TailWarmupSize || off < warmup.TailStart(h.size) {
		return
	}
	if val, ok := app.playbackRegistry.Load(h.path); ok {
		ps := val.(*PlaybackState)
		ps.mu.RLock()
		confirmed := !ps.ConfirmedAt.IsZero()
		ps.mu.RUnlock()
		if confirmed {
			return
		}
	}
	warmup.DiskWarmup.WriteTail(h.hash, h.fileID, data, off, h.size)
}

func (h *MkvHandle) readInner(fuseCtx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	now := time.Now()
	timing := &ReadTiming{StartTime: now}
	defer func() {
		timing.TotalTime = time.Since(timing.StartTime)
		globalProfilingStats.RecordRead(timing)
	}()

	if off >= h.size {
		return fuse.ReadResultData(nil), 0
	}

	// The engine refused to wake this torrent: it is not stored, and asking again
	// would not change that. Read maps this to EIO; the handle must be reopened.
	if h.wakeRefused.Load() {
		return nil, errnoWakeRefused
	}

	h.mu.Lock()
	// Late hash recovery: if Open() failed to resolve (metadata lag), retry now.
	if h.hash == "" && h.url != "" {
		if hash, fileID, err := resolveTargetFile(h.url, h.size, h.path); err == nil {
			h.hash = hash
			h.fileID = fileID
			logger.Info(fmt.Sprintf("[LateResolution] Recovered hash for %s: %s", filepath.Base(h.path), h.hash[:8]), "file", filepath.Base(h.path), "hash", h.hash[:8])
			go h.pumpOnce.Do(func() {
				h.startNativePump(h.hash, h.fileID)
			})
		}
	}

	idleTime := now.Sub(h.lastActivityTime)
	isFirstBlock := (off == 0) || (idleTime > time.Duration(gc().WarmStartIdleSeconds)*time.Second)
	h.lastActivityTime = now
	h.lastRead.Store(now.UnixNano())

	// V750: Update PlaybackState inference tracking
	if val, ok := app.playbackRegistry.Load(h.path); ok {
		if ps, ok := val.(*PlaybackState); ok {
			ps.mu.Lock()
			ps.ReadCount++
			ps.LastReadAt = now
			// Tail-zone reads (MKV Cues probes) and unconfirmed reads (Plex background scans)
			// must never count as real playback progress here.
			if shouldRecordResumeOff(ps.IsHealthy, h.isPrimaryHandle.Load(), off, h.size) {
				ps.LastSeekOff = off
				h.wasPrimary.Store(true)
			}
			ps.mu.Unlock()
		}
	}

	if now.Sub(h.lastGlobalUpdate) > 1*time.Minute {
		globalCleanupManager.UpdateActivity(h.path)
		h.lastGlobalUpdate = now
	}
	h.mu.Unlock()

	prevOff := atomic.LoadInt64(&h.lastOff)
	atomic.StoreInt64(&h.lastOff, off)

	h.noteTailHold(prevOff, off, len(dest))
	h.promoteIfStreaming(prevOff, off, len(dest))

	// A probe is a second handle on a file somebody is watching that is not that player's
	// successor: it reads what it asked for and leaves the shared pump, the primary and the
	// read-ahead cache alone.
	probe := h.shadowsActivePlayer()
	h.boostColdStart(off, probe)

	// Transition WARMUP→STREAMING on resume (first read >= warmup.FileSize), seek (jump > budget),
	// or plain sequential progression past the warmup zone (no seek ever happens during linear
	// playback from offset 0 - without this case, h.state stayed stuck in stateWarmup for the
	// entire rest of playback, paying GetAvailableRange's uncached filepath.Join/Clean cost on
	// every single read instead of the cheap steady-state path; found via CPU profiling a 4K HDR
	// stream where this uncached path work reached ~29% of total CPU).
	// Checked after SSD path above so initial reads within warmup zone are still served.
	if h.state.Load() == stateWarmup {
		isSeek := false
		naturalProgression := false
		if prevOff == -1 {
			if off >= warmup.FileSize {
				isSeek = true
			}
		} else if off != 0 {
			budget := int64(gc().ReadAheadBudget)
			if off > prevOff+budget || prevOff > off+budget {
				isSeek = true
			} else if off >= warmup.FileSize {
				naturalProgression = true
			}
		}
		if isSeek || naturalProgression {
			h.state.Store(stateStreaming)
			if naturalProgression {
				logger.Info(fmt.Sprintf("[Warmup] Sequential read past warmup zone (off=%dMB): %s→%s.", off/(1024*1024), stateName(stateWarmup), stateName(stateStreaming)))
			} else {
				logger.Info(fmt.Sprintf("[Warmup] Seek/Resume detected (off=%dMB): %s→%s.", off/(1024*1024), stateName(stateWarmup), stateName(stateStreaming)))
			}

			// WarmLanding on new handle: pre-fetch chunk at resume position while pump catches
			// up. Fires only for new handles (prevOff==-1) since V286b covers existing handles.
			// A scanner's first read past the head is a probe, not a resume (Dezoxy fork).
			if prevOff == -1 && h.hash != "" && !probe && !isScanRead(h) && !budgetBlocksFetch(h.path, off, int64(len(dest)), h.size) {
				warmChunk := raCache.ChunkSize(h.path)
				warmStart := (off / warmChunk) * warmChunk
				// Unified with predictive-prefetch key format to avoid double-fetching the same chunk.
				warmKey := fmt.Sprintf("%s:%d", h.path, warmStart)
				if _, loaded := app.inFlightPrefetches.LoadOrStore(warmKey, true); !loaded {
					// Non-blocking slot check before spawning: under a saturated semaphore (e.g.
					// a Plex scan burst touching hundreds of files), spawning first and waiting
					// inside the goroutine meant a goroutine was created only to sit blocked for
					// up to 300ms doing nothing. A `default` check costs microseconds and avoids
					// the spawn entirely when there's no room, without blocking this Read() call.
					select {
					case masterDataSemaphore <- struct{}{}:
						goOff, goKey, goHash, goFileID, goSize := warmStart, warmKey, h.hash, h.fileID, h.size
						safeGo(func() {
							defer app.inFlightPrefetches.Delete(goKey)
							defer func() { <-masterDataSemaphore }()
							fetchEnd := goOff + warmChunk - 1
							if fetchEnd >= goSize {
								fetchEnd = goSize - 1
							}
							if fetchEnd <= goOff {
								return
							}
							bufPtr := readBufferPool.Get().(*[]byte)
							defer readBufferPool.Put(bufPtr)
							limit := int64(len(*bufPtr))
							if fetchEnd-goOff+1 < limit {
								limit = fetchEnd - goOff + 1
							}
							n, err := nativeBridge.FetchBlock(goHash, goFileID, goOff, (*bufPtr)[:limit])
							if err == nil && n > 0 {
								raCache.Put(h.path, goOff, goOff+int64(n)-1, (*bufPtr)[:n])
								noteFillAttributed(h.path, goOff, int64(n), goSize)
								logger.Info(fmt.Sprintf("[WarmLanding] Pre-fetched chunk at %dMB for new handle", goOff/(1024*1024)))
							}
						})
					default:
						app.inFlightPrefetches.Delete(warmKey)
					}
				}
			}
		}
	}

	// Interrupt pump on genuine seeks; skip reads served from the SSD tail warmup (the
	// pump must stay alive for the rest of the file, and the tail read is already served).
	// V286b: interruptPending prevents cascade — when multiple handles share a pump,
	// only the first to detect the seek fires Interrupt(); others skip until the pump
	// restarts and resets the flag (preventing the thrash loop seen in the logs).
	budget := int64(gc().ReadAheadBudget)
	h.mu.Lock()
	seekReader := h.nativeReader // capture: a concurrent Release nils the field
	h.mu.Unlock()
	if seekReader != nil && shouldInterruptPump(prevOff, off, budget, shouldServeTailFromSSD(off, h.size, warmup.TailWarmupSize) || h.tailHold.Load() > 0) {
		// Promote to primary on a genuine seek once playback is confirmed/inferred - mirrors the
		// refCount 0->1 "primary reconnect" promotion at attach time (main.go:1030): a secondary
		// (attached) handle doing a real seek during confirmed playback IS the player, and needs
		// isPrimaryHandle so the pump loop's playerOff sync and Release's ps.playerOff
		// persistence (see nativePump) pick it up instead of only the original primary handle,
		// which may be an idle stale probe by now.
		if !h.isPrimaryHandle.Load() && !probe {
			if val, ok := app.playbackRegistry.Load(h.path); ok {
				if pbs, ok := val.(*PlaybackState); ok && (pbs.GetStatus() || pbs.IsInferredPlayback()) {
					becomePrimary(h)
				}
			}
		}
		var ps *NativePumpState
		if val, ok := app.activePumps.Load(h.path); ok {
			ps = val.(*NativePumpState)
		}
		// The pump follows the primary: a probe's seek has nothing of it to interrupt.
		if !probe && (ps == nil || !ps.interruptPending.Swap(true)) {
			seekReader.Interrupt()
			torrstor.ResetShield()
			h.state.Store(stateStreaming)
			logger.Info(fmt.Sprintf("[V286b] Interrupt pump for seek+shield reset: %dMB → %dMB (%s→%s)", prevOff/(1024*1024), off/(1024*1024), stateName(stateWarmup), stateName(stateStreaming)))

			// Warm landing zone: pre-fetch the first chunk at the seek target while the
			// pump sleeps its 200ms retry. Starts anacrolix piece download ~200ms earlier,
			// reducing FetchBlock blocking time in Read() and preventing smbd D-state.
			if h.hash != "" && !budgetBlocksFetch(h.path, off, int64(len(dest)), h.size) {
				warmChunk := raCache.ChunkSize(h.path)
				warmStart := (off / warmChunk) * warmChunk
				// Unified with predictive-prefetch key format to avoid double-fetching the same chunk.
				warmKey := fmt.Sprintf("%s:%d", h.path, warmStart)
				if _, loaded := app.inFlightPrefetches.LoadOrStore(warmKey, true); !loaded {
					// See the matching comment on the other WarmLanding site above: non-blocking
					// slot check before spawning avoids creating a goroutine that just blocks.
					select {
					case masterDataSemaphore <- struct{}{}:
						goOff, goKey, goHash, goFileID, goSize := warmStart, warmKey, h.hash, h.fileID, h.size
						safeGo(func() {
							defer app.inFlightPrefetches.Delete(goKey)
							defer func() { <-masterDataSemaphore }()
							fetchEnd := goOff + warmChunk - 1
							if fetchEnd >= goSize {
								fetchEnd = goSize - 1
							}
							if fetchEnd <= goOff {
								return
							}
							bufPtr := readBufferPool.Get().(*[]byte)
							defer readBufferPool.Put(bufPtr)
							limit := int64(len(*bufPtr))
							if fetchEnd-goOff+1 < limit {
								limit = fetchEnd - goOff + 1
							}
							n, err := nativeBridge.FetchBlock(goHash, goFileID, goOff, (*bufPtr)[:limit])
							if err == nil && n > 0 {
								raCache.Put(h.path, goOff, goOff+int64(n)-1, (*bufPtr)[:n])
								noteFillAttributed(h.path, goOff, int64(n), goSize)
								logger.Info(fmt.Sprintf("[WarmLanding] Pre-fetched chunk at %dMB for seek target", goOff/(1024*1024)))
							}
						})
					default:
						app.inFlightPrefetches.Delete(warmKey)
					}
				}
			}
		}
	}

	// Serve warmup zone from SSD (up to 80MB with boundary chunk); stateWarmup gate skips SSD on resume/seek.
	// A scan read takes it in any state: a probe that went to the tail and back to the head
	// is still a probe (Dezoxy fork).
	if h.usesWarmup && warmup.DiskWarmup != nil && h.hash != "" && (h.state.Load() == stateWarmup || isScanRead(h)) {
		warmupCoverage := warmup.DiskWarmup.GetAvailableRange(h.hash, h.fileID)
		if off < warmupCoverage {
			n, _ := warmup.DiskWarmup.ReadAt(h.hash, h.fileID, dest, off)
			if n > 0 {
				timing.UsedCache = true
				timing.BytesRead = n
				if off == 0 {
					logger.Info(fmt.Sprintf("[DiskWarmup] HIT %s off=0 (%dKB)", filepath.Base(h.path), n/1024), "file", filepath.Base(h.path))
				}
				atomic.StoreInt64(&h.lastOff, off)

				h.mu.Lock()
				h.lastLen = n
				h.lastTime = now
				h.mu.Unlock()
				ttffRead(h.path, srcWarmupHead, time.Since(now), n, off)
				return fuse.ReadResultData(dest[:n]), 0
			}
		}
	}

	// Serve tail from SSD whenever the read lands in the tail warmup coverage, regardless
	// of handle state: the WARMUP→STREAMING seek/resume transition above runs before any
	// state-gated tail check could - a resume in the last 16MB was served by network
	// FetchBlock instead of the complete SSD tail file (~16s TTFF on a 36.6GB remux,
	// 2026-08-06). ReadTail is range-safe: returns (0,nil) when the tail file's coverage
	// doesn't include the offset.
	if h.usesWarmup && warmup.DiskWarmup != nil && shouldServeTailFromSSD(off, h.size, warmup.TailWarmupSize) {
		n, _ := warmup.DiskWarmup.ReadTail(h.hash, h.fileID, dest, off, h.size)
		if n > 0 {
			timing.UsedCache = true
			timing.BytesRead = n
			h.mu.Lock()
			h.lastLen, h.lastTime = n, now
			h.mu.Unlock()
			ttffRead(h.path, srcWarmupTail, time.Since(now), n, off)
			return fuse.ReadResultData(dest[:n]), 0
		}

		// On SSD tail miss, use stateless FetchBlock to preserve head pump.
		nFetch, err := nativeBridge.FetchBlock(h.hash, h.fileID, off, dest)
		if err == nil && nFetch > 0 {
			if h.usesWarmup && warmup.DiskWarmup != nil {
				warmup.DiskWarmup.WriteTail(h.hash, h.fileID, dest[:nFetch], off, h.size)
				// The tail pieces are in memory now: complete the window so the next scan of
				// this file never reaches the swarm (index cache, Dezoxy fork).
				if gc().WarmupDir != "" && isScanRead(h) {
					warmup.DiskWarmup.EnsureTail(h.hash, h.fileID, h.size)
				}
			}

			timing.UsedCache = false
			timing.BytesRead = nFetch
			h.mu.Lock()
			h.lastLen, h.lastTime = nFetch, now
			h.mu.Unlock()
			ttffRead(h.path, srcFetchBlock, time.Since(now), nFetch, off)
			return fuse.ReadResultData(dest[:nFetch]), 0
		}
	}
	end := off + int64(len(dest)) - 1

	cacheStart := time.Now()
	if n := raCache.CopyTo(h.path, off, end, dest); n > 0 {
		timing.CacheHitTime = time.Since(cacheStart)
		timing.UsedCache = true
		timing.BytesRead = n

		atomic.StoreInt64(&h.lastOff, off)

		// Predictive prefetch: fetch next chunk if pump is absent or near boundary.
		chunkSize := raCache.ChunkSize(h.path)
		nextChunkStart := (off/chunkSize + 1) * chunkSize

		if !probe && !isScanRead(h) && (!h.hasSlot.Load() || (nextChunkStart-off < chunkSize/4)) && !raCache.Exists(h.path, nextChunkStart) &&
			!budgetBlocksFetch(h.path, nextChunkStart, chunkSize, h.size) {
			prefetchKey := fmt.Sprintf("%s:%d", h.path, nextChunkStart)
			if _, loaded := app.inFlightPrefetches.LoadOrStore(prefetchKey, true); !loaded {
				goStart, goKey, goHash, goFileID := nextChunkStart, prefetchKey, h.hash, h.fileID
				fetchEnd := goStart + chunkSize - 1
				if fetchEnd >= h.size {
					fetchEnd = h.size - 1
				}
				// Non-blocking slot check before spawning: avoids creating a goroutine that just
				// blocks up to 500ms waiting on a saturated semaphore during a Plex scan burst
				// (same reasoning as the WarmLanding sites above).
				if fetchEnd <= goStart {
					app.inFlightPrefetches.Delete(prefetchKey)
				} else {
					select {
					case masterDataSemaphore <- struct{}{}:
						safeGo(func() {
							defer app.inFlightPrefetches.Delete(goKey)
							defer func() { <-masterDataSemaphore }()

							if goHash != "" {
								bufPtr := readBufferPool.Get().(*[]byte)
								defer readBufferPool.Put(bufPtr)

								limit := int64(len(*bufPtr))
								if fetchEnd-goStart+1 < limit {
									limit = fetchEnd - goStart + 1
								}

								n, err := nativeBridge.FetchBlock(goHash, goFileID, goStart, (*bufPtr)[:limit])
								if err == nil && n > 0 {
									raCache.Put(h.path, goStart, goStart+int64(n)-1, (*bufPtr)[:n])
									noteFillAttributed(h.path, goStart, int64(n), h.size)
								}
							}
							// HTTP Fallback REMOVED
						})
					default:
						app.inFlightPrefetches.Delete(prefetchKey)
					}
				}
			}
		}

		h.mu.Lock()
		h.lastLen = n
		h.lastTime = now
		h.mu.Unlock()

		ttffRead(h.path, srcRACacheHit, time.Since(cacheStart), n, off)
		return fuse.ReadResultData(dest[:n]), 0
	}

	// Reader budget: a miss past the margin of an unconfirmed file starts no fetch, pump, slot
	// wait or wake. The gate holds the read for a late webhook first; Read maps the refusal to
	// EIO. Cache hits, the SSD windows and reads wholly inside them never get here refused.
	if errno := h.budgetGate(fuseCtx, off, len(dest)); errno != 0 {
		return nil, errno
	}

	target := int(end - off + 1)
	lastOff := atomic.LoadInt64(&h.lastOff)
	isSeq := (off == lastOff+int64(h.lastLen)) || (lastOff >= 0 && abs(off-(lastOff+int64(h.lastLen))) <= gc().SequentialTolerance)
	isStreaming := (len(dest) >= int(gc().StreamingThreshold)) || isSeq
	timing.IsStreaming = isStreaming

	fetchEnd := end
	var fetchSize int64 = int64(target)
	if isStreaming {
		raSize := int64(gc().ReadAheadBase)

		if isFirstBlock {
			raSize = int64(gc().ReadAheadInitial)
		}
		if isScanRead(h) {
			raSize = scanFetchSize(off, int64(target), warmup.FileSize, raSize)
		}

		fetchEnd = off + raSize - 1
		if fetchEnd >= h.size {
			fetchEnd = h.size - 1
		}
		// Opt-in (unconfirmed_readahead_clamp): no read-ahead past the head window edge
		// for a file with no confirmed playback.
		fetchEnd = clampReadahead(h.path, off, end, fetchEnd, h.size)
		fetchSize = fetchEnd - off + 1
	}
	if probe {
		fetchSize = int64(target) // no read-ahead window: it would fill the shared cache
	}
	h.mu.Lock()
	h.lastLen, h.lastTime = len(dest), now
	h.mu.Unlock()
	atomic.StoreInt64(&h.lastOff, off)

	if h.hasSlot.Load() {
		h.dropOrphanedPumpRef()
	}
	if !h.hasSlot.Load() {
		pumpCreationMu.Lock()

		// pumpCreationMu serializes attaches on this path: a concurrent read on this same
		// handle may have attached while we waited, and a double attach would pin the pump in
		// refCount. The hasSlot write itself is guarded by h.mu inside attachToExistingPump.
		if h.hasSlot.Load() {
			pumpCreationMu.Unlock()
			goto pumpSlotResolved
		}
		// Attach to existing pump if one is already running for this path.
		if val, ok := app.activePumps.Load(h.path); ok {
			ps := val.(*NativePumpState)
			if h.attachToExistingPump(ps) {
				logger.Info(fmt.Sprintf("[V258] Handle ATTACHED to existing active pump (Refs: %d): %s", atomic.LoadInt32(&ps.refCount), filepath.Base(h.path)), "file", filepath.Base(h.path))
			}
		}
		// Unlock early if attached or not needed
		if h.hasSlot.Load() {
			pumpCreationMu.Unlock()
		} else {
			// On-the-fly pump upgrade for confirmed playback with available slot.
			// A read held in the tail zone (an index probe) leaves the pump to the player's own reads.
			if isStreaming && h.hash != "" && !probe && h.tailHold.Load() == 0 {
				if val, ok := app.playbackRegistry.Load(h.path); ok {
					if ps, ok := val.(*PlaybackState); ok && (ps.GetStatus() || ps.IsInferredPlayback()) {
						select {
						case masterDataSemaphore <- struct{}{}:
							upReader := nativeBridge.NewStreamReader(h.hash, h.fileID, h.size)
							pumpCtx, pumpCancel := context.WithCancel(context.Background())
							h.mu.Lock()
							h.hasSlot.Store(true)
							h.refDropped.Store(false)
							h.nativeReader = upReader
							h.pumpCancel = pumpCancel
							h.mu.Unlock()

							sharedState := &NativePumpState{
								cancel:   pumpCancel,
								reader:   upReader,
								path:     h.path,
								refCount: 1,
							}
							app.activePumps.Store(h.path, sharedState)
							h.mu.Lock()
							h.pumpState = sharedState
							h.mu.Unlock()
							h.countOpen()
							if h.released.Load() { // as in attachRef; and no pump is left for a handle that is gone
								h.uncountOpen()
								pumpCancel()
							}

							logger.Info(fmt.Sprintf("[Pump] Upgraded on-the-fly for confirmed playback: %s", filepath.Base(h.path)), "file", filepath.Base(h.path))

							hHash := metainfo.NewHashFromHex(h.hash)
							if t := web.BTS.GetTorrent(hHash); t != nil {
								t.SetAggressiveMode(true, GetEffectiveConcurrencyLimit())
								logger.Info(fmt.Sprintf("[Pump] Aggressive mode enabled on-the-fly for: %s", h.hash[:8]), "hash", h.hash[:8])
							}

							upgradedState := sharedState
							safeGo(func() {
								h.nativePump(pumpCtx, upReader, off, upgradedState)
							})
						default:
							// Reserve full, stay in burst mode for now
						}
					}
				}
			}
			pumpCreationMu.Unlock() // Final unlock
		}
	}

pumpSlotResolved:
	// If still no slot (scan or reserve full), acquire a temporary slot for this read
	if !h.hasSlot.Load() {
		select {
		case masterDataSemaphore <- struct{}{}:
			defer func() { <-masterDataSemaphore }()
		case <-fuseCtx.Done():
			return nil, syscall.EINTR
		case <-time.After(30 * time.Second):
			logger.Info(fmt.Sprintf("[MasterSemaphore] Timeout waiting for slot: %s", filepath.Base(h.path)), "file", filepath.Base(h.path))
			return nil, syscall.ETIMEDOUT
		}
	}

	// Rate limiting for non-streaming (metadata) requests only; streaming bypasses to preserve playback priority.
	if !isStreaming {
		rateLimitCtx, rateLimitCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer rateLimitCancel()
		if err := globalRateLimiter.Acquire(rateLimitCtx); err != nil {
			logger.Info(fmt.Sprintf("Rate limit timeout: %v", err), "err", err)
			return nil, vfs.ToErrno(err)
		}
	}

	// Queueing (semaphore/rate-limiter) is not cache service time — re-stamp.
	cacheStart = time.Now()
	if n := raCache.CopyTo(h.path, off, end, dest); n > 0 {
		atomic.StoreInt64(&h.lastOff, off)
		h.mu.Lock()
		h.lastLen = n
		h.lastTime = now
		h.mu.Unlock()
		ttffRead(h.path, srcRACacheHit, time.Since(cacheStart), n, off)
		return fuse.ReadResultData(dest[:n]), 0
	}

	// FALLBACK: Data Fetch with V265 Retry
	// If cache miss, use FetchAhead. Retry up to 3 times if torrent not ready (async Wake).
	var n int

	// releaseFlight is set when this read leads the singleflight below. The read-ahead window
	// keeps filling after this function returns, so the flight is released from the fill's
	// completion callback (once the cache is populated) instead of on return - a follower woken
	// earlier would find nothing cached and start a duplicate fetch of the same chunk.
	releaseFlight := func() {}
	fetchHandedOff := false

	// Dedup concurrent misses in the same chunk: leader fetches, followers wait and copy.
	// A probe does not lead the flight: its fetch fills no cache, so a player miss in the same
	// chunk waiting behind it would wait for nothing. It fetches on its own.
	if h.hash != "" && !probe {
		flightChunk := raCache.ChunkSize(h.path)
		flightKey := fmt.Sprintf("%s:%d", h.path, (off/flightChunk)*flightChunk)
		newFlight := &fetchFlight{done: make(chan struct{})}
		if val, loaded := app.inFlightFetches.LoadOrStore(flightKey, newFlight); loaded {
			fl := val.(*fetchFlight)
			copyStart := time.Now()
			// The leader re-caches its window as it fills, so poll instead of waiting for the
			// whole window: a follower one FUSE block behind the leader gets its bytes after one
			// fill step, not after the remaining 15MB.
			leaderDone := false
			for {
				if nCopy := raCache.CopyTo(h.path, off, end, dest); nCopy > 0 {
					fetchFlightDedupCount.Add(1)
					timing.UsedCache = true
					timing.BytesRead = nCopy
					atomic.StoreInt64(&h.lastOff, off)
					h.mu.Lock()
					h.lastLen = nCopy
					h.lastTime = now
					h.mu.Unlock()
					ttffRead(h.path, srcRACacheHit, time.Since(copyStart), nCopy, off)
					return fuse.ReadResultData(dest[:nCopy]), 0
				}
				if leaderDone {
					break // leader finished without covering our range
				}
				select {
				case <-fl.done:
					leaderDone = true // one more CopyTo, then fall through
				case <-fuseCtx.Done():
					return nil, syscall.EINTR
				case <-time.After(20 * time.Millisecond):
				}
			}
			// Leader failed or missed our range — fetch directly, don't re-register.
		} else {
			var once sync.Once
			releaseFlight = func() {
				once.Do(func() {
					app.inFlightFetches.Delete(flightKey)
					close(newFlight.done)
				})
			}
			defer func() {
				if !fetchHandedOff {
					releaseFlight()
				}
			}()
		}
	}

	if h.hash != "" {
		if woke, err := rewake(fuseCtx, h.hash, h.magnet, h.fileID, torrentLive, h.wake); err != nil {
			if errors.Is(err, native.ErrNotStored) {
				// Not the swarm failing: no fetch follows, no failed read is marked and
				// no dead-swarm verdict can come of it. Latched so a second read makes
				// no engine call.
				if h.wakeRefused.CompareAndSwap(false, true) {
					noteWakeRefused(h.path, h.hash, "read")
				}
				return nil, errnoWakeRefused
			}
			if errors.Is(err, native.ErrEngineNotReady) {
				// Transient: the engine is not up yet. Nothing is recorded or latched, so
				// the next read asks again; the caller sees the same EIO.
				noteEngineNotReady(h.path, "read")
				return nil, errnoWakeRefused
			}
			logger.Info(fmt.Sprintf("[VFS] Re-activation failed for %s: %v", filepath.Base(h.path), err), "file", filepath.Base(h.path), "err", err)
			if !torrentLive(h.hash) {
				// The wake is stored-only now, so a failed one (engine busy, not
				// connected) says nothing about the swarm: no fetches, no failed-read
				// mark, no dead-swarm verdict.
				return nil, syscall.EIO
			}
		} else if woke {
			countReopen(h.path, h.hash)
			logger.Info(fmt.Sprintf("[VFS] Re-activated %s after the engine closed it as idle", filepath.Base(h.path)), "file", filepath.Base(h.path))
		}
		// V753: the very first read of a session can miss raCache/warmup even when
		// headReady was already true at Open() (partial coverage from a prior open),
		// which skips forceTorrentWarmupActive there (main.go ~916, gated on
		// !headReady). That leaves hedgeWatchdog not running yet - playback-pressure
		// hasn't engaged either, since the pump has barely started - so this exact
		// fetch has zero hedge protection. Force it here instead, bounded to the
		// warmup head window: forcing it for a post-idle resume deep in the file
		// (isFirstBlock is also true after WarmStartIdleSeconds) would set
		// warmupActive with no way back to false, since processWrite's off>FileSize
		// early return (warmup.go) means COMPLETED - the only call site that clears
		// it - never fires for writes past the head window.
		if h.usesWarmup && isFirstBlock && off <= warmup.FileSize {
			forceTorrentWarmupActive(h.hash, h.fileID)
		}
		fetchStart := time.Now()

		for attempt := 0; attempt < 3; attempt++ {
			bufPtr := readBufferPool.Get().(*[]byte)

			limit := fetchSize
			if limit > int64(len(*bufPtr)) {
				limit = int64(len(*bufPtr))
			}
			buf := (*bufPtr)[:limit]

			// The window keeps filling after this read returns. Re-cache on every step so the
			// reads queued behind this one find their bytes as they land instead of waiting for
			// the whole window; the buffer goes back to the pool only once the fill is done.
			nFetch, err := nativeBridge.FetchAhead(h.hash, h.fileID, off, buf, dest[:target],
				func(total int, done bool, _ error) {
					if total > 0 && !probe {
						raCache.Put(h.path, off, off+int64(total)-1, buf[:total])
					}
					if !done {
						return
					}
					if total > 0 {
						writeWarmupFromFetch(h, off, buf[:total])
					}
					releaseFlight()
					readBufferPool.Put(bufPtr)
				})
			if err == nil && nFetch > 0 {
				n = nFetch
				fetchHandedOff = true
				noteReadaheadOvershoot(h.path, off, int64(target), fetchSize, h.size)
				timing.HTTPFetchTime = time.Since(fetchStart)
				goto DATA_READY
			}
			if attempt < 2 {
				select {
				case <-fuseCtx.Done():
					return nil, syscall.EINTR
				case <-time.After(500 * time.Millisecond):
				}
			}
		}
	}

	// Terminal once the bounded retries are spent. EAGAIN reads as "try again" to
	// a scanner, which then blocks or loops on bytes that are never coming.
	ttffReadFailed(h.path)
	return nil, syscall.EIO

DATA_READY:

	timing.BytesRead = target

	if n > 0 {
		// The bytes are already in dest and the read-ahead window behind them is still
		// filling; raCache.Put and the SSD warmup writes happen in FetchAhead's callback.
		ttffRead(h.path, srcFetchBlock, timing.HTTPFetchTime, n, off)

		// Update sequential detection state
		atomic.StoreInt64(&h.lastOff, off)
		h.mu.Lock()
		h.lastLen = target
		h.mu.Unlock()

		globalCleanupManager.UpdateOffset(h.path, off, target)

		// Prefetch next chunk if in last 25% of current chunk and pump is absent or lagging.
		chunkSize := raCache.ChunkSize(h.path)
		currentChunkIndex := off / chunkSize
		nextChunkStart := (currentChunkIndex + 1) * chunkSize
		distanceToNext := nextChunkStart - off

		maxCached := raCache.MaxCachedOffset(h.path)
		isLagging := maxCached < nextChunkStart

		if isStreaming && !probe && (!h.hasSlot.Load() || isLagging) && !budgetBlocksFetch(h.path, nextChunkStart, chunkSize, h.size) {
			if distanceToNext < chunkSize/4 {
				prefetchKey := fmt.Sprintf("%s:%d", h.path, nextChunkStart)
				if _, loaded := app.inFlightPrefetches.LoadOrStore(prefetchKey, true); !loaded {
					goStart, goSize, goKey, goHash, goFileID := nextChunkStart, chunkSize, prefetchKey, h.hash, h.fileID
					safeGo(func() {
						defer app.inFlightPrefetches.Delete(goKey)

						// Check if already in cache to avoid useless work
						if raCache.Exists(h.path, goStart) {
							return // Already cached
						}

						ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
						defer cancel()
						if err := globalRateLimiter.Acquire(ctx); err != nil {
							return
						}

						fetchEnd := goStart + goSize - 1
						if fetchEnd >= h.size {
							fetchEnd = h.size - 1
						}
						if fetchEnd <= goStart {
							return
						}

						select {
						case masterDataSemaphore <- struct{}{}:
							defer func() { <-masterDataSemaphore }()
						default:
							return // Skip if pool is saturated
						}

						// Zero-Network Native Prefetch (V227 Phase 7)
						if goHash != "" {
							bufPtr := readBufferPool.Get().(*[]byte)
							defer readBufferPool.Put(bufPtr)

							limit := int64(len(*bufPtr))
							if fetchEnd-goStart+1 < limit {
								limit = fetchEnd - goStart + 1
							}

							n, err := nativeBridge.FetchBlock(goHash, goFileID, goStart, (*bufPtr)[:limit])
							if err == nil && n > 0 {
								raCache.Put(h.path, goStart, goStart+int64(n)-1, (*bufPtr)[:n])
								noteFillAttributed(h.path, goStart, int64(n), h.size)
							}
							return
						}

						// HTTP Fallback REMOVED
					})
				}
			}
		}

		return fuse.ReadResultData(dest[:n]), 0
	}

	return fuse.ReadResultData(dest[:0]), 0
}

// torrentLive reports whether the engine holds the torrent in RAM; a closed one is only
// a database record without an engine torrent.
var torrentLive = func(hash string) bool {
	return torr.PeekTorrent(hash).UnderlyingTorrent() != nil
}

// rewake re-activates a handle's torrent that the engine closed while the handle stayed
// open, and reports whether it did. The engine closes a torrent TorrentDisconnectTimeout
// after its last engine reader goes, and a handle without a pump holds none between
// reads: Plex's transcoder buffers a minute ahead and then pauses, and its next read
// found no torrent and failed with EIO, which killed the transcode (Dezoxy fork).
func rewake(ctx context.Context, hash, magnet string, fileID int, live func(string) bool,
	wake func(context.Context, string, int) error) (bool, error) {
	if hash == "" || magnet == "" || wake == nil || live(hash) {
		return false, nil
	}
	if err := wake(ctx, magnet, fileID); err != nil {
		return false, err
	}
	return true, nil
}

// ownsPumpState reports whether a releasing handle still owns the pump registered
// for its path. activePumps is keyed by path and the state is replaced wholesale,
// so a handle that outlived its pump would otherwise decrement a count it never
// incremented, driving refCount negative and firing the "last handle closed"
// teardown while a live handle is still reading.
func ownsPumpState(handleState, registered *NativePumpState) bool {
	return handleState != nil && handleState == registered
}

func (h *MkvHandle) Release(fuseCtx context.Context) syscall.Errno {
	// Once per handle: a repeat would drop the shared pump's reference a second time (a
	// sibling's), move the resume position to wherever a late read left lastOff, and free the
	// open-tracker count twice.
	if !h.released.CompareAndSwap(false, true) {
		return 0
	}
	ttffReleaseClose(h.path)
	logger.Info(fmt.Sprintf("=== RELEASE VIRTUAL === path=%s", h.path), "path", h.path)
	return h.releaseResources()
}

// sweepOrphan is the orphan sweep's cleanup for a handle idle for an hour. It frees what
// Release frees but leaves Release's latch alone: the handle can still read (a pump that
// exited lets it re-attach and take a new reference) and the kernel's own Release must then
// drop that reference, once.
func (h *MkvHandle) sweepOrphan() {
	ttffReleaseClose(h.path)
	_ = h.releaseResources()
}

// releaseResources is Release's work. The pump reference and the open count are dropped once
// per attach (refDropped); the rest may run twice (the sweep, then the kernel's Release).
func (h *MkvHandle) releaseResources() syscall.Errno {
	// Delete from activeHandles here, not at the end: the early returns below (probe
	// without slot, replaced pump) would otherwise leak this entry, and both the live
	// handle checks (anyLiveHandleFor*) and cleanup's activePaths trust the map.
	app.activeHandles.Delete(h)

	// A handle that counted itself open when it took a pump reference and has lost the pump
	// since (a creator whose pump ended, an attached handle whose pump is gone) gives the
	// count back here: the block below only runs while a pump is registered and the handle
	// still holds its slot.
	if _, live := app.activePumps.Load(h.path); !h.hasSlot.Load() || !live {
		h.uncountOpen()
	}

	if val, ok := app.activePumps.Load(h.path); ok {
		ps := val.(*NativePumpState)
		// Only primary handles persist position; secondary probes have arbitrary offsets.
		if h.isPrimaryHandle.Load() {
			if pos := h.followOff(); pos > 0 {
				atomic.StoreInt64(&ps.playerOff, pos)
			}
		}
		// Only decrement if this handle acquired a slot; probe/header reads must not decrement.
		if !h.hasSlot.Load() {
			return 0
		}
		if !h.refDropped.CompareAndSwap(false, true) {
			return 0 // the sweep dropped it, and nothing attached since
		}
		h.uncountOpen()
		h.mu.Lock()
		owned := h.pumpState
		h.mu.Unlock()
		if !ownsPumpState(owned, ps) {
			logger.Info(fmt.Sprintf("[Pump] Stale handle for %s: pump was replaced, refCount left alone", filepath.Base(h.path)), "file", filepath.Base(h.path))
			return 0
		}
		newRefs := atomic.AddInt32(&ps.refCount, -1)
		logger.Info(fmt.Sprintf("[Pump] Release handle for %s (Remaining Refs: %d)", filepath.Base(h.path), newRefs), "file", filepath.Base(h.path), "new_refs", newRefs)

		if newRefs <= 0 {
			// V306: If playback is healthy (webhook media.play confirmed), skip grace period.
			// The pump stays alive until media.stop webhook sets IsHealthy=false and kills it.
			// This survives long pauses, buffering gaps, and Apple TV re-reads without
			// killing the pump and causing freeze on resume.
			if pbVal, ok := app.playbackRegistry.Load(h.path); ok {
				if pbState := pbVal.(*PlaybackState); pbState.IsHealthy {
					logger.Info(fmt.Sprintf("[V306] Healthy playback — pump stays alive (no grace period) for %s", filepath.Base(h.path)), "file", filepath.Base(h.path))
					// Stop any pending grace timer from a previous release cycle
					if oldTimer, ok := app.pumpTimers.LoadAndDelete(h.path); ok {
						oldTimer.(*time.Timer).Stop()
					}
					goto skipGrace
				}
			}

			// Grace period: 30s for unconfirmed probes/scans.
			graceDuration := 30 * time.Second
			if pbVal, ok := app.playbackRegistry.Load(h.path); ok {
				if pbState := pbVal.(*PlaybackState); !pbState.ConfirmedAt.IsZero() {
					graceDuration = 90 * time.Second
					if pbState.IsInferredPlayback() {
						graceDuration = 5 * time.Minute // V750
						logger.Info(fmt.Sprintf("[V750] Inferred playback — extended grace (5m) for %s", filepath.Base(h.path)), "file", filepath.Base(h.path))
					}
				} else if pbState.IsInferredPlayback() {
					graceDuration = 5 * time.Minute // V750
					logger.Info(fmt.Sprintf("[V750] Inferred playback — extended grace (5m) for %s", filepath.Base(h.path)), "file", filepath.Base(h.path))
				}
			}
			if oldTimer, ok := app.pumpTimers.LoadAndDelete(h.path); ok {
				oldTimer.(*time.Timer).Stop()
			}
			var t *time.Timer
			t = time.AfterFunc(graceDuration, func() {
				app.pumpTimers.CompareAndDelete(h.path, t)
				if val, ok := app.activePumps.Load(h.path); ok {
					psNow := val.(*NativePumpState)
					if atomic.LoadInt32(&psNow.refCount) <= 0 {
						if psNow.cancel != nil {
							psNow.cancel()
						}
						app.activePumps.Delete(h.path)
						logger.Info(fmt.Sprintf("[V420] Shared Pump Terminated (Grace Period Expired): %s", filepath.Base(h.path)), "file", filepath.Base(h.path))
					}
				}
			})
			app.pumpTimers.Store(h.path, t)
			logger.Info(fmt.Sprintf("[V420] Last handle closed: Shared Pump entering %s grace period for %s", graceDuration, filepath.Base(h.path)), "grace_duration", graceDuration, "file", filepath.Base(h.path))
		}

	skipGrace:
	}

	// Nil local reference only; pump goroutine owns the reader lifecycle via captured copy.
	h.mu.Lock()
	h.nativeReader = nil
	h.mu.Unlock()

	// Fast-drop (5s) for scanner probes never confirmed by webhook; 30s otherwise.
	retentionDelay := 30 * time.Second
	lastOffset := atomic.LoadInt64(&h.lastOff)
	isProbeOnly := lastOffset < 2*1024*1024 // < 2MB = probe/scanner, not real playback
	if h.hasWarmup && isProbeOnly {
		if val, ok := app.playbackRegistry.Load(h.path); ok {
			state := val.(*PlaybackState)
			state.mu.RLock()
			stopped := state.IsStopped
			everConfirmed := !state.ConfirmedAt.IsZero()
			state.mu.RUnlock()
			// Fast-drop only if: explicitly stopped OR never confirmed by any webhook
			if stopped || !everConfirmed {
				retentionDelay = 5 * time.Second
			}
		}
	}

	if oldTimer, ok := app.priorityTimers.LoadAndDelete(h.path); ok {
		oldTimer.(*time.Timer).Stop()
	}
	var t *time.Timer
	t = time.AfterFunc(retentionDelay, func() {
		app.priorityTimers.CompareAndDelete(h.path, t)

		// V750: Don't remove priority if pump is still active for this path
		if _, pumpOk := app.activePumps.Load(h.path); pumpOk {
			logger.Info(fmt.Sprintf("[V750] Priority retained — pump still active for %s", filepath.Base(h.path)), "file", filepath.Base(h.path))
			return
		}

		// V306: Don't remove priority if playback is confirmed healthy — even if
		// pump goroutine exited unexpectedly, the torrent must stay alive for the
		// next Read() to create a new pump without a gap.
		if val, ok := app.playbackRegistry.Load(h.path); ok {
			if pbState := val.(*PlaybackState); pbState.GetStatus() {
				logger.Info(fmt.Sprintf("[V306] Priority retained — healthy playback confirmed for %s", filepath.Base(h.path)), "file", filepath.Base(h.path))
				return
			}
		}

		// O(1): controlla se il path ha ancora handle aperti prima di disabilitare priority.
		if globalOpenTracker.IsPathOpen(h.path) {
			return
		}

		if val, ok := app.playbackRegistry.Load(h.path); ok {
			state := val.(*PlaybackState)
			if state.Hash != "" {
				hHash := metainfo.NewHashFromHex(state.Hash)

				// O(1): controlla se altri file dello stesso torrent sono ancora aperti.
				if globalOpenTracker.IsHashOpen(state.Hash) {
					// Another file from the same torrent is still open, keep priority.
					return
				}

				if t := web.BTS.GetTorrent(hHash); t != nil {
					t.IsPriority.Store(false)
					t.SetAggressiveMode(false, 0)

					// Fast-drop scanner handles never confirmed by webhook.
					scannerDrop := false
					if h.hasWarmup && isProbeOnly {
						state.mu.RLock()
						everConfirmed := !state.ConfirmedAt.IsZero()
						state.mu.RUnlock()
						if !state.IsHealthy && !everConfirmed {
							scannerDrop = true
						}
					}

					if scannerDrop {
						t.AddExpiredTime(5 * time.Second)
						logger.Info(fmt.Sprintf("[V272] Scanner fast-drop for Hash %s (5s retention)", state.Hash), "hash", state.Hash)
					} else {
						t.AddExpiredTime(30 * time.Second)
						logger.Info(fmt.Sprintf("[NativeBridge] Priority disabled for Hash %s (All handles closed)", state.Hash), "hash", state.Hash)
					}
				}
			}
		}
		// Registry entry kept (without priority) for fast webhook-triggered resume.
		// Cleanup handled by GlobalCleanupManager (15 min timeout).
		// playbackRegistry.Delete(h.path)
	})
	app.priorityTimers.Store(h.path, t)

	return 0
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
