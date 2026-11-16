package daemon

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"flodni/internal/apiauth"
	"flodni/internal/apiauth/jwt"
	"flodni/internal/config"
	server "flodni/internal/gostorm"
	"flodni/internal/library"
)

const authUsage = `usage: flodni auth new-token --name NAME --scopes a,b
  NAME is [a-z0-9][a-z0-9_-]{0,31}; scopes: admin, engine, library, metrics, read, scrape, upload, webhook.
  Prints the token once (line 1 of stdout) and the JSON entry to add to the "tokens" array of the
  tokens file (line 2). It writes no file and starts no daemon.`

// authMain is `flodni auth`: owner-run, offline. new-token makes a token from 32 bytes of
// crypto/rand and prints it to stdout, once, with the entry that holds its hash. The
// token is not logged and not written anywhere. Exit 0 on success, 2 for a bad
// invocation (nothing is printed to stdout then), 1 if no random bytes could be read
// or the result could not be written to stdout.
func authMain(args []string, out, errOut io.Writer) int {
	usage := func(why string) int {
		_, _ = fmt.Fprintf(errOut, "auth: %s\n%s\n", why, authUsage)
		return 2
	}
	if len(args) == 0 || args[0] != "new-token" {
		return usage("a subcommand is required: new-token")
	}
	fs := flag.NewFlagSet("auth new-token", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	name := fs.String("name", "", "token name")
	scopes := fs.String("scopes", "", "comma-separated scopes")
	if err := fs.Parse(args[1:]); err != nil {
		return usage(err.Error())
	}
	if fs.NArg() != 0 {
		return usage("unexpected argument " + fmt.Sprintf("%q", fs.Arg(0)))
	}
	if err := apiauth.ValidName(*name); err != nil {
		return usage(err.Error())
	}
	set, err := apiauth.ParseScopes(*scopes)
	if err != nil {
		return usage("--scopes: " + err.Error())
	}
	token, hash, err := apiauth.GenerateToken()
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "auth: no random bytes: %v\n", err)
		return 1
	}
	entry, err := json.Marshal(apiauth.FileEntry{Name: *name, Scopes: set.Names(), SHA256: hash})
	if err != nil {
		_, _ = fmt.Fprintf(errOut, "auth: %v\n", err)
		return 1
	}
	if _, err := fmt.Fprintf(out, "%s\n%s\n", token, entry); err != nil {
		_, _ = fmt.Fprintf(errOut, "auth: the token could not be written to stdout: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintln(errOut, "Line 1 of stdout is the token: store it now, it is not shown again and Flodni keeps only its hash.")
	_, _ = fmt.Fprintln(errOut, `Line 2 is the entry: add it to the "tokens" array of auth_tokens_file, then restart.`)
	return 0
}

// startAuth decides the auth mode from the configuration (ADR 0004). An error is a
// refusal to start and Run exits on it; the token and the hashes are never logged.
func startAuth(cfg *config.Config, log *slog.Logger) (*apiauth.Tokens, error) {
	auth, err := apiauth.Load(apiauth.LoadOptions{
		APIToken:      cfg.APIToken,
		TokensFile:    cfg.AuthTokensFile,
		GoStormURL:    cfg.GoStormBaseURL,
		Logger:        log,
		JWTConfigured: cfg.AuthJWT != nil,
	})
	if err != nil {
		return nil, err
	}
	return auth, nil
}

// startJWT builds the verifier for the proxy's assertion. An auth_jwt block the verifier
// refuses (incomplete, header Authorization, a jwks_url that is not https) stops the
// start. Nil, nil when the block is absent. It does not fetch: the first assertion
// does, so an outage of the provider never blocks the start.
func startJWT(cfg *config.Config) (apiauth.JWTVerifier, error) {
	if cfg.AuthJWT == nil {
		return nil, nil
	}
	v, err := jwt.New(*cfg.AuthJWT)
	if err != nil {
		return nil, err
	}
	return v, nil
}

// authRefusals counts every refusal of both gates for flodni_auth_refusals_total.
var authRefusals apiauth.Refusals

// newControlGate wraps the control routes in the gate main serves: the credential
// store of the start, the router's own pattern for each request, the optional proxy
// assertion, the Host allowlist and the refusal counter.
func newControlGate(routes *apiauth.Registrar, tokens *apiauth.Tokens, verifier apiauth.JWTVerifier, cfg *config.Config) http.Handler {
	opts := []apiauth.Option{
		apiauth.WithTokens(tokens),
		apiauth.WithPattern(routes.Pattern),
		apiauth.WithObserver(authRefusals.Observe),
		apiauth.WithAllowedHosts(cfg.EffectiveAllowedHosts()),
		apiauth.WithPanelHTTPS(cfg.AuthPanelHTTPS),
	}
	if verifier != nil {
		opts = append(opts, apiauth.WithJWT(verifier))
	}
	return apiauth.Middleware("", logger, routes, opts...)
}

// engineHTTPClient is the client of Flodni's own calls to its engine. In scoped mode
// the hash file holds nothing Flodni could send, so the request goes in-process, below
// the gate, with no credential and no URL; otherwise it is the default transport (a
// remote engine still works with api_token alone).
func engineHTTPClient(mode apiauth.Mode, timeout time.Duration) *http.Client {
	c := &http.Client{Timeout: timeout}
	if mode == apiauth.ModeScoped {
		c.Transport = server.InProcess()
	}
	return c
}

// newLibraryEngineClient is the library's client of the engine: in-process in scoped
// mode, the configured URL and api_token otherwise.
func newLibraryEngineClient() *library.GoStormClient {
	if app.auth.Mode() == apiauth.ModeScoped {
		return library.NewGoStormClientTransport(server.InProcess())
	}
	return library.NewGoStormClient(gc().GoStormBaseURL, gc().APIToken)
}

// gostormSettingsProxy is the panel's same-origin proxy to the engine's settings API.
// The panel must not build an absolute URL of its own: behind a reverse proxy the
// GoStorm port is not exposed, and an http:// call from an https:// page is blocked as
// mixed content, which left the whole GoStorm section empty and unsaveable. In scoped
// mode it goes in-process: no gostorm_url, no credential.
func gostormSettingsProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	scoped := app.auth.Mode() == apiauth.ModeScoped
	base := gc().GoStormBaseURL
	if scoped {
		base = "http://engine.internal" // ignored by the transport
	} else if base == "" {
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
	if t := gc().APIToken; t != "" && !scoped {
		req.Header.Set("Authorization", "Bearer "+t)
	}
	resp, err := engineHTTPClient(app.auth.Mode(), 15*time.Second).Do(req)
	if err != nil {
		http.Error(w, "gostorm unreachable: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// refusedByHandler lets a handler that refuses a request itself (the cookie login)
// report it to flodni_auth_refusals_total: route is its pattern, reason one of
// invalid, scope, csrf or session.
func refusedByHandler(route, reason string) {
	authRefusals.Observe(route, apiauth.ScopeFor(apiauth.Control, http.MethodPost, route, ""), reason)
}

// configureEngineAuth hands the engine the credential store the start decided, before
// the engine listens. A nil store closes the engine (it answers nothing): it is never
// read as "no credential configured". Run calls it; a test drives it.
func configureEngineAuth(auth *apiauth.Tokens, cfg *config.Config, log *slog.Logger) {
	server.SetAPIToken(cfg.APIToken, log)
	server.SetAuth(auth, authRefusals.Observe)
}
