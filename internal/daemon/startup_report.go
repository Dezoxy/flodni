package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"sync/atomic"

	"flodni/internal/apiauth"
	"flodni/internal/config"
	"flodni/internal/gostorm/settings"
	"flodni/internal/warmup"
)

// startupSchema versions the startup report. Adding a field keeps it; renaming or
// removing one, or changing a meaning, raises it.
const startupSchema = 1

// Prune statuses.
const (
	pruneRan     = "ran"
	pruneSkipped = "skipped"
	pruneFailed  = "failed"
)

// Migration results of the state database.
const (
	migrationAlreadyCompleted = "already_completed"
	migrationMigrated         = "migrated"
	migrationFailed           = "failed" // the open or the migration failed; the daemon runs on JSON
	migrationDisabled         = "disabled"
)

// stateUnmountedEnv is set to 1 by the container entrypoint when the state directory is
// not on a mounted volume (the "is NOT on a mounted volume" warning).
const stateUnmountedEnv = "FLODNI_STATE_UNMOUNTED"

// pruneResult is the outcome of one startup prune.
type pruneResult struct {
	Status     string `json:"status"` // ran | skipped | failed
	Reason     string `json:"reason,omitempty"`
	RowsPruned int    `json:"rows_pruned"`
	guarded    bool   // skipped by the walk guard: the "prune skipped" log line
}

// scanResult is what the startup walk and its prunes leave behind.
type scanResult struct {
	invalidStubs int
	unstoredRan  bool
	unstoredN    int
	unstoredOf   int
	readability  pruneResult
	fileState    pruneResult
	inodes       pruneResult
}

// startupFacts are the stages' own notes; the rest of the report is read from where the
// code already holds it when the report is built.
type startupFacts struct {
	configParseFailed bool
	pathsDiffer       bool
	stateDBEnabled    bool
	stateDBPath       string
	stateDBOpenFailed bool
	stateDBMigration  string // "" until the state database stage ran
	inodeFilesLoaded  int
	mountPath         string
	mounted           bool
	scanned           bool
	scan              scanResult
	auth              *apiauth.Tokens // nil reads as open mode
}

// The report as served and logged.
type startupReport struct {
	Schema     int    `json:"schema"`
	Version    string `json:"version"`
	ConfigPath string `json:"config_path"`
	Warmup     struct {
		Active           bool   `json:"active"`
		Dir              string `json:"dir"`
		InitialSizeBytes int64  `json:"initial_size_bytes"`
		DroppedStale     int    `json:"dropped_stale"`
		ResiduesDropped  int    `json:"residues_dropped"`
	} `json:"warmup"`
	StateDB struct {
		Active    bool   `json:"active"`
		Path      string `json:"path"`
		Migration string `json:"migration"`
	} `json:"state_db"`
	InodeMap struct {
		FilesLoaded int `json:"files_loaded"`
	} `json:"inode_map"`
	FUSE struct {
		MountPath string `json:"mount_path"`
		Mounted   bool   `json:"mounted"`
	} `json:"fuse"`
	// Null when the count did not run (no stub found).
	LibraryUnstored *struct {
		N  int `json:"n"`
		Of int `json:"of"`
	} `json:"library_unstored"`
	Prunes struct {
		Readability pruneResult `json:"readability"`
		FileState   pruneResult `json:"file_state"`
		Inodes      pruneResult `json:"inodes"`
	} `json:"prunes"`
	InvalidStubs int      `json:"invalid_stubs"`
	Alarms       []string `json:"alarms"`
	// Auth is additive (schema stays 1): the mode, the token count and the scopes in
	// use. It never holds a name, a hash, a path or a key.
	Auth apiauth.Report `json:"auth"`
}

// startupReporter collects the startup stages and emits the report once, when the mount
// is up and the startup walk with its prunes has finished.
type startupReporter struct {
	mu    sync.Mutex
	facts startupFacts
	logf  func(format string, args ...any)
	doc   atomic.Pointer[[]byte]
}

// note records facts from a stage.
func (r *startupReporter) note(f func(*startupFacts)) {
	r.mu.Lock()
	f(&r.facts)
	r.mu.Unlock()
}

func (r *startupReporter) markMounted(path string) {
	r.note(func(f *startupFacts) { f.mountPath, f.mounted = path, true })
	r.tryEmit()
}

func (r *startupReporter) markScanned(s scanResult) {
	r.note(func(f *startupFacts) { f.scan, f.scanned = s, true })
	r.tryEmit()
}

// tryEmit builds, stores and logs the report the first time both stages are done.
func (r *startupReporter) tryEmit() {
	r.mu.Lock()
	if !r.facts.mounted || !r.facts.scanned || r.doc.Load() != nil {
		r.mu.Unlock()
		return
	}
	facts := r.facts
	r.mu.Unlock()

	body, err := json.Marshal(buildStartupReport(facts))
	if err != nil {
		if r.logf != nil {
			r.logf("[Startup] report not built: %v", err)
		}
		return
	}
	if !r.doc.CompareAndSwap(nil, &body) {
		return
	}
	if r.logf != nil {
		r.logf(startupReportFmt, body)
	}
}

// startupReportFmt is the one log line; the object after it is the /api/startup body.
const startupReportFmt = "[Startup] report %s"

// buildStartupReport joins the stages' notes with what the code already holds.
func buildStartupReport(f startupFacts) startupReport {
	var rep startupReport
	rep.Schema = startupSchema
	rep.Version = AppVersion
	if c := gc(); c != nil {
		rep.ConfigPath = c.ConfigPath
	}
	w := warmup.Started()
	rep.Warmup.Active, rep.Warmup.Dir = w.Active, w.Dir
	rep.Warmup.InitialSizeBytes = w.InitialSizeBytes
	rep.Warmup.DroppedStale, rep.Warmup.ResiduesDropped = w.Dropped, w.Residues

	rep.StateDB.Active = stateDB != nil
	rep.StateDB.Path = f.stateDBPath
	rep.StateDB.Migration = f.stateDBMigration
	if !f.stateDBEnabled {
		rep.StateDB.Migration = migrationDisabled
	}
	rep.InodeMap.FilesLoaded = f.inodeFilesLoaded
	rep.FUSE.MountPath, rep.FUSE.Mounted = f.mountPath, f.mounted
	if f.scan.unstoredRan {
		rep.LibraryUnstored = &struct {
			N  int `json:"n"`
			Of int `json:"of"`
		}{f.scan.unstoredN, f.scan.unstoredOf}
	}
	rep.Prunes.Readability = f.scan.readability
	rep.Prunes.FileState = f.scan.fileState
	rep.Prunes.Inodes = f.scan.inodes
	rep.InvalidStubs = f.scan.invalidStubs
	rep.Auth = f.auth.Report()

	rep.Alarms = []string{}
	for _, a := range []struct {
		id string
		on bool
	}{
		{"config_parse_failed", f.configParseFailed},
		{"config_paths_differ", f.pathsDiffer},
		{"settings_migrated", settings.SettingsMigrated.Load()},
		{"settings_corrupt", settings.SettingsQuarantined.Load()},
		{"torrents_migrated", settings.TorrentsMigrated.Load() > 0},
		{"head_density_check", w.HeadsChecked > 0},
		{"corrupt_cache", warmup.CorruptRemoved.Load() > 0},
		{"statedb_migrated", f.stateDBMigration == migrationMigrated},
		{"statedb_open_failed", f.stateDBOpenFailed},
		{"statedb_migration_failed", f.stateDBMigration == migrationFailed && !f.stateDBOpenFailed},
		{"prune_skipped", f.scan.readability.guarded || f.scan.fileState.guarded || f.scan.inodes.guarded},
		{"ghost_inodes_pruned", f.scan.inodes.RowsPruned > 0},
		{"state_not_on_mounted_volume", os.Getenv(stateUnmountedEnv) == "1"},
	} {
		if a.on {
			rep.Alarms = append(rep.Alarms, a.id)
		}
	}
	return rep
}

// handler serves GET /api/startup: 503 until the report exists. The control server's
// token gate covers it like every route but the panel page and /api/health.
func (r *startupReporter) handler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body := r.doc.Load()
	if body == nil {
		http.Error(w, "startup report not ready", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(*body)
}

// noteAuth records the auth mode the start-up settled on.
func (r *startupReporter) noteAuth(t *apiauth.Tokens) {
	r.note(func(f *startupFacts) { f.auth = t })
}

// noteConfigLoad records what the config load and the path override found.
func (r *startupReporter) noteConfigLoad(warnings []string) {
	r.note(func(f *startupFacts) {
		f.configParseFailed = config.ParseFailed.Load()
		f.pathsDiffer = len(warnings) > 0
	})
}
