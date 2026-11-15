package daemon

import (
	"encoding/json"
	"flodni/internal/atomicfile"
	"flodni/internal/config"
	"fmt"
	"net/http"
	"os"
	"syscall"
	"time"
)

// handleConfig serves GET and POST /api/config.
func handleConfig(w http.ResponseWriter, r *http.Request) {
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
		// No token enforced: any client that reaches the port could choose the token
		// the next start enforces. Setting it needs the environment or the file, so a
		// posted token is refused whatever its value (comparing it with a pending one
		// would answer guesses about that one).
		if app.controlToken == "" && newCfg.APIToken != "" {
			refuseOpenPort(w, r, http.StatusForbidden,
				"api_token cannot be set or changed through the API while no token is enforced"+setTokenHint)
			return
		}
		onDisk := config.LoadConfig()
		if app.controlToken == "" {
			// A file that does not parse would be replaced by defaults, dropping a token
			// the owner is typing into it.
			if config.ParseFailed.Load() {
				refuseOpenPort(w, r, http.StatusConflict,
					"config.json does not parse; fix it before saving through the API")
				return
			}
			// allowed_hosts is the same kind of setting: a client could clear or widen
			// the list the next start enforces. A changed list is refused; an absent or
			// equal one saves as before.
			if _, changed := onDisk.ResolveAllowedHosts(newCfg.AllowedHosts); changed {
				refuseOpenPort(w, r, http.StatusForbidden,
					"allowed_hosts cannot be changed through the API while no token is enforced: "+
						"set it through MKV_PROXY_ALLOWED_HOSTS or by editing config.json, then restart flodni")
				return
			}
			// Keep the token the file holds now, not the one read at start: the owner may
			// have added one by hand and not restarted yet.
			newCfg.APIToken = onDisk.ResolveAPIToken("")
		} else {
			// The panel never sees the token; an empty post keeps the one in the file.
			newCfg.APIToken = gc().ResolveAPIToken(newCfg.APIToken)
		}
		// A panel save keeps the list as the file holds it, never the environment's.
		if resolved, changed := onDisk.ResolveAllowedHosts(newCfg.AllowedHosts); !changed {
			newCfg.AllowedHosts = resolved
		}
		// Paths that came from the command line are not written to the
		// file: keep what the file had.
		if newCfg.PhysicalSourcePath == physicalSourcePath {
			newCfg.PhysicalSourcePath = onDisk.PhysicalSourcePath
		}
		if newCfg.FuseMountPath == virtualMountPath {
			newCfg.FuseMountPath = onDisk.FuseMountPath
		}
		// Update file
		data, _ := json.MarshalIndent(newCfg, "", "  ")
		if err := atomicfile.WriteFileKeepMode(gc().ConfigPath, data, 0600); err != nil {
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
		applyLogConfig()
		logger.Info("[Config] Updated via Dashboard API")
		w.WriteHeader(200)
	}
}

// setTokenHint says where a token is set when the API may not set it.
const setTokenHint = ": set api_token through the FLODNI_API_TOKEN environment variable " +
	"or by editing config.json, then restart flodni"

// refuseOpenPort answers a refused open-port request with a JSON error and logs it the
// way the bearer middleware logs a 401 (method, path, address).
func refuseOpenPort(w http.ResponseWriter, r *http.Request, status int, msg string) {
	logger.Warn(fmt.Sprintf("[Auth] refused %s %s from %s: %s", r.Method, r.URL.Path, r.RemoteAddr, msg),
		"method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr, "status", status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// restartSelf asks the process to stop gracefully; a test replaces it.
var restartSelf = func() {
	restartRequested.Store(true)
	go func() {
		time.Sleep(150 * time.Millisecond)
		p, _ := os.FindProcess(os.Getpid())
		if err := p.Signal(syscall.SIGTERM); err != nil {
			logger.Warn(fmt.Sprintf("WARNING: restart requested but SIGTERM to self failed: %v", err), "err", err)
		}
	}()
}

// handleRestart serves POST /api/restart.
func handleRestart(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if app.controlToken == "" {
		refuseOpenPort(w, r, http.StatusForbidden, "restart is refused while no token is enforced"+setTokenHint)
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
	restartSelf()
}
