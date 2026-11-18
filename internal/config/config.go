package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"flodni/internal/flog"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ParseFailed is true when the latest LoadConfig found a config file it could not parse
// and fell back to the defaults. The startup report reads it.
var ParseFailed atomic.Bool

// parseFailedFmt is the log line for that case; operators grep for "Failed to parse".
const parseFailedFmt = "[Config] WARNING: Failed to parse %s: %v"

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
	LogLevel               string `json:"log_level"`  // DEBUG shows debug lines; anything else is INFO (WARN, ERROR reserved, behave like INFO)
	LogFormat              string `json:"log_format"` // "text" (default) or "json"

	// --- FUSE Timing ---
	AttrTimeoutSeconds     float64 `json:"attr_timeout_seconds"`
	EntryTimeoutSeconds    float64 `json:"entry_timeout_seconds"`
	NegativeTimeoutSeconds float64 `json:"negative_timeout_seconds"`
	// The three timeouts are startup-only mount options, each 0 to MaxFuseTimeoutSeconds;
	// a value outside refuses the start. A TTL above the default is a start-up warning
	// (ADR 5: no kernel dentry invalidation yet).
	// FuseDisableSplice turns off splicing from files to /dev/fuse (go-fuse DisableSplice).
	// Startup-only; off, as before it was an option.
	FuseDisableSplice bool `json:"fuse_disable_splice"`

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
	// AuthTokensFile is the path of the scoped-token file (ADR 0004, see
	// internal/apiauth). Set, both servers enforce the scopes in it; api_token must
	// then be unset or the daemon refuses to start. FLODNI_AUTH_TOKENS_FILE
	// overrides it. Read once at start; Flodni never writes the file.
	AuthTokensFile string `json:"auth_tokens_file"`
	// AuthJWT, when set, is the proxy-signed identity (ADR 0004); nil: off. Its
	// Validate refusals (incomplete block, a non-https jwks_url, header Authorization)
	// stop the start: jwt.New applies them when the daemon builds the verifier.
	AuthJWT *AuthJWT `json:"auth_jwt,omitempty"`
	// AuthPanelHTTPS says the panel is served over https behind a TLS tunnel: the
	// CSRF check then needs an https Origin (ADR 0004). Startup-only.
	AuthPanelHTTPS bool `json:"auth_panel_https,omitempty"`
	// AllowedHosts, when non-empty, makes both servers refuse (403) a request whose
	// Host hostname is not listed: the defence against DNS rebinding (see
	// internal/apiauth). IP literals, localhost and the machine's own hostname are
	// always allowed. Empty: off. MKV_PROXY_ALLOWED_HOSTS (comma list) overrides it.
	// Hostnames only, normalised by finalize; read at start, a save needs a restart.
	AllowedHosts []string `json:"allowed_hosts"`
	// hostsOn is true when the list was non-empty as written, so a list with no valid
	// entry keeps the check on (only the always-allowed hosts) instead of turning it off.
	hostsOn bool
	// hostsForce makes the list count as non-empty as written when its text is not
	// usable as a list: a value of the wrong type in config.json, or an environment
	// value with no entry. The check then stays on with only the always-allowed hosts.
	hostsForce bool
	// fileAllowedHosts is the list as config.json holds it, before the environment
	// override and normalisation, so a panel save never writes the environment's list.
	fileAllowedHosts []string
	// fileAPIToken is APIToken as read from config.json, before the environment
	// override, so a panel save never writes the environment's token to disk.
	fileAPIToken string
	// fileAuthTokensFile is AuthTokensFile as config.json holds it, before the
	// environment override: a panel save writes this, never the environment's path.
	fileAuthTokensFile string
	// unknownKeys are the top-level keys of config.json no setting names exactly.
	unknownKeys []string

	// filePlexToken is Plex.Token as read from config.json, and plexTokenFromEnv says
	// the environment supplied the one in use. A panel save writes the file's token,
	// never the environment's. GET /api/config never returns the token either.
	filePlexToken    string
	plexTokenFromEnv bool

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
	// WarmupEvictionOrder is how the quota ranks files inside each class (never played
	// first, then played): "written" (the default) takes the oldest written first;
	// "read" (opt-in, roadmap step 22) takes the least recently read first, by an
	// in-memory last-hit time that is not persisted (a file not read since start ranks
	// by its write time). Read at start. An unknown value is replaced by "written".
	WarmupEvictionOrder string `json:"warmup_eviction_order"`
	// WarmNextEpisode (opt-in, roadmap step 23) fills the windows of the next episode
	// of a show while a play of the previous one is confirmed by the media server. It
	// runs through warm on add's machinery, so it needs WarmOnAdd and WarmupDir, and
	// it never evicts: it only fills where the cache has room. Off by default.
	WarmNextEpisode bool `json:"warm_next_episode"`
	// WarmNextEpisodeSameTorrent lets WarmNextEpisode also fill an episode that is a
	// file of the torrent being played (a season pack). Off by default: a second
	// reader on the playing torrent splits the engine's read-ahead.
	WarmNextEpisodeSameTorrent bool `json:"warm_next_episode_same_torrent"`
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

	// UnconfirmedReadaheadClamp is an opt-in switch: a read that misses inside the head
	// window of a file with no confirmed playback gets read-ahead only up to the window
	// edge, never into the middle the budget protects (reader_budget.go). Off unless the
	// owner turns it on; read at every read. With it off the read-ahead is as it always
	// was, and flodni_reader_budget_readahead_overshoot_bytes_total counts what it takes
	// past the edge.
	UnconfirmedReadaheadClamp bool `json:"unconfirmed_readahead_clamp"`

	// AttributeFillsToReader is an opt-in switch: bytes that the pump and the prefetches
	// fetch past the windows of a file with no confirmed playback count toward the
	// session's margin, not only the bytes the reader later reads (reader_budget.go). It
	// moves when the margin is crossed, so it can move when refusal starts; off unless
	// the owner turns it on. The attributed bytes are counted either way, in
	// flodni_reader_budget_attributed_fill_bytes_total.
	AttributeFillsToReader bool `json:"attribute_fills_to_reader"`

	// RedialDroppedPeer is an opt-in switch: when the only connected peer of a private
	// torrent drops and that peer's address came from the torrent's own tracker, the
	// address is added to the torrent once, a few seconds later, instead of waiting for
	// the next announce (internal/gostorm/torr/redial.go). No DHT, PEX or public tracker
	// is involved. Off unless the owner turns it on; read when a peer drops.
	RedialDroppedPeer bool `json:"redial_dropped_peer"`

	// EngineRefillV2 is an opt-in switch for the engine's request-refill changes adapted from
	// upstream (internal/anacrolix-torrent, ClientConfig.RefillV2): a cap on the candidates a
	// refill sorts, a steal of requests from a holder that delivers nothing, a refill at half a
	// queue instead of an empty one, and released timers. Off unless the owner turns it on, and
	// read once at start: a change needs a restart. With it off the engine runs the code it
	// always ran.
	EngineRefillV2 bool `json:"engine_refill_v2"`

	// EngineColdBoost is an opt-in switch adapted from upstream: a torrent that wakes with a
	// per-torrent connection limit below 35 gets 35 until 1 MiB of useful data has arrived or
	// 30 s have passed, then the configured limit comes back (internal/gostorm/torr/coldboost.go).
	// Off unless the owner turns it on, and read once at start: a change needs a restart.
	EngineColdBoost bool `json:"engine_cold_boost"`

	// PlaybackWebhookWarnDays is the window of the "no play webhook" warning
	// (internal/daemon/webhook_status.go): it is raised when a reader session closed unconfirmed in the
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

	// TrackerScrapeHours is the interval of the scheduled tracker scrape pass
	// (internal/scrape, EXPERIMENTAL): every stored torrent is asked once per pass at
	// its own trackers. 0 turns the schedule off; POST /api/torrents/scrape still runs
	// a pass. A negative value or one over a year (8760) is replaced by the default
	// (24). Read at start-up.
	TrackerScrapeHours int `json:"tracker_scrape_hours"`
	// TrackerScrapeBatch is the most hashes in one HTTP scrape request (UDP keeps its
	// own 70 per packet). 1 to 200; anything else is replaced by the default (100).
	// Read at start-up.
	TrackerScrapeBatch int `json:"tracker_scrape_batch"`
	// TrackerScrapePaceMs is the least gap in milliseconds between two scrape
	// requests to one tracker host. 100 to 60000; anything else is replaced by the
	// default (1000). Read at start-up.
	TrackerScrapePaceMs int `json:"tracker_scrape_pace_ms"`

	// PlexPathMap maps a media server library path prefix to the path of the same
	// files inside this container, for libraries whose files are Sonarr/Radarr-named
	// symlinks into the mount (webhook.go, playback.go pathMapStubNames). Off while
	// empty. A webhook whose file name matches no registered file is resolved through
	// the longest matching prefix: the mapped path is checked once with Lstat and, if
	// it is a symlink, the base name of its target is matched. Both sides must be
	// absolute; an entry that is not is dropped with a warning at load. Read live.
	// The targets are also the library roots a background walker reads (lstat and
	// readlink only) to tell the stub a library symlink points at from a stub it
	// does not, when a webhook matches several (daemon/linked_stubs.go).
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

// UnknownKeys lists, by name and sorted, the top-level keys of config.json that are
// not the exact name of a setting (a removed setting, a typo, another letter case).
// Names only, never a value. Run logs them once at start; they do not stop it.
func (c *Config) UnknownKeys() []string { return append([]string(nil), c.unknownKeys...) }

// Redacted returns a copy that is safe to send to the control panel: the API
// token and the Plex token are blanked.
func (c Config) Redacted() Config {
	c.APIToken = ""
	c.Plex.Token = ""
	return c
}

// PlexTokenSource says where the Plex token in use comes from: "env", "file" or ""
// (none). The panel shows it instead of the token.
func (c Config) PlexTokenSource() string {
	switch {
	case c.plexTokenFromEnv:
		return "env"
	case c.Plex.Token != "":
		return "file"
	}
	return ""
}

// ResolvePlexToken returns the Plex token to persist when the panel posts a config.
// The panel never sees the token, so an empty value keeps the one in config.json
// (never the environment's); anything else is taken as given.
func (c *Config) ResolvePlexToken(posted string) string {
	if posted == "" {
		return c.filePlexToken
	}
	return posted
}

// FileAuthTokensFile is auth_tokens_file as config.json holds it, without the
// environment's override. It is startup-only: a save writes this value.
func (c *Config) FileAuthTokensFile() string { return c.fileAuthTokensFile }

// ResolveAPIToken returns the token to persist when the panel posts a config.
// The panel never sees the token, so an empty value (what GET returns) keeps
// the one in config.json; anything else is taken as given.
func (c *Config) ResolveAPIToken(posted string) string {
	if posted == "" {
		return c.fileAPIToken
	}
	return posted
}

// LoadConfig loads configuration from environment variables with defaults.
//
// A config file that does not exist is valid: defaults. One that exists but
// cannot be read, or does not parse after stripJSONComments (a type error
// counts), returns an error naming the path and the cause (never file
// content). Beside the error the Config holds the defaults and whatever keys
// json decoded before it failed; only the healthcheck uses that, to keep a
// metrics_port that did decode. Nothing else acts on it: the daemon's start
// refuses, because a start that cannot read its configuration cannot know
// whether the file names auth settings, and a save answers 409.
func LoadConfig() (Config, error) {
	// 1. Initial Defaults (V138 Gold Standard)
	cfg := Config{
		MasterConcurrencyLimit: 25,
		ReadAheadBudgetMB:      256,
		MetadataCacheSizeMB:    50, // Default 50MB for metadata
		FuseBlockSize:          1048576,
		StreamingThresholdKB:   128,
		PumpOnOpen:             true,
		LogLevel:               "INFO",
		LogFormat:              "text",

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
		WarmupEvictionOrder:     "written",
		PlaybackWebhookWarnDays: 7,

		PlaybackKeepaliveMinutes:     15,
		PlaybackKeepaliveMaxTorrents: 4,

		TrackerScrapeHours:  24,
		TrackerScrapeBatch:  100,
		TrackerScrapePaceMs: 1000,

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
	var loadErr error
	data, readErr := os.ReadFile(cfg.ConfigPath)
	missing := errors.Is(readErr, fs.ErrNotExist)
	if missing {
		// A dangling symlink reads as not-exist but is a file that is there.
		if _, err := os.Lstat(cfg.ConfigPath); err == nil {
			missing = false
			readErr = errors.New("a symlink that points nowhere")
		}
	}
	if readErr != nil && !missing {
		loadErr = fmt.Errorf("config file %s exists but cannot be read: %w", cfg.ConfigPath, readErr)
	}
	if readErr == nil {
		// V138: Support comments in JSON by stripping them before unmarshaling
		cleanData := stripJSONComments(data)
		if err := json.Unmarshal(cleanData, &cfg); err != nil {
			ParseFailed.Store(true)
			flog.Printf(flog.Std, parseFailedFmt, cfg.ConfigPath, err)
			loadErr = fmt.Errorf("config file %s does not parse: %w", cfg.ConfigPath, err)
		} else if unknown, err := checkTopLevelKeys(cleanData); err != nil {
			// An auth key that is mistyped, repeated or null would leave auth off or
			// pointed at the wrong place without a word: the start is refused.
			loadErr = fmt.Errorf("config file %s: %w", cfg.ConfigPath, err)
		} else {
			ParseFailed.Store(false)
			cfg.unknownKeys = unknown
			flog.Std.Info(fmt.Sprintf("[Config] Loaded settings from %s", cfg.ConfigPath), "config_path", cfg.ConfigPath)
			cfg.fileAPIToken = cfg.APIToken
			cfg.fileAuthTokensFile = cfg.AuthTokensFile
			cfg.fileAllowedHosts = append([]string(nil), cfg.AllowedHosts...)
			cfg.filePlexToken = cfg.Plex.Token
			// Backward compat: if gostorm_url was not present in config, fall back to legacy torrserver_url key
			if cfg.GoStormBaseURL == "" {
				var raw map[string]json.RawMessage
				if json.Unmarshal(cleanData, &raw) == nil {
					if v, ok := raw["torrserver_url"]; ok {
						var s string
						if json.Unmarshal(v, &s) == nil && s != "" {
							cfg.GoStormBaseURL = s
							flog.Std.Info(fmt.Sprintf("[Config] Loaded GoStormBaseURL from legacy key 'torrserver_url': %s", s), "s", s)
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

	if loadErr == nil {
		loadErr = cfg.validateFuse()
	}
	return cfg, loadErr
}

// MaxFuseTimeoutSeconds is the largest FUSE attribute, entry or negative timeout.
const MaxFuseTimeoutSeconds = 60

// validateFuse refuses a timeout outside 0..MaxFuseTimeoutSeconds (NaN included).
func (c *Config) validateFuse() error {
	for _, t := range []struct {
		key string
		v   float64
	}{
		{"attr_timeout_seconds", c.AttrTimeoutSeconds},
		{"entry_timeout_seconds", c.EntryTimeoutSeconds},
		{"negative_timeout_seconds", c.NegativeTimeoutSeconds},
	} {
		if !(t.v >= 0 && t.v <= MaxFuseTimeoutSeconds) {
			return fmt.Errorf("%s is %v: it must be from 0 to %d seconds", t.key, t.v, MaxFuseTimeoutSeconds)
		}
	}
	return nil
}

// authKeys are the exact names of the auth settings.
var authKeys = map[string]bool{"auth_tokens_file": true, "auth_jwt": true, "auth_panel_https": true}

// knownKeys are the exact json names of the settings, and the one legacy name
// LoadConfig still reads (torrserver_url).
var knownKeys = func() map[string]bool {
	set := map[string]bool{"torrserver_url": true}
	var walk func(t reflect.Type)
	walk = func(t reflect.Type) {
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.Anonymous && f.Type.Kind() == reflect.Struct {
				walk(f.Type)
				continue
			}
			if !f.IsExported() {
				continue
			}
			// encoding/json names an untagged exported field by its Go name.
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			switch name {
			case "-":
			case "":
				set[f.Name] = true
			default:
				set[name] = true
			}
		}
	}
	walk(reflect.TypeOf(Config{}))
	return set
}()

// mentionsAuth reports whether a key, folded the way encoding/json folds keys, contains
// "auth": a key like auth-tokens-file or authTokensFile is a mistyped auth setting, and
// would otherwise only warn and leave the daemon open. (Neither letter of "auth" has a
// special fold partner, so a lower-casing is the fold.)
func mentionsAuth(key string) bool { return strings.Contains(strings.ToLower(key), "auth") }

// jwtKeys are the exact keys of the auth_jwt block.
var jwtKeys = map[string]bool{"issuer": true, "audience": true, "jwks_url": true, "header": true, "allowed_emails": true}

// checkExactKeys refuses an object that repeats a key or uses one that is not one of
// allowed, byte for byte: encoding/json keeps the last duplicate and matches keys
// without regard to case, so a repeated key could swap a setting silently.
func checkExactKeys(raw json.RawMessage, allowed map[string]bool) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil // not an object: the typed decode has already refused or accepted it
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, _ := tok.(string)
		if !allowed[key] {
			return fmt.Errorf("the key %q is not one of the block's exact keys", key)
		}
		if seen[key] {
			return fmt.Errorf("the key %q is given twice", key)
		}
		seen[key] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil
		}
	}
	return nil
}

// checkTopLevelKeys reads the keys of the top-level object byte for byte. It refuses a
// key that mentions auth (or is api_token in another spelling) but is not the exact
// setting, an auth key or api_token given twice, a null auth_tokens_file or api_token
// (null decodes as the empty string, which reads as "no credential"), and an auth_jwt
// block with a repeated or re-cased key. Any other key no setting names is returned
// for one warning. The data has already decoded, so the syntax is valid; it is read
// without regard to case or folding, which is the point.
func checkTopLevelKeys(data []byte) ([]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, nil
	}
	seen := map[string]bool{}
	var unknown []string
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, nil
		}
		key, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil
		}
		isNull := string(bytes.TrimSpace(raw)) == "null"
		switch {
		case authKeys[key] || key == "api_token":
			if seen[key] {
				return nil, fmt.Errorf("the credential key %q is given twice", key)
			}
			seen[key] = true
			if isNull && (key == "auth_tokens_file" || key == "api_token") {
				return nil, fmt.Errorf("%s is null: write a value, or remove the key", key)
			}
			if key == "auth_jwt" && !isNull {
				if err := checkExactKeys(raw, jwtKeys); err != nil {
					return nil, fmt.Errorf("auth_jwt: %w", err)
				}
			}
		case mentionsAuth(key):
			return nil, fmt.Errorf("the key %q looks like an auth setting but is not one (known: auth_tokens_file, auth_jwt, auth_panel_https)", key)
		case strings.EqualFold(key, "api_token"):
			return nil, fmt.Errorf("the key %q is api_token in another spelling: use the exact key", key)
		case !knownKeys[key]:
			unknown = append(unknown, key)
		}
	}
	sort.Strings(unknown)
	return unknown, nil
}

// stripJSONComments removes // comments from JSON data. It is string-aware: a //
// inside a JSON string (a URL, "/mnt//media", a token) is data, escapes included,
// and only a // outside a string starts a comment that runs to the end of the
// line. The newline is kept, so offsets and line numbers stay as in the file.
// Block comments were never supported and are not.
func stripJSONComments(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString, escaped := false, false
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
		case c == '"':
			inString = true
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, '\n')
			}
			continue
		}
		out = append(out, c)
	}
	return out
}

// CheckLoadable reports whether data, as written to config.json, would load:
// the same strip and unmarshal LoadConfig runs. The save path runs it on the
// bytes it is about to write, so a save can never produce a file the next start
// refuses.
func CheckLoadable(data []byte) error {
	var c Config
	return json.Unmarshal(stripJSONComments(data), &c)
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
		c.plexTokenFromEnv = true
	}
	if v := os.Getenv("FLODNI_API_TOKEN"); v != "" {
		c.APIToken = v
	}
	if v := os.Getenv("FLODNI_AUTH_TOKENS_FILE"); v != "" {
		c.AuthTokensFile = v
	}
	if v := os.Getenv("MKV_PROXY_ALLOWED_HOSTS"); v != "" {
		c.AllowedHosts = strings.Split(v, ",")
		c.hostsForce = true // set but with no entry (",") still means on, not off
	}
	if v := os.Getenv("MKV_PROXY_LOG_LEVEL"); v != "" {
		c.LogLevel = v
	}
	if v := os.Getenv("MKV_PROXY_LOG_FORMAT"); v != "" {
		c.LogFormat = v
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

// EffectiveAllowedHosts is the list the control server enforces: AllowedHosts, or
// only "localhost" when the list is on but has no valid entry (an empty list means
// off). Nil when the allowlist is off.
func (c *Config) EffectiveAllowedHosts() []string {
	if !c.hostsOn {
		return nil
	}
	out := normalizeAllowedHosts(c.AllowedHosts, false)
	if len(out) == 0 {
		out = []string{"localhost"}
	}
	return out
}

// EngineAllowedHosts is the list the engine's server enforces: the control list plus
// the hostname of gostorm_url, which flodni's own calls to the engine send as Host,
// so a name there cannot lock the daemon out of its engine. Nil when the allowlist
// is off. The control server does not need that name and does not get it.
func (c *Config) EngineAllowedHosts() []string {
	out := c.EffectiveAllowedHosts()
	if out == nil {
		return nil
	}
	// An IP literal and localhost always pass, so they add nothing to the set; leaving
	// them out keeps a change between two of them from reading as a change of the list.
	if u, err := url.Parse(c.GoStormBaseURL); err == nil && u.Hostname() != "" {
		h, _ := normalizeHost(u.Hostname())
		if _, isIP := netip.ParseAddr(h); isIP != nil && h != "localhost" && h != "" {
			out = normalizeAllowedHosts(append(out, h), false)
		}
	}
	return out
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
	switch strings.ToLower(strings.TrimSpace(c.LogFormat)) {
	case "", "text":
		c.LogFormat = "text"
	case "json":
		c.LogFormat = "json"
	default:
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: log_format %q is not \"text\" or \"json\"; using \"text\"", c.LogFormat), "log_format", c.LogFormat)
		c.LogFormat = "text"
	}
	if c.MediaServerType != "" && c.MediaServerType != "plex" {
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: media_server_type %q: Jellyfin is no longer supported and Flodni uses Plex", c.MediaServerType), "media_server_type", c.MediaServerType)
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
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: played_protection_days %d is negative; using 90", c.PlayedProtectionDays), "played_protection_days", c.PlayedProtectionDays)
		c.PlayedProtectionDays = 90
	}

	if c.WarmCatchupPerHour < 0 {
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: warm_catchup_per_hour %d is negative; using 20", c.WarmCatchupPerHour), "warm_catchup_per_hour", c.WarmCatchupPerHour)
		c.WarmCatchupPerHour = 20
	}

	if c.WarmCatchupScope != "all" && c.WarmCatchupScope != "retries" {
		if c.WarmCatchupScope != "" {
			flog.Std.Warn(fmt.Sprintf("[Config] WARNING: warm_catchup_scope %q is not \"all\" or \"retries\"; using \"all\"", c.WarmCatchupScope), "warm_catchup_scope", c.WarmCatchupScope)
		}
		c.WarmCatchupScope = "all"
	}

	// Above ~100,000 days the protection time overflows a duration and would turn
	// protection off by accident.
	if c.WarmupEvictionOrder != "written" && c.WarmupEvictionOrder != "read" {
		if c.WarmupEvictionOrder != "" {
			flog.Std.Warn(fmt.Sprintf("[Config] WARNING: warmup_eviction_order %q is not \"written\" or \"read\"; using \"written\"", c.WarmupEvictionOrder), "warmup_eviction_order", c.WarmupEvictionOrder)
		}
		c.WarmupEvictionOrder = "written"
	}
	if c.PlayedProtectionDays > 36500 {
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: played_protection_days %d is absurd; using 0 (never expire)", c.PlayedProtectionDays), "played_protection_days", c.PlayedProtectionDays)
		c.PlayedProtectionDays = 0
	}

	// The margin is a measurement threshold. Above 1 TiB (1<<20 MB) it is larger than
	// any file served here, so it is a typo, not a choice.
	if c.UnconfirmedReadMarginMB < 0 || c.UnconfirmedReadMarginMB > 1<<20 {
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: unconfirmed_read_margin_mb %d is out of range; using 64", c.UnconfirmedReadMarginMB), "unconfirmed_read_margin_m_b", c.UnconfirmedReadMarginMB)
		c.UnconfirmedReadMarginMB = 64
	}
	c.UnconfirmedReadMargin = c.UnconfirmedReadMarginMB * 1024 * 1024

	// Above 100 years the window is a typo, not a choice. 0 stays 0: warning off.
	if c.PlaybackWebhookWarnDays < 0 || c.PlaybackWebhookWarnDays > 36500 {
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: playback_webhook_warn_days %d is out of range; using 7", c.PlaybackWebhookWarnDays), "playback_webhook_warn_days", c.PlaybackWebhookWarnDays)
		c.PlaybackWebhookWarnDays = 7
	}

	// 0 stays 0: keep-alive off. Above a day the ceiling is a typo, not a choice.
	if c.PlaybackKeepaliveMinutes < 0 || c.PlaybackKeepaliveMinutes > 1440 {
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: playback_keepalive_minutes %d is out of range; using 15", c.PlaybackKeepaliveMinutes), "playback_keepalive_minutes", c.PlaybackKeepaliveMinutes)
		c.PlaybackKeepaliveMinutes = 15
	}
	if c.PlaybackKeepaliveMaxTorrents < 1 {
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: playback_keepalive_max_torrents %d is below 1; using 4", c.PlaybackKeepaliveMaxTorrents), "playback_keepalive_max_torrents", c.PlaybackKeepaliveMaxTorrents)
		c.PlaybackKeepaliveMaxTorrents = 4
	}

	// 0 stays 0: the schedule is off, a manual pass still works.
	if c.TrackerScrapeHours < 0 || c.TrackerScrapeHours > 8760 {
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: tracker_scrape_hours %d is out of range; using 24", c.TrackerScrapeHours), "tracker_scrape_hours", c.TrackerScrapeHours)
		c.TrackerScrapeHours = 24
	}
	if c.TrackerScrapeBatch < 1 || c.TrackerScrapeBatch > 200 {
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: tracker_scrape_batch %d is outside 1 to 200; using 100", c.TrackerScrapeBatch), "tracker_scrape_batch", c.TrackerScrapeBatch)
		c.TrackerScrapeBatch = 100
	}
	if c.TrackerScrapePaceMs < 100 || c.TrackerScrapePaceMs > 60000 {
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: tracker_scrape_pace_ms %d is outside 100 to 60000; using 1000", c.TrackerScrapePaceMs), "tracker_scrape_pace_ms", c.TrackerScrapePaceMs)
		c.TrackerScrapePaceMs = 1000
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
				flog.Std.Warn(fmt.Sprintf("[Config] WARNING: plex_path_map entry %q -> %q needs an absolute prefix and an absolute target; ignoring it", prefix, target), "prefix", prefix, "target", target)
				continue
			}
			clean := filepath.Clean(prefix)
			if _, dup := valid[clean]; dup {
				flog.Std.Warn(fmt.Sprintf("[Config] WARNING: plex_path_map has the prefix %q twice; keeping the first", clean), "clean", clean)
				continue
			}
			valid[clean] = filepath.Clean(target)
		}
		c.PlexPathMap = valid
	}

	c.hostsOn = c.hostsForce || hasEntry(c.AllowedHosts)
	c.AllowedHosts = normalizeAllowedHosts(c.AllowedHosts, true)
	if c.hostsOn && len(c.AllowedHosts) == 0 {
		flog.Std.Warn("[Config] WARNING: allowed_hosts has no valid entry (or is not a list); the allowlist stays ON and admits only IP literals, localhost, this machine's hostname and the gostorm_url host")
	}

	// Calculate MetadataCacheSize in bytes
	c.MetadataCacheSize = c.MetadataCacheSizeMB * 1024 * 1024
	if c.MetadataCacheSize < 1*1024*1024 {
		c.MetadataCacheSize = 1 * 1024 * 1024 // Min 1MB
	}

	c.StreamingThreshold = c.StreamingThresholdKB * 1024
	c.PreloadWorkers = c.PreloadWorkersCount
}

func warnBadHost(warn bool, raw string) {
	if warn {
		flog.Std.Warn(fmt.Sprintf("[Config] WARNING: allowed_hosts entry %q is not a bare hostname (no scheme, path, wildcard or leading dot); ignoring it", raw), "entry", raw)
	}
}

func hasEntry(in []string) bool {
	for _, e := range in {
		if strings.TrimSpace(e) != "" {
			return true
		}
	}
	return false
}

// HostsSaveChanged reports whether saving posted over this config (a fresh load of
// config.json) would change the engine allowlist a start enforces. It compares the
// derived list, so every input is covered: allowed_hosts, gostorm_url and the
// environment. A post whose list is absent keeps the file's; one equal to what the
// file holds, or to the running config's, is unchanged. Unless it changed, posted's
// allowed_hosts is replaced by the file's own text, so a hand edit survives a save.
func (c *Config) HostsSaveChanged(posted, running *Config) bool {
	if posted.AllowedHosts == nil {
		posted.AllowedHosts = c.fileAllowedHosts
	}
	p := *posted
	if p.GoStormBaseURL == "" { // absent: the file's, as for the list
		p.GoStormBaseURL = c.GoStormBaseURL
	}
	p.applyEnvOverrides() // a start applies the environment to whatever the file holds
	p.hostsOn = p.hostsForce || hasEntry(p.AllowedHosts)
	p.AllowedHosts = normalizeAllowedHosts(p.AllowedHosts, false)
	got := p.EngineAllowedHosts()
	if sameSet(got, c.EngineAllowedHosts()) || sameSet(got, running.EngineAllowedHosts()) {
		posted.AllowedHosts = c.fileAllowedHosts
		return false
	}
	return true
}

func sameSet(a, b []string) bool {
	if (a == nil) != (b == nil) {
		return false
	}
	m := map[string]bool{}
	for _, e := range a {
		m[e] = true
	}
	for _, e := range b {
		if !m[e] {
			return false
		}
		delete(m, e)
	}
	return len(m) == 0
}

// ValidateAllowedHosts returns an error naming the first non-blank entry that is
// not a bare hostname, so a post can be refused instead of saved.
func ValidateAllowedHosts(in []string) error {
	for _, raw := range in {
		if e := strings.TrimSpace(raw); e != "" {
			if _, ok := normalizeHost(e); !ok {
				return fmt.Errorf("allowed_hosts entry %q is not a bare hostname (no scheme, path, wildcard or leading dot)", raw)
			}
		}
	}
	return nil
}

// normalizeHost lowercases a host or host:port and strips port, brackets and one
// trailing dot. ok is false for anything that is not a bare hostname or IP literal.
func normalizeHost(e string) (string, bool) {
	if strings.ContainsAny(e, "/\\@*? #%") {
		return "", false
	}
	host := e
	if h, _, err := net.SplitHostPort(e); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" || strings.HasPrefix(host, ".") || strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return "", false
	}
	return host, true
}

// normalizeAllowedHosts returns the entries normalised (see normalizeHost) and
// de-duplicated, in order. A blank entry is skipped; one that is not a bare hostname
// is dropped with a warning (the allowlist then stays stricter, never wider). A
// non-ASCII name is kept but warned about: browsers send the xn-- form.
func normalizeAllowedHosts(in []string, warn bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, raw := range in {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		host, ok := normalizeHost(e)
		if !ok {
			warnBadHost(warn, raw)
			continue
		}
		if warn && !isASCII(host) {
			flog.Std.Warn(fmt.Sprintf("[Config] WARNING: allowed_hosts entry %q is not ASCII; a browser sends the xn-- form, so list that", raw), "entry", raw)
		}
		if !seen[host] {
			seen[host] = true
			out = append(out, host)
		}
	}
	return out
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

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
func (c *Config) LogConfig(logger *slog.Logger) {
	logger.Info("=== Configuration ===")
	logger.Info(fmt.Sprintf("Source: %s", c.ConfigPath), "config_path", c.ConfigPath)
	logger.Info(fmt.Sprintf("MasterConcurrencyLimit: %d", c.MasterConcurrencyLimit), "master_concurrency_limit", c.MasterConcurrencyLimit)
	logger.Info(fmt.Sprintf("ReadAheadBudget: %d MB", c.ReadAheadBudgetMB), "read_ahead_budget_m_b", c.ReadAheadBudgetMB)
	logger.Info(fmt.Sprintf("FUSE Block Size: %d", c.FuseBlockSize), "fuse_block_size", c.FuseBlockSize)
	logger.Info(fmt.Sprintf("StreamingThreshold: %d KB", c.StreamingThresholdKB), "streaming_threshold_k_b", c.StreamingThresholdKB)
	logger.Info(fmt.Sprintf("LogLevel: %s", c.LogLevel), "log_level", c.LogLevel)
	logger.Info(fmt.Sprintf("LogFormat: %s", c.LogFormat), "log_format", c.LogFormat)
	logger.Info(fmt.Sprintf("GoStormBaseURL: %s", c.GoStormBaseURL), "go_storm_base_u_r_l", c.GoStormBaseURL)
	logger.Info(fmt.Sprintf("FUSE Timeouts (Attr/Entry/Neg): %.1f/%.1f/%.1f", c.AttrTimeoutSeconds, c.EntryTimeoutSeconds, c.NegativeTimeoutSeconds))
	if c.FuseDisableSplice {
		logger.Info("FUSE splice: disabled")
	}
	if c.FuseMaxInflightMB > 0 {
		logger.Info(fmt.Sprintf("FUSE MaxInflightRequestBytes: %d MB", c.FuseMaxInflightMB), "fuse_max_inflight_m_b", c.FuseMaxInflightMB)
	} else {
		logger.Info("FUSE MaxInflightRequestBytes: unlimited")
	}
	logger.Info(fmt.Sprintf("HTTP Retries: %d, Delay: %dms", c.MaxRetryAttempts, c.RetryDelayMS), "max_retry_attempts", c.MaxRetryAttempts, "retry_delay_m_s", c.RetryDelayMS)
	logger.Info(fmt.Sprintf("Preload Engine: Workers=%d", c.PreloadWorkersCount), "preload_workers_count", c.PreloadWorkersCount)
	logger.Info(fmt.Sprintf("Network: MetricsPort=%d", c.MetricsPort), "metrics_port", c.MetricsPort)
	if c.hostsOn {
		logger.Info(fmt.Sprintf("Allowed hosts: [%s] (plus IP literals, localhost, this machine's hostname and the gostorm_url host)", strings.Join(c.AllowedHosts, ", ")), "allowed_hosts", c.AllowedHosts)
	} else {
		logger.Info("Allowed hosts: off (any Host is served; DNS rebinding is not stopped)")
	}
	logger.Info("=====================")
}
