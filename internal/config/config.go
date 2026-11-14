package config

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"flodni/internal/atomicfile"
)

// Config holds all configurable parameters for the FUSE proxy
type Config struct {
	// --- Internal / Derived Fields ---
	ConfigPath string `json:"-"`
	RootPath   string `json:"-"` // V138: Root path for state/config (default: /home/pi)

	// --- Core Tuning (JSON Mapped) ---
	MasterConcurrencyLimit int    `json:"master_concurrency_limit"` // Global limit for concurrent HTTP requests to GoStorm
	ReadAheadBudgetMB      int64  `json:"read_ahead_budget_mb"`     // Global budget for read-ahead in MB
	MetadataCacheSizeMB    int64  `json:"metadata_cache_size_mb"`   // Size of metadata LRU cache in MB (V178)
	FuseBlockSize          int    `json:"fuse_block_size_bytes"`
	StreamingThresholdKB   int64  `json:"streaming_threshold_kb"`
	LogLevel               string `json:"log_level"`

	// --- FUSE Timing ---
	AttrTimeoutSeconds     float64 `json:"attr_timeout_seconds"`
	EntryTimeoutSeconds    float64 `json:"entry_timeout_seconds"`
	NegativeTimeoutSeconds float64 `json:"negative_timeout_seconds"`

	// FuseMaxInflightMB caps the RAM go-fuse checks out for in-flight requests.
	// Each request reserves ~MaxWrite (4MB), so a scan storm is unbounded at 0.
	// Backpressure: past the cap go-fuse stops reading /dev/fuse. 0 = unlimited.
	FuseMaxInflightMB int64 `json:"fuse_max_inflight_mb"`

	// --- HTTP Resilience ---
	MaxRetryAttempts int `json:"max_retry_attempts"`
	RetryDelayMS     int `json:"retry_delay_ms"`

	// --- Preload Engine ---
	PreloadWorkersCount  int `json:"preload_workers_count"`
	WarmStartIdleSeconds int `json:"warm_start_idle_seconds"`

	// --- Connectivity ---
	GoStormBaseURL string `json:"gostorm_url"`
	MetricsPort    int    `json:"metrics_port"`

	// APIToken, when set, is required as a bearer token on every control route
	// except the static panel page (see internal/apiauth). Empty: no auth.
	// FLODNI_API_TOKEN overrides it. GET /api/config never returns it.
	APIToken string `json:"api_token"`
	// fileAPIToken is APIToken as read from config.json, before the environment
	// override, so a panel save never writes the environment's token to disk.
	fileAPIToken string

	// --- FUSE Paths ---
	// Fallback when CLI args are omitted. CLI args always take precedence.
	PhysicalSourcePath string `json:"physical_source_path"` // Real MKV dir (e.g. /mnt/torrserver)
	FuseMountPath      string `json:"fuse_mount_path"`      // FUSE virtual mount (e.g. /mnt/torrserver-go)

	// --- Legacy Compatibility Fields (populated from above) ---
	DefaultFileSize         int64         `json:"-"`
	ReadAheadBudget         int64         `json:"-"`
	MetadataCacheSize       int64         `json:"-"` // V178
	ReadAheadBase           int64         `json:"-"`
	ReadAheadInitial        int64         `json:"-"`
	StreamingThreshold      int64         `json:"-"`
	UnconfirmedReadMargin   int64         `json:"-"` // bytes, from UnconfirmedReadMarginMB
	SequentialTolerance     int64         `json:"-"`
	MaxConcurrentHTTP       int           `json:"-"`
	RateLimitRequestsPerSec int           `json:"-"`
	PreloadWorkers          int           `json:"-"`
	MaxConnsPerHost         int           `json:"-"`
	ConcurrencyLimit        int           `json:"-"`
	KeepaliveInterval       time.Duration `json:"-"`
	KeepaliveIdleStart      time.Duration `json:"-"`
	KeepaliveMaxIdle        time.Duration `json:"-"`
	CacheTTL                time.Duration `json:"-"`
	UID                     uint32        `json:"-"`
	GID                     uint32        `json:"-"`

	// --- Disk Warmup ---
	DiskWarmupQuotaGB int64 `json:"disk_warmup_quota_gb"` // Total SSD quota for warmup cache (default: 32)
	// WarmupDir turns the warmup into an index cache (Dezoxy fork): set, the head and
	// tail copies live there whether or not GoStorm stores pieces on disk (UseDisk),
	// tail coverage survives restarts, and the head is capped at WarmupHeadSizeMB
	// with no boundary chunk past it. Empty keeps upstream's behaviour: warmup only
	// with UseDisk, under TorrentsSavePath, at a fixed 64 MB head.
	WarmupDir string `json:"warmup_dir"`
	// WarmupHeadSizeMB and WarmupTailSizeMB size the per-file windows. Used only with
	// WarmupDir; upstream keeps 64 MB and 16 MB.
	WarmupHeadSizeMB int64 `json:"warmup_head_size_mb"`
	WarmupTailSizeMB int64 `json:"warmup_tail_size_mb"`
	// WarmOnAdd completes the head and tail windows of every file the library API files
	// (Dezoxy fork, Readiness step 2): the swarm is read once, at add time, instead of
	// at the first play. Off by default: it downloads those windows for every title
	// added, which costs download volume on a private tracker. Read at each add, and
	// effective only with WarmupDir set.
	WarmOnAdd bool `json:"warm_on_add"`
	// WarmCatchupPerHour is the pace of catch-up (Readiness step 3): the library files
	// whose windows were never warmed are fed to the warmer at this many files per hour,
	// newest first, never evicting. It runs only with WarmOnAdd on and WarmupDir set,
	// and needs the state database. 0 turns catch-up off, with its day-scale retries;
	// a negative value is refused at load and replaced by the default (20). Read on
	// each feed.
	WarmCatchupPerHour int `json:"warm_catchup_per_hour"`
	// WarmCatchupScope says which files catch-up feeds (roadmap, step 8): "all" (the
	// default) is every library file not warm on disk, not evicted and not failing;
	// "retries" is only the files warm on add attempted and did not finish (a failed
	// read, dropped_quota, lost, write_failed, or queued for over an hour with no
	// outcome), never the cold backlog. An unknown value is replaced by "all" at load.
	WarmCatchupScope string `json:"warm_catchup_scope"`
	// PlayedProtectionDays is how long a play confirmed by the media server keeps a
	// file out of the first rounds of quota eviction, counted from its last play.
	// After that the file ranks as never played. 0 means a play protects forever;
	// a negative value is refused at load and replaced by the default (90).
	PlayedProtectionDays int `json:"played_protection_days"`

	// UnconfirmedReadMarginMB is how many bytes a reader with no confirmed playback may
	// take, past the head and tail windows, before the session is logged and counted
	// (reader_budget.go). On its own it only measures; with RefuseUnconfirmedReads on it is also where refusal starts. 0 logs a session at its
	// first byte past the windows; a negative or absurd value is replaced by the default.
	UnconfirmedReadMarginMB int64 `json:"unconfirmed_read_margin_mb"`

	// RefuseUnconfirmedReads is the opt-in switch for refusing reads: once a session of a
	// file with no confirmed playback has taken the margin past the windows, further
	// fetches are held for a webhook and then refused with EIO (reader_budget.go). Off
	// unless the owner turns it on; read at every read. It enforces only after a play
	// webhook matched a file since start. With it off no read is
	// refused or slowed.
	RefuseUnconfirmedReads bool `json:"refuse_unconfirmed_reads"`

	// PlaybackWebhookWarnDays is the window of the "no play webhook" warning
	// (webhook_status.go): it is raised when a reader session closed unconfirmed in the
	// last N days and no play webhook matched a file in that time. 0 turns the warning
	// off; a negative or absurd value is replaced by the default (7). Read live.
	PlaybackWebhookWarnDays int `json:"playback_webhook_warn_days"`

	// PlaybackKeepaliveMinutes is the keep-alive's idle ceiling (keepalive.go): a torrent
	// with a webhook-confirmed playback is not closed for idleness until this many
	// minutes after the last read or play/resume webhook, or until the stop webhook. It
	// is the keep-alive's one off switch: 0 turns it off and every torrent idle-closes,
	// a confirmed one included (priority no longer holds it). A negative or absurd (over
	// a day) value is replaced by the default (15). Read live.
	PlaybackKeepaliveMinutes int `json:"playback_keepalive_minutes"`
	// PlaybackKeepaliveMaxTorrents caps how many torrents the keep-alive holds at once,
	// the most recently active first. The rest fall back to the plain idle close. Minimum
	// 1: 0 or a negative value is replaced by the default (4), so 0 never means off or
	// unlimited here. Size it so cap x CacheSize stays well under the memory limit. Read
	// live.
	PlaybackKeepaliveMaxTorrents int `json:"playback_keepalive_max_torrents"`

	// PlexPathMap maps a media server library path prefix to the path of the same
	// files inside this container, for libraries whose files are Sonarr/Radarr-named
	// symlinks into the mount (webhook.go, playback.go pathMapStubNames). Off while
	// empty. A webhook whose file name matches no registered file is resolved through
	// the longest matching prefix: the mapped path is checked once with Lstat and, if
	// it is a symlink, the base name of its target is matched. Both sides must be
	// absolute; an entry that is not is dropped with a warning at load. Read live.
	PlexPathMap map[string]string `json:"plex_path_map"`

	// --- External Services (V1.4.6) ---
	Plex struct {
		URL         string `json:"url"`
		Token       string `json:"token"`
		LibraryID   int    `json:"library_id"`
		TVLibraryID int    `json:"tv_library_id"`
	} `json:"plex"`

	// --- Media Server ---
	// MediaServerType is kept only so an old config.json still parses. Plex is the only
	// media server; any value but "" or "plex" (Jellyfin was removed) logs a warning.
	MediaServerType string `json:"media_server_type"`

	// PumpOnOpen starts the read-ahead pump as soon as a file is opened (upstream's
	// behaviour). Off, a file opened by a scanner (Plex, Sonarr/Radarr's ffprobe,
	// Bazarr's hash) is served by small on-demand fetches, and the pump starts once a
	// webhook confirms playback or the reads look like playback (IsInferredPlayback).
	// Dezoxy fork: a library scan otherwise cost ~500 MB of read-ahead per file.
	PumpOnOpen bool `json:"pump_on_open"`

	// --- State DB (V1.7.1) ---
	EnableStateDB bool   `json:"enable_state_db"` // default: true
	StateDBPath   string `json:"state_db_path"`   // default: <STATE>/tiramisu.db
}

// Save persists the current configuration to config.json
func (c *Config) Save() error {
	// 1. Marshal config to JSON
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	// 2. Write to file
	return atomicfile.WriteFileKeepMode(c.ConfigPath, data, 0600)
}

// Redacted returns a copy that is safe to send to the control panel: the API
// token is blanked.
func (c Config) Redacted() Config {
	c.APIToken = ""
	return c
}

// ResolveAPIToken returns the token to persist when the panel posts a config.
// The panel never sees the token, so an empty value (what GET returns) keeps
// the one in config.json; anything else is taken as given.
func (c *Config) ResolveAPIToken(posted string) string {
	if posted == "" {
		return c.fileAPIToken
	}
	return posted
}

// LoadConfig loads configuration from environment variables with defaults
func LoadConfig() Config {
	// 1. Initial Defaults (V138 Gold Standard)
	cfg := Config{
		MasterConcurrencyLimit: 25,
		ReadAheadBudgetMB:      256,
		MetadataCacheSizeMB:    50, // Default 50MB for metadata
		FuseBlockSize:          1048576,
		StreamingThresholdKB:   128,
		PumpOnOpen:             true,
		LogLevel:               "INFO",

		AttrTimeoutSeconds:     1.0,
		EntryTimeoutSeconds:    1.0,
		NegativeTimeoutSeconds: 0.0,
		FuseMaxInflightMB:      0, // off: config.json-only knob, matches go-fuse's own default

		MaxRetryAttempts: 6,
		RetryDelayMS:     500,

		PreloadWorkersCount:  4,
		WarmStartIdleSeconds: 6,

		DiskWarmupQuotaGB: 15,
		WarmupHeadSizeMB:  64,
		WarmupTailSizeMB:  16,

		PlayedProtectionDays:    90,
		UnconfirmedReadMarginMB: 64,
		WarmCatchupPerHour:      20,
		WarmCatchupScope:        "all",
		PlaybackWebhookWarnDays: 7,

		PlaybackKeepaliveMinutes:     15,
		PlaybackKeepaliveMaxTorrents: 4,

		GoStormBaseURL: "http://127.0.0.1:8090",
		MetricsPort:    9080,

		EnableStateDB: true,

		// Legacy Fixed Defaults
		DefaultFileSize:         30 * 1024 * 1024 * 1024,
		ReadAheadBase:           16 * 1024 * 1024,
		ReadAheadInitial:        16 * 1024 * 1024,
		SequentialTolerance:     512 * 1024,
		RateLimitRequestsPerSec: 500,
		KeepaliveInterval:       15 * time.Second,
		KeepaliveIdleStart:      8 * time.Second,
		KeepaliveMaxIdle:        600 * time.Second,
		CacheTTL:                10 * time.Second,
		UID:                     1000,
		GID:                     1000,
	}

	// 2. Resolve Config Path — always co-located with the binary
	if p := os.Getenv("MKV_PROXY_CONFIG_PATH"); p != "" {
		cfg.ConfigPath = p
	} else {
		exe, err := os.Executable()
		if err == nil {
			cfg.ConfigPath = filepath.Join(filepath.Dir(exe), "config.json")
		} else {
			cfg.ConfigPath = "config.json" // fallback: CWD
		}
	}

	// 3. Try to load JSON
	if data, err := os.ReadFile(cfg.ConfigPath); err == nil {
		// V138: Support comments in JSON by stripping them before unmarshaling
		cleanData := stripJSONComments(data)
		if err := json.Unmarshal(cleanData, &cfg); err != nil {
			log.Printf("[Config] WARNING: Failed to parse %s: %v", cfg.ConfigPath, err)
		} else {
			log.Printf("[Config] Loaded settings from %s", cfg.ConfigPath)
			cfg.fileAPIToken = cfg.APIToken
			// Backward compat: if gostorm_url was not present in config, fall back to legacy torrserver_url key
			if cfg.GoStormBaseURL == "" {
				var raw map[string]json.RawMessage
				if json.Unmarshal(cleanData, &raw) == nil {
					if v, ok := raw["torrserver_url"]; ok {
						var s string
						if json.Unmarshal(v, &s) == nil && s != "" {
							cfg.GoStormBaseURL = s
							log.Printf("[Config] Loaded GoStormBaseURL from legacy key 'torrserver_url': %s", s)
						}
					}
				}
			}
		}
	}

	// 4. Override from environment (Highest Priority)
	cfg.applyEnvOverrides()

	// 5. Finalize and map derived fields
	cfg.finalize()

	return cfg
}

// stripJSONComments removes // comments from JSON data and preserves valid syntax.
// It is careful not to strip // when part of a URL (e.g., http://).
func stripJSONComments(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	var result []string
	for _, line := range lines {
		// Find // but only if not preceded by : (simple check for http://)
		idx := strings.Index(line, "//")
		if idx != -1 {
			if idx > 0 && line[idx-1] == ':' {
				// It's likely a URL, look for another // later in the line
				secondIdx := strings.Index(line[idx+2:], "//")
				if secondIdx != -1 {
					line = line[:idx+2+secondIdx]
				}
			} else {
				line = line[:idx]
			}
		}
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return []byte(strings.Join(result, " "))
}

func (c *Config) applyEnvOverrides() {
	if v := os.Getenv("MKV_PROXY_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			c.MasterConcurrencyLimit = n
		}
	}
	if v := os.Getenv("MKV_PROXY_READ_AHEAD_BUDGET"); v != "" {
		if size, err := parseBytes(v); err == nil {
			c.ReadAheadBudgetMB = size / (1024 * 1024)
		}
	}
	if v := os.Getenv("MKV_PROXY_GOSTORM_URL"); v != "" {
		c.GoStormBaseURL = v
	}
	if v := firstEnv("FLODNI_PLEX_URL", "TIRAMISU_PLEX_URL", "GOSTREAM_PLEX_URL", "PLEX_URL"); v != "" {
		c.Plex.URL = v
	}
	if v := firstEnv("FLODNI_PLEX_TOKEN", "TIRAMISU_PLEX_TOKEN", "GOSTREAM_PLEX_TOKEN", "PLEX_TOKEN"); v != "" {
		c.Plex.Token = v
	}
	if v := os.Getenv("FLODNI_API_TOKEN"); v != "" {
		c.APIToken = v
	}
	if v := os.Getenv("MKV_PROXY_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("MKV_PROXY_UID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			c.UID = uint32(n)
		}
	}
	if v := os.Getenv("MKV_PROXY_GID"); v != "" {
		if n, err := strconv.ParseUint(v, 10, 32); err == nil {
			c.GID = uint32(n)
		}
	}
}

func firstEnv(keys ...string) string {
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			return value
		}
	}
	return ""
}

func (c *Config) finalize() {
	if c.MediaServerType != "" && c.MediaServerType != "plex" {
		log.Printf("[Config] WARNING: media_server_type %q: Jellyfin is no longer supported and Flodni uses Plex", c.MediaServerType)
	}

	// At 0 the pump semaphore is unbuffered and its non-blocking send never succeeds.
	if c.MasterConcurrencyLimit < 1 {
		c.MasterConcurrencyLimit = 1
	}

	// Sync legacy fields with unified master limit
	c.ConcurrencyLimit = c.MasterConcurrencyLimit
	c.MaxConcurrentHTTP = c.MasterConcurrencyLimit
	c.MaxConnsPerHost = c.MasterConcurrencyLimit

	// Map JSON fields to internal logic fields
	// Calculate ReadAheadBudget in bytes
	c.ReadAheadBudget = c.ReadAheadBudgetMB * 1024 * 1024
	if c.ReadAheadBudget < 10*1024*1024 {
		c.ReadAheadBudget = 10 * 1024 * 1024 // Min 10MB
	}
	// Budget must cover at least one adaptive chunk (ReadAheadBase, up to 16MB): a smaller
	// budget makes every pump Put() exceed it permanently (the just-added chunk is exempt from
	// eviction), turning the soft-limit throttle in nativePumpChunk into a permanent near-freeze.
	if c.ReadAheadBudget < c.ReadAheadBase {
		c.ReadAheadBudget = c.ReadAheadBase
	}

	// A cap below a handful of requests (~4MB each) serialises FUSE instead of
	// bounding it: go-fuse stops reading /dev/fuse until one completes.
	if c.FuseMaxInflightMB < 0 {
		c.FuseMaxInflightMB = 0
	} else if c.FuseMaxInflightMB > 0 && c.FuseMaxInflightMB < 16 {
		c.FuseMaxInflightMB = 16
	}

	if c.PlayedProtectionDays < 0 {
		log.Printf("[Config] WARNING: played_protection_days %d is negative; using 90", c.PlayedProtectionDays)
		c.PlayedProtectionDays = 90
	}

	if c.WarmCatchupPerHour < 0 {
		log.Printf("[Config] WARNING: warm_catchup_per_hour %d is negative; using 20", c.WarmCatchupPerHour)
		c.WarmCatchupPerHour = 20
	}

	if c.WarmCatchupScope != "all" && c.WarmCatchupScope != "retries" {
		if c.WarmCatchupScope != "" {
			log.Printf("[Config] WARNING: warm_catchup_scope %q is not \"all\" or \"retries\"; using \"all\"", c.WarmCatchupScope)
		}
		c.WarmCatchupScope = "all"
	}

	// Above ~100,000 days the protection time overflows a duration and would turn
	// protection off by accident.
	if c.PlayedProtectionDays > 36500 {
		log.Printf("[Config] WARNING: played_protection_days %d is absurd; using 0 (never expire)", c.PlayedProtectionDays)
		c.PlayedProtectionDays = 0
	}

	// The margin is a measurement threshold. Above 1 TiB (1<<20 MB) it is larger than
	// any file served here, so it is a typo, not a choice.
	if c.UnconfirmedReadMarginMB < 0 || c.UnconfirmedReadMarginMB > 1<<20 {
		log.Printf("[Config] WARNING: unconfirmed_read_margin_mb %d is out of range; using 64", c.UnconfirmedReadMarginMB)
		c.UnconfirmedReadMarginMB = 64
	}
	c.UnconfirmedReadMargin = c.UnconfirmedReadMarginMB * 1024 * 1024

	// Above 100 years the window is a typo, not a choice. 0 stays 0: warning off.
	if c.PlaybackWebhookWarnDays < 0 || c.PlaybackWebhookWarnDays > 36500 {
		log.Printf("[Config] WARNING: playback_webhook_warn_days %d is out of range; using 7", c.PlaybackWebhookWarnDays)
		c.PlaybackWebhookWarnDays = 7
	}

	// 0 stays 0: keep-alive off. Above a day the ceiling is a typo, not a choice.
	if c.PlaybackKeepaliveMinutes < 0 || c.PlaybackKeepaliveMinutes > 1440 {
		log.Printf("[Config] WARNING: playback_keepalive_minutes %d is out of range; using 15", c.PlaybackKeepaliveMinutes)
		c.PlaybackKeepaliveMinutes = 15
	}
	if c.PlaybackKeepaliveMaxTorrents < 1 {
		log.Printf("[Config] WARNING: playback_keepalive_max_torrents %d is below 1; using 4", c.PlaybackKeepaliveMaxTorrents)
		c.PlaybackKeepaliveMaxTorrents = 4
	}

	// An entry whose prefix or target is not absolute cannot match a library path or
	// name a file here, so it is dropped rather than guessed at. The map is replaced, not
	// edited: the caller may share the decoded one.
	if len(c.PlexPathMap) > 0 {
		valid := make(map[string]string, len(c.PlexPathMap))
		keys := make([]string, 0, len(c.PlexPathMap))
		for prefix := range c.PlexPathMap {
			keys = append(keys, prefix)
		}
		sort.Strings(keys) // a duplicate after cleaning keeps the first, deterministically
		for _, prefix := range keys {
			target := c.PlexPathMap[prefix]
			if !filepath.IsAbs(prefix) || !filepath.IsAbs(target) {
				log.Printf("[Config] WARNING: plex_path_map entry %q -> %q needs an absolute prefix and an absolute target; ignoring it", prefix, target)
				continue
			}
			clean := filepath.Clean(prefix)
			if _, dup := valid[clean]; dup {
				log.Printf("[Config] WARNING: plex_path_map has the prefix %q twice; keeping the first", clean)
				continue
			}
			valid[clean] = filepath.Clean(target)
		}
		c.PlexPathMap = valid
	}

	// Calculate MetadataCacheSize in bytes
	c.MetadataCacheSize = c.MetadataCacheSizeMB * 1024 * 1024
	if c.MetadataCacheSize < 1*1024*1024 {
		c.MetadataCacheSize = 1 * 1024 * 1024 // Min 1MB
	}

	c.StreamingThreshold = c.StreamingThresholdKB * 1024
	c.PreloadWorkers = c.PreloadWorkersCount
}

// parseBytes parses byte size strings like "80MB", "128KB", "1GB"
func parseBytes(s string) (int64, error) {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, nil
	}
	multipliers := map[string]int64{
		"KB": 1024, "MB": 1024 * 1024, "GB": 1024 * 1024 * 1024,
		"K": 1024, "M": 1024 * 1024, "G": 1024 * 1024 * 1024,
	}
	for suffix, mult := range multipliers {
		if len(s) > len(suffix) && s[len(s)-len(suffix):] == suffix {
			numPart := s[:len(s)-len(suffix)]
			if n, err := strconv.ParseInt(numPart, 10, 64); err == nil {
				return n * mult, nil
			}
		}
	}
	return 0, strconv.ErrSyntax
}

// LogConfig logs the active configuration
func (c *Config) LogConfig(logger *log.Logger) {
	logger.Printf("=== Configuration ===")
	logger.Printf("Source: %s", c.ConfigPath)
	logger.Printf("MasterConcurrencyLimit: %d", c.MasterConcurrencyLimit)
	logger.Printf("ReadAheadBudget: %d MB", c.ReadAheadBudgetMB)
	logger.Printf("FUSE Block Size: %d", c.FuseBlockSize)
	logger.Printf("StreamingThreshold: %d KB", c.StreamingThresholdKB)
	logger.Printf("LogLevel: %s", c.LogLevel)
	logger.Printf("GoStormBaseURL: %s", c.GoStormBaseURL)
	logger.Printf("FUSE Timeouts (Attr/Entry/Neg): %.1f/%.1f/%.1f", c.AttrTimeoutSeconds, c.EntryTimeoutSeconds, c.NegativeTimeoutSeconds)
	if c.FuseMaxInflightMB > 0 {
		logger.Printf("FUSE MaxInflightRequestBytes: %d MB", c.FuseMaxInflightMB)
	} else {
		logger.Printf("FUSE MaxInflightRequestBytes: unlimited")
	}
	logger.Printf("HTTP Retries: %d, Delay: %dms", c.MaxRetryAttempts, c.RetryDelayMS)
	logger.Printf("Preload Engine: Workers=%d", c.PreloadWorkersCount)
	logger.Printf("Network: MetricsPort=%d", c.MetricsPort)
	logger.Printf("=====================")
}
