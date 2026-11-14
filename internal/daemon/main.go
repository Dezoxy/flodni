package daemon

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
	"flodni/internal/apiauth"
	"flodni/internal/atomicfile"
	"flodni/internal/cache"
	"flodni/internal/catalog"
	"flodni/internal/catalog/mediaserver"
	"flodni/internal/config"
	server "flodni/internal/gostorm"
	"flodni/internal/gostorm/native"
	"flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr"
	torrstor "flodni/internal/gostorm/torr/storage/torrstor"
	"flodni/internal/library"
	"flodni/internal/lockmgr"
	"flodni/internal/metadb"
	"flodni/internal/opentracker"
	"flodni/internal/ratelimit"
	"flodni/internal/readahead"
	"flodni/internal/registry"
	"flodni/internal/tuner"
	"flodni/internal/vfs"
	"flodni/internal/warmer"
	"flodni/internal/warmup"
	"fmt"
	"golang.org/x/sys/cpu"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// --- CONFIGURAZIONE (V71-Zero-Latency) ---
// Note: Configuration is now handled entirely by config.go via globalConfig
// Constants removed to ensure single source of truth

var logger = log.New(os.Stdout, "[GoProxy] ", log.LstdFlags)

// masterDataSemaphore limits concurrent data operations (Native, HTTP, Prefetch).
var masterDataSemaphore chan struct{}

var startTime = time.Now()
var metaCache *cache.LRUCache
var raCache = readahead.New(readahead.Config{
	Base:   func() int64 { return gc().ReadAheadBase },
	Budget: func() int64 { return gc().ReadAheadBudget },
	Logf:   func(format string, args ...any) { logger.Printf(format, args...) },
	Go:     safeGo,
})

var globalRateLimiter *ratelimit.RateLimiter
var globalLockManager *lockmgr.LockManager
var globalDirCache *vfs.DirCache

var nativeBridge engineBridge

var globalCleanupManager *CleanupManager
var globalTorrentRemover *TorrentRemover
var globalConfig atomic.Pointer[config.Config]

func gc() *config.Config { return globalConfig.Load() }

func GetEffectiveConcurrencyLimit() int {
	return gc().MasterConcurrencyLimit
}

var tailFillTargets sync.Map // path -> tailFillTarget

// StateDB is the optional SQLite backend for persistent state.
var stateDB *metadb.DB

var physicalSourcePath string
var virtualMountPath string

var backgroundStopChan = make(chan struct{})
var backgroundStopOnce sync.Once

// restartExitCode tells a supervisor that this exit was a restart request from
// the Control Panel, not a crash. The Docker entrypoint restarts only on this
// code; systemd restarts on any. Keep in sync with docker-entrypoint.sh.
const restartExitCode = 75

var restartRequested atomic.Bool

// readBufferPool size matches Config.ReadAheadBase (set in main).
var readBufferPool *sync.Pool

var activeHandles sync.Map      // key: *MkvHandle, value: bool
var inFlightPrefetches sync.Map // key: "path:offset", value: bool

// inFlightFetches dedups the sync FetchBlock fallback: concurrent misses in the same chunk
// share one fetch instead of each running FetchBlock independently.
var inFlightFetches sync.Map // key: "path:chunkAlignedOffset", value: *fetchFlight
var fetchFlightDedupCount atomic.Int64

// fuseShortReadCount counts reads served to FUSE with fewer bytes than requested, away from EOF.
// FUSE runs on the kernel page cache here (no direct_io, see the mount options in main()), so the
// kernel zero-fills the rest of the page and marks it uptodate: that region then serves zeros
// silently. A short read is therefore data corruption, not a slow read, and must stay visible.
var fuseShortReadCount atomic.Int64

// Outcome of the refill in Read: repaired = the gap was filled and the read served whole,
// failed = it could not be, and EIO was returned rather than caching a zero-filled page.
var fuseShortReadRepaired atomic.Int64
var fuseShortReadFailed atomic.Int64

// A blocked FUSE read puts smbd in D-state, which is why fetch timeouts were cut to 8s in
// V283; the refill must stay well inside that budget rather than stacking another 8s on top.
const (
	shortReadRefillAttempts = 2
	shortReadRefillBudget   = 4 * time.Second
)

type fetchFlight struct {
	done chan struct{} // closed by the leader once its result is in raCache
}

var activePumps sync.Map    // Map[string]*NativePumpState — one pump per file path
var pumpTimers sync.Map     // key: path, value: *time.Timer
var priorityTimers sync.Map // key: path, value: *time.Timer

// OpenTracker: contatori O(1) per handle aperti (per hash e per path).
// Permette query rapide da cleanup e priority timer senza scansionare activeHandles.
var globalOpenTracker = opentracker.New()

// Serializes concurrent pump creation for the same file.
var pumpCreationMu sync.Mutex

// safeGo runs a function in a new goroutine with panic recovery.
func safeGo(fn func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Printf("[PANIC] Background goroutine recovered: %v\n%s", r, debug.Stack())
			}
		}()
		fn()
	}()
}

//go:embed settings.html
var settingsHTML []byte

// checkHardwareSupport warns when the CPU sits below the Raspberry Pi 4's effective floor.
// On amd64 that means AVX2: any x86_64 CPU without it (everything before ~2013, Haswell/
// Excavator) forces Go's crypto/sha1 onto its slowest pure-scalar path, and full-swarm piece
// verification struggles to keep up with real playback bitrates - the failure mode diagnosed
// on a 2008 Core2Quad (100% CPU, stutter, no hardware acceleration to fall back on). It is a
// warning, not a gate: the operator decides whether the machine is good enough for their
// bitrates, and a logged reason beats an unexplained stutter. arm64 needs no equivalent check:
// NEON is mandatory in every ARMv8-A chip, so any real arm64 CPU - including the Pi4's own
// Cortex-A72 - already clears this floor. Writes to stderr because it runs before the loggers
// exist; systemd captures it into the service log either way.
func checkHardwareSupport() {
	if runtime.GOARCH == "amd64" && !cpu.X86.HasAVX2 {
		fmt.Fprintln(os.Stderr, "WARNING: this CPU does not support AVX2 (pre-2013 x86_64: before Intel Haswell / AMD Excavator).")
		fmt.Fprintln(os.Stderr, "WARNING: SHA1 piece verification falls back to its slowest path, which may not sustain real playback bitrates.")
		fmt.Fprintln(os.Stderr, "WARNING: expect high CPU and stuttering on high-bitrate files. Starting anyway.")
	}
}

func Run() {
	// `flodni healthcheck` is the container health probe: before anything else
	// runs, and without the daemon, the mount or the flags below.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheckMain())
	}

	checkHardwareSupport()

	var dbPath string
	flag.StringVar(&dbPath, "path", "", "path to database and config")
	flag.Parse()

	// Default --path to the directory containing the binary (portable install)
	if dbPath == "" {
		if exe, err := os.Executable(); err == nil {
			dbPath = filepath.Dir(exe)
		}
	}
	if dbPath != "" {
		if settings.Args == nil {
			settings.Args = &settings.ExecArgs{}
		}
		settings.Args.Path = dbPath
		// settings.Path is what the GoStorm layer actually joins against for config.db,
		// the settings JSON, trackers.txt, accs.db and the blocklist fallback. It was never
		// assigned, so all of those resolved relative to the working directory: under
		// systemd that happens to be the install dir, but in Docker it is the image's
		// WORKDIR, outside every mounted volume - so torrents and GoStorm settings were
		// lost on "docker rm". Migrate anything already written there before switching.
		settings.Path = dbPath
		settings.MigrateFromWorkdir(dbPath)
	}

	source, mount := flag.Arg(0), flag.Arg(1)

	cfg := config.LoadConfig()

	if dbPath != "" {
		// If dbPath is a directory, use it as RootPath; if a file, use its parent.
		if fi, err := os.Stat(dbPath); err == nil && fi.IsDir() {
			cfg.RootPath = dbPath
		} else {
			cfg.RootPath = filepath.Dir(dbPath)
		}
	} else {
		// Default to /home/pi if no flag provided (for backward compat)
		cfg.RootPath = "/home/pi"
	}

	globalConfig.Store(&cfg)

	// CLI args take precedence; fall back to config.json values if omitted
	if source == "" {
		source = gc().PhysicalSourcePath
	}
	if mount == "" {
		mount = gc().FuseMountPath
	}
	if source == "" || mount == "" {
		fmt.Println("Usage: flodni [--path /path/to/db] <source_path> <mount_path>")
		fmt.Println("  Or set physical_source_path and fuse_mount_path in config.json")
		os.Exit(1)
	}
	physicalSourcePath = source
	virtualMountPath = mount
	// The mounted paths are the only paths: the library writes stubs under
	// PhysicalSourcePath, so it must be the one FUSE serves.
	effCfg := *gc()
	for _, w := range applyEffectivePaths(&effCfg, source, mount) {
		logger.Printf("WARNING: %s", w)
	}
	globalConfig.Store(&effCfg)

	gc().LogConfig(logger)

	// Before the engine starts serving streams (Dezoxy fork, see torrstor.BoundedReadahead).
	torrstor.BoundedReadahead = !gc().PumpOnOpen
	server.SetAPIToken(gc().APIToken)
	// Keep-alive counters (Reliability step 8, phase a): count the engine's idle
	// closes. Set before the engine and its ticker start. The hook only counts; it
	// changes nothing about when a torrent closes.
	torr.OnIdleClose = countIdleClose
	// Keep-alive (phase b): the engine asks before it closes an idle torrent. The
	// predicate reads the playback state only and never calls into the engine.
	torr.HoldExpiry = keepaliveHold
	go func() {
		logger.Println("Starting Embedded GoStorm Engine...")
		server.Start() // Starts Web Server on 8090 and Engine
	}()
	// Give engine a moment to init (hash maps etc)
	time.Sleep(2 * time.Second)

	// Before the cache can evict: the played state it orders victims by. The state
	// database attaches below once it is open.
	fileStates.setProtectionDays(gc().PlayedProtectionDays)
	fileStates.logf = logger.Printf
	warmup.IsPlayed = fileStates.IsPlayed
	warmup.OnEvicted = fileStates.OnEvicted
	// Warm on add (idle until warm_on_add is set): a removal cancels it before the
	// torrent's records are forgotten.
	addWarmer = warmer.New(addWarmerDeps(fileStates, backgroundStopChan))
	warmup.OnHashRemoved = onHashRemoved(addWarmer, fileStates)
	warmup.InitDiskWarmup(warmup.Options{
		QuotaGB: gc().DiskWarmupQuotaGB,
		Dir:     gc().WarmupDir,
		HeadMB:  gc().WarmupHeadSizeMB,
		TailMB:  gc().WarmupTailSizeMB,
	})
	// Index mode (Dezoxy fork): nothing reaches the SSD before its pieces pass their
	// checksum, and scans fetch only the pieces they touch.
	if gc().WarmupDir != "" {
		warmup.RangeVerified = func(hash string, fileID int, off, n int64) bool {
			return torr.PeekTorrent(hash).RangeVerified(fileID, off, n)
		}
		warmup.PiecesVerified = func(hash string, fileID int, off, n int64) (int, int) {
			return torr.PeekTorrent(hash).VerifiedPieces(fileID, off, n)
		}
		warmup.RequestPieces = func(hash string, fileID int, off, n int64) {
			torr.PeekTorrent(hash).RequestRange(fileID, off, n)
		}
	}
	// Fires synchronously from DiskWarmup's single writeWorker goroutine at the exact moment a
	// head warmup fetch starts/completes - avoids the race where a caller checking IsWarmingUp()
	// right after WriteChunk() returns could read stale state, since WriteChunk only enqueues and
	// returns immediately (see OnWarmupStateChange doc comment in internal/warmup/warmup.go).
	warmup.OnWarmupStateChange = func(hash string, fileID int, active bool) {
		if tr := torr.PeekTorrent(hash); tr != nil && tr.Torrent != nil {
			tr.Torrent.SetWarmupActive(active, fileID)
		}
	}
	// Lets the warmup package pull the tail window itself (EnsureTail); it cannot import
	// the bridge, so the dependency is injected the same way as the callback above.
	warmup.TailFetch = func(hash string, fileID int, off int64, buf []byte) (int, error) {
		if nativeBridge == nil {
			return 0, fmt.Errorf("native bridge unavailable")
		}
		return nativeBridge.FetchBlock(hash, fileID, off, buf)
	}
	go registry.StartRegistryWatchdog(backgroundStopChan)

	masterDataSemaphore = make(chan struct{}, gc().MasterConcurrencyLimit)
	startHandleGC()

	// Initialize global helpers
	globalRateLimiter = ratelimit.NewRateLimiter(gc().RateLimitRequestsPerSec, 1*time.Second)
	globalLockManager = lockmgr.NewLockManager(1 * time.Hour)

	poolSize := int(gc().ReadAheadBase)
	if poolSize == 0 {
		poolSize = 16 * 1024 * 1024
	}
	readBufferPool = &sync.Pool{
		New: func() interface{} {
			buf := make([]byte, poolSize)
			return &buf
		},
	}
	logger.Printf("ReadBufferPool initialized with size: %d bytes (matches ReadAheadBase)", poolSize)

	// Config is loaded: anchor the raCache recycled-buffer pool to the budget now (at package
	// init gc() was nil, so readahead.New kept the static 32-buffer default).
	raCache.AnchorPoolToConfig()

	catalog.SetRetryDefaults(gc().MaxRetryAttempts, time.Duration(gc().RetryDelayMS)*time.Millisecond)

	nativeClient := native.NewNativeClient()
	nativeBridge = nativeEngine{nativeClient}
	warmWake = nativeClient.WakeStored

	go tuner.Start(context.Background())

	// Metadata LRU cache: capacity from config, 24h TTL.
	metaCache = cache.NewLRUCache(gc().MetadataCacheSize, 24*time.Hour)

	// Deterministic inode map ensures Plex doesn't see "new files" after restarts.
	if err := InitGlobalInodeMap(GetStateDir(), logger); err != nil {
		logger.Printf("WARNING: Failed to initialize inode map: %v (falling back to filename hash)", err)
	}

	// V1.7.1: Optional SQLite State DB for unified persistence.
	if gc().EnableStateDB {
		dbPath := gc().StateDBPath
		if dbPath == "" {
			dbPath = filepath.Join(GetStateDir(), "tiramisu.db")
		}
		var err error
		stateDB, err = metadb.New(dbPath, logger)
		if err != nil {
			logger.Printf("WARNING: Failed to open StateDB: %v (falling back to JSON)", err)
			stateDB = nil
		} else {
			// Migrate JSON files if needed
			if err := stateDB.MigrateFromJSON(GetStateDir()); err != nil {
				logger.Printf("WARNING: StateDB migration failed: %v (falling back to JSON)", err)
				_ = stateDB.Close() // already falling back to JSON
				stateDB = nil
			} else {
				// Wire up DB to InodeMap, then reload from DB (covers boot 2+ where JSON is gone)
				globalInodeMap.SetDB(stateDB)
				if err := globalInodeMap.LoadFromDisk(); err != nil {
					logger.Printf("WARNING: Failed to reload InodeMap from DB: %v", err)
				}
				registry.SetRegistryDB(stateDB)
				webhookStatus.attach(stateDB)
				if err := fileStates.attach(stateDB); err != nil {
					logger.Printf("WARNING: Failed to load file state: %v (played marks stay in memory, no warm records)", err)
				} else if gc().WarmupDir != "" {
					// Index mode only: elsewhere writes are not verified, so nothing is recorded.
					// Wired only after a successful attach, so a record is never held in memory alone.
					fileStates.deliveredAt = warmDeliveredAt
					warmup.OnWindowsCached = fileStates.windowsCached
					go fileStates.runRecorder(backgroundStopChan)
				}
				// V750: Restore playback states from previous sessions
				restorePlaybackStates(stateDB)
				registry.SetStateDir(GetStateDir())
				logger.Printf("[StateDB] Active: %s", dbPath)

				// V304 ban persistence: restore prior bans (30-day TTL) and persist new ones.
				if ips, err := stateDB.LoadV304Bans(30 * 24 * time.Hour); err != nil {
					logger.Printf("WARNING: Failed to load V304 bans: %v", err)
				} else if len(ips) > 0 {
					torrent.V304LoadBans(ips)
					logger.Printf("[V304] Restored %d persisted peer bans", len(ips))
				}
				// Reachability outcomes: the readability record at /api/torrents/health
				// reports the counter, so a client can tell a dead release from a slow one.
				native.ReachabilityOutcome = newReachabilityRecorder(stateDB, logger)
				startReadabilityGauge(stateDB, globalInodeMap.LibraryHashCount, backgroundStopChan, logger)

				banDB := stateDB
				torrent.V304SetOnBan(func(ip string) {
					if err := banDB.SaveV304Ban(ip); err != nil {
						logger.Printf("WARNING: Failed to persist V304 ban for %s: %v", ip, err)
					}
				})
			}
		}
	}

	startWebhookStatus(webhookStatus, backgroundStopChan)

	finishGlobalInodeMapInit(logger)

	// The dir cache must exist before the startup builder runs: a nil cache there
	// is a panic in a bare goroutine that would kill the process before mount.
	globalDirCache = vfs.NewDirCache(10 * time.Second)

	// Pre-populate cache at startup to improve Plex scan performance.
	cacheBuilder := NewStartupCacheBuilder(source, metaCache, logger)
	cacheBuilder.Start()

	globalCleanupManager = NewCleanupManager(logger, metaCache, nativeBridge)
	globalCleanupManager.Start()

	globalTorrentRemover = NewTorrentRemover(logger)

	// Own mux: nothing else in the process (expvar, pprof) can add routes to the control port.
	mux := http.NewServeMux()
	mux.HandleFunc("/plex/webhook", handlePlexWebhook)

	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		cacheStats := metaCache.Stats()
		cleanupStats := globalCleanupManager.Stats()
		lockStats := globalLockManager.Stats()

		// Get read-ahead buffer stats (for dashboard FUSE Buffer display)
		raTotal, raActive, raEntries := raCache.Stats()
		raStale := raTotal - raActive
		raBudget := gc().ReadAheadBudget
		raPercent := float64(raTotal) / float64(raBudget) * 100
		raActivePercent := float64(raActive) / float64(raBudget) * 100
		raStalePercent := float64(raStale) / float64(raBudget) * 100

		warmupBuckets := warmup.WarmupDurationBucketCounts()
		warmupBucketsJSON := fmt.Sprintf("[%d,%d,%d,%d,%d,%d,%d,%d]",
			warmupBuckets[0], warmupBuckets[1], warmupBuckets[2], warmupBuckets[3],
			warmupBuckets[4], warmupBuckets[5], warmupBuckets[6], warmupBuckets[7])

		// Task 4: hedge counters live per-Torrent on the fork - sum trigger count across active
		// torrents, report circuit breaker as open if any active torrent currently has it tripped.
		var hedgeTriggerTotal int64
		deadlinePieces := 0
		hedgeCircuitOpenAny := false
		for _, tr := range torr.ListActiveTorrent() {
			if tr.Torrent == nil {
				continue
			}
			hedgeTriggerTotal += tr.Torrent.HedgeTriggerCount()
			deadlinePieces += tr.Torrent.PieceDeadlineCount()
			if tr.Torrent.HedgeCircuitOpen() {
				hedgeCircuitOpenAny = true
			}
		}
		// Peer view split by transport - conn slots are capped, so a transport is only worth
		// the share of useful bytes it returns for the share of slots it takes.
		peerStats := torr.CollectPeerTransportStats()
		// Gradient2 closed-loop state: the dry run reads backing_off/limit_max to see the loop
		// react to queueing before its 32-request ceiling is raised.
		g2Stats := torr.CollectGradient2Stats()

		shortStream, shortFetch := native.ShortReadCounts()
		repairedStream, unfilledStream := native.ShortReadRepairCounts()

		_, _ = fmt.Fprintf(w, `{"version":"%s", "config_source":"%s", "uptime":"%s", "cache_entries":%d, "cache_size_mb":%.2f, "cleanup_hashes":%d, "cleanup_offsets":%d, "cleanup_activities":%d, "locks_total":%d, "master_concurrency_limit":%d, "streaming_threshold_kb":%d, "config_preload_workers":%d, "max_conns_per_host":%d, "read_ahead_total_bytes":%d, "read_ahead_active_bytes":%d, "read_ahead_stale_bytes":%d, "read_ahead_entries":%d, "read_ahead_budget":%d, "read_ahead_percent":%.2f, "read_ahead_active_percent":%.2f, "read_ahead_stale_percent":%.2f, "warmup_duration_buckets_lt_2_5_10_15_30_60_120_gte120s":%s, "hedge_trigger_count":%d, "hedge_circuit_open":%t, "fetch_singleflight_dedup":%d, "peer_eject_count":%d, "v304_banned_peers":%d, "fuse_short_reads":%d, "fuse_short_reads_repaired":%d, "fuse_short_reads_failed":%d, "short_read_stream":%d, "short_read_fetch":%d, "short_read_repaired":%d, "short_read_unfilled":%d, "peer_conns_tcp":%d, "peer_conns_utp":%d, "peer_rated_tcp":%d, "peer_rated_utp":%d, "peer_useful_bps_tcp":%.0f, "peer_useful_bps_utp":%.0f, "peer_eject_count_utp":%d, "peer_churn_count":%d, "peer_churn_count_utp":%d, "deadline_pieces":%d, "gradient2_peers":%d, "gradient2_backing_off":%d, "gradient2_limit_min":%d, "gradient2_limit_max":%d}`,
			AppVersion,
			gc().ConfigPath,
			time.Since(startTime),
			cacheStats.Entries, float64(cacheStats.Size)/(1024*1024),
			cleanupStats.DeletedHashesTotal, cleanupStats.OffsetsTotal, cleanupStats.ActivitiesTotal,
			lockStats.TotalLocks,
			gc().MasterConcurrencyLimit,
			gc().StreamingThreshold/1024,
			gc().PreloadWorkers,
			gc().MaxConnsPerHost,
			raTotal, raActive, raStale, raEntries, raBudget,
			raPercent, raActivePercent, raStalePercent,
			warmupBucketsJSON,
			hedgeTriggerTotal, hedgeCircuitOpenAny, fetchFlightDedupCount.Load(), peerStats.EjectTotal, torr.V304BannedCount(),
			fuseShortReadCount.Load(), fuseShortReadRepaired.Load(), fuseShortReadFailed.Load(), shortStream, shortFetch, repairedStream, unfilledStream,
			peerStats.ConnsTCP, peerStats.ConnsUTP, peerStats.RatedTCP, peerStats.RatedUTP,
			peerStats.UsefulBpsTCP, peerStats.UsefulBpsUTP,
			peerStats.EjectUTP, peerStats.ChurnTotal, peerStats.ChurnUTP, deadlinePieces,
			g2Stats.Peers, g2Stats.BackingOff, g2Stats.LimitMin, g2Stats.LimitMax)
	})

	mux.HandleFunc("/webhook", handlePlexWebhook)

	mux.HandleFunc("/metrics/profiling", func(w http.ResponseWriter, r *http.Request) {
		totalReads, cacheHits, httpFetches, streamingReads, avgHTTPLatency, avgCacheLatency, cacheHitRate := globalProfilingStats.Stats()

		_, _ = fmt.Fprintf(w, `{"version":"%s", "total_reads":%d, "cache_hits":%d, "cache_hit_rate_pct":%.2f, "http_fetches":%d, "streaming_reads":%d, "non_streaming_reads":%d, "avg_http_latency_ms":%.2f, "avg_cache_latency_ms":%.2f, "max_conns_per_host":%d}`,
			AppVersion,
			totalReads,
			cacheHits,
			cacheHitRate,
			httpFetches,
			streamingReads,
			totalReads-streamingReads,
			float64(avgHTTPLatency.Microseconds())/1000.0,
			float64(avgCacheLatency.Microseconds())/1000.0,
			gc().MaxConnsPerHost)
	})

	mux.HandleFunc("/metrics/prometheus", handlePrometheus)

	mux.HandleFunc("/metrics/ttff", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, `{"sessions_completed":%d,"sessions_filtered":%d,`+
			`"open_to_header_ms":%s,"open_to_deep_read_ms":%s,"seek_latency_ms":%s,`+
			`"read_latency_ms":{"warmup_head":%s,"warmup_tail":%s,"racache_hit":%s,"fetch_block":%s},`+
			`"stalls":{"count":%d,"max_ms":%d}}`,
			ttffStats.sessionsCompleted.Load(), ttffStats.sessionsFiltered.Load(),
			histJSON(&ttffStats.openToHeader), histJSON(&ttffStats.openToDeepRead), histJSON(&ttffStats.seekLatency),
			histJSON(&ttffStats.warmupHead), histJSON(&ttffStats.warmupTail),
			histJSON(&ttffStats.raCacheHit), histJSON(&ttffStats.fetchBlock),
			ttffStats.stallCount.Load(), ttffStats.maxStallMS.Load())
	})

	mux.HandleFunc("/api/sessions", handleSessions)
	// Assigned only when the database is open: a nil *metadb.DB in the interface
	// would not compare equal to nil and the handler would never answer 503.
	var healthStore readabilityLister
	if stateDB != nil {
		healthStore = stateDB
	}
	mux.HandleFunc("/api/torrents/health", newTorrentsHealthHandler(healthStore, fileStates.WarmSnapshot, logger))
	mux.HandleFunc("/api/health", newHealthChecks().handler)
	mux.HandleFunc("/api/warmup/evict", newWarmupEvictHandler(evictWarmupTo))

	mux.HandleFunc("/control", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(settingsHTML)
	})

	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(gc().Redacted())
			return
		}
		if r.Method == "POST" {
			var newCfg config.Config
			if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			// The panel never sees the token; an empty post keeps the one in the file.
			newCfg.APIToken = gc().ResolveAPIToken(newCfg.APIToken)
			// Paths that came from the command line are not written to the
			// file: keep what the file had.
			onDisk := config.LoadConfig()
			if newCfg.PhysicalSourcePath == physicalSourcePath {
				newCfg.PhysicalSourcePath = onDisk.PhysicalSourcePath
			}
			if newCfg.FuseMountPath == virtualMountPath {
				newCfg.FuseMountPath = onDisk.FuseMountPath
			}
			// Update file
			data, _ := json.MarshalIndent(newCfg, "", "  ")
			if err := atomicfile.WriteFile(gc().ConfigPath, data, 0644); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			// Reload in memory (V1.4.0 Live Update)
			cfg := config.LoadConfig()
			// A save must not reintroduce a path other than the mounted one.
			_ = applyEffectivePaths(&cfg, physicalSourcePath, virtualMountPath)
			// RootPath comes from -path at startup, not from the file; without it
			// GetStateDir falls back to /home/pi/STATE after every save.
			cfg.RootPath = gc().RootPath
			globalConfig.Store(&cfg)
			logger.Printf("[Config] Updated via Dashboard API")
			w.WriteHeader(200)
		}
	})

	// Same-origin proxy to the GoStorm settings API. The Control Panel must not
	// build an absolute URL of its own: behind a reverse proxy the GoStorm port is
	// not exposed, and an http:// call from an https:// page is blocked as mixed
	// content, which left the whole GoStorm section empty and unsaveable.
	mux.HandleFunc("/api/gostorm/settings", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		base := gc().GoStormBaseURL
		if base == "" {
			http.Error(w, "gostorm_url not configured", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), "POST",
			strings.TrimRight(base, "/")+"/settings", bytes.NewReader(body))
		if err != nil {
			http.Error(w, "build request: "+err.Error(), http.StatusInternalServerError)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		if t := gc().APIToken; t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
		if err != nil {
			http.Error(w, "gostorm unreachable: "+err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	})

	mux.HandleFunc("/api/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "method not allowed", 405)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"ok":true}`))
		// Flush response before exiting
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// Graceful shutdown, then exit with restartExitCode: systemd restarts on any
		// code, and the Docker entrypoint's supervision loop restarts only on this one.
		restartRequested.Store(true)
		go func() {
			time.Sleep(150 * time.Millisecond)
			p, _ := os.FindProcess(os.Getpid())
			if err := p.Signal(syscall.SIGTERM); err != nil {
				logger.Printf("WARNING: restart requested but SIGTERM to self failed: %v", err)
			}
		}()
	})

	// Library API (external clients)
	// Adds one title without any discovery: register the
	// torrent, wait for its file list, write the stub. It exists so an agent that has
	// no access to the filesystem can still file a title into the library.
	{
		var registry library.EpisodeRegistry
		if stateDB != nil {
			registry = stateDB
		}
		libMgr := library.New(library.Config{
			MoviesDir:      filepath.Join(gc().PhysicalSourcePath, "movies"),
			TVDir:          filepath.Join(gc().PhysicalSourcePath, "tv"),
			GoStormURL:     gc().GoStormBaseURL,
			GoStorm:        library.NewGoStormClient(gc().GoStormBaseURL, gc().APIToken),
			Registry:       registry,
			InvalidatePath: invalidateSyncRemovedPath,
			// Read-only view of the holes the reaper left, for a client that can decide
			// what to do about them.
			Gaps: func() ([]library.Gap, error) {
				if stateDB == nil {
					return nil, nil
				}
				rows, err := stateDB.EpisodeGaps()
				if err != nil {
					return nil, err
				}
				out := make([]library.Gap, 0, len(rows))
				for _, g := range rows {
					// Same spelling the sync logs use, so a client and the log agree on
					// what the show is called.
					show := library.ShowNameFromEpisodePath(g.FilePath)
					out = append(out, library.Gap{
						EpisodeKey: g.EpisodeKey, Show: show, Season: g.Season,
						ShowIMDB: g.ShowIMDB, Path: g.FilePath,
						DeadHash: g.DeadHash, RemovedAt: g.RemovedAt,
						LastAttempt: g.LastAttempt,
					})
				}
				return out, nil
			},
			// Same record the FUSE unlink handler writes: without it a later
			// add can file the title back.
			Blacklist: func(path, hash string) error {
				if globalTorrentRemover == nil {
					return fmt.Errorf("the torrent remover is not running")
				}
				if len(hash) != 40 {
					return fmt.Errorf("malformed info hash %q", hash)
				}
				return globalTorrentRemover.addToBlacklist(hash, globalTorrentRemover.deriveTitleFromPath(path))
			},
			Logger:       logger,
			OnAdded:      onLibraryAdded,
			MediaServer:  mediaserver.New(gc().Plex.URL, gc().Plex.Token),
			MovieSection: gc().Plex.LibraryID,
			TVSection:    gc().Plex.TVLibraryID,
		})
		// rm through the mount removes the stub the same way the API does, and drops
		// the torrent only when no other stub uses it.
		unlinkRemove = func(ctx context.Context, path string) error {
			_, err := libMgr.Remove(ctx, library.RemoveRequest{Path: path})
			return err
		}
		// Catch-up (idle unless warm_on_add is on): warms the titles already filed.
		startCatchup(libMgr)
		libHandler := library.NewHandler(libMgr)
		mux.HandleFunc("/api/library/add", libHandler.Add)
		mux.HandleFunc("/api/library/remove", libHandler.Remove)
		mux.HandleFunc("/api/library/list", libHandler.List)
		mux.HandleFunc("/api/library/inspect", libHandler.Inspect)
	}

	// Control server: metrics, dashboard API and the library API.
	// WriteTimeout bounds the slowest handler, POST /api/library/add. Worst
	// case from internal/library: the .torrent upload (30 s, gostorm client
	// timeout) + AddTorrent (30 s) + the metadata wait (maxMetadataWait 300 s,
	// plus one 30 s poll and one 3 s sleep past the deadline) + the replacement
	// read (replacementReadTimeout 60 s) + a cleanup RemoveTorrent (30 s) = 483 s.
	// 600 s leaves margin. Waiting on another add of the same hash is not
	// counted: that client is cut off, the add itself still completes.
	// ReadTimeout covers the body, capped at 1 MiB (maxBodyBytes): 60 s is
	// about 17 KB/s. No route streams (checked: no Flush loop, SSE or log tail).
	// A failed listen is logged, not fatal: the FUSE mount and the engine do not
	// depend on this port, and the process has always run on without it.
	if gc().APIToken != "" {
		logger.Printf("[Control] bearer-token auth enabled")
	}
	controlSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", gc().MetricsPort),
		Handler:           apiauth.Middleware(gc().APIToken, logger, mux),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      600 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		if err := controlSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Printf("[Control] server on %s stopped: %v", controlSrv.Addr, err)
		}
	}()

	// Graceful shutdown: saves the inode map before exit.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)
	var server *fuse.Server // Declare here to be accessible in goroutine
	var err error           // Declare err here too
	go func() {
		sig := <-sigChan
		logger.Printf("Received signal %v, initiating graceful shutdown...", sig)
		shutdownStarted.Store(true)

		code := shutdown(shutdownSteps{
			closeSessions: closeAllSessions,
			saveState: func() {
				if globalInodeMap == nil {
					return
				}
				if globalInodeMap.IsDirty() {
					if err := globalInodeMap.SaveToDisk(); err != nil {
						logger.Printf("InodeMap: Shutdown save FAILED: %v", err)
					} else {
						files, dirs, _, _ := GetInodeMapStats()
						logger.Printf("InodeMap: Shutdown save complete (%d files, %d dirs)", files, dirs)
					}
				}
				ShutdownGlobalInodeMap()
			},
			// Stop the managers explicitly: os.Exit bypasses defer statements.
			stopBackground: func() {
				backgroundStopOnce.Do(func() { close(backgroundStopChan) })
				if globalRateLimiter != nil {
					globalRateLimiter.Stop()
				}
				if globalLockManager != nil {
					globalLockManager.Stop()
				}
				if globalCleanupManager != nil {
					globalCleanupManager.Stop()
				}
			},
			unmount: func() error {
				if server == nil {
					return nil
				}
				return server.Unmount()
			},
			detach:          func() error { return lazyDetach(mount) },
			restartRequired: restartRequested.Load(),
			logf:            logger.Printf,
		}, shutdownDeadline, shutdownDetachAfter)
		logger.Printf("Graceful shutdown finished, exiting with %d", code)
		os.Exit(code)
	}()

	// Crea root node virtuale
	rootData := &VirtualMkvRoot{sourcePath: source}

	// Enable attribute caching from config
	attrTimeout := time.Duration(gc().AttrTimeoutSeconds * float64(time.Second))
	entryTimeout := time.Duration(gc().EntryTimeoutSeconds * float64(time.Second))
	negativeTimeout := time.Duration(gc().NegativeTimeoutSeconds * float64(time.Second))

	// go-fuse checks out ~MaxWrite per in-flight request; 0 leaves that unbounded.
	// Past the cap it stops reading /dev/fuse, so this trades RAM for latency.
	maxInflight := int(gc().FuseMaxInflightMB * 1024 * 1024)

	server, err = fs.Mount(mount, rootData, &fs.Options{
		AttrTimeout: &attrTimeout, EntryTimeout: &entryTimeout,
		NegativeTimeout: &negativeTimeout,
		MountOptions: fuse.MountOptions{
			MaxInflightRequestBytes: maxInflight,
			AllowOther:              true,
			MaxBackground:           gc().ConcurrencyLimit,
			// MaxWrite:                 1024 * 1024,
			MaxWrite: 4 * 1024 * 1024, // Samba Turbo: 4MB write buffer
			// MaxReadAhead:             1024 * 1024,
			MaxReadAhead:             4 * 1024 * 1024, // Samba Turbo: 4MB read-ahead
			RememberInodes:           true,            // ENABLED: safe with explicit cache control
			ExplicitDataCacheControl: true,            // attribute-driven invalidation only, not open-time
			SyncRead:                 false,           // ENABLED ASYNC READS for 4K performance
			// NFS Export: Stable filesystem identification
			FsName: "flodni",
		},
		UID: gc().UID, // Default file ownership: pi user (1000)
		GID: gc().GID, // Default file ownership: pi group (1000)
	})
	if err != nil {
		log.Fatal(err)
	}

	logger.Printf("FUSE mounted at %s with VirtualMkvRoot, all systems active", mount)

	go ttffCleanupLoop()

	server.Wait()
	if deferToShutdown() {
		select {} // the shutdown goroutine exits with the right code by its deadline
	}
}

func startHandleGC() {
	ticker := time.NewTicker(15 * time.Minute)
	safeGo(func() {
		for range ticker.C {
			count := 0
			activeHandles.Range(func(key, value interface{}) bool {
				h := key.(*MkvHandle)
				h.mu.Lock()
				idle := time.Since(h.lastActivityTime)
				path := h.path
				h.mu.Unlock()

				if idle > 1*time.Hour {
					logger.Printf("[V239] Force closing orphan handle (idle 1h): %s", path)
					// We simulate a FUSE Release to cleanup all resources
					_ = h.Release(context.Background())
					count++
				}
				return true
			})
			if count > 0 {
				logger.Printf("[V239] Handle GC: cleaned %d orphan handles", count)
			}
		}
	})
}

// GetStateDir returns the centralized state directory path.
func GetStateDir() string {
	if gc().RootPath == "" {
		return "/home/pi/STATE"
	}
	return filepath.Join(gc().RootPath, "STATE")
}
