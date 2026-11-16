package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"flodni/internal/config"
)

// healthCheckTimeout bounds each check. A hung FUSE mount blocks stat and
// readdir in the kernel, so a check that cannot answer in time reports
// "timeout" instead of holding the handler (and the container health check).
const healthCheckTimeout = 2 * time.Second

// checkResult is one check's outcome. Reason is a fixed word, never an error
// string: errors carry paths and URLs, and /api/health is open without a token.
type checkResult struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason"`
}

// healthChecks holds the three checks as fields so tests can stub them.
type healthChecks struct {
	mount, engine, stateDB func() checkResult
	timeout                time.Duration
	// warnings lists the advisory warnings (nil = none). It must read memory only: it
	// is not a check, and it never changes the status or the HTTP code.
	warnings func() []string
}

type healthReport struct {
	Status string                 `json:"status"`
	Checks map[string]checkResult `json:"checks"`
	// Warnings is always present, [] when clear. Fixed words only: the endpoint is open.
	Warnings []string `json:"warnings"`
}

// runCheck runs fn in its own goroutine and gives up after d. A check stuck in
// the kernel keeps its goroutine, which is the price of not hanging the caller.
func runCheck(d time.Duration, fn func() checkResult) checkResult {
	done := make(chan checkResult, 1)
	go func() { done <- fn() }()
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case r := <-done:
		return r
	case <-t.C:
		return checkResult{Reason: "timeout"}
	}
}

// healthStatus: down when the mount fails (nothing can be read), degraded when
// only the engine or the state database fails, ok otherwise.
func healthStatus(mount, engine, db checkResult) string {
	switch {
	case !mount.OK:
		return "down"
	case !engine.OK || !db.OK:
		return "degraded"
	}
	return "ok"
}

func (h healthChecks) report() healthReport {
	fns := map[string]func() checkResult{"mount": h.mount, "engine": h.engine, "state_db": h.stateDB}
	type named struct {
		name string
		res  checkResult
	}
	out := make(chan named, len(fns))
	for name, fn := range fns {
		go func() { out <- named{name, runCheck(h.timeout, fn)} }()
	}
	checks := make(map[string]checkResult, len(fns))
	for range fns {
		n := <-out
		checks[n.name] = n.res
	}
	warnings := []string{}
	if h.warnings != nil {
		warnings = append(warnings, h.warnings()...)
	}
	return healthReport{
		Status:   healthStatus(checks["mount"], checks["engine"], checks["state_db"]),
		Checks:   checks,
		Warnings: warnings,
	}
}

// handler serves GET /api/health: 200 when ok, 503 otherwise.
func (h healthChecks) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rep := h.report()
	code := http.StatusOK
	if rep.Status != "ok" {
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(rep)
}

// newHealthChecks wires the real checks. The engine check is HTTP, not
// in-process: GoStorm keeps its torrent session private, and GET /echo also
// proves the port the library and the bridge's callers use is answering.
func newHealthChecks() healthChecks {
	return healthChecks{
		mount:    checkMount,
		engine:   checkEngine,
		stateDB:  checkStateDB,
		timeout:  healthCheckTimeout,
		warnings: activeWarnings,
	}
}

// checkMount: the virtual mount path is a mountpoint (its device differs from
// its parent's) and its root can be opened and listed.
func checkMount() checkResult {
	mnt := virtualMountPath
	if mnt == "" {
		return checkResult{Reason: "not configured"}
	}
	root, err := os.Stat(mnt)
	if err != nil {
		return checkResult{Reason: "stat failed"}
	}
	parent, err := os.Stat(filepath.Join(mnt, ".."))
	if err != nil {
		return checkResult{Reason: "stat failed"}
	}
	rs, ok1 := root.Sys().(*syscall.Stat_t)
	ps, ok2 := parent.Sys().(*syscall.Stat_t)
	if !ok1 || !ok2 || uint64(rs.Dev) == uint64(ps.Dev) { //nolint:unconvert // Dev is int32 on some platforms
		return checkResult{Reason: "not a mountpoint"}
	}
	f, err := os.Open(mnt)
	if err != nil {
		return checkResult{Reason: "open failed"}
	}
	defer func() { _ = f.Close() }() // read-only handle
	if _, err := f.Readdirnames(1); err != nil && !errors.Is(err, io.EOF) {
		return checkResult{Reason: "list failed"}
	}
	return checkResult{OK: true, Reason: "ok"}
}

// checkEngine asks GoStorm's GET /echo, which stays open when a token is set.
func checkEngine() checkResult {
	base := strings.TrimRight(gc().GoStormBaseURL, "/")
	if base == "" {
		return checkResult{Reason: "not configured"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/echo", nil)
	if err != nil {
		return checkResult{Reason: "bad url"}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return checkResult{Reason: "unreachable"}
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<10)) // drain for connection reuse
	_ = resp.Body.Close()                                        // body is read; nothing to act on
	if resp.StatusCode != http.StatusOK {
		return checkResult{Reason: fmt.Sprintf("status %d", resp.StatusCode)}
	}
	return checkResult{OK: true, Reason: "ok"}
}

// checkStateDB runs SELECT 1 when the state database is on.
func checkStateDB() checkResult {
	if !gc().EnableStateDB {
		return checkResult{OK: true, Reason: "disabled"}
	}
	if stateDB == nil {
		// Enabled but failed to open at startup: running on the JSON fallback.
		return checkResult{Reason: "not open"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), healthCheckTimeout)
	defer cancel()
	var one int
	if err := stateDB.SQL().QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil {
		return checkResult{Reason: "query failed"}
	}
	return checkResult{OK: true, Reason: "ok"}
}

// healthcheckMain is `flodni healthcheck`: it asks the running daemon's
// /api/health on the control port and returns the exit code. It reads the
// config for the port only; it starts nothing and touches no mount.
func healthcheckMain() int {
	log.SetOutput(io.Discard) // LoadConfig logs through the standard logger
	return runHealthcheck(healthcheckURL())
}

// healthcheckURL is the daemon's /api/health on the configured control port. A
// config file that cannot be read or parsed (an owner mid-edit) is not an
// unhealthy daemon: the probe falls back to the default port, which is the one
// a running daemon would have kept.
func healthcheckURL() string {
	cfg, err := config.LoadConfig()
	port := cfg.MetricsPort
	if err != nil {
		port = defaultControlPort
	}
	return fmt.Sprintf("http://127.0.0.1:%d/api/health", port)
}

// defaultControlPort is metrics_port when the file does not set it.
const defaultControlPort = 9080

// runHealthcheck GETs url within 5 s, prints one line and returns 0 on HTTP
// 200, 1 otherwise.
func runHealthcheck(url string) int {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Println("unhealthy: no answer from the control server")
		return 1
	}
	defer func() { _ = resp.Body.Close() }() // body is read below; nothing to act on
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	line := strings.Join(strings.Fields(string(body)), " ")
	if resp.StatusCode != http.StatusOK {
		fmt.Printf("unhealthy: HTTP %d %s\n", resp.StatusCode, line)
		return 1
	}
	fmt.Printf("healthy: %s\n", line)
	return 0
}
