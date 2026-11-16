package daemon

import (
	"encoding/json"
	"flodni/internal/flog"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"flodni/internal/apiauth"
	"flodni/internal/config"
	"flodni/internal/vfs"
	"flodni/internal/warmup"
)

func getStartup(t *testing.T, h http.Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/startup", nil))
	return rec
}

func TestStartupEndpointAnswers503BeforeTheReport(t *testing.T) {
	r := &startupReporter{}
	if rec := getStartup(t, http.HandlerFunc(r.handler)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code = %d, want 503", rec.Code)
	}
	// One stage alone is not enough.
	r.markMounted("/m")
	if rec := getStartup(t, http.HandlerFunc(r.handler)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code after the mount alone = %d, want 503", rec.Code)
	}
	rec := httptest.NewRecorder()
	r.handler(rec, httptest.NewRequest(http.MethodPost, "/api/startup", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d, want 405", rec.Code)
	}
}

// A simulated startup: the walk runs over a library with a ghost inode, an orphan
// readability row, an out-of-range stub and a stub the engine has no record for; the
// mount comes up after.
func TestStartupReportAfterASimulatedStartup(t *testing.T) {
	b, _ := pruneEnv(t)
	t.Setenv(stateUnmountedEnv, "")
	im := vfs.NewInodeMap(filepath.Join(t.TempDir(), "inodes.json"), flog.NewText(io.Discard))
	prev := globalInodeMap
	globalInodeMap = im
	t.Cleanup(func() { globalInodeMap = prev })
	im.AddFile("/ghost/movies/gone.mkv", orphHash, 1)

	b.foundHashes = map[string]bool{}
	const tvHash = "dddddddddddddddddddddddddddddddddddddddd"
	writeStub(t, filepath.Join(b.sourcePath, "movies", "Good_2000_1080p_cafe.mkv"), inLibHash, 1<<30)
	writeStub(t, filepath.Join(b.sourcePath, "movies", "Tiny_2001_1080p_cafe.mkv"), invalidHash, 1024)
	writeStub(t, filepath.Join(b.sourcePath, "tv", "Show", "S01E01_cafe.mkv"), tvHash, 1<<30)
	b.hasMetainfo = func(h string) bool { return h == inLibHash }

	var logs lockedBuilder
	r := &startupReporter{logf: flog.Logf(flog.NewText(&logs))}
	r.note(func(f *startupFacts) {
		f.stateDBEnabled, f.stateDBPath, f.stateDBMigration = true, "/state/x.db", migrationMigrated
		f.inodeFilesLoaded = 4
	})
	done := make(chan struct{})
	b.onDone = func(s scanResult) { r.markScanned(s); close(done) }
	b.Start()
	<-done
	if rec := getStartup(t, http.HandlerFunc(r.handler)); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("code before the mount = %d, want 503", rec.Code)
	}
	r.markMounted("/mnt/virtual")

	rec := getStartup(t, http.HandlerFunc(r.handler))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("code %d type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	w := warmup.Started() // another test in the package may have started a cache
	want := map[string]any{
		"schema":      float64(1),
		"version":     AppVersion,
		"config_path": "",
		"warmup": map[string]any{"active": w.Active, "dir": w.Dir, "initial_size_bytes": float64(w.InitialSizeBytes),
			"dropped_stale": float64(w.Dropped), "residues_dropped": float64(w.Residues)},
		"state_db":         map[string]any{"active": true, "path": "/state/x.db", "migration": "migrated"},
		"inode_map":        map[string]any{"files_loaded": float64(4)},
		"fuse":             map[string]any{"mount_path": "/mnt/virtual", "mounted": true},
		"library_unstored": map[string]any{"n": float64(2), "of": float64(3)},
		"prunes": map[string]any{
			"readability": map[string]any{"status": "ran", "rows_pruned": float64(1)},
			"file_state":  map[string]any{"status": "ran", "rows_pruned": float64(0)},
			"inodes":      map[string]any{"status": "ran", "rows_pruned": float64(1)},
		},
		"invalid_stubs": float64(1),
		"alarms":        []any{"statedb_migrated", "ghost_inodes_pruned"},
		// No auth configured: open mode, nothing in use.
		"auth": map[string]any{"mode": "open", "tokens": float64(0), "scopes": []any{}, "jwt": false},
	}
	for k, v := range want {
		if !reflect.DeepEqual(got[k], v) {
			t.Errorf("%s = %v, want %v", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("report has %d keys, want %d: %v", len(got), len(want), got)
	}

	// The same object is the one log line.
	if line := strings.TrimSpace(logs.String()); line != "[Startup] report "+rec.Body.String() {
		t.Fatalf("log line %q\nbody %q", line, rec.Body.String())
	}
	if strings.Contains(strings.TrimSpace(rec.Body.String()), "\n") {
		t.Fatalf("the report must be one line: %q", rec.Body.String())
	}
	// A late stage does not emit a second line.
	r.markMounted("/mnt/virtual")
	if n := strings.Count(logs.String(), "[Startup] report"); n != 1 {
		t.Fatalf("%d report lines, want 1", n)
	}
}

// One directory without stubs makes the readability and file-state prunes skip; the
// report says so with the reason the guard logged, and the inode prune (which needs
// only both directories to exist) still runs.
func TestStartupReportNamesTheEmptyDirectory(t *testing.T) {
	b, _ := pruneEnv(t)
	t.Setenv(stateUnmountedEnv, "")
	im := vfs.NewInodeMap(filepath.Join(t.TempDir(), "inodes.json"), flog.NewText(io.Discard))
	prev := globalInodeMap
	globalInodeMap = im
	t.Cleanup(func() { globalInodeMap = prev })

	b.foundHashes = map[string]bool{}
	b.hasMetainfo = func(string) bool { return true }
	if err := os.MkdirAll(filepath.Join(b.sourcePath, "movies"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, filepath.Join(b.sourcePath, "tv", "Show", "S01E01_cafe.mkv"), inLibHash, 1<<30)
	var logs lockedBuilder
	b.logger = flog.NewText(&logs)
	r := &startupReporter{logf: flog.Logf(flog.NewText(&logs))}
	done := make(chan struct{})
	b.onDone = func(s scanResult) { r.markScanned(s); close(done) }
	b.Start()
	<-done
	r.markMounted("/mnt/virtual")

	rec := getStartup(t, http.HandlerFunc(r.handler))
	var rep startupReport
	if err := json.Unmarshal(rec.Body.Bytes(), &rep); err != nil {
		t.Fatal(err)
	}
	const reason = "movies holds no stubs (complete=false errors=0 stubs=1)"
	for name, p := range map[string]pruneResult{"readability": rep.Prunes.Readability, "file_state": rep.Prunes.FileState} {
		if p.Status != "skipped" || p.Reason != reason {
			t.Errorf("%s = %+v, want skipped with %q", name, p, reason)
		}
	}
	if rep.Prunes.Inodes.Status != "ran" {
		t.Errorf("inodes = %+v, want ran", rep.Prunes.Inodes)
	}
	for _, line := range []string{
		"Startup cache: movies holds no stubs",
		"Readability prune skipped: " + reason,
		"File state prune skipped: " + reason,
	} {
		if !strings.Contains(logs.String(), line+"\n") {
			t.Errorf("log lacks %q:\n%s", line, logs.String())
		}
	}
	if !strings.Contains(logs.String(), "[Startup] report "+rec.Body.String()+"\n") || strings.Count(logs.String(), "[Startup] report") != 1 {
		t.Errorf("the report line must be the body, once:\n%s", logs.String())
	}
}

type lockedBuilder struct {
	mu sync.Mutex
	sb strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sb.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sb.String()
}

func TestStartupReportSkippedPrunesAndAlarms(t *testing.T) {
	t.Setenv(stateUnmountedEnv, "1")
	skip := pruneResult{Status: pruneSkipped, Reason: pruneReason(false, 0, 3, ""), guarded: true}
	rep := buildStartupReport(startupFacts{
		configParseFailed: true,
		pathsDiffer:       true,
		stateDBEnabled:    true,
		stateDBOpenFailed: true,
		stateDBMigration:  migrationFailed,
		scan:              scanResult{readability: skip, inodes: pruneResult{Status: pruneSkipped, Reason: "inode map not initialised"}},
	})
	want := []string{"config_parse_failed", "config_paths_differ", "statedb_open_failed", "prune_skipped", "state_not_on_mounted_volume"}
	if !reflect.DeepEqual(rep.Alarms, want) {
		t.Fatalf("alarms = %v, want %v", rep.Alarms, want)
	}
	if rep.Prunes.Readability.Reason != "complete=false errors=0 stubs=3" || rep.StateDB.Migration != "failed" {
		t.Fatalf("report = %+v", rep)
	}
	if rep.LibraryUnstored != nil {
		t.Fatal("library_unstored must be null when the count did not run")
	}

	t.Setenv(stateUnmountedEnv, "")
	off := buildStartupReport(startupFacts{})
	if off.StateDB.Migration != migrationDisabled || off.StateDB.Active || len(off.Alarms) != 0 {
		t.Fatalf("state DB off: %+v", off)
	}
	if b, _ := json.Marshal(off); !strings.Contains(string(b), `"alarms":[]`) || !strings.Contains(string(b), `"library_unstored":null`) {
		t.Fatalf("empty alarms must be [] and the count null: %s", b)
	}
}

func TestStartupEndpointNeedsTheToken(t *testing.T) {
	r := &startupReporter{}
	r.markMounted("/m")
	r.markScanned(scanResult{})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/startup", r.handler)
	h := apiauth.Middleware("secret", nil, mux)

	do := func(target, auth string) int {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if c := do("/api/startup", ""); c != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", c)
	}
	if c := do("/api/startup", "Bearer wrong"); c != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", c)
	}
	if c := do("/api/startup?token=secret", ""); c != http.StatusUnauthorized {
		t.Errorf("query token = %d, want 401: only the webhook routes take one", c)
	}
	if c := do("/api/startup", "Bearer secret"); c != http.StatusOK {
		t.Errorf("right token = %d, want 200", c)
	}
}

// The startup lines an operator greps for. A logging change must keep these texts, or
// say so; the report at /api/startup is the stable interface.
func TestStartupLineTextsArePinned(t *testing.T) {
	if got, want := fmt.Sprintf(stateDBActiveFmt, "/s/x.db"), "[StateDB] Active: /s/x.db"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := fmt.Sprintf(stateDBOpenFailedFmt, io.EOF); !strings.Contains(got, "Failed to open StateDB") {
		t.Errorf("got %q", got)
	}
	if got, want := fmt.Sprintf(fuseMountedFmt, "/m"), "FUSE mounted at /m with VirtualMkvRoot, all systems active"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if startupReportFmt != "[Startup] report %s" {
		t.Errorf("report line format = %q", startupReportFmt)
	}
}

func TestPathsDifferWarningIsPinned(t *testing.T) {
	var cfg config.Config
	cfg.PhysicalSourcePath = "/cfg/real"
	got := applyEffectivePaths(&cfg, "/cli/real", "/cli/virtual")
	if len(got) != 1 || !strings.Contains(got[0], `physical_source_path "/cfg/real" differs from the mounted source path "/cli/real"`) {
		t.Fatalf("warnings = %q", got)
	}
}

func TestPruneAndUnstoredLinesArePinned(t *testing.T) {
	var out strings.Builder
	lg := flog.NewText(&out)
	pruneGuard(lg, "Readability", false, 0, 1, nil)
	if got, want := strings.TrimSpace(out.String()), "Readability prune skipped: complete=false errors=0 stubs=1"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	b, _ := pruneInodeEnv(t)
	var logs strings.Builder
	b.logger = flog.NewText(&logs)
	b.pruneInodes(true)
	if got, want := strings.TrimSpace(logs.String()), "Startup GC: Pruned 1 ghost inodes"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	ub, ulogs := unstoredEnv(t, map[string]bool{strings.Repeat("a", 40): true, strings.Repeat("b", 40): true},
		strings.Repeat("a", 40), strings.Repeat("b", 40))
	ub.warnUnstored()
	if got, want := strings.TrimSpace(ulogs.String()), "Library torrents with no stored engine record with metainfo: 0 of 2"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	ub, ulogs = unstoredEnv(t, nil, strings.Repeat("a", 40))
	ub.warnUnstored()
	if !strings.Contains(ulogs.String(), "WARNING: 1 of 1 library torrents have no stored engine record with metainfo (first 1: ") {
		t.Errorf("got %q", ulogs.String())
	}
}

// The "is NOT on a mounted volume" warning lives in the container entrypoint, not in Go:
// pin its text, and the variable the daemon reads for the alarm.
func TestEntrypointWarningIsPinned(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docker", "docker-entrypoint.sh"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{`is NOT on a mounted volume.`, "export " + stateUnmountedEnv + "=1"} {
		if !strings.Contains(s, want) {
			t.Errorf("docker-entrypoint.sh lacks %q", want)
		}
	}
}

// The auth section in all three modes: the line and the endpoint body are the same
// bytes, every key of the v0.2.6 report is still there with its type, and nothing in
// it carries a secret, a name, a hash or a path.
func TestStartupReportAuthSectionInEveryMode(t *testing.T) {
	good := fakeAuthToken('a')
	file := filepath.Join(t.TempDir(), "tokens-secret-path.json")
	body := `{"tokens":[` +
		`{"name":"hooknamezz","scopes":["webhook"],"sha256":"` + authHash(good) + `"},` +
		`{"name":"opsnamezz","scopes":["read","admin"],"sha256":"` + authHash(fakeAuthToken('b')) + `"}]}`
	if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	load := func(o apiauth.LoadOptions) *apiauth.Tokens {
		o.GoStormURL = "http://127.0.0.1:8090"
		tok, err := apiauth.Load(o)
		if err != nil {
			t.Fatal(err)
		}
		return tok
	}
	modes := []struct {
		name string
		tok  *apiauth.Tokens
		want map[string]any
	}{
		{"open", load(apiauth.LoadOptions{}),
			map[string]any{"mode": "open", "tokens": float64(0), "scopes": []any{}, "jwt": false}},
		{"legacy_token", load(apiauth.LoadOptions{APIToken: "legacy-secret-value"}),
			map[string]any{"mode": "legacy_token", "tokens": float64(1), "jwt": false,
				"scopes": []any{"admin", "engine", "library", "metrics", "read", "scrape", "upload", "webhook"}}},
		{"scoped", load(apiauth.LoadOptions{TokensFile: file}),
			map[string]any{"mode": "scoped", "tokens": float64(2), "jwt": false,
				"scopes": []any{"admin", "read", "webhook"}}},
	}
	// The keys the v0.2.6 report had, with their types.
	v026 := map[string]string{"schema": "float64", "version": "string", "config_path": "string", "warmup": "map",
		"state_db": "map", "inode_map": "map", "fuse": "map", "prunes": "map", "invalid_stubs": "float64", "alarms": "slice"}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			var logs lockedBuilder
			r := &startupReporter{logf: flog.Logf(flog.NewText(&logs))}
			r.noteAuth(m.tok)
			r.markMounted("/mnt/virtual")
			r.markScanned(scanResult{})
			rec := getStartup(t, http.HandlerFunc(r.handler))
			if rec.Code != http.StatusOK {
				t.Fatalf("code %d", rec.Code)
			}
			if line := strings.TrimSpace(logs.String()); line != "[Startup] report "+rec.Body.String() {
				t.Fatalf("log line and body differ:\n%q\n%q", line, rec.Body.String())
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got["schema"] != float64(1) {
				t.Errorf("schema = %v, want 1: the section is additive", got["schema"])
			}
			for k, typ := range v026 {
				v, ok := got[k]
				if !ok {
					t.Errorf("v0.2.6 key %q is gone", k)
					continue
				}
				var have string
				switch v.(type) {
				case float64:
					have = "float64"
				case string:
					have = "string"
				case map[string]any:
					have = "map"
				case []any:
					have = "slice"
				}
				if have != typ {
					t.Errorf("key %q is %s, was %s", k, have, typ)
				}
			}
			if !reflect.DeepEqual(got["auth"], m.want) {
				t.Errorf("auth = %v, want %v", got["auth"], m.want)
			}
			for _, secret := range []string{"hooknamezz", "opsnamezz", "legacy-secret-value", good, authHash(good), "tokens-secret-path"} {
				if strings.Contains(rec.Body.String(), secret) || strings.Contains(logs.String(), secret) {
					t.Errorf("the report holds %q", secret)
				}
			}
		})
	}
}

// A report built before the auth stage noted anything reads as open mode.
func TestStartupReportAuthDefaultsToOpen(t *testing.T) {
	rep := buildStartupReport(startupFacts{})
	b, _ := json.Marshal(rep.Auth)
	if string(b) != `{"mode":"open","tokens":0,"scopes":[],"jwt":false}` {
		t.Fatalf("auth = %s", b)
	}
}
