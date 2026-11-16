## Threat model: control API and its token

### Scope and status

This models the network surface of the daemon: the control server, the
engine's server, the FUSE mount and the play webhook. It is a STRIDE pass
over what the code does today (observed implementation, read from the
files named below). It does not cover the torrent swarm, peer discovery or
the privacy of private torrents; that is hard rule 3 in `CLAUDE.md`, pinned
by tests. Nothing here is a measurement or a penetration test.

Authentication is opt-in. With `api_token`, `FLODNI_API_TOKEN` and
`auth_tokens_file` all empty every route is open, as before (the open-port
refusals of T-15 still apply). `api_token` alone is one token with every scope.
A tokens file turns on scoped mode (v0.3.0, [ADR 0004](../decisions/0004-scoped-authentication.md)):
named tokens with scopes, enforced on both ports from one route table, and
optionally a proxy-signed identity and a panel session cookie for a person. The
design assumes a single maintainer on a trusted network, not a hostile one.

### Assets

| Asset | Why it matters |
|---|---|
| `config.json` | Holds the API token, tracker credentials and the media server token. Written by `POST /api/config`. |
| Stub library on the host | Adding or removing titles through the library routes changes what the media server sees. |
| The process and its host | `POST /api/restart` and the engine's shutdown route stop playback. |
| Playback state | A play webhook raises a torrent's priority and holds it open. |
| Bandwidth, disk and memory | Library adds fetch metadata and write stubs; the process shares one memory limit with the engine. |

### Actors

| Actor | Assumed capability |
|---|---|
| Maintainer | Holds the `admin` credential (a token, the proxy sign-in or the cookie); uses the panel and automation. Trusted. |
| Scoped caller | A script or job holding one token with a few scopes (T-17). Trusted for those scopes only. |
| Media server | Posts play webhooks; reads the mount. Trusted, but cannot send headers on webhooks. |
| LAN client | Can reach the published ports; holds no token. Untrusted when a token is set. |
| Browser on the network | A web page the maintainer or a household member opens, on any site. Holds no token and sends requests from their machine to the open ports, so it reaches a loopback or private address the page's own server could not. Sends an `Origin` header on cross-site requests. |
| Internet client | Reaches the ports only if they are forwarded or published. Untrusted. |
| Local user on the host | Can read the mount (`AllowOther` is on) and the files on disk. Not defended against here. |

### Trust boundaries

| Boundary | Listener | Authentication |
|---|---|---|
| B1 control server | `:<metrics_port>`, default 9080, all interfaces (`internal/daemon/main.go`) | Opt-in: bearer token (`api_token`, or scoped tokens from a file), then the session cookie, then the proxy-signed identity (`internal/apiauth`) |
| B2 GoStorm server | engine port, default 8090 (`internal/gostorm/web/server.go`) | The same tokens, bearer header only; never the cookie or the proxy's header |
| B3 FUSE mount | kernel to daemon, `AllowOther` | Filesystem permissions only |
| B4 play webhook | `/plex/webhook` and `/webhook` on B1 | Token via header or `?token=` (in scoped mode a webhook-only token) |

### Threats

IDs are stable; each row names the boundary it belongs to.

| ID | Boundary | STRIDE | Threat | Mitigation today | Residual |
|---|---|---|---|---|---|
| T-01 | B1, B2 | Spoofing | A client without the token calls a control route | Bearer token, constant-time compare, length check first (`apiauth.go`); 401 logged with method, path and address, never the credential Scoped mode (`auth_tokens_file`, ADR 0004): the compare is over SHA-256 hashes of every entry, in constant time with no early exit, after a shape check (`fl_` plus 43 base64url characters) that never hashes a malformed value; a token is bound to its scopes, so a valid token without the scope the route needs gets 403 (reason `scope`) beside the 401. Both servers read one route table and deny by default (an unlisted route, method or engine action needs `admin`); every refusal is counted in `flodni_auth_refusals_total` by route, scope and reason. | None when the token is set; nothing at all when it is not (T-02) |
| T-02 | B1, B2, B4 | Spoofing, Elevation | No token configured, so every route is open, including config write, restart, library add and remove | The default is documented in `SECURITY.md`; `.example` config carries a loopback `gostorm_url`. With no token enforced, `POST /api/config` refuses to set or change `api_token` and `POST /api/restart` is refused, both 403 (`control_api.go`), so a client cannot choose or erase the next start's token or restart the daemon through the control port Scoped mode closes it for a deployment that uses it: both servers deny by default, the engine's `POST /shutdown` and any `/stream` that adds need `admin`, and a start that cannot decide the credentials (an unreadable `config.json`, a bad tokens file, a repeated, null or mistyped auth key, `api_token` together with a tokens file) refuses to start instead of serving open. The engine refuses to start unless the daemon handed it its credential decision, and a gate built with no store answers 503, logged and counted. The default with nothing configured is still open and documented, and the open-port refusals remain for it. | Gap: every other route stays open, including other config keys and library add and remove; GoStorm's port answers `POST /shutdown` without a token, which ends the process (`os.Exit(0)`), so a client can still stop the service and whether it comes back depends on the supervisor; the daemon starts without warning of an open control port beyond one log line when auth is on The `POST /shutdown` gap closes in scoped mode (it needs `admin`). |
| T-03 | B1, B2 | Information disclosure | The token or its traffic is read on the wire | The token is never logged or returned: `Config.Redacted` blanks it, and the Plex token, for `GET /api/config` A tokens file holds SHA-256 hashes, never a token, so a read-only mount or a backup of it leaks no credential. `GET /api/startup` and the `[Startup] report` line carry the mode, the token count and the scopes in use, never a name, hash or path. | Gap: no TLS on the control port (T-04) A passive reader on the wire still sees a bearer token (T-04). `config.json` and its backups stay plaintext while `api_token` or the Plex token is kept there. |
| T-04 | B1 | Information disclosure, Tampering | Passive or active attacker on the path sees the token and every payload | None in flodni; `SECURITY.md` says to front it with a TLS proxy | Gap: no TLS on B1. GoStorm can serve TLS when its own settings enable it; the control port cannot |
| T-05 | B4 | Information disclosure | `?token=` on a webhook URL lands in proxy and access logs and in the media server's own configuration | Only the two webhook paths accept a query token, matched on the exact path (`controlWebhook` in `apiauth.go`); no other route does. In scoped mode a token in `?token=` is accepted only if its scopes are exactly `webhook`, so the credential in a URL can forge a play event and do nothing else; any other token there, and any `token` parameter on any other route, is refused (`query_not_allowed`) before a lookup. The gate hands the handler a request with the parameter removed, and `internal/redact` masks `token=` in every Flodni log line, including after a panic. `api_token`-only mode is unchanged | Gap: a hop that logs URLs (a reverse proxy, the media server's own stored webhook URL) still holds the webhook token; its reach is a forged play event (T-06). What a proxy in front logs is outside Flodni |
| T-06 | B4 | Spoofing | Anyone who can reach the port posts a forged play event | Needs the token when set; the payload is matched against registered files only (`internal/daemon/webhook.go`), so an unknown title does nothing In scoped mode a webhook-only credential exists and is the only kind that rides in a URL; a forged event needs it. | Gap: with `api_token` empty the webhook is unauthenticated (the open-port refusals in T-02 do not cover it; there is no separate webhook secret); a forged event can hold a registered torrent open With a tokens file the gap is closed for that mode. |
| T-07 | B4 | Denial of service | An oversized or malformed webhook exhausts memory | Form posts are capped at 10 MiB with `MaxBytesReader`; JSON posts at 10 MiB with `LimitReader` (`webhook.go`) | A 10 MiB body is still held in memory per request |
| T-08 | B1 | Denial of service | Slow or oversized requests tie up the server | `ReadHeaderTimeout` 10 s, `ReadTimeout` 60 s, `IdleTimeout` 120 s, `WriteTimeout` 600 s (`main.go`); library bodies capped at 1 MiB (`internal/library/handler.go`); evict body at 1 KiB (`warmup_evict.go`) | No rate limiting or lockout; the token can be guessed at full network speed `flodni_auth_refusals_total` makes guessing visible; a generated token (256 random bits) cannot be guessed, an owner-chosen `api_token` can. |
| T-09 | B2 | Denial of service | Slow headers or idle connections on the engine port | `ReadHeaderTimeout` 10 s, `IdleTimeout` 120 s (`internal/gostorm/web/server.go`) | No read or write timeout, deliberately: streams are long-lived |
| T-10 | B1 | Tampering, Elevation | A caller rewrites `config.json` or restarts the daemon | Token required when set; writes go through `atomicfile.WriteFile`; the token is preserved when the panel posts an empty one (`ResolveAPIToken`) In scoped mode a config write needs `admin`, and `api_token`, `auth_tokens_file`, `auth_jwt` and `auth_panel_https` cannot be changed through the API at all (403, nothing written), so a leaked `admin` token, a session or an admitted proxy user cannot plant a credential that survives a restart. | Config posted by a token holder is trusted as given; paths are not validated |
| T-11 | B1 | Tampering | A caller adds or removes titles, or points an add at an unexpected source | Token required when set; add and remove are the only routes that change the library In scoped mode library writes need `library`; a `webhook`, `metrics`, `read`, `scrape` or `upload` token can do none. Library add and inspect accept only a `magnet:` link with an anchored 40-hex or 32-base32 hash (400 otherwise, before any engine call), so `http(s)` and `file:` links never reach the engine through them. | A token holder can add any magnet or `.torrent`; there is no allow-list A `library` token reaches the engine's add through Flodni (see T-17). |
| T-12 | B1 | Information disclosure | Unauthenticated health leaks state | `GET /api/health` returns status words only; `GET /control` serves the static panel, which holds no data `GET /api/live` answers `ok` and checks nothing: the cheap probe for a container or load balancer, with no `warnings` to leak. | Its `warnings` array tells a caller whether play webhooks are arriving |
| T-13 | B3 | Information disclosure | A local user on the host reads media through the mount | None beyond file modes on the stubs | Out of scope: the host is trusted, and `AllowOther` is needed for the media server |
| T-14 | B1, B2 | Repudiation | No record of who changed what | 401s are logged; routine calls are not In scoped mode each mutating success on a library, config, restart or engine-write route logs the token's name (a label the owner chose, never the token), and every refusal is counted by route, scope and reason. | Gap: one shared token, no per-caller identity, no audit log Still no audit log: names are in the log stream only, and `api_token` alone names no caller. |
| T-15 | B1, B2 | Spoofing, Tampering | With no token, a page in a browser on the network POSTs to the open port (a `text/plain` body needs no preflight, and the handlers parse JSON without checking Content-Type), so config write, library add and remove run with no token | With no token, `internal/apiauth` answers 403 to any request whose `Origin` hostname differs from its `Host` hostname, logged with method, path and address; `Origin: null` and an unparsable `Origin` are refused; the engine sends no CORS headers (`Access-Control-Allow-Origin: *` and private-network access were removed) For a request authenticated by a session cookie or a proxy header the primary defence is not the hostname-only check, which a page on another port of the same host passes and which `SameSite` does not stop (the same host is the same site): a non-GET needs an `Origin` whose host and port equal `Host`, `Sec-Fetch-Site: same-origin` when present, and `Content-Type: application/json`; a refusal counts as `csrf`. The engine gate never reads a cookie or the proxy's header. | Gap: only hostnames are compared, because a TLS tunnel delivers plain http with the original `Host`, so a page on the same hostname but another port (another service on the host) is not stopped. Browsers omit `Origin` on navigations and on `<img>` and `<script>` GETs, so a GET reaches the gate with none and passes: on the engine port `GET /play/:hash/:id` and `GET /stream` on a stored hash wake that torrent from its record (a record without metainfo gets 404). `GET`/`HEAD /stream` no longer adds on an open port: it plays only a hash the engine already holds, and answers 403 to any other link, to `http(s)` and `file:` links before any fetch (so no SSRF or local file read) and to `save` and `preload`; with a token set it adds only a hash the engine does not hold. `/shutdown` and the Plex webhook were GET-reachable and now take POST only. DNS rebinding passes by default: Origin and Host both carry the attacker's name, so the page is same-origin to the browser and can read responses (list, then remove torrents). It is mitigated when `allowed_hosts` is set: both servers then answer 403 to a `Host` hostname that is not listed (IP literals, `localhost`, the machine's own hostname and the `gostorm_url` host always pass), token or not; a list with no valid entry stays on, and with no token the API refuses to change it; a changed `gostorm_url` host is refused the same way, since it feeds the engine's list; the default is empty, which leaves rebinding open, IP-only deployments included, so set it everywhere (an IP-only one uses `localhost`). IP literals are exempt because rebinding always rides on a name: the browser sends the page's hostname as `Host`. With no token, `Sec-Fetch-Site: cross-site` is also refused on any method except to the open routes (`/control`, `/api/health`, `/echo`); browsers send it only over https or to `localhost`, so it covers less than it seems. A client with no `Origin` is the T-02 case. With a token set the check is off and the token is the gate. A reverse proxy in front of the control port must keep the `Host` header, or the panel's own POSTs are refused The open-port gaps above are unchanged for a deployment with no credential. |
| T-16 | B1, B2 | Tampering, Information disclosure | **Tracker injection (hard rule 3), fixed.** An add (magnet, `torrs://`, `.torrent` upload, or the library add and inspect, which reach the engine through `POST /torrents`) for a hash the engine stores or holds carried the request's trackers into the live torrent and, through the saved record, into every later wake, so one add announced a private torrent's info hash to a foreign tracker for good | `addUnderLock` (`internal/gostorm/torr/lifecycle.go`) merges a request's trackers only into a record that is known public; otherwise the record's trackers replace them. `NewTorrent` (`torrent.go`) hands `AddTorrentSpec` no trackers for a torrent already held unless its metainfo shows it public. Unknown metainfo counts as private. One log line carries a count, never a URL. Pinned in `retrack_test.go` | Re-adding with new trackers is now remove, then add. A first add of a hash with no record and not held takes its own link's trackers, as it must |
| T-17 | B1, B2 | Elevation | A scoped token is stolen. Callers keep tokens in a container's environment and `docker inspect`, in the media server's stored webhook URL and in shell history | Each token holds only its caller's scopes, so the blast radius is the scope: `webhook` forges a play event and nothing else; `metrics` reads metrics; `read` reads state; `library` files and removes titles; `engine` streams any title the engine holds but cannot add by link (the open-port `/stream` rule applies to every caller but `admin`); `upload` adds a torrent; `admin` does everything. Rotation is: add the new entry, move the caller, remove the old entry, restart. One `admin` token is kept offline as the break-glass credential | Gap: `library` reaches the engine's add through Flodni, so its reach includes what an add can fetch. Flodni's own engine calls carry the in-process identity, which is `admin` on the engine: the input checks of the control routes that call it (the `magnet` scheme check) are the boundary, not the scope. A token is plaintext at the caller. A stolen `admin` token or an admitted proxy user can do everything `admin` does |
| T-18 | B1 | Spoofing, Elevation | The proxy-signed identity (`auth_jwt`): a forged, replayed or foreign assertion, or a forged header sent to the port | `internal/apiauth/jwt` checks, in order: one header value of three strict base64url segments; `alg` `RS256` or `ES256` only, chosen by the key's type and never by the token; a `kid` that names a cached key; a valid signature; `iss`, `aud`, `exp` and `nbf` with 60 s of skew; `email` in `allowed_emails` when that list is set. Keys come over https with a 5 s timeout, a 64 KiB cap and no redirects, parsed strictly (RSA 2048 bits or more, P-256 on the curve). Keys unavailable fails closed (`jwt_keys_unavailable`), the bearer and cookie still work, and the start never waits on the provider. `auth_jwt` needs the tokens file, its header may not be `Authorization`, and the engine port ignores the header. A verified assertion is `admin` on the control port, with the CSRF rules of T-15 | A copied assertion can be replayed until `exp`. A key the provider revokes stays trusted until the next successful fetch, up to 24 hours. The JWKS URL is a blind fetch to an address the owner named. The audience must be this application's own: a user the proxy admits to another application would otherwise be an admin here. An admitted proxy user can do everything `admin` does |
| T-19 | B1 | Spoofing, Elevation | The panel session cookie (`flodni_session`, ADR 0004 step 6): theft, forgery, a cookie that outlives a logout, a forged cross-site post | `POST /control/login` needs an `admin` token. The cookie holds name, expiry and a generation, with an HMAC-SHA256 tag under a key drawn at random at each start; `HttpOnly`, `SameSite=Strict`, an absolute 12-hour expiry (no sliding window), `Secure` when the request or the proxy says https or `auth_panel_https` is set. Logout increments the generation, so every earlier cookie stops verifying, and a restart does the same. Control port and scoped mode only; the engine gate never reads it. Each cookie request needs the CSRF rules of T-15 | Cookies ignore ports: the browser sends `flodni_session` to every port of the hostname, so another service on the same hostname receives it. `HttpOnly` keeps scripts away from it, not that service's server, and only the control gate reads it. No lockout or rate limit on login (T-08). Theft within the 12 hours is repelled only by a logout or a restart |
| T-20 | B1 | Tampering | A page on another origin frames the control panel and tricks a signed-in operator (cookie or proxy session) into clicking | `GET /control` answers `Content-Security-Policy: frame-ancestors 'none'` and `X-Frame-Options: DENY`, so no origin can frame it; every state-changing request still needs the CSRF rules of T-15 | A browser that honours neither header |

### Known gaps

Ordered by what to fix first. None is scheduled here; see
[the roadmap](../../roadmap.md) for what is planned.

1. No TLS on the control port (T-04). The token and every payload travel in
   clear text; mitigate with a TLS proxy on any untrusted network.
2. Authentication is opt-in (T-02, T-06). With no credential configured the
   other config keys, the library routes and the webhook stay open; setting one
   needs the environment or the file, and a restart needs the service manager.
   GoStorm's `POST /shutdown` stops the process without a token on an open
   port; in scoped mode it needs `admin`.
3. `?token=` on a webhook URL is logged by proxies (T-05). Plex cannot send
   headers, so the query form exists on those two paths only; in scoped mode it
   takes a webhook-only token, which can forge a play event and nothing else.
4. No rate limiting or lockout (T-08).
5. With `api_token` alone the webhook caller, the panel and automation share
   one credential (T-14). A tokens file gives each its own, scoped; moving to it
   is one restart (README, "Scoped tokens").
6. The open-port Origin refusal compares hostnames only (T-15): a page on
   the same hostname but another port, DNS rebinding unless `allowed_hosts` is
   set, any client that sends no `Origin`, and the GET routes `/play/:hash/:id`
   and `/stream` on the engine port (a page can still wake a stored hash;
   neither adds) still reach an open port.

7. Residuals of scoped mode (T-17 to T-19): tokens are plaintext at the callers
   (compose environment, `docker inspect`, the media server's stored URL, shell
   history); a `library` token reaches the engine's add through Flodni; an
   assertion can be replayed until `exp` and a revoked JWKS key is trusted for up
   to 24 hours; the cookie is sent to every port of the hostname (only the
   control gate reads it); there is no TLS (T-04), no rate limit (T-08), and an
   `admin` token or an admitted proxy user can do everything `admin` does.

### Assumptions

- The control and engine ports are reachable from the host and a trusted
  network only. If that is false, T-02 and T-04 are the first things to
  change.
- The media server is not hostile. A forged webhook is treated as a
  nuisance, not as a route to the filesystem: its payload is matched, never
  executed or used as a path.

### Evidence

Read, not run: `internal/apiauth/apiauth.go`, `internal/apiauth/scopes.go`,
`internal/apiauth/tokens.go`, `internal/apiauth/jwt`, `internal/daemon/panel.go`,
`internal/gostorm/web/scope.go`, `internal/daemon/main.go`,
`internal/daemon/webhook.go`, `internal/gostorm/web/server.go`,
`internal/library/handler.go`, `internal/config/config.go`. The token
behaviour is covered by `internal/apiauth/apiauth_test.go`; the Origin refusal by
`internal/apiauth/origin_test.go` and the engine's missing CORS headers by
`internal/gostorm/web/server_test.go`. Scoped mode is covered by
`internal/apiauth/gate_test.go`, `internal/apiauth/scopes_test.go`,
`internal/gostorm/web/scope_test.go` and `internal/daemon/panel_headers_test.go`,
and end to end, on both ports, by the `scoped` variant of `tests/e2e/run.sh`.
