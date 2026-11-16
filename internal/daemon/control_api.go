package daemon

import (
	"encoding/json"
	"flodni/internal/apiauth"
	"flodni/internal/atomicfile"
	"flodni/internal/config"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"
)

// handleConfig serves GET and POST /api/config.
func handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		w.Header().Set("Content-Type", "application/json")
		// The tokens are never returned; plex_token_source tells the panel whether a
		// Plex token is in use and where it comes from ("env", "file" or "").
		_ = json.NewEncoder(w).Encode(struct {
			config.Config
			PlexTokenSource string `json:"plex_token_source"`
		}{gc().Redacted(), gc().PlexTokenSource()})
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
		if err := config.ValidateAllowedHosts(newCfg.AllowedHosts); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if !app.authOn() && newCfg.APIToken != "" {
			refuseOpenPort(w, r, http.StatusForbidden,
				"api_token cannot be set or changed through the API while no token is enforced"+setTokenHint)
			return
		}
		// Same for the Plex token: it is a credential for another system, so with no
		// token enforced a client could not be trusted to replace it, or to point the
		// URL it is sent to somewhere else.
		if !app.authOn() && newCfg.Plex.Token != "" {
			refuseOpenPort(w, r, http.StatusForbidden,
				"plex.token cannot be set or changed through the API while no token is enforced"+setPlexTokenHint)
			return
		}
		// A file that exists but cannot be read or parsed would be replaced by defaults,
		// dropping a token the owner is typing into it. Refused in every mode.
		onDisk, loadErr := config.LoadConfig()
		if loadErr != nil {
			// The cause is logged (path and error class, never file content) so an owner
			// can tell a directory from a syntax error without guessing.
			logger.Warn(fmt.Sprintf("[Config] save refused, config.json is unusable: %v", loadErr), "err", loadErr)
			refuseOpenPort(w, r, http.StatusConflict,
				"config.json cannot be read or parsed; fix it before saving through the API")
			return
		}
		// Scoped mode holds the auth settings at the copy read at start (refuseScopedSave).
		// The other modes keep the file's own values, as before.
		scoped := app.auth.Mode() == apiauth.ModeScoped
		if scoped {
			if refuseScopedSave(w, r, &newCfg) {
				return
			}
		} else {
			// auth_jwt names who counts as an admin at the next start: startup-only in every
			// mode. A post that carries a block is refused unless it equals the file's or the
			// running one (the panel echoes the running block, which a hand edit not yet
			// picked up makes differ from the file's). Either way the file's block is kept
			// as it is, so a post without one must not drop it and a hand edit survives.
			if newCfg.AuthJWT != nil && !newCfg.AuthJWT.Same(onDisk.AuthJWT) && !newCfg.AuthJWT.Same(gc().AuthJWT) {
				refuseOpenPort(w, r, http.StatusForbidden, "auth_jwt is startup-only; edit config.json")
				return
			}
			newCfg.AuthJWT = onDisk.AuthJWT
			// auth_tokens_file is read at start and never changed through the API, in every
			// mode. A post that names another path is refused; an absent or equal one (the
			// file's, or the running one the panel echoes, which may come from the
			// environment) saves as before. Either way the file's own value is written back,
			// never a posted one and never the environment's.
			if p := newCfg.AuthTokensFile; p != "" && p != onDisk.FileAuthTokensFile() && p != gc().AuthTokensFile {
				refuseOpenPort(w, r, http.StatusForbidden, "auth_tokens_file is startup-only; edit config.json, then restart flodni")
				return
			}
			newCfg.AuthTokensFile = onDisk.FileAuthTokensFile()
		}
		if !app.authOn() {
			// allowed_hosts is the same kind of setting, and gostorm_url feeds the engine's
			// list, so the derived list is gated, not each field: a client could clear or
			// widen what the next start enforces. A post that leaves it as it is saves as before.
			if onDisk.HostsSaveChanged(&newCfg, gc()) {
				refuseOpenPort(w, r, http.StatusForbidden,
					"allowed_hosts and the gostorm_url host cannot be changed through the API while no token is enforced: "+
						"set them through MKV_PROXY_ALLOWED_HOSTS or by editing config.json, then restart flodni")
				return
			}
			// Keep the token the file holds now, not the one read at start: the owner may
			// have added one by hand and not restarted yet.
			newCfg.APIToken = onDisk.ResolveAPIToken("")
			if plexURLChanged(newCfg.Plex.URL, onDisk.Plex.URL) {
				refuseOpenPort(w, r, http.StatusForbidden,
					"plex.url cannot be changed through the API while no token is enforced"+setPlexTokenHint)
				return
			}
		} else if !scoped {
			// The panel never sees the token; an empty post keeps the one in the file.
			newCfg.APIToken = gc().ResolveAPIToken(newCfg.APIToken)
		}
		// A panel save keeps the list as the file holds it, never the environment's. On the
		// open path HostsSaveChanged has already done this; here it covers the token path.
		if app.authOn() {
			onDisk.HostsSaveChanged(&newCfg, gc())
		}
		// The stored token goes wherever plex.url points, and the panel cannot read it
		// back, so a changed URL without a typed token would let a caller aim the token
		// at a host of their choosing.
		if newCfg.Plex.Token == "" && onDisk.Plex.Token != "" && plexURLChanged(newCfg.Plex.URL, onDisk.Plex.URL) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "re-enter the Plex token when changing plex.url"})
			return
		}
		// The panel never sees the Plex token either. An empty post keeps the one the
		// file holds now (read fresh, like the api_token hand edit); a typed one is
		// taken as given. An environment token is never written to the file.
		newCfg.Plex.Token = onDisk.ResolvePlexToken(newCfg.Plex.Token)
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
		// A save must never write a file the next start refuses: prove the bytes load.
		if err := checkLoadable(data); err != nil {
			logger.Warn(fmt.Sprintf("[Config] save refused, the result would not load: %v", err), "err", err)
			refuseOpenPort(w, r, http.StatusBadRequest, "the saved settings would not load back; nothing was written")
			return
		}
		if err := atomicfile.WriteFileKeepMode(gc().ConfigPath, data, 0600); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		// Reload in memory (V1.4.0 Live Update)
		cfg, err := config.LoadConfig()
		if err != nil {
			http.Error(w, "saved, but the file no longer loads: "+err.Error(), http.StatusInternalServerError)
			return
		}
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

// refuseScopedSave holds the startup-only settings in scoped mode (ADR 0004): api_token,
// auth_tokens_file, auth_jwt and auth_panel_https are read at start and never changed
// through the API, so a leaked admin token, a session or an admitted proxy user cannot
// plant a credential, or their own issuer, that outlives the owner's rotation and a
// restart. A post that would change one answers 403 and writes nothing. A post that
// leaves them alone keeps the values of the copy read at start (gc()), never a re-read
// of the file, so a hand edit made since then does not ride along with someone's save.
// A live gostorm_url that is not loopback is refused too: the next start would
// refuse it. It reports whether it answered.
func refuseScopedSave(w http.ResponseWriter, r *http.Request, newCfg *config.Config) bool {
	run := gc()
	switch {
	case newCfg.APIToken != "":
		refuseOpenPort(w, r, http.StatusForbidden, "api_token is startup-only in scoped mode; it must stay unset (remove it from config.json)")
		return true
	case newCfg.AuthTokensFile != "" && newCfg.AuthTokensFile != run.FileAuthTokensFile() && newCfg.AuthTokensFile != run.AuthTokensFile:
		refuseOpenPort(w, r, http.StatusForbidden, "auth_tokens_file is startup-only; edit config.json, then restart flodni")
		return true
	case newCfg.AuthJWT != nil && !newCfg.AuthJWT.Same(run.AuthJWT):
		refuseOpenPort(w, r, http.StatusForbidden, "auth_jwt is startup-only; edit config.json")
		return true
	case newCfg.AuthPanelHTTPS != run.AuthPanelHTTPS:
		refuseOpenPort(w, r, http.StatusForbidden, "auth_panel_https is startup-only; edit config.json, then restart flodni")
		return true
	}
	if u := newCfg.GoStormBaseURL; u != "" {
		if err := apiauth.RequireLoopback(u); err != nil {
			refuseOpenPort(w, r, http.StatusForbidden, "gostorm_url must stay a loopback address in scoped mode: the engine is called without a credential")
			return true
		}
	}
	newCfg.APIToken = run.ResolveAPIToken("")
	newCfg.AuthTokensFile = run.FileAuthTokensFile()
	newCfg.AuthJWT = run.AuthJWT
	newCfg.AuthPanelHTTPS = run.AuthPanelHTTPS
	return false
}

// checkLoadable is config.CheckLoadable; a test replaces it to force a refusal,
// because the strip no longer lets a real input fail.
var checkLoadable = config.CheckLoadable

// setTokenHint says where a token is set when the API may not set it.
const setTokenHint = ": set api_token through the FLODNI_API_TOKEN environment variable " +
	"or by editing config.json, then restart flodni"

// setPlexTokenHint says where the Plex settings are set when the API may not set them.
const setPlexTokenHint = ": set plex.token through the FLODNI_PLEX_TOKEN environment variable " +
	"or plex.url and plex.token by editing config.json, then restart flodni"

// plexURLChanged reports whether posted points the Plex token somewhere else than
// stored: a different scheme, host or port. An empty post sends the token nowhere.
func plexURLChanged(posted, stored string) bool {
	if posted == "" {
		return false
	}
	origin := func(s string) string {
		u, err := url.Parse(strings.TrimSpace(s))
		if err != nil || u.Host == "" {
			return strings.ToLower(strings.TrimRight(strings.TrimSpace(s), "/"))
		}
		return strings.ToLower(u.Scheme + "://" + u.Host)
	}
	return origin(posted) != origin(stored)
}

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
	if !app.authOn() {
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
