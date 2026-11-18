# Flodni

Flodni presents torrents as ordinary media files. Plex scans and plays them
from a FUSE mount, and the bytes are fetched
from the swarm when they are read. Nothing is downloaded to disk in full.

It is one Go binary: a torrent engine, a FUSE virtual filesystem and a small
control API.

## Status

Flodni is a hard fork of
[Tiramisu](https://github.com/MrRobotoGit/tiramisu) by Matteo Rancilio,
started on 2026-11-10. It no longer follows upstream; the reasons are in
[ADR 0001](docs/architecture/decisions/0001-hard-fork-tiramisu.md).

The code is Tiramisu's with this fork's reliability changes. The Go module,
the binary, the image paths and the environment variables are named Flodni.
It is maintained by one person for their own use. Expect breaking changes
and no support.

## Documentation

| Document | What it holds |
|---|---|
| [docs/roadmap.md](docs/roadmap.md) | What is planned, in order, and what is done. |
| [docs/ideas.md](docs/ideas.md) | What could be improved, not yet planned. |
| [docs/architecture/README.md](docs/architecture/README.md) | The C4 model, its views and the decisions. |
| [README.upstream.md](README.upstream.md) | Tiramisu's user guide as inherited: install, configuration, API. |
| [docs/deploy/least-privilege.md](docs/deploy/least-privilege.md) | A narrower container profile than `SYS_ADMIN` with AppArmor unconfined. Designed, not verified on a host. |
| [SECURITY.md](SECURITY.md) | How to report a vulnerability. |

## Building

Linux only: the FUSE layer is cgo against `libfuse3`.

```bash
sudo apt-get install -y libfuse3-dev pkg-config
make go-check   # gofmt, build, vet
make go-test    # tests with the race detector
```

The container image builds anywhere Docker runs:

```bash
docker build -f docker/Dockerfile -t flodni:dev .
```

`make e2e` runs the end-to-end test (Docker and `/dev/fuse`).
`FLODNI_TEST_SESSION_IDLE_CLOSE` and `FLODNI_TEST_SESSION_TICK` exist for that
test only; they are not configuration.

## Running

The image mounts the FUSE view at `/mnt/flodni-virtual`, reads library
files from `/mnt/flodni-real` and keeps its state under `/state`. Set the
paths with `FLODNI_ROOT_PATH`, `FLODNI_SOURCE_PATH`, `FLODNI_MOUNT_PATH`,
`FLODNI_STATE_DIR`, `FLODNI_LOG_DIR`, `FLODNI_LOG_FILE` and
`FLODNI_HOST_MOUNT_HINT`; the Plex settings use `FLODNI_PLEX_URL` and
`FLODNI_PLEX_TOKEN`. The older `TIRAMISU_*` names still work and lose to the
`FLODNI_*` ones. Existing state carries over: the database is still
`tiramisu.db` and the log file `tiramisu.log`.

Shutdown first closes every open session, so each gets its verdict and
`[Session]` line, then saves state and unmounts; a mount that will not
unmount is detached lazily after 5 s, and the process exits by 8 s at the latest. That
fits inside Docker's default 10 s `stop_grace_period`. A longer grace period
is fine; do not set it below 10 s, or Docker kills the process before the
deadline.

Deleting a title with `rm` through the mount removes its stub. The torrent
and its cache files go only when no other stub uses it, so deleting one
episode of a season pack keeps the pack for the others. The release is not
added to the blacklist. An `.mkv` outside the `movies/` and `tv/` directories
is refused with `EPERM` and left in place. A media server may rescan after the
removal.

### Page cache on a second open

The kernel drops a file's cached pages at every open unless the filesystem
says to keep them, and that drop waits for every page a read in flight still
holds, so one slow read (a fetch, a slot wait, the opt-in budget hold below)
would freeze every new open of the file. Flodni keeps the cache
(`FOPEN_KEEP_CACHE`) when another handle of the same path is already open, so
a second reader is not held up by the first one's slow read. The first open of
a path, with no handle live, still drops the cache, which is what clears the
pages of a release that was replaced. Reads on an already open descriptor were
never affected. The pages stay valid because a file's inode number comes
from its torrent file (hash and index), so a replacement release has another
inode and its own cache.

### Scoped tokens (`auth_tokens_file`)

One shared `api_token` opens every route. A tokens file gives each caller its
own token with only the scopes it needs, so a leaked webhook URL cannot write
the configuration. The file holds names, scopes and SHA-256 hashes, never a
token; it is read once at start and Flodni never writes it.

Make a token and its entry, offline (no daemon, no file written):

```sh
flodni auth new-token --name webhook --scopes webhook
```

Line 1 of stdout is the token, shown once: give it to the caller. Line 2 is the
entry; add it to the `tokens` array of the file:

```json
{"tokens": [{"name": "webhook", "scopes": ["webhook"], "sha256": "<64 hex characters>"}]}
```

`new-token` hashes the token for you: the entry's `sha256` is the SHA-256 of the
token, hex (`printf %s "$TOKEN" | sha256sum` gives the same). A token is 256
random bits, so a plain hash is enough; a hand-hashed short password is outside
the model, which is why `new-token` is the only supported way to make one. The
file can be mounted read-only and backed up without leaking a credential.

#### The scopes

A token holds one or more of eight scopes. `admin` implies all the others; no
other scope implies another (a `library` token cannot read sessions unless it
also holds `read`).

| Scope | What it opens |
| --- | --- |
| `webhook` | The two play webhooks (`POST`), the only routes that take `?token=`. |
| `metrics` | `GET /metrics*` on the control port. |
| `read` | Health detail, the startup report, sessions, the library list, file and audit. |
| `library` | Library add, remove, confirm and inspect. |
| `scrape` | `POST /api/torrents/scrape` and its status. |
| `engine` | The engine's torrent list, active and get, settings get, cache and the streaming routes. `/stream` plays or reports a hash the engine already holds and never adds by link. |
| `upload` | `POST /torrent/upload`, which adds a torrent. |
| `admin` | Everything above, plus config reads and writes, restart, warmup evict, the engine's settings set, torrent add and removal on the engine, shutdown, and `/stream` that adds. |

`GET /api/live`, `GET /api/health`, `GET /control` and the engine's `GET /echo`
need no credential. A route, method or engine action the table does not list
needs `admin`.

#### One token per caller

Give each caller the scopes it needs and nothing more; no caller needs `admin`
day to day.

| Caller | Scopes |
| --- | --- |
| a media bridge that files titles | `library`, `upload` |
| a health job that retries dead swarms | `read`, `scrape`, `engine`, `library` |
| a removal job | `library` |
| a playback guard that checks sessions and torrent health (`GET /api/sessions`, `GET /api/torrents/health`; `GET /api/health` is open and needs no token) | `read` |
| an operator CLI | `library`, `upload`, `engine`, `metrics` |
| a webhook sender that cannot set headers (the `?token=` form) | `webhook` |
| a metrics scraper | `metrics` |
| the panel behind an identity-aware proxy | the proxy's assertion (below), which is `admin` |
| the break-glass credential | `admin`, kept offline |

`GET /api/library/list` needs `read`, not `library`. That is six tokens plus the
break-glass one. The break-glass `admin` token is
also the way into the panel when the identity provider is down.

A configuration that uses the tokens file and the proxy sign-in looks like this
(the `auth_jwt` values show Cloudflare Access; see "Proxy-signed identity (IdP
mode)" below for what is checked and how the keys are fetched):

```json
{
  "auth_tokens_file": "/run/secrets/flodni-tokens.json",
  "auth_jwt": {
    "issuer":   "https://<team>.cloudflareaccess.com",
    "audience": "<application audience tag>",
    "jwks_url": "https://<team>.cloudflareaccess.com/cdn-cgi/access/certs",
    "header":   "Cf-Access-Jwt-Assertion",
    "allowed_emails": []
  }
}
```

Migrate in this order, because both settings together stop the start and the
change is one restart:

1. Make one token per caller with `flodni auth new-token` and put the entries in
   the file. Keep one `admin` token offline as the break-glass credential.
2. Move each caller to its own token: the media server's webhook URL to a
   `webhook`-only token (the only kind `?token=` accepts), the scraper to a
   `metrics` one, the bridge to `library` and `upload`, and so on. Do this
   before the restart if the callers can hold both; a caller still sending the
   old `api_token` is refused after it.
3. Remove `api_token` from `config.json` and from `FLODNI_API_TOKEN`.
4. Set `auth_tokens_file` in `config.json` (or `FLODNI_AUTH_TOKENS_FILE`, which
   wins) and restart. Check `GET /api/startup`: `auth.mode` is `scoped`.

To roll back, restore `api_token` and remove the file setting.

A token is `fl_` plus 43 characters. A name matches `[a-z0-9][a-z0-9_-]{0,31}`
(`internal` is reserved). Scopes are `admin`, `engine`, `library`, `metrics`,
`read`, `scrape`, `upload` and `webhook`; `admin` implies all the others. To
rotate, add the new entry, move the caller to the new token, remove the old
entry and restart. The file's keys are exact and lower case (`tokens`, `name`,
`scopes`, `sha256`); any other spelling stops the start.

Both servers (the control port and the engine's 8090 and TLS port) enforce the
scopes from one route table, and deny by default: a route, method or engine
action the table does not list needs `admin`. The engine port reads the bearer
header only. A token may ride in `?token=` only on the two webhook routes and
only if its scopes are exactly `webhook`; anywhere else the parameter is refused
(`query_not_allowed`) without a lookup. A verified `auth_jwt` assertion is
`admin` on the control port, and its non-GET requests need an `Origin` of the
same host and port, `Sec-Fetch-Site: same-origin` when present, and
`Content-Type: application/json`. `auth_panel_https` (startup-only, default
false) says the panel is served over https, so that `Origin` must be https.

`flodni_auth_refusals_total{route,scope,reason}` counts every refusal of both
gates, in every mode. `route` is the route pattern from the table (never a raw
path), `unmatched` for a path no row names, or `pre_route` for a refusal by the
engine's outer gate, which runs before routing; `scope` is the scope the route
needs (empty for `pre_route`), never the presented token's, so no name or token
can become a label. `reason` is one of:

| Reason | Meaning |
| --- | --- |
| `missing` | No credential on a route that needs one. |
| `invalid` | A credential was presented and is wrong (unknown token, bad shape). |
| `scope` | A valid token without the scope the route needs. |
| `query_not_allowed` | A `token` query parameter where it is not allowed, or on a webhook with a token that holds more than `webhook`. |
| `body` | The engine's action peek refused the body (too large, unparsable, encoded). |
| `csrf` | The `Origin`, `Sec-Fetch-Site` or `Content-Type` rule for a cookie or proxy-header request. |
| `session` | A session cookie that is expired, tampered with or from before a logout. |
| `jwt_malformed`, `jwt_alg`, `jwt_unknown_kid`, `jwt_keys_unavailable`, `jwt_signature`, `jwt_issuer`, `jwt_audience`, `jwt_expired`, `jwt_not_yet_valid`, `jwt_claims` | A proxy assertion was present and failed. |
| `host`, `origin`, `cross_site` | The Host allowlist and the open-port `Origin` and `Sec-Fetch-Site` refusals. |
| `closed` | The gate was built with no credential store and answers 503: a wiring mistake, never a configuration. The engine also logs an error at start. |

A refusal is logged with the method, the quoted path, the address and the
reason, never the query, a token, a cookie or a header value; a success on a
mutating route logs the token's name.

Flodni refuses to start, and never falls back to an open port, in each of these
cases; the message names the cause and never prints a token:

- the tokens file cannot be read or parsed, is not a regular file, is over
  64 KiB, has trailing data, an unknown or re-cased key, or a repeated one;
- the file holds no token or more than 64, an entry without scopes or with an
  unknown scope, a repeated name or hash, a hash that is not 64 lower-case hex
  characters or is the hash of the empty string, or a bad or reserved name;
- `api_token` (or `FLODNI_API_TOKEN`) and `auth_tokens_file` are both set:
  remove `api_token`, there is no alias;
- `auth_jwt` is set without `auth_tokens_file`, or its block is incomplete,
  names `Authorization` as its header, or has a `jwks_url` that is not https;
- `gostorm_url` is not a loopback address (the engine is called without a
  credential);
- `config.json` exists but cannot be read or parsed, in every mode, because the
  pointer to the tokens file lives there;
- `config.json` holds `api_token` more than once, re-cased (`API_TOKEN`,
  `Api_Token`) or as `null`, or `auth_tokens_file`, `auth_jwt` or
  `auth_panel_https` repeated, re-cased or as `null`;
- `config.json` holds any other top-level key whose name contains `auth`,
  whatever its case or separators (`auth_token_file`, `auth-tokens-file`,
  `author`);
- the `auth_jwt` block repeats, re-cases or adds a key.

Any other unknown top-level key only logs one warning that lists the names. A
file writable by group or others only logs a warning. `GET /api/startup` and the
`[Startup] report` line carry an `auth` section (`mode` of `open`,
`legacy_token` or `scoped`, the token count, the scopes in use, and `jwt`) with
no name, hash or path. With only `api_token` set, behaviour is as before: one
token with every scope, `?token=` on the two webhook routes, and a wrong bearer
on a webhook still falls through to the query token.

In scoped mode `api_token`, `auth_tokens_file`, `auth_jwt` and
`auth_panel_https` are startup-only: `POST /api/config` answers 403 (and writes
nothing) to a change, as it does to a non-loopback `gostorm_url`. A save
writes back the file's current value of `auth_tokens_file`, `auth_jwt` and
`auth_panel_https`, in every mode, so a hand edit made since the start survives
it; a post may echo either the running value or the file's, and one that matches
neither is refused. `api_token` must stay unset in scoped mode. Flodni's own calls to its
engine go in-process, with no credential and no URL.

#### Panel sign-in (session cookie)

A person signs in to the panel with an `admin` token, so the browser keeps no
token. `POST /control/login` with `{"token": "..."}` answers 204 and sets the
`flodni_session` cookie; a token without `admin` gets 403, an unknown one 401
(counted as `scope` and `invalid`). `POST /control/logout` revokes every
session. Both routes exist only in scoped mode (404 in open and `api_token`-only
mode) and only the control port reads the cookie, never the engine's.

- The cookie holds the token entry's name, an expiry and a generation, with an
  HMAC-SHA256 tag. It is `HttpOnly`, `SameSite=Strict`, `Path=/`, with no
  `Domain`, and expires 12 hours after login (absolute: use never extends it).
- `Secure` is set when the request arrived over TLS, when the login's `Origin`
  is https, when a proxy said `X-Forwarded-Proto: https`, or when
  `auth_panel_https` is true. Logout clears the cookie with the same rule.
- The HMAC key is 32 random bytes made at each start and kept in memory, so a
  restart signs everyone out. Logout increments a process-wide generation, so
  every cookie issued before it stops verifying, a stolen copy included. The
  cookie also names its token entry, which must still exist and hold `admin` on
  every request.
- A cookie request is an ambient-credential request: a non-GET needs the same
  `Origin` host and port, `Sec-Fetch-Site: same-origin` when present, and
  `Content-Type: application/json`, as `auth_jwt` requests do (refusals count as
  `csrf`; a bad or revoked cookie counts as `session` and is not retried as
  another credential).
- A 401 in scoped mode on the control port carries `Flodni-Auth: login`, which
  makes the panel show its sign-in form; without that header (open and
  `api_token`-only mode) the panel keeps its bearer prompt.
- Cookies ignore ports: the browser sends `flodni_session` to every port of
  the panel's hostname, so run no other service you do not trust on it.
- The panel page answers `Content-Security-Policy: frame-ancestors 'none'` and
  `X-Frame-Options: DENY`.
- Every control-port response (the panel, every API route, refusals and the open
  routes) carries `X-Content-Type-Options: nosniff` and `Referrer-Policy:
  same-origin`. `Strict-Transport-Security: max-age=31536000` is added only when the
  request is known to be https: it came over TLS, the first `X-Forwarded-Proto` is
  `https`, or `auth_panel_https` is true. The engine port is unchanged.

#### Upgrading from v0.2.x to v0.3.0

With no token configured nothing about the open port changes, except the items
marked "every mode". Read these before the first start of v0.3.0:

- A `config.json` that exists but cannot be read or parsed stops the start, in
  every mode. v0.2.6 started on the defaults and raised the `config_parse_failed`
  alarm: clear that alarm in `GET /api/startup` first.
- `config.json` keys are stricter, in every mode: an unknown key whose name
  contains `auth` (a typo such as `auth_token_file`, or `author`) and a repeated,
  null or re-cased `api_token` or `auth_*` key stop the start. The one that
  bites is `API_TOKEN`: v0.2.6 read the file case-insensitively, so
  `{"API_TOKEN": "..."}` was a working token; it now refuses. Rename it
  `api_token`.
- `api_token` together with `auth_tokens_file` stops the start; see the
  migration order above.
- The library add and inspect routes accept only a `magnet:` link whose first
  `xt` is an anchored 40-hex or 32-base32 hash, in every mode; any other link,
  `http(s)` and `file:` included, is 400 before the engine is called.
- `POST /torrents` (the engine port) no longer reads a `file:` path: the
  link is refused as an unknown scheme. An `http(s)` link is the only fetch an
  add makes: at most 10 MiB, 60 s in all, up to 3 redirects and only on the
  host it started at (a redirect to a `magnet:` link still works). The poster
  check has one 10 s deadline in total, HEAD and GET fallback together, and the
  same redirect rule.
- With `save_to_db` set, `POST /torrents` answers only after the record is
  saved: 200 when it is, 504 when no metadata arrived before the timeout (the
  optional `metadata_wait`, in seconds, bounds it), 500 when the save failed (a
  JSON `error` says which). Without `save_to_db` it answers at once, as before.
  `POST /torrent/upload` with `save` still saves in the background. The library
  add sends its `metadata_wait` on, so a dead swarm answers 504 "no metadata"
  within about that wait plus 5 s, as before, not 502; the add call is capped at
  2 minutes (it was 30 s per call before the engine waited for the record).
- The engine answers 404, not a 307 redirect, to `POST /torrents/` with a
  trailing slash, in every mode.
- Scoped mode, once chosen: both ports deny by default, a wrong bearer is
  refused rather than falling through to a query token, and `gostorm_url` must
  be a loopback address. A caller holding the old `api_token` is refused after
  the restart that adopts the file.

The command-line paths (the entrypoint's `FLODNI_SOURCE_PATH` and
`FLODNI_MOUNT_PATH`) win over `physical_source_path` and `fuse_mount_path` in
`config.json`; a different value there is logged and overridden.

The control server on `metrics_port` (default 9080) is open unless you set a
token. Set `api_token` in `config.json` or `FLODNI_API_TOKEN` (the variable
wins) and every route except the panel page and
`/api/health` needs `Authorization: Bearer <token>`. The Plex webhook cannot send
headers, so `/plex/webhook` and `/webhook` also accept the token in the URL,
for example `http://host:9080/plex/webhook?token=<token>`; no other route
does. `/control` loads without it and asks for the token on its first call,
then keeps it in the browser's local storage. `GET /api/config` never
returns the token, and saving from the panel leaves it unchanged.
It does not return `plex.token` either (empty), but adds
`plex_token_source` (`env`, `file` or empty) so the panel can say whether a
Plex token is in use. The panel's token field starts empty: leave it empty to
keep the stored token, type to replace it. A post with an empty or absent
`plex.token` keeps the one `config.json` holds at that moment (a hand edit
included); an environment token (`FLODNI_PLEX_TOKEN`) is never written to the
file. The panel cannot clear the token; delete `plex.token` from `config.json`.
The stored token is sent wherever `plex.url` points, so a save that changes the
URL's scheme, host or port needs a typed token: without one it is refused with
409 (`re-enter the Plex token when changing plex.url`), and the panel makes the
token field required once the URL field changes. On an open port (no
`api_token`), `POST /api/config` also refuses with 403 a non-empty `plex.token`
and a changed `plex.url`, as it does for `api_token`; set them through
`FLODNI_PLEX_TOKEN` or by editing `config.json`, then restart. A body that leaves
both unchanged or absent saves as before.
With no token enforced (the port is open), two control routes are refused with
403 and a JSON `error`, because any client that reaches the port could
otherwise take the service over: `POST /api/config` when the body carries a non-empty
`api_token` (whatever its value; a body without it, or with it empty, saves as
before and keeps the token `config.json` holds now, one added by hand since the
start included; nothing is written on a refusal), `POST /api/config` while
`config.json` cannot be read or parsed (409, so a file being edited is not replaced
by defaults; with a token set this answers 409 too), and `POST /api/restart`
altogether. Each refusal is logged with
method, path and address. Set the token through `FLODNI_API_TOKEN` or by editing
`config.json` and restart the service yourself; from then on an authenticated
caller can change the token and restart through the API. The Control Panel's
Restart button shows that hint instead of restarting while the port is open.
A panel save keeps the permission bits of an existing `config.json` (a new
file is created 0600), and secrets are better passed as the
`FLODNI_API_TOKEN` and `FLODNI_PLEX_TOKEN` environment variables than stored
in the file. The
same token covers GoStorm's port 8090 (every route except `GET /echo`, header
only, no `?token=`); external clients of 8090 must send it. On an open port
(no token) both servers refuse a request that carries an `Origin` header whose
hostname differs from the `Host` it was sent to: 403, logged with method, path
and address, so a web page in a browser on your network cannot drive the API
with a cross-site POST. Only hostnames are compared, not scheme or port,
because a TLS-terminating tunnel hands Flodni plain http with the original
`Host` while the browser's `Origin` says `https`. A request with no `Origin`
(Plex, curl, scripts, and the GET navigations and `<img>` loads a browser
sends without one) is unchanged; `Origin: null` is refused. Because a page can
still send those GETs, `GET`/`HEAD /stream` on an open port never adds a
torrent: it plays or reports a hash the engine already stores or holds, and
answers 403 ("adding by GET is disabled; use `POST /torrents`") for any other
link, for an `http(s)` or `file:` link (refused before any fetch, so no
server-side request and no local file read), and for `&save` or `&preload`
(they change state). A stored hash wakes from its record only, so the link's
trackers are never used; a record without metainfo answers 404. A page can
still wake a stored hash through `GET /stream` or `GET /play`. With a token
set, `/stream` keeps adding a hash the engine does not hold. Add through
`POST /torrents` or `POST /torrent/upload`. The panel's own
save and restart calls are same-origin POSTs that do carry `Origin`, and pass
because the hostnames match, so a reverse proxy in front of the control port
must preserve the `Host` header (nginx: `proxy_set_header Host $host;`), or
every panel save gets 403; a Cloudflare tunnel preserves it by default. A page
on the same hostname but another port is not stopped. The engine on 8090 no
longer sends CORS headers; its `wipe` torrent action, which deleted every
stored torrent and had no caller, is gone; and `/shutdown` and the Plex
webhook no longer answer GET (`/shutdown` is `POST`, the webhook 405s other
methods), because a GET from a web page carries no `Origin`. There is no
TLS: see `SECURITY.md`.

**DNS rebinding is open by default.** The `Origin` check cannot stop it: the
attacker's hostname resolves to your LAN address, so `Origin` and `Host` both
carry the attacker's name and match. An IP-only deployment is just as exposed,
because the server accepts any name as `Host`. Set a Host allowlist everywhere;
an IP-only deployment uses `["localhost"]`:

| Key | Env | Values | Default |
| --- | --- | --- | --- |
| `allowed_hosts` | `MKV_PROXY_ALLOWED_HOSTS` (comma list, wins over the file) | hostnames, for example `["flodni.example", "flodni"]` | empty: off |

When the list is not empty, both servers (control port and engine port) answer
403 with `host not allowed` to a request whose `Host` hostname is not listed,
with a token or without, and log it with method, path, address and the refused
Host. The match ignores case, the port and a trailing dot. Entries are
lowercased and stripped of a port at load; an entry with a scheme, a path, a
wildcard or a leading dot is not a hostname and is dropped with a warning
(a non-ASCII name is kept with a warning: list the `xn--` form a browser
sends). A list that is not empty as written but has no valid entry, or that has
the wrong type in `config.json`, or an environment value with no entry, fails
closed: the check stays on and admits only the always-allowed hosts below, with
a warning at start. Only an empty list is off. With no token enforced,
`POST /api/config` refuses (403) a body that would change what the next start
enforces: `allowed_hosts`, or a `gostorm_url` host that adds a new name. Edit
the file or the environment and restart. An invalid posted list is refused
with 400. The effective list is logged once at start, and a save from the
panel needs a restart to apply; so does a `gostorm_url` change while the list
is on, because the engine's list is fixed at start.

Always allowed in addition: IP literals (v4 and v6), `localhost` and this
machine's own hostname (in Docker the container's hostname, not the service
name, unless you set `hostname:`). The engine port also admits the hostname of
`gostorm_url`, which Flodni's own calls use. Every other name a client uses
must be listed: a script or a container that calls `http://flodni:9080` needs
`flodni` in the list. Behind a gateway that rewrites `Host`, list the name the
gateway sends, which may not be the name in the Plex webhook URL; the refusal
log shows it. Wildcards are not supported.

With no token, both servers also refuse `Sec-Fetch-Site: cross-site` on any
method, logged like the `Origin` refusals. `same-origin`, `same-site`, `none`
and a missing header pass, so the panel's own fetches pass, behind a TLS tunnel
too. A browser sends the header only over https or to `localhost`, so a page
fetching a plain-http LAN address sends none and only the `Origin` check and the
allowlist apply: it is a second signal, not a replacement. The routes that hold
no data and are open anyway (`/control`, `/api/health`, `/api/live`, the engine's `/echo`)
are exempt, so a link from another site still opens the panel; a cross-site
navigation to any other route is refused without a token.

`GET /api/health` on the control port reports the mount, the engine and the
state database, each as `{"ok": ..., "reason": "..."}`, with an overall
`status` of `ok` (HTTP 200), `degraded` (engine or state database down) or
`down` (mount down), the last two with HTTP 503. It is open even when a token
is set and returns status words only. The image's health check runs
`flodni healthcheck`, which requests it on `127.0.0.1:<metrics_port>` and
exits 0 on HTTP 200 and 1 otherwise; run it with
`docker exec <container> flodni healthcheck`. If `config.json` cannot be read or
parsed at that moment (an edit in progress), it asks the port the file
names if that key still decoded, else the default 9080, rather than report
the daemon unhealthy.

`GET /api/live` on the control port answers `200` and `ok` and checks nothing, so
a container or load balancer that only needs to know the process serves can poll
it cheaply. Unlike `/api/health` it runs no mount, engine or database check and
returns no `warnings`. It is open even when a token is set, for `GET` only
(other methods are refused like any other route).

**An unreadable `config.json` refuses to start.** A `config.json` that exists but
cannot be read, or does not parse (comments are stripped first; a value of the
wrong type counts), stops flodni at start with a line on stderr naming the file
and the cause, in every mode, token or not. Starting on defaults would open both
servers whatever the file said about auth. A missing file is still valid: defaults.
Fix the file (or remove it on purpose) and start again. Before upgrading from
v0.2.6, check `GET /api/startup` for the `config_parse_failed` alarm and clear it:
v0.2.6 started on the defaults and raised the alarm, and this version refuses.

The body also carries `warnings`, an array that is always present and is `[]`
when nothing is wrong. Its one value today is `no_play_webhook`, an advisory:
it never changes `status` or the HTTP code, so a deployment without a play
webhook stays healthy. The array is computed from memory, without a check.
See "Play webhook" below for what raises it.

### Engine state and backup

`config.db`, in the engine's state directory, holds the only copy of each
torrent's metainfo since the strict wake (see "A stub the engine does not
store"). Every commit to it is fsynced, so a crash or a power loss keeps the
records already saved. The file is created with mode 0600; a wider existing
file is tightened to 0600 at start, with one log line. On shutdown the daemon
unmounts, then stops the engine and closes the database. A commit costs two
fdatasyncs (plus a sync when the file grows). The wake save is debounced to
once per torrent per 30 s and skipped when the record is unchanged; the
idle-close save and a record update are not debounced. Watch the first-byte
and wake metrics after upgrading.

A commit that fails (a full disk, an I/O error) is not saved: the wake save
then reports "not saved" and does not start its 30 s debounce, so the next
save retries the record at once, and a removal that failed leaves the record in
place. Every failure is logged and counted in
`flodni_engine_db_write_errors_total{op}` (`set`, `rem` or `clear`, all present
from 0). Alert on any increase: a record that did not commit exists only in
memory and is lost at the next restart. A torrent added through the API is
answered before its record is written (the save runs after the metadata
arrives), so the counter, not the add response, is where a failed save shows.

To back it up, stop flodni and copy `config.db`: a cold copy is the safe one.
To restore, stop flodni, copy `config.db` back and start it.

### Foreign trackers in stored records

Some private trackers' own `.torrent` files carry a second tracker that is not
theirs (a cross-upload, with someone else's key). It is stored in the record, and
every wake announces to it. Two offline commands show and remove them. Both open
`config.db` directly, so **stop flodni first**; if the file is locked they exit 2
with "config.db is in use: stop flodni first". **Copy `config.db` before running
`remove-host`.** Pass the engine's state directory with `--state` (the daemon's
`--path`; without it the binary's directory). Run them as the daemon's user;
root needs `--allow-root`. They never print a tracker URL, path, query or
passkey, only hosts and info hashes.

```
flodni trackers audit [--hashes] [--json] [--allow-root] [--state <dir>]
flodni trackers remove-host [--dry-run] [--hashes] [--allow-same-site] [--allow-root] [--state <dir>] <host>
```

- `audit` is read-only. Per record, the primary tracker is the first tracker of
  the first tier, and its own domain is the registrable domain of its host: the
  last two labels (three under a built-in list of common two-label suffixes such
  as `co.uk`, `org.uk` and `com.hu`). Every other host is listed, most records
  first, as `host  records=N  private=N`. A host whose site name (the first
  label of its registrable domain) is the primary's, such as `t.example.org`
  beside `tracker.example.com`, is listed apart under "same site, other domain":
  it is usually the tracker's own second domain, but a lookalike reads the same,
  so it is never hidden. The summary is `scanned= private= public=
  foreign_records= same_site_records= unparsed_trackers= no_primary=`
  (trackers whose host could not be parsed; records with no parsable host at
  all), and `--json` marks each host with `same_site`. A host that is same-site
  for one record and foreign for another gets one row per class. A "primaries"
  section lists each primary site with its record count: check that it is your
  own, because a record whose `.torrent` lists a foreign tracker first has that
  tracker as its primary. A record whose metainfo is missing or unreadable counts as
  private. The domain rule is an approximation, not the Public Suffix List.
- `remove-host <host>` removes every tracker with that host (case-insensitive,
  trailing dot ignored) from every record, drops tiers left empty and keeps the
  order of the rest. Nothing else in the record changes; each changed record is
  one fsynced transaction. It refuses (exit 2, nothing written) a host in the
  primary tracker's own domain of any record it would touch, always, and a
  same-site host unless `--allow-same-site` is given: check that host in the
  audit first. With `--hashes`, a refusal first lists the hashes of the records
  that cause it. The run is all-or-nothing. A host argument with a port or
  brackets is a usage error, and a host no record has prints "no record has that
  host". Before each write the new bytes are decoded and checked against the
  plan (the trackers as planned, every other field byte-identical); on a
  mismatch it writes nothing more, prints the hash and exits 1. `--dry-run`
  writes nothing and prints the same summary,
  `records_changed= trackers_removed= private= public=`. The flags go before
  the host.

In Docker the image's entrypoint does not forward arguments, so
`docker run <image> trackers ...` starts the daemon. Stop the container, then run
the binary with `--entrypoint` against the state volume. The image has no `USER`,
hence `--allow-root`:

```
docker run --rm --entrypoint /usr/local/bin/flodni -v <state>:/state <image> \
  trackers audit --allow-root --state /state
```

(`docker exec` cannot work here: the tools need the daemon stopped, and
`scrub-stubs` below is the one that is safe beside a running daemon.) Recommended
sequence: stop flodni, back up `config.db`, copy it to a scratch directory and
rehearse there (audit, then `--dry-run`, then a real run on the copy), check the
result with a second audit, and only then put the copy in place and start. Delete
the copies afterwards: they hold passkeys.

## Startup report

Once the FUSE mount is up and the startup walk with its prunes has finished, the
daemon logs one line, `[Startup] report {json}`, and serves the same object at
`GET /api/startup` on the control port; in the `json` log format the line's
`msg` carries that object byte for byte, as in the text format. The endpoint
needs the token like every route but the panel page and `/api/health`, and
answers 503 until the report exists.
It is built once and does not change afterwards. A pre-upgrade rehearsal can
read it instead of grepping the startup lines; those lines keep their text for
now (a test pins them), and the report is the interface that will stay stable
when logging changes.

`schema` is 1. A new field keeps it; a rename, a removal or a changed meaning
raises it.

| Field | Meaning |
| --- | --- |
| `version`, `config_path` | The build and the config file it read. |
| `warmup` | `active`, `dir`, `initial_size_bytes`, `dropped_stale`, `residues_dropped`: the `[DiskWarmup]` start lines. Not active: zeros and an empty `dir`. |
| `state_db` | `active`, `path`, `migration`: `already_completed`, `migrated`, `failed` (the open or the migration failed; the daemon runs on JSON) or `disabled`. |
| `inode_map.files_loaded` | Files in the inode map before the startup walk. |
| `fuse` | `mount_path`, `mounted`. |
| `library_unstored` | `n` of `of`: library torrents with no stored engine record with metainfo. `null` when no stub was found to count. |
| `prunes` | `readability`, `file_state`, `inodes`, each `status` (`ran`, `skipped` or `failed`), a `reason` when not `ran`, and `rows_pruned`. A guarded skip carries the walk's `complete`, `errors` and `stubs`. |
| `invalid_stubs` | Stubs that parse but declare a size out of range. |
| `alarms` | Ids of the alarm conditions that fired, `[]` when none. |
| `auth` | `mode` (`open`, `legacy_token` or `scoped`), `tokens` (count), `scopes` (the sorted union in use) and `jwt`; no name, hash or path. |

Alarm ids: `config_parse_failed`, `config_paths_differ`, `settings_migrated`,
`settings_corrupt`, `torrents_migrated` (a legacy `torrserver.db` was carried
over), `head_density_check` (the one-shot head check ran; see Cache disk),
`corrupt_cache`, `statedb_migrated`, `statedb_open_failed`,
`statedb_migration_failed`, `prune_skipped`, `ghost_inodes_pruned` and
and `state_not_on_mounted_volume` (the container entrypoint's warning). The report
is taken once, so a `corrupt_cache` removal after it is not in it.

## Proxy-signed identity (IdP mode)

`internal/apiauth/jwt` verifies an identity assertion that an identity-aware
proxy puts in a request header (ADR 0004). The
control gate calls it, and `auth_jwt` needs `auth_tokens_file` (without one
the daemon refuses to start). The block, when you add it to
`config.json`:

```json
"auth_jwt": {
  "issuer":   "https://<team>.cloudflareaccess.com",
  "audience": "<application audience tag>",
  "jwks_url": "https://<team>.cloudflareaccess.com/cdn-cgi/access/certs",
  "header":   "Cf-Access-Jwt-Assertion",
  "allowed_emails": []
}
```

The values show Cloudflare Access; `<team>` and the audience tag are your own.
Any proxy that signs an RS256 or ES256 assertion and publishes a JWKS fits,
such as oauth2-proxy. The mode is off when the block is absent, and
`allowed_emails` (optional; ASCII only, matched case-insensitively for A-Z; a
token with `email_verified` false is refused when it is set) is off while
empty. `issuer`, `audience`, `jwks_url` and `header` are required; `header`
may not be `Authorization`, and `jwks_url` must be `https`. Those refusals
(and an incomplete block) stop the start. `auth_jwt`
is startup-only: `POST /api/config` never writes it, and a post that carries a
block that equals neither the file's nor the running one is refused with 403
(a post without one keeps the file's). The audience must be this application's
own, or anyone the proxy admits to another application passes.

What a token must satisfy: exactly one header value (at most 16 KiB) of three
strict unpadded base64url segments; `alg` `RS256` or `ES256`, matching the
key's type (the key decides, never the token); a `kid` that names a key in the
JWKS; no `crit` (`jwk`, `jku`, `x5c` and `x5u` are ignored); a valid signature
(an ES256 signature is exactly 64 bytes); `iss` equal to `issuer`; `aud`
(string or array, not null or empty) holding `audience`; `exp` not past and
`nbf` not ahead, with 60 seconds of skew; a `sub`; and an `email` in
`allowed_emails` when that list is set. An `email` claim with any non-ASCII
byte is refused whether or not the list is set (Unicode case folding would
otherwise let a look-alike match). Failures carry a reason: `missing` (no
header) or `jwt_malformed`, `jwt_alg`, `jwt_unknown_kid`,
`jwt_keys_unavailable`, `jwt_signature`, `jwt_issuer`, `jwt_audience`,
`jwt_expired`, `jwt_not_yet_valid`, `jwt_claims`.

The keys come from `jwks_url` over https with a 5-second timeout, a 64 KiB cap
and no redirects. Only RSA keys of 2048 bits or more and P-256 EC keys on the
curve are kept, honouring each key's `use` (`sig`) and `alg`; a key without a
`kid`, or a repeated `kid`, fails the fetch, as does an empty set. Keys are
cached for 24 hours; an unknown `kid` triggers one refresh, at most once a
minute (after failures the wait grows 1, 2, 4 up to 15 minutes), with one
fetch in flight; a failed fetch is logged once, by cause class only (never the
URL). If the provider is down the last good set keeps working until it is 24
hours old, then verification fails closed. A key the provider revokes stays
trusted until the next successful fetch.

## Play webhook

Some features act on a play that the media server reports by webhook, and do
nothing without one. A webhook that never arrives, is refused by the token
check or matches no file looks exactly like a library nobody plays: nothing
fails, the features are just inactive.

| Feature | Needs the webhook to | State |
| --- | --- | --- |
| Played-file protection (see "Warmup cache eviction") | mark a file played, so eviction ranks it last | Built |
| Reader budget refusal (see "Reader budget") | arm: refusal starts at the first matched play webhook after start, and a webhook confirms a play | Built, opt-in, off by default (`refuse_unconfirmed_reads`) |
| Keep-alive of a confirmed play (see "Keep-alive") | confirm the play, so its torrent is not closed for idleness | Built, on by default (`playback_keepalive_minutes`) |

- **Set it up.** Point the media server's webhook at
  `http://<host>:<metrics_port>/plex/webhook` (`/webhook` answers the same
  way). With `api_token` set, the URL needs `?token=<token>`, because
  Plex cannot send headers; without it every webhook is answered
  401 and nothing is counted. A webhook counts when it is a play or resume
  of a movie or an episode. It is matched when it resolves to one file of
  the library.
- **How a webhook is matched.** Plex Media Server's own webhook names no
  file: its `Metadata` has no `Media`, `Part` or `file` (seen on a live
  server). So a play from Plex is matched by steps 3 to 5 below, and steps 1
  and 2 apply only to a sender that includes the path, in Plex's JSON shape
  (`Metadata.Media[].Part[].file`; see "Other senders"). The first match
  wins: (1) the base name of a reported path, byte for byte, against the
  registered files; (2) with
  `plex_path_map` set, the stub name behind a library symlink (below);
  (3) the IMDb id; (4) when only a library type and an IMDb id are known, the
  one registered file of that type with no id yet; (5) a fuzzy title match,
  which can pick the wrong file. When several files tie on the title and the
  webhook carries a year, only those whose name holds that year stay ("Alien"
  is a substring of "Aliens"). A play is confirmed and marked played only for
  the file the match resolved to.
- **`plex_path_map`** in `config.json` (default empty = off, read live) is for
  a library of Sonarr or Radarr named symlinks into the mount, where the
  media server's file name never equals the stub's (`Show Name - S01E02 -
  Title WEBDL-720p.mkv` against `Show_Name_S01E02_1a2b3c4d.mkv`). It maps a
  media server path prefix to the path of the same files inside this
  container, for example `{"/movies/": "/media/movies/", "/series/":
  "/media/series/"}`; both sides must be absolute, and an entry that is not
  is logged as a `WARNING` and ignored. Only when no file name matched, each
  reported path is taken through the longest matching prefix, checked once
  with `lstat`, and when it is a symlink its target's base name is matched
  against the registered files like a file name (a relative target is read
  against the link's folder). It does not walk directories or follow a chain,
  and a missing path or a regular file matches nothing, so the later steps
  run as without it. The same name decides which file a play marks as played
  (the strictness of that choice is unchanged) and which file a stop
  releases. Use it when
  `flodni_playback_webhooks_received_total{matched="false"}` moves for a
  library of symlinks; the library must be mounted read-only in this container
  at the mapped path. The lookup is meant for a local (not network) library
  bind and runs inline in the webhook handler, at most 8 paths per webhook: a
  hung mount delays that webhook. Prefixes and targets are cleaned at load;
  a prefix given twice keeps the first in sorted order, with a warning.
  Unit-tested against symlinks in a temporary folder. It never matches a
  webhook Plex Media Server sends, which names no path (above); it needs a
  sender that does.
- **Which rule matched.**
  `flodni_playback_webhook_match_rule_total{rule}` counts the same webhooks
  by rule: `basename`, `path_map`, `imdb` (the id, or the bootstrap), `fuzzy`
  or `none`, and `linked` (below). `fuzzy` is a guess. With Plex as the
  sender, `basename` and `path_map` stay at 0 and plays land on `imdb`,
  `linked` or `fuzzy`; `imdb` is exact only while one registered file holds
  that id.
- **Several files fit: the linked one.** Where a library holds Sonarr or Radarr
  symlinks into the mount, a release an upgrade replaced stays as its own stub
  and no symlink points at it any more, so several stubs can share one IMDb
  id. A background task, never the webhook handler, walks the targets of
  `plex_path_map` (the container-side roots) every 5 minutes, and once more,
  at most every 30 seconds, after a webhook found several candidates. It uses
  the directory listing and `readlink` only: it never follows a link, opens a
  file or touches a target, and a dangling link still counts. It publishes the
  set of stub base names some library symlink points at (a relative target is
  read against the link's folder). When the IMDb pass or the fuzzy pass (the
  best level, with ties) yields more than one candidate for a play, resume or
  stop, and exactly one of them is in that set, that one is the match. For a
  show, the fuzzy tie counts only files whose own season and episode equal the
  webhook's, and none is picked when the webhook carries no season or episode;
  for a movie, only files whose name carries the webhook's year, and none is
  picked when it carries no year (a title can be a substring of another).
  A play matched this way is counted as `rule="linked"` (a stop is not counted
  by rule), and it marks that file played when the played mark's own tiers
  reach the same file. A stop matched this way is honoured with a handle open,
  like a unique IMDb match, only when the file's season and episode (a
  movie's year) agree with the webhook's. The library-type bootstrap (step 4)
  is not narrowed. With none or several of them linked, with no
  `plex_path_map`, or before the first walk has finished, matching is as
  before; one candidate is never narrowed.
  Limits: a set older than 15 minutes (three walks) is not used, so a walk
  that keeps failing falls back to the matching above; the webhook that asks
  for an early walk is matched with the old set; a walk that exceeds its
  bounds (2 million entries or 2 minutes) or cannot read a root keeps the
  previous set; an unreadable folder inside a root is logged once and skipped,
  and a stub reached only through a folder symlink (never followed) or such a
  folder counts as unlinked; a root that is itself a symlink is resolved once
  per walk; the roots must be mounted in this container. Unit-tested against
  symlinks in a temporary folder; not run against a media server.
- **Other senders.** Any client may post the same JSON (`Content-Type:
  application/json`, or a form field `payload`), with the same token rule.
  The handler reads:
  - `event`: `media.play`, `media.resume` or `media.stop`; anything else is
    ignored.
  - `Metadata.librarySectionType`: `movie` or `show` (not `episode`);
    anything else is ignored.
  - `Metadata.Media[].Part[].file`: the file's path in the sender's library,
    for steps 1 and 2.
  - For the fallbacks: `Metadata.title`, `grandparentTitle`, `year`,
    `parentIndex` and `index`. Numbers may be quoted.
  - An IMDb id anywhere in the body as `"imdb://tt…"`, for example
    `"Guid":[{"id":"imdb://tt…"}]`.

  A stop is paired by the same matching, not by a player or session id. A
  body that does not parse is logged and dropped.
- **Check it.** The startup line, `[Webhook]`, says when the last matched
  play webhook arrived (`never` if none), whether played-file protection has
  a recorded play (`active`) or not (`inactive`), whether the reader budget
  refusal is `off` or `on` (it arms at the first matched play webhook after
  that start, never from a stored time), the warning window, and the
  keep-alive settings (`keep-alive: on (15 min ..., max 4 torrents)`, with
  `inactive until a play webhook matches` while none has matched, or
  `keep-alive: off`). At run time
  `flodni_playback_webhooks_received_total{matched}` counts the webhooks and
  `flodni_playback_webhook_last_timestamp_seconds{matched}` is the time of the
  last of each kind (0 = none seen). `matched="false"` moving while
  `matched="true"` does not means webhooks arrive but resolve to no file.
  A webhook refused for a missing or wrong token logs
  `[Control] 401 POST /plex/webhook from <address>` (or `/webhook`).
- **The warning.** It is raised when a reader session crossed
  `unconfirmed_read_margin_mb` and closed with no play confirmed within
  `playback_webhook_warn_days`, and no matched play webhook arrived in that
  time (a webhook never received counts). It appears as `no_play_webhook` in
  the `warnings` array of `/api/health`, as
  `flodni_warning{name="no_play_webhook"}` set to 1, and as one
  `WARNING: [Webhook]` log line when it turns on (one more line when it
  clears, none while it holds). It is re-evaluated hourly and when such a
  session closes. A session that crossed the margin is one the budget
  counted; see "Reader budget" for what counts.
- **A false warning.** A hasher, a copy or a media server's analysis pass
  also closes as an unconfirmed session, so the warning can fire in a
  library where nobody played anything and none was expected. When every
  such session since start (or since the last matched webhook) came from a
  process named like a known hasher or analysis tool (`md5sum`, `sha1sum`,
  `sha224sum`, `sha256sum`, `sha384sum`, `sha512sum`, `b2sum`, `cksum`,
  `xxhsum`, `ffprobe`, `mediainfo`), the log line says it is possibly a false
  warning. The warning still fires; nothing is excluded. The name is empty
  for a reader in another PID namespace (a media server in its own
  container), which then counts as unknown, so the note appears only for
  tools run where Flodni can see the process. The process name is
  not known for times restored from a restart, so a restart removes that
  note. A file smaller than the head and tail windows plus the margin never
  crosses the margin, so a library of only small files stays silent.
- **`playback_webhook_warn_days`** in `config.json` (default 7, read live; 0
  turns the warning off, the startup line and the series stay; a negative or
  absurd value is logged and replaced by 7).
- **State.** The three times (last matched webhook, last unmatched webhook,
  last unconfirmed session) are in the state database table `daemon_marks`
  (schema 15), written at most once an hour each. With the state database
  off they live since start and are gone at restart.

## Keep-alive

Built (Reliability step 8), on by default. The engine closes a torrent that
has had no reader for its idle timeout (15 s unless the engine's settings say
otherwise), which keeps idle titles cheap. A media server reads in bursts, and
a direct play or a throttled transcoder can wait longer than that between
reads; the torrent then closes and the next read reopens it with a new
announce and a new search for peers. The keep-alive stops the close while a
play is confirmed.

- **What it holds.** A torrent is not closed for idleness while a file of it
  has a webhook-confirmed playback that is not stopped. It needs the play
  webhook: with none configured, reachable and matching, nothing is confirmed
  and the keep-alive silently does nothing. See "Play webhook". A reader with
  no confirmed playback, a library scan, warm on add and catch-up keep the
  plain idle close. The hold is decided from the playback state each time the
  engine would close the torrent, so a torrent that was closed and reopened
  is held again and nothing is left to clear. It only suppresses a close: it
  adds no tracker, DHT, PEX or wake, so a private torrent gets nothing it
  would not have had.
- **How it ends.** A stop webhook releases the torrent at once; the engine
  then closes it after the stop's 30 s grace. Otherwise it ends
  `playback_keepalive_minutes` after the last activity, which is the later of
  the last read of the file and the last play or resume webhook. A pause
  webhook does not extend it, so a pause longer than the ceiling costs one
  reconnect on resume. A missed stop webhook is covered by the same ceiling.
  A read of a confirmed file whose stop never arrived (a scanner touching it)
  starts the ceiling again.
- **The cap.** At most `playback_keepalive_max_torrents` torrents are held at
  once. The most recently active win; a torrent over the cap falls back to the
  plain idle close and is counted once in `flodni_keepalive_evicted_total`.
  The ranking counts every confirmed playback, including torrents that still
  have a reader and so need no hold, so the held gauge can sit below the cap.
  Each held torrent keeps its engine cache, up to `CacheSize` (default
  128 MB), and its peer connections. Size the cap so that cap x `CacheSize`
  stays well under the container's memory limit: four held torrents at 512 MB
  is about 2 GB. The read-ahead cache is one global budget and does not grow
  with the number of plays. A held torrent keeps uploading what its cache
  holds, under the configured limits, for the play plus the ceiling.
- **The off switch.** `playback_keepalive_minutes` (default 15, read live) is
  the one switch: 0 turns the keep-alive off and every torrent idle-closes, a
  confirmed one included (priority no longer holds a torrent open). A negative
  value or one over 1440 is logged and replaced by 15. A ceiling above 15
  minutes is cut short when no handle is open, because the playback state is
  dropped 15 minutes after the last activity.
  `playback_keepalive_max_torrents` (default 4, read live) is the cap, never a
  switch: its minimum is 1, and 0 or a negative value is logged and replaced
  by 4.
- **Metrics.** `flodni_keepalive_held_torrents` (gauge),
  `flodni_keepalive_releases_total{reason="stop|ceiling"}` and
  `flodni_keepalive_evicted_total`, all present from 0; and
  `flodni_torrent_idle_closes_total{confirmed}` and
  `flodni_torrent_reopens_total{confirmed}` to see whether confirmed plays
  still close. The `[Webhook]` startup line states the effective settings.
- **Limits.** The 15 s idle timeout still applies to a torrent with no
  confirmed playback. The read-ahead pump's own 2 hour idle limit for a
  confirmed session is unchanged, and an open stream still restarts its engine
  reader every idle timeout; neither is part of the keep-alive. The state is
  rebuilt at most once a second, so a read shows in the hold within a second;
  a stop or a confirming webhook takes effect at once. Tested by unit tests
  and by the end-to-end `keep-alive` variant (a confirmed torrent is held
  through two idle windows and released by a stop webhook); the ceiling is
  unit-tested only.

## Engine stall gaps

Two gaps in the vendored engine could leave a reader waiting. Both are
implemented and tested with stand-ins; neither was shown to remove a stall on
a real swarm in this repository's end-to-end test, which no longer stalls at
the places they were first seen (the stale cache priorities, now dropped).

- **An idle peer is not woken by an order-only change (always on).** The
  engine asks its peers to update their requests when a priority change alters
  the set of pieces it wants, but not when the change only reorders them. A
  peer with nothing in flight then sits idle until the next chunk, keepalive
  or announce. The cache that sets the priorities (`torrstor`) now asks every
  peer of the torrent to update its requests after a priority change it
  makes, through the engine's existing public `AllowDataUpload` (Flodni never
  disallows uploads, which a test pins by scanning the source for
  `DisallowDataUpload`; so its only effect is that update). The engine itself
  is not changed. The engine package pins both halves in `peer_wake_test.go`:
  an order-only change wakes no peer, and `AllowDataUpload` wakes every one.
  The wake fires only when a priority differs from what the cache set before
  (a repeated identical set wakes nobody), and wakes within 20 ms of each
  other are merged into one, run after the last of them. It wakes every peer,
  including choked ones the engine's own wake would skip; the cost on a large
  public swarm is not measured. It is a fix and has no switch.
- **`redial_dropped_peer`** in `config.json` (default `false`, read when a
  peer drops). The engine does not keep a dropped peer to redial, and floors a
  tracker's announce interval at one minute, so a private torrent whose only
  peer drops waits up to a minute to hear of it again. With the switch on, when
  a connection that this torrent dialed from its own tracker drops, and
  3 s later the torrent is private, still missing data and has no connected
  peer, that address is added back to the same torrent once. Nothing else is
  involved: no DHT, no PEX, no public tracker, no address from anywhere but
  the dropped connection; connections from any other source (DHT, PEX,
  incoming, hole-punch, direct) are never redialed, nor is a torrent that
  is not private. A torrent is redialed at most once a minute, so a seeder that
  drops again at once is not called in a loop (the engine's own dial limits
  still apply). The 3 s and the 1 minute are constants. The series
  `flodni_engine_peer_redials_total` counts the redials. The privacy cases are
  pinned in `internal/gostorm/torr/redial_test.go`. Not covered: an incoming
  connection that drops (its remote port is not a listening one), a peer
  restored from the stored torrent record when a closed torrent wakes (the
  engine tags those as direct, so the redial skips them, and the main
  single-seeder case after a wake is therefore not covered), and two peers
  dropping together (one address is redialed, not both). A redial the torrent
  did not take (an address it already knows) is not counted.
- **Re-announce after the tracker's `min interval`** (engine; no switch).
  The engine now keeps the `min interval` an HTTP tracker sends with its
  announce interval (a UDP tracker sends none). The engine method
  `AnnounceWhenDue` lets a torrent whose swarm is starved announce again
  after that `min interval`, and only if the tracker sent one that is shorter
  than its interval. It is for private torrents, whose regular announce
  cannot be shortened (a tracker's `min interval`, the unofficial key most
  trackers send, is the least a client must wait; its regular interval is
  longer): with no `min interval`, or with a UDP tracker, it sends nothing
  and returns 0. The regular announce loop and the
  forced announce share one per-tracker record, so neither starts while the
  other is in flight, the regular loop waits its normal interval from the
  forced announce's end, and at most one forced announce goes out between two
  regular ones. A forced announce follows only a successful regular one; if it
  fails, the regular loop's next announce moves to the later of its previous
  due time and one min interval after the failure (never a full interval from
  it, never sooner than the min interval). A malformed `min interval` is
  ignored and the announce proceeds. A private torrent reaches only the
  trackers it carries: no DHT, PEX or added tracker. Implemented and tested with a fake tracker in
  `internal/anacrolix-torrent/forced_announce_test.go`; the daemon does not
  call it yet, so a private torrent still gets nothing beyond the regular
  loop in a running Flodni.
- **`engine_refill_v2`** in `config.json` (default `false`, read once at
  start: a change needs a restart). Four request-refill changes adapted from
  upstream, none of them measured on a real swarm yet, so they are opt-in:
  a refill sorts at most the larger of two pieces and four queues of
  candidate chunks instead of the whole readahead window; a peer whose
  requests another peer holds may be robbed of them when it has delivered
  nothing for 2 s while expecting chunks (the steal grace still applies);
  a peer's queue is topped up when it falls to half of its target instead of
  at empty, paused for 5 s after the peer rejects a request it held; and the
  request-update timer, which is compiled out today, would use a timer that
  releases its function when stopped. The queue target is whatever the
  engine's adaptive pipeline (or Gradient2) says, so the half-queue point and
  the candidate cap follow it. With the switch off none of this code is
  reached: the refill decisions are the ones the engine made before. To A/B
  it on one swarm, run a period with it off and a period with it on, same
  library and viewers, and compare `flodni_read_stalls_total`,
  `flodni_session_first_byte_seconds`, `flodni_readahead_refetched_bytes_total`
  and the sessions' `stalls` and
  `max_stall_ms`. The
  decisions are pinned in `internal/anacrolix-torrent/refill_flagoff_test.go`
  (switch off) and `refill_v2_test.go` (switch on).
- **`engine_cold_boost`** in `config.json` (default `false`, read once at
  start: a change needs a restart). When a player starts reading a file, that
  is the primary handle's first read past the header region (the SSD head
  window), the torrent's per-connection limit is raised from the configured
  value to 35 if it is below 35 (the default is 25), until 1 MiB of useful
  data has arrived or 30 s have passed. Then the configured limit is restored
  exactly once, also when the torrent closes during the boost. Library scans,
  warm on add, probe handles, header reads and tail probes never start it,
  and a later seek on the same handle does not start it again. The end is
  checked on the once-a-second stats tick, so the boost ends within a second
  of either condition. It helps a swarm with more peers than the limit, where
  a cold start waits for one of them to unchoke the torrent; it does little
  for a small private swarm that has fewer peers than the limit anyway. A
  limit of 35 or more is left alone. It adds no peer source: discovery is
  unchanged, so a private torrent stays private. To A/B it, run a period with
  it off and a period with it on, same library and viewers, and compare
  `flodni_session_first_byte_seconds` and `flodni_read_stalls_total`. With the
  switch off the code is not reached. Pinned in
  `internal/gostorm/torr/coldboost_test.go` (the engine side) and
  `internal/daemon/coldboost_handle_test.go` (when it starts).

## Readability record

`GET /api/torrents/health` on the control port lists what Flodni has
recorded about each torrent's reads. With a token set it needs the bearer
header (no `?token=`). It answers 503 when the state database is off:
nothing is recorded then. The fields below are the supported contract for
outside tools; log wording and stub names are not (the `[Session]` log
line below is the one log format that is).

| Field | Meaning |
|---|---|
| `hash` | Full info hash, lower-case hex. Join it with `GET /api/library/list`. |
| `consecutive_failures` | Sessions in a row that ended with a failed read and nothing served by the swarm. A session counts once, and only if it lasted 25 s. |
| `first_failure_at` | Start of the current run of failures; `null` when the count is 0. |
| `last_failure_at` | Last recorded failure; kept after a reset. |
| `last_failure_reason` | `no_peers`, `timeout` (peers connected, the read gave up), `not_active` (the engine did not have the torrent running), `unknown` (the engine did not answer in time), `metainfo_timeout` (the metainfo did not arrive within a wake's 45 s wait, see below), `not_stored` (the engine refused to wake the torrent because it keeps no record of it; not a failed read, see below); `null` for older records. |
| `last_failure_source` | `mount`: a reader through the FUSE mount. `wake`: a wake whose metainfo never arrived (an open through the mount, or a warm-up). |
| `last_swarm_read_at` | Close time of the last session in which the swarm served a read, or the delivery time of a warm-on-add fill that completed a file's windows from the swarm; `null` if none was recorded. |

`?failing=1` keeps torrents with at least one consecutive failure,
`?min_failures=N` those with at least N.

- Only bytes from the swarm reset the count. A cache hit never does.
- `not_stored` is not a swarm failure. A read through the mount on a torrent
  the engine refused to wake (see "A stub the engine does not store") sets
  `last_failure_reason` to `not_stored`, `last_failure_source` to `mount` and
  `last_failure_at`, once per handle. A wake before the engine is up
  (`engine not ready`) is never recorded. It does not change `consecutive_failures`
  or `first_failure_at`, so such a torrent is not listed by `?failing=1` or
  `?min_failures=N`, and it is never a dead-swarm verdict. A session on it
  reports `failed_reads` 0. A later swarm read or failure replaces the reason.
- No entry means no evidence either way, not that the torrent reads.
- A `null` `last_swarm_read_at` is not a failure: reads answered from the
  caches do not set it.
- A session whose bytes came from the read-ahead pump rather than direct
  fetches may not set `last_swarm_read_at`; treat a single failure as weak
  evidence.
- A wake that had to fetch the metainfo from the swarm resets the count
  without setting `last_swarm_read_at`.
- `metainfo_timeout` is a swarm-side failure: the wake waited 45 s and the
  swarm could not deliver the metainfo (a private torrent with no peers looks
  like this). The open fails with EIO before a session exists, so it is
  recorded by the wake, with `last_failure_source` `wake`. It adds one to
  `consecutive_failures` (and starts `first_failure_at`), but at most once per
  torrent per 10 minutes: a player's retries, each a 45 s wait, are one
  occasion, so a burst cannot reach a threshold. A metainfo that arrives, or a
  swarm read, ends the streak. The check discovers no peers and never
  contacts one: it only reports a wait that already ran. A cancelled open or
  a torrent closed during the wait is not recorded.
- A cancelled read, a read Flodni refuses for the reader
  budget, and an open that fails before any read are not recorded either.
- Times are UTC and are taken when the session closes, up to two minutes
  after its last read.
- An entry goes when the last stub of its torrent is removed through the
  library API, replaced by an add, or deleted through the mount. A session
  still open on that torrent is closed first, by the usual close rules (a
  session under 25 s gets no dead-swarm verdict), so it cannot write the
  entry back. A stub deleted directly on disk leaves the entry in place. Such
  entries stay until a startup prunes them. The prune runs only after a
  complete scan: both `movies` and `tv` exist and each held at least one
  stub, and no read failed. It is skipped otherwise, and the log says so. A stub
  that parses but declares a size outside the valid range is logged with
  its path and counted separately (the start-up summary line); it does not
  block the prune and its torrent's rows stay. An unreadable directory
  or file, or a stub that does not parse, still blocks it. The periodic
  inode prune applies the same test to its own walk.
- Join from `GET /api/library/list`: an entry whose hash that list does
  not carry belongs to a torrent that left the library. Ignore it.
- New reason and source values may appear; ignore the ones you do not
  know. `GET /api/library/list` always reports `file_index` (the
  engine's index, starting at 1; 0 means the stub carries none). Per-file
  records are the experimental `files` array below.

### Per-file records (experimental)

Each torrent object carries a `files` array, always present (`[]` when empty)
and followed only by `swarm` (below). It is **experimental**: it is outside
the contract sentence above and the stable-names promise, and its names may
change until its first consumer runs against it. It is served only with the
state database on. Three things write it. In index mode (`warmup_dir` set), a read through
the mount that leaves a file's head and tail windows both complete records the
file. In any cache mode, a quota eviction records `cache: evicted`, also for a
file with no earlier record. With `warm_on_add` set, the warmer records every
attempt, its catch-up included (see "Warm on add"). A file nobody opened and
the warmer never tried has no record. A file whose windows were already
complete before this version may have none until a later read writes to them.
A torrent with records and no failure row is listed with
`consecutive_failures` 0 and nulls, unless `?failing` or `?min_failures` is
set; those keep their per-torrent meaning.

| Field | Meaning |
|---|---|
| `file_index` | The engine's file index, as in `GET /api/library/list`. |
| `readability` | `warmed`: the swarm delivered a piece under the windows (the engine stamps each piece when it passes its checksum); `present`: warm on add completed the windows and the engine holds no delivery time (rare); `failed`: a warm-on-add fill timed out; `unknown`: no verdict. `present` never replaces `warmed`; `failed` does, and the delivery time stays. The mount path writes only `warmed` and leaves `unknown` when the engine holds no stamp. |
| `cache` | State of the windows on disk, separate from `readability`: `cached`; `evicted` (quota eviction); `dropped_quota` and `dropped_no_evidence` (warm on add found no room and did not fetch, the first when it was allowed to evict never-played files, the second when it was not); `lost` (the windows vanished with no quota eviction mark) and `write_failed` (a write failed), both retried; `unknown` before any record. It is the last recorded event, not a live probe: a cache directory wiped by hand still reads `cached`. |
| `failed_attempts` | Warm-on-add fills in a row that got no bytes from the swarm in time; a `warmed` record resets it. Cache-side failures do not count. |
| `first_failed_at`, `last_attempt_at` | Start of the current run of failed fills, and the last warm-on-add attempt that recorded; `null` when unset. The mount path sets neither. |
| `last_swarm_delivery_at` | When the engine last completed a piece under the windows, not when the windows were written: a fill that read from RAM hours later still carries the delivery time. |

- Eviction changes `cache` only; it never changes `readability`.
- Times are RFC 3339 UTC or `null`.
- A scan by a media server that pulls the windows from the swarm counts as a
  swarm delivery, because the stamp is the fact, not who asked.
- The mount path does not touch `last_swarm_read_at` or the failure count of
  the torrent; the session verdict still owns those. Warm on add stamps
  `last_swarm_read_at` (with the delivery time, and it ends the failure
  count) only for a `warmed` outcome; `present`, `failed` and the cache-side
  outcomes never touch the per-torrent record.
- The series `flodni_warm_fills_total{trigger="mount",outcome}` counts the
  records the mount path wrote (`warmed`, or `cached` when no stamp exists);
  `trigger="add"` is warm on add.

### Swarm signal (experimental)

Each torrent object also carries a `swarm` key, always present, followed only
by `scrape` (below). It is **experimental**, outside the contract sentence above and the
stable-names promise, like `files`. It is `null` for a torrent the engine does
not have open: Flodni never wakes a closed torrent to fill it, and a torrent
with no record is not listed here at all.

| Field | Meaning |
|---|---|
| `awake` | Always `true` when the object is present. |
| `peers` | Peers connected to the engine now. |
| `seeders` | Connected peers that have every piece; `null` until the metainfo is in. |
| `last_piece_at` | RFC 3339 UTC time the engine last completed a piece of this torrent; `null` if none since it opened (the stamp is not kept across a close). |
| `rate_bps` | Download rate in bytes per second as the engine's one-second ticker last measured it. |

- The values come from a snapshot the daemon rebuilds every 15 s from the
  engine's existing stats; a request reads the snapshot and does not touch the
  engine. A torrent opened or closed since the last snapshot shows the old
  state for up to 15 s, and nothing is served before the first snapshot.
- `flodni_swarm_awake_torrents` (gauge) is the number of torrents in that
  snapshot.
- Not a verdict: zero peers on an awake torrent is a fact about now, not a
  dead swarm. Tested against a fake engine only; not run against a real swarm.

### Tracker scrape (experimental)

**Experimental**, apart from the stable fields listed below, and outside the
contract sentence above like `files` and `swarm`. Flodni asks each stored torrent's own
trackers how many seeders and leechers they know and reports the best answer
per torrent. Built and tested against local fake trackers (HTTP and UDP), not
against a real tracker.

**Stable fields.** These keys now have a consumer and are stable: they keep
their name and JSON type, and they are present in every object, null included.
- In each torrent's `scrape`:
  - `seeders` (number or null);
  - `answered`, `failed` and `unknown` (numbers);
  - `source` (`"http"`, `"udp"` or null);
  - `error` (string or null);
  - `run_id` (number).
- The top-level `scrape_pass.run_id` (number).

A test pins them (`scrape_contract_test.go`). The other fields stay
experimental: `leechers`, `scraped_at`, `unscrapable` and
`scrape_pass.finished_at`. So does the wording of `error`; its value set may
grow.

- **Which trackers.** The announce list of the torrent's stored record, as it
  is. A private torrent (BEP 27), and one whose metadata is not known yet and
  so might be, is asked at its own trackers only: every tier of its record
  that holds a host of the public lists Flodni adds on its own account
  (matched by lower-case host, so a rotated-out entry, another path, scheme or
  case still matches) is dropped whole, and there is no DHT, PEX or public
  fallback in this path. Residual gap: a record saved before the retracker
  gate keeps no record of where a tier came from, so a legacy tier in which no
  entry's host is in any current list cannot be told apart from the torrent's
  own. HTTP trackers are scraped at the BEP 48 URL (the last
  path segment `announce` becomes `scrape`; a tracker without one is skipped
  and counted), UDP trackers per BEP 15. Other schemes are skipped.
- **Passes.** A pass scrapes every stored torrent once. Hashes of one tracker
  go together: `tracker_scrape_batch` per HTTP request (default 100, 1 to 200;
  UDP keeps its 70 per packet). Each tracker host has its own worker, with its
  own timeout (10 s per request) and backoff: at least `tracker_scrape_pace_ms`
  (default 1000, 100 to 60000) between two requests to one host, doubling after
  each failed request (30 s, up to 6 hours or the interval), at least the
  tracker's own `min_request_interval` or failure reason interval, and after
  three failures in a row the host's remaining torrents of that pass get an
  error. A slow or failing host never delays another host. A block longer than
  2 minutes (a long backoff, or a tracker's long minimum interval) is never
  waited out inside a pass: the host's remaining torrents end that pass with
  `host backing off`, and the next pass serves the torrents tried longest ago
  first. A pass therefore ends in minutes, not hours.
  **Network.** HTTP scrapes use the engine's tracker proxy and user agent when
  one is configured; with a proxy configured UDP trackers are not scraped
  (UDP cannot go through it; logged as `udp skipped: proxy`), and an invalid
  proxy setting stops the scrape from starting. Before the torrent store is
  open (the first seconds after start) a pass is refused, `POST` answers 503.
  Answers over 512 KiB, nesting beyond 6 levels and redirects are refused.
- **Schedule.** `tracker_scrape_hours` (default 24, 0 to 8760) is the interval
  between scheduled passes, each start varying by up to 10 %, and each host
  waiting a random part of interval/48 (at most 10 minutes) so hosts do not
  start together. **0 turns the schedule off**; the on-demand route still
  works. The settings are read at start-up. Results are in memory: after a
  restart a torrent shows `scrape: null` until the next pass reaches it.
- **On demand.** `POST /api/torrents/scrape` (all torrents) or
  `?hash=<40 hex>` (one) starts a pass and answers 202 at once with
  `{"run_id": N, "started_at": ...}`. While a pass runs a second POST answers
  409 with the running `run_id`; an unknown hash is 404. Both routes need the
  bearer token when `api_token` is set. A manual pass has no start jitter but
  keeps the pacing and backoff.
- **Status.** `GET /api/torrents/scrape/status` returns the latest pass:
  `run_id`, `trigger` (`schedule` or `manual`), `scope` (`all` or `one`),
  `state` (`running` or `done`), `started_at`, `finished_at` (null while
  running), `torrents`, `ok`, `errors`, `skipped` (counts of torrents:
  answered, ended in an error, nothing to scrape), and `previous` with the
  finished pass before it (or null).
- **Health.** Each torrent object has `scrape`, null until a pass has reached
  it:

| Field | Meaning |
|---|---|
| `seeders`, `leechers` | The most seeders among the trackers that **answered with a count** (the leechers of that answer): 0 if every tracker that answered said 0; **null, never 0, when no tracker gave a count** (all failed, or all omitted the hash). For HTTP only a hash present in the tracker's `files` is a count (0 included); an omitted hash is "not known to this tracker" and is not counted. UDP answers a fixed triple per hash, so its 0 stays 0, and on a 0 tie an HTTP answer wins over a UDP one. |
| `answered`, `failed`, `unscrapable`, `unknown` | Integer facts about the torrent's trackers in that pass: gave a count; failed, timed out or were skipped as `host backing off`; cannot be scraped (`wss://` or another scheme, no BEP 48 scrape URL, UDP behind a proxy); answered over HTTP and omitted the hash. Flodni reports the facts and **the verdict is the caller's**: a 0 with `failed` above 0 is a weaker signal than a 0 with `failed` 0, and a 0 with `answered` 1 of several trackers is weaker than a 0 from all of them. |
| `source` | `http` or `udp`: the answer the numbers came from (a UDP 0 may mean unknown, so a consumer can weight it lower); null when there are no numbers. |
| `scraped_at` | RFC 3339 UTC time the entry was written. |
| `error` | Set only when no tracker gave a count, else `null`: the first failure as the host (`scheme://host:port`) and a failure class (`timeout`, `connection failed`, `http status N`, `tracker: <its reason>`, `malformed response`, `response too large`, `host backing off`, `internal error`, ...), or `unknown_to_tracker` when the trackers that answered all omitted the hash. Never a URL. |
| `run_id` | The pass that wrote the entry. |

  The response also has a top-level `scrape_pass` with the `run_id` and
  `finished_at` of the last finished pass over all torrents (null before the
  first; a one-torrent pass or one cut short by a shutdown does not count).
  If one tracker of a torrent answered and another failed, the answer is used
  and there is no error; `failed` says so. A torrent a pass did not reach (a
  one-torrent pass, a shutdown, a torrent added during the pass, no scrapeable
  tracker) keeps its
  previous entry with its old `run_id`; nothing writes an empty or zero entry.
  Entries of torrents that are no longer stored are dropped at the next full
  pass.
- **Secrets.** Announce URLs carry a passkey. No log line, health field,
  status field or metric holds an announce or scrape URL, a path or a query;
  they name the host only, and text a tracker sends back has every token of
  the announce URL (case-insensitively, longest first, and the first and last
  8 characters of a long one) replaced by `***` and is cut to 80 characters.
- **Metrics.** `flodni_tracker_scrapes_total{result}` (`ok`, `error`,
  `skipped`, counted per torrent and tracker; a hash a tracker omitted
  counts as `error`), `flodni_torrents_by_seeders{bucket}`
  (`0`, `1-2`, `3+`, torrents with numbers only),
  `flodni_tracker_scrape_pass_running` (0 or 1) and
  `flodni_tracker_scrape_last_pass_finished_timestamp_seconds` (absent before
  the first full pass).
- Not covered: a tracker that answers 0 for a torrent it does not know reads
  as 0 seeders (BEP 48 gives no way to tell them apart), trackers behind
  redirects or authentication other than the URL, IPv6 literals in UDP
  trackers are untested.

## Session log

A session covers the reads of one torrent-backed file, from an open until
the file has been idle for a minute. `GET /api/sessions` lists the open
sessions and the last 200 closed ones, in memory. Each closed session is
also written to the log as one line, so a log store keeps the history. The
image tees the log to `tiramisu.log` under `$FLODNI_LOG_DIR` (default
`/state/logs`); that file survives a container recreate only when its
directory is on a mounted volume, and it is not rotated. Without the log FIFO the line
goes to stdout only. A session still open at shutdown is closed then, with
the same rules: one younger than 25 s is never condemned as a dead swarm.

```text
[GoProxy] 2026/11/12 09:02:10 [Session] {"path":"/mnt/flodni-real/tv/Example/Example.S01E01_0123abcd.mkv","hash":"0123456789abcdef0123456789abcdef01234567","size":31461601,"opened_at":"2026-11-12T09:00:05Z","closed_at":"2026-11-12T09:02:10Z","bytes_served":31461601,"first_byte_ms":12,"reads":241,"failed_reads":0,"outcome":"served","file_index":1,"bytes_fetch_block":135393,"bytes_racache_hit":31326208,"bytes_warmup_head":0,"bytes_warmup_tail":0,"swarm_served":true,"duration_ms":125000,"failure_reason":null,"reopens":0,"stalls":0,"max_stall_ms":0,"confirmed":false,"id":7,"resumed_from":0}
```

Everything after `[Session] ` is one JSON object on one line, the same
object `/api/sessions` returns (an open session there also carries the
four fields marked "open sessions only"); there it has outcome `open`,
no `closed_at` and live counters, so the byte fields can differ from
`bytes_served` by a read in flight. Parse the JSON; do not grep the path,
which `json.Marshal` escapes (`&`, `<`, `>`). Key order is not part of the
contract. The prefix and the fields below are the
supported contract: fields may be added; none is renamed, retyped or
removed. Ignore fields and outcome values you do not know. The `[TTFF]`
and `[DeadSwarm]` lines are unchanged, and their wording is not a contract.

| Field | Type | Meaning |
|---|---|---|
| `path` | string | The stub's path in the library. |
| `hash` | string | Full info hash, lower-case hex. |
| `file_index` | number | The index the stub carries, as `file_index` in `GET /api/library/list`; 0 if it carries none. |
| `size` | number | File size in bytes. |
| `opened_at`, `closed_at` | string | UTC, RFC 3339, whole seconds. |
| `duration_ms` | number | Open to close, including the idle wait before the close. |
| `first_byte_ms` | number or null | Open to the first byte served; `null` if none was. |
| `bytes_served` | number | Bytes handed to the kernel; the sum of the four fields below. |
| `bytes_fetch_block` | number | Bytes of reads that waited for the engine. |
| `bytes_racache_hit` | number | Bytes of reads answered from the read-ahead cache in memory. |
| `bytes_warmup_head`, `bytes_warmup_tail` | number | Bytes of reads answered from the head or tail window on disk. |
| `swarm_served` | boolean | A read was answered through the engine while a peer was connected: the rule that sets `last_swarm_read_at`. |
| `reads`, `failed_reads` | number | Reads served, and reads that gave up. |
| `failure_reason` | string or null | Why the last failed read gave up (the values of `last_failure_reason`); `null` if no read failed. Set whenever a read failed; only `dead_swarm` says the session counted as a failure. |
| `reopens` | number | Times a read on an open handle woke a torrent the engine had closed as idle. The wake at open does not count. |
| `stalls`, `max_stall_ms` | number | Served reads that took over one second, and the slowest one in milliseconds. Failed reads are not counted. |
| `confirmed` | boolean | A play webhook confirmed the file while the session was open, or it was confirmed when the session started. Sticky: a later stop webhook does not clear it. |
| `id`, `resumed_from` | number | A number for the session, unique within one daemon run (it restarts at 1), and the `id` of the session this one continues; 0 for a session that began with an open. Follow `resumed_from` to stitch a paused playback back together. |
| `outcome` | string | `served`: 1 MiB or more, or over 64 KiB with a read past 8 MiB or in the tail window. `probe`: fewer bytes, a scan. `unserved`: no bytes. `dead_swarm`: the session counted as a failure in the readability record, whatever it served; it never appears when the state database is off. The thresholds may change. |
| `session_age_seconds` | number | Open sessions only, not in the log line: seconds since the session opened. |
| `last_read_offset`, `last_read_end` | number | Open sessions only, not in the log line: where the last served read started, and where it ended (start plus the bytes returned), in file bytes. Absent until a read has returned data. |
| `ahead_bytes` | number | Open sessions only, not in the log line: how many bytes from `last_read_end` on the player can read without waiting. It walks forward over the read-ahead cache and the verified pieces together and stops at the first byte neither holds, at the end of the file, or after 1 GiB. Absent when `last_read_end` is, when the stub carries no `file_index`, or when the engine does not hold the torrent. Worked out when you ask, never on the read path. |
| `frontier_offset` | number | Open sessions only, not in the log line: the end (exclusive, in file bytes) of the run of verified pieces that starts at `last_read_end`, the engine alone, without the cache. It equals `last_read_end` when the piece under it is not complete, and stops at the end of the file or 1 GiB past `last_read_end`. Present when `ahead_bytes` is. |
| `pump_offset` | number | Open sessions only, not in the log line: the file offset the pump of this file fetches next. Present while a pump runs for the file. Read it with `player_off` and `ahead_bytes` to see whether a shallow `ahead_bytes` is a pump that lags or one that is not following the player. |
| `player_off` | number | Open sessions only: the player position the pump last saved (written when a primary handle is released), so it can trail the live position. Present with `pump_offset`. |
| `primary` | bool | Open sessions only: whether a primary handle is open on the file, the handle the pump follows. Present with `pump_offset`. |
| `refs` | number | Open sessions only: how many handles share the file's pump. Present with `pump_offset`. |

- The four byte fields name the path that answered each read, not where
  the bytes came from. Bytes fetched from the swarm in a session reappear
  under the cache fields when later reads find them there, and the engine
  can answer a fetch from pieces it already holds. Only `swarm_served`
  says the swarm delivered.
- `served` means the reader got bytes, not that the swarm sent them.
- A session is per file, not per open or per playback. Opens that overlap
  share one. A playback that pauses for more than a minute (30 s once the
  session is older than 30 minutes) closes its session; the first read
  after that, on the same handle, opens a new one with `resumed_from` set,
  so a paused playback is one session per stretch of reads. Each session
  gets its own verdict: a resumed session that fails is judged on its own
  reads and, like any session, only counts as `dead_swarm` once it has
  lasted 25 s. A scan's handle that releases closes its session and leaves
  nothing to resume. A session that ended when another handle closed before
  the first byte shows as `unserved`, and its next read starts a resumed
  session.
- `reopens`, `stalls` and `max_stall_ms` are lower bounds: a play with long
  gaps spans several sessions, one per pause. The process-wide counters
  `flodni_torrent_reopens_total` and `flodni_read_stalls_total` have no
  such gap.
- The `confirmed` label of the three process-wide counters is per torrent: a
  scanner's stall on a sibling episode of a confirmed season pack counts
  `confirmed="true"`, while that session's own `confirmed` is false. The
  label comes from a set rebuilt once a second, so a webhook can show up in
  it a second late.
- Counting edges: a read that waits over a second for another reader's
  fetch counts its own stall; the wake's own duration is not part of the
  stall measure; reopens can exceed idle closes, because two reads in
  different cache chunks can both wake a closed torrent.
- A session still open when Flodni stops gets no line.
- `bytes_served` counts what Flodni handed to the kernel. The kernel's
  page cache answers repeated reads without Flodni seeing them.
- The line carries the library path. Remove it before sharing a log.

## Reader budget (counting always; refusal opt-in, off by default)

Counting is implemented and always on. Refusal is implemented, opt-in and off
by default (`refuse_unconfirmed_reads`). It is tested against a fake engine
and end to end: the `budget-refusal` variant of `make e2e` runs it against the
real mount and engine (below); the parked pump is covered by the fake engine
only. A file that no media server has confirmed as playing can still be read
end to end by a scanner, a hasher or a copy. Flodni counts what such a reader
takes past the head and tail windows and logs the session once. With the
switch off it never refuses or slows a read.

- **What counts.** A served read counts when the engine or the read-ahead
  cache answered it, the bytes lie outside the head window and the tail
  window, and the file has no playback confirmed by a webhook. Reads the
  warmup files answer never count. The unit is the session (overlapping
  opens of one file, ended after 60 s idle).
- **The counter** `flodni_unconfirmed_read_bytes_total` is the bytes the
  reader took past the windows, whichever path served them. It is not bytes
  downloaded: with the pump on (the default), the engine fill happened
  earlier through the pump or a prefetch. What the pump and the prefetches
  fill is counted separately, per session, and can be made part of the
  margin (see "Fill attribution" below).
- **`unconfirmed_read_margin_mb`** in `config.json` (default 64, 0 logs at the
  first byte past the windows; a negative or absurd value is replaced by 64).
  When a session's counted bytes reach the margin, one `[ReaderBudget]` line
  is written with `"event":"over_margin"`: `hash`, `file_index`, `size`,
  `swarm_bytes`, `cache_bytes`, `margin`, `open_age_ms`, `comm`, `pid`, `uid`
  and `enforced` (false unless `refuse_unconfirmed_reads` is on and a
  webhook has matched; see below). It carries no path or file name. The
  line prefix and field names are meant to be stable. `swarm_bytes` and
  `cache_bytes`
  split the counted bytes by the read path that answered, for debugging:
  `swarm_bytes` waited for the engine (which can answer from pieces it
  already holds), `cache_bytes` came from the read-ahead cache. Neither is
  bytes downloaded; with the pump on, almost everything shows as
  `cache_bytes`.
- **Late confirmation.** If a webhook then confirms that file, a second line,
  `"event":"confirmed_late"`, carries `hash`, `file_index`, `size`,
  `swarm_bytes`, `cache_bytes` and `late_ms`: a playback that a budget would
  have cut. `flodni_unconfirmed_read_sessions_total` counts each crossed
  session once, when it closes. The end-to-end test covers this path: the
  `late-confirm` variant of `make e2e` posts a play webhook in the middle of
  a read that has crossed the margin and checks the sha256 of the read,
  `flodni_playback_webhooks_received_total{matched="true"}`, the
  `confirmed_late` session count and the line.
- **`comm`, `pid`, `uid`** come from the FUSE request of the latest open of
  the file. `comm` is read from `/proc/<pid>/comm` when the line is written,
  and is empty when the pid is 0 or not visible (another PID namespace, or
  the process already exited). The opener is not always the process that
  reads.
- **`flodni_playback_webhooks_received_total{matched}`** counts play and
  resume webhooks for a movie or an episode; `matched="true"` when the
  webhook resolved to one file. The refusal gate below arms only once
  `matched="true"` has been seen since start.
- **`refuse_unconfirmed_reads`** in `config.json` (default `false`, read at
  each read). Implemented, opt-in. When on, and once a play webhook has
  matched a file since start, a session that took the margin past the windows
  since the file was last confirmed (or since the start) gets no further
  fetches from the swarm for a file that is not confirmed as playing:
  - **What is refused.** A cache miss for bytes outside the two windows. The
    read waits up to 15 s on its handle for a webhook (a late webhook lifts
    the hold and the read goes ahead); after that, and for every later read
    on that handle, it fails at once with `EIO`. The 15 s is a constant, not
    a key. A short read is never returned (the kernel would cache the zeros),
    and `EAGAIN` makes scanners loop.
  - **What is always served.** A request the read-ahead cache covers in
    full, the SSD head and tail windows, the tail-window fetch, reads wholly
    inside a window, the wake at open, and warm on add and catch-up. A
    request that is only partly cached, or that crosses a window edge, is
    refused whole. The kernel retries page by page, which serves the cached
    pages and fails at the first uncached one. Measured end to end: a reader
    that started 512 KiB before the 20 MiB cut, where the read-ahead cache
    held bytes up to 32 MiB, got exactly the cached 12.5 MiB (the cache hits
    grew by the same 13107200 bytes), half a 1 MiB request past the last
    whole one. When nothing was cached at that offset, the reader got none.
  - **The pump** (`pump_on_open`, or the one started for inferred playback)
    is parked while its next chunk would be refused: its engine reader stays
    open and unread, and it resumes on confirmation. It is never cancelled.
  - **A refusal is not a failed read.** It never reaches the failure count
    or the readability record, so it cannot produce a dead-swarm verdict.
    A short read that ends in a refused sub-read is not counted as a failed
    repair (it still counts as a short read). A caller that leaves while a
    sub-read is held gets `EINTR`, not a failed read; this is the one
    behaviour change with the switch off, for the older `EINTR` exits of a
    sub-read.
  - **After a confirmation or a stop** the tally starts afresh: a play that
    ends does not leave the file refused on the next read.
  - **Log and metrics.** One `"event":"refused"` line per session (`hash`,
    `file_index`, `size`, the counted bytes, `margin`, `offset` of the first
    refused read, `held_ms`, `comm`, `pid`, `uid`, `enforced`), no path.
    `over_margin` reports `"enforced":true` when the key is on and a webhook
    has matched since start. `confirmed_late` gains `refused_reads`.
  - **Overshoot.** The margin is "stops starting fetches", not a byte cap:
    read-ahead already in flight completes, and the parked pump's engine
    reader may fill its own read-ahead. With the pump on, "cached" includes
    what the pump had fetched ahead when the margin was crossed. A miss
    inside the head window fetches a full read-ahead that can run past the
    window edge, and the warm-landing prefetch fetches the whole chunk around
    a window read. Measured end to end (pump off, windows 4 MiB, margin
    16 MiB): a scan got 20 to 32 MiB before the refusal, the 20 MiB cut plus
    up to 12 MiB of read-ahead already in flight, and the engine pulled no
    more from the seeder afterwards (622 bytes in 16 s). With the pump on it
    is not measured. The cut is on counted bytes, not on the reader's offset:
    the kernel sends one scan's read-ahead as parallel requests, so a later
    request can be served and counted before an earlier one is gated. That
    earlier request is then refused, and a reader that reads in order stops
    a little short of the cut (once 128 KiB, 19.875 MiB here), by at most the
    read-ahead window. The end-to-end test checks the counted bytes against
    the margin and gives the reader's bytes that slack.
    The head-window part is counted and can be clamped, see
    the next two bullets.
  - **Read-ahead overshoot (counted always, clamp opt-in).** Implemented,
    tested against the fake engine. A miss inside the head window is never
    refused, and its read-ahead can run past the window edge into the middle.
    `flodni_reader_budget_readahead_overshoot_bytes_total` counts the bytes
    the engine was asked to fetch past the edge for such a read, on a file
    with no confirmed playback: what was asked for, not what was downloaded,
    and only for reads that started inside the head window (a miss in the
    middle is the budget's own business). It only counts. Whether the number
    is worth a clamp is for the owner to read off a few weeks of it.
    **`unconfirmed_readahead_clamp`** in `config.json` (default `false`, read
    at each read): when on, the read-ahead of a read that starts inside the
    head window of a file with no confirmed playback stops at the window edge,
    and never short of the bytes the read itself asked for. The next read,
    at the edge, is an ordinary miss for the budget to judge. The cost is
    a smaller read-ahead there: a play that is not yet confirmed fetches the
    bytes past the edge one request later. It changes no refusal and works
    with `refuse_unconfirmed_reads` on or off. With it on the counter stays
    at 0. A file with a confirmed playback is never clamped or counted. Not
    measured on a real swarm, and not part of `make e2e`.
  - **Fill attribution (counted always, margin opt-in).** Implemented,
    tested against the fake engine. The pump and the prefetches (the two
    warm-landing fetches and the two predictive ones) fill the read-ahead
    cache ahead of the reader, and until now only the bytes the reader
    later read counted toward its margin. Each such fill past the two windows,
    for a file that has a session and no confirmed playback, is now added to
    the session's fill tally and to
    `flodni_reader_budget_attributed_fill_bytes_total`; the `[ReaderBudget]`
    lines carry it as `fill_bytes` when it is not 0. That counting changes
    nothing and is always on. **`attribute_fills_to_reader`** in `config.json`
    (default `false`, read at each call) makes the margin compare the larger
    of the served tally and the fill tally, not their sum, since a filled
    byte the reader then reads is one byte of cost seen from both ends. It is
    off by default because it moves when the margin is crossed: sooner for a
    reader with a pump or a prefetch running, so refusal starts sooner, and
    `over_margin` is logged sooner. The fill is attributed to the session of
    the file, not to one handle: two readers of one file share it. The
    read-ahead of a miss (the overshoot above) is not part of it.
  - **Limits, check before turning it on.** A webhook later than 15 s, lost,
    or matched to the wrong file breaks that play shortly after the margin
    (a resume on a recently played file with a lost webhook is refused too:
    inferred playback is not exempt). Look at `late_ms` on `confirmed_late`
    lines and at `flodni_playback_webhooks_received_total{matched="false"}`
    first. A media server's full-file analysis (thumbnails, intro detection)
    of an unplayed file will fail past the margin. Reads on a handle after
    its session closed are not budgeted. Held reads occupy FUSE background
    slots for up to 15 s: with `fuse_max_inflight_mb` set, each held read
    keeps about 4 MB checked out (at the 16 MB floor, four held reads stop
    the whole mount for up to 15 s). A hold also keeps the kernel's pages of
    that file locked, so an open of a file with no other live handle waits for
    it (see "Page cache on a second open"). A hold adds up
    to 15 s to one read on top of the slot wait and fetch bounds, and can
    outlast the 4 s refill budget.
  - **More limits.** The tally is per file across opens: a player that opens
    a file a scanner just pushed past the margin has no margin left, only the
    15 s hold, and every new open of a refused file waits 15 s again. A stop
    webhook un-confirms the file for every reader, so a second client, or a
    stop that arrives after the next play, is cut after margin plus hold.
    Windows are exempt by byte, not by page: the tail window starts on a 4 KiB
    boundary, so the page that straddles the file's size minus the tail size is
    inside it, but kernel read-ahead across the head edge can stall a head read
    15 s once per open. A parked pump resumes when its session closes (no session, no
    refusal); for inferred playback it can then fetch up to the read-ahead
    budget once.
- **End-to-end test.** The `budget-refusal` variant (margin 16 MiB, windows
  4 MiB, `pump_on_open` off, auth off) reads 40 MiB from the middle of the
  fixture with no matched webhook (not armed: it passes), arms the gate with
  a play and a stop, and then: a `dd` of the file fails with `EIO` after at
  least the windows plus the margin and far below the file size, with one
  `refused` line (`"enforced":true`, `held_ms` near 15000, no file name) and
  no failed read in `/api/sessions`, in
  `flodni_fuse_short_read_results_total{result="failed"}` or the readability
  record; the seeder is asked for nothing more; page-aligned reads inside
  both windows still succeed; while a reader is held, `stat`, the reads of a
  descriptor opened before the hold, and fresh opens reading a head and a
  tail window (buffered and `O_DIRECT`) each answer in under 1 s (since the
  page-cache fix above); a reader killed mid-hold is not a failed read;
  a play that arrives 5 s into a held full read lifts the hold (the sha256
  matches, `holds_total{result="lifted"}` and
  `confirmed_after_refusal_total` move); after a play and a stop a new
  reader gets the margin again before it is refused; and when the session
  closes there is no `[DeadSwarm]` line. It reads the whole file only once,
  held, because re-fetching pieces the engine's 128 MiB cache already
  evicted stalls the engine for tens of seconds (the stall the other
  variants report) and would show as a timeout, not a refusal. Not covered
  end to end: the pump park with `pump_on_open` on.
- **Refusal series (all start at 0 and move only with the switch on):**
  `flodni_unconfirmed_read_refused_reads_total`,
  `flodni_unconfirmed_read_holds_total{result="lifted|refused"}` and
  `flodni_unconfirmed_read_confirmed_after_refusal_total`.

Limits: sessions open at shutdown are not counted; reads the kernel page
cache answers never reach Flodni; a fuzzy webhook match or a state
restored at start can mark a file confirmed that is being scanned. A
confirmation also outlives the playback: it stays until a stop webhook, or
until 15 minutes after the last handle of the file closes, and a scan in
that window is not counted.

## Warmup cache eviction

With the warmup cache on, a new head file that would pass the quota evicts
whole media files, head and tail together. Files never played go first, then
played files; each class goes oldest written first. An eviction does not stop at
the quota: it goes down to 95% of it (a fixed mark, not a setting), so the next
head writes find room instead of each starting another directory walk. The
headroom comes from files never played; a played file is taken only as far as
the quota needs. This is built and runs
today. The cache also keeps an in-memory list of which files are warm; it is
internal and not served, and only the aggregates below are exported
(`flodni_warmup_files`). Evicting the least recently read file is built but
off by default (`warmup_eviction_order`, below). Warm on add and its catch-up,
which fill windows on their own, are described in their own section below; both
are off by default.

The quota counts allocated blocks, not apparent sizes: a sparse file (a tail
written in pieces, a head with holes) counts what it holds on disk, as the
start-up walk, the eviction walk, writes and removals all do. Where a platform
reports no block count for a non-empty file, its apparent size stands in. The
cache keeps a ledger of what each file has contributed to the total, so a
removal gives back exactly that, and every change to the total happens under
one lock. Where a filesystem reports blocks late (ZFS within a transaction
group), a write counts at least the bytes it wrote: the total never reads low
because of the lag, and the next eviction walk corrects the rest. The window
lengths and the coverage the cache serves still follow the logical length of
the files. An eviction walk holds that lock while it reads the directory, and
a head write waits for it; the daemon logs a walk that takes more than 50 ms.

- **Eviction order (built, off by default).** `warmup_eviction_order` is
  `"written"` (the default, the order above) or `"read"`: inside each class
  (never played first, then played) the file read least recently goes first.
  A file's read time is the last cache hit on its head or tail, kept in memory
  only (not persisted, not served); a file not read since the daemon started
  ranks by its write time, and a file written after its last read ranks by that
  write. The tracking costs one atomic load on the read path and nothing is
  stamped under the default order. The played protection, the protected files of
  a fill in flight and the lock order of the quota walk are unchanged. An
  unknown value is logged at load and replaced by `"written"`; it is read at
  start. **The measurement that should decide switching it on:**
  `flodni_warmup_cold_reopen_total{previously_played="true"}` over
  `flodni_playback_starts_total` reaching 5% of plays over a few weeks at a
  full quota. The `previously_played="false"` series is a different problem
  (warm data evicted before its first play points at the quota or the order, not
  at recency) and does not argue for this switch. Not measured yet: it has not
  run against real plays.

- **Played** means a playback confirmed by Plex's play webhook, never a
  scanner's read. A webhook marks at most one file: a movie by file name,
  IMDB id or a unique title (and year); an episode by file name, or by a
  unique IMDB id that agrees with the file's season and episode, or by series
  title, season and episode together (`Metadata.parentIndex` and
  `Metadata.index`). When several candidates fit equally, or an episode
  webhook carries no season and episode and no file name, no file is marked:
  a missed play costs one file its protection, a wrong mark would protect a
  season. Marking does not change which file the webhook starts playback
  for.
- **Protection expires.** A play protects its file for `played_protection_days`
  (default 90) counted from the last play; after that the file ranks as never
  played. `0` means a play protects forever. A negative value is logged at
  load and replaced by 90.
- **Limits.** Without a webhook nothing is ever played, so the order is
  oldest first. With the state database off (`enable_state_db`) the marks
  live in memory and are gone at restart. A play whose webhook arrives
  before the file is opened is not marked. When only played files remain,
  any read through the mount can evict the oldest played file, because a
  play's first reads cannot be told from a scan before the webhook arrives.
- **State.** The marks are in the state database table `file_state` (schema
  13): the last play and whether the quota evicted the file. Schema 14 adds
  the experimental per-file record (see "Per-file records") to the same
  rows; schema 15 adds `daemon_marks`, three daemon times (see "Play
  webhook"); schema 16 adds `queued_at` (see "Scope" under catch-up).
  Eviction removes bytes only; the readability record stays. A torrent
  removed from the library loses its marks, and a startup prune after a
  complete library scan drops the marks of torrents that left it.
- **Log line.** Each evicted file logs one line, `[WarmupEvict] ` followed by
  a JSON object: `hash` (full), `file` (index), `head_bytes`, `tail_bytes`
  (0 when that window was not on disk), `age_s` (since the newer window was
  written), `played` (inside its protection) and `trigger` (`head_create`,
  `warm_add` for an eviction made to fit a warm-on-add fill, or `request` for
  one made by `POST /api/warmup/evict`). It carries no path. The prefix and
  field names are meant to be stable.
- **Free space on request.** `POST /api/warmup/evict` with the body
  `{"target_gb": 10}` (or `{"target_bytes": 10737418240}`; give exactly one,
  not negative; `0` empties what may be evicted) evicts down to that size now,
  in the same order as the quota: files never played first, then played files,
  each oldest first. It answers `{"evicted_files": N, "evicted_bytes": N,
  "total_bytes_after": N, "target_bytes": N}`; a target at or above the
  current total evicts nothing. `400` is a bad target, `405` is any method
  but POST, `503` is the cache off. **It is destructive:** it deletes warmup
  cache files (a file that was played goes too once the unplayed ones are
  not enough), which then come back only through the swarm or a refill. It
  needs the `api_token` bearer header when a token is set (no `?token=`),
  deletes only inside the cache directory, and touches no torrent, stub or
  record except the eviction marks a quota eviction also writes. Each file
  logs the `[WarmupEvict]` line with `trigger` `request`, and the eviction
  counters move as for a quota eviction. The configured quota is not
  changed: the next new head still evicts down to it.
- **Cold reopen.** `flodni_warmup_cold_reopen_total` counts a confirmed play
  on a session that opened with no ready head, of a file with an eviction
  mark; it counts once per eviction and clears the mark. `previously_played`
  says whether the file had been played before, expired or not.
  `flodni_playback_starts_total` counts the same confirmed starts (not
  playing to playing), the denominator for the rate. A start whose webhook
  resolves to no single file is still counted, but its cold reopen is not,
  so the rate is a lower bound. Neither counts a play whose webhook matches
  no open file.

### Cache disk

With `warmup_dir` on its own disk, make the disk part of the start. This is
the deployment side; Flodni cannot tell an unmounted disk from an empty
directory.

- Make the start depend on the mount: `RequiresMountsFor=` in the systemd
  unit, or a Compose bind mount without `create_host_path` (so Docker fails
  instead of creating the path on the host).
- Keep the empty mountpoint unwritable (`chattr +i` on it while nothing is
  mounted), so a create under it fails.
- Use a journalling filesystem (ext4 or XFS). Heads are not synced, and FAT,
  exFAT and NTFS can expose bytes that were extended but never written after
  an unclean unplug.
- Restart Flodni after the disk returns. A directory that vanished at runtime
  is not re-created, and the totals are not rebuilt without a restart.
- Never delete `.dense-migrated` by hand. A missing marker next to a populated
  cache now checks every head for holes at the next start and drops only
  those (one WARNING line with the counts, and each dropped head named), so a
  deleted marker costs a scan, not the cache; a cache that has the marker is
  not checked. Copy a cache without sparse options (no `rsync -S`, no
  `cp --sparse=always`), and keep the marker with the copy: a head with a
  stored run of zeros reads as a hole. ZFS with compression does the same
  (it stores all-zero records as holes), so with a missing marker it can drop
  heads that hold long zero runs.

What Flodni does in each case:

- **Absent at start.** It creates the directory and runs on whatever
  filesystem holds the path, with a WARNING `warmup_dir is not a separate
  mount` when the directory already existed on its parent's filesystem. That
  is only a hint, since a plain directory looks the same, and it does not
  refuse to start. If the directory cannot be created, it logs a WARNING and
  runs with no cache.
- **Vanished at runtime.** Nothing re-creates it. Head and tail writes and
  fills fail and log, reads miss and go to the engine, and the quota walk
  keeps its old total instead of storing 0. A hung filesystem can still block
  reads; there is no timeout.
- **Read-only.** Heads already there are served (one WARNING, once); nothing
  new is written, and a file the quota cannot unlink is neither evicted nor
  marked evicted.
- **Full.** Failed creates and writes are logged, nothing false is recorded,
  and a fill reports that the file was evicted.

## Filing and naming

`POST /api/library/add` names what it writes from the request, not from the
release. These rules apply to new adds only: a stub already on disk is never
renamed and its registry row is never re-keyed, and stubs are found by the
`link=` hash in their content, not by name.

- **Specials.** A pack file named `S00E01` is filed as season 0, under
  `Season.00` (the folder Plex reads as Specials), never as the requested
  season's episode 1. A single-episode add does not take a special's file for
  the requested episode either.
- **Samples.** A video file is skipped when "sample" is a word in its name or
  in the folder directly above it (not the release's root folder) and it is
  under 50 MiB, even when no larger file names the same episode. Each skip is
  logged (`ignored sample`). An explicit `file_index` still files the file it
  names. A single-episode add whose torrent does not name the episode takes
  the largest file that names no episode at all, never one named for another
  episode or a special.
- **Movie year.** The year in a movie's stub name is the request's
  `release_date` year. A number in the title (`Blade Runner 2049`,
  `Dune (2021)`) is part of the title and is not read as a second year. A title
  with no ASCII letter or digit is named with the first eight hash characters
  instead of an empty base with a leading underscore.
- **Episode key.** The registry key folds Latin letters with diacritics to
  their base letters (`Élite` gives `elite_s01e01`; before, the accented letter
  vanished and `Lite` shared the key). A show whose rows were registered under
  the old form keeps them: an add finds the row by the old key when the new one
  has none, and replaces it in place.

## Engine settings: what applies live

`POST /settings` (`action: set`, or `def` for the defaults; the panel's Apply uses
`set`) stores the settings, then applies what changed. How it applies depends on how
the engine reads the field:

| Applied | Fields |
| --- | --- |
| **In place**: stored, no torrent is dropped, the torrent client stays | `CacheSize`, `ReaderReadAHead`, `PreloadCache`, `UseDisk`, `TorrentsSavePath`, `RemoveCacheOnDrop`, `RetrackersMode`, `TorrentDisconnectTimeout`, `ResponsiveMode`, `AdaptiveShield`, and the fields the engine does not read after start (`BlockListURL`, `SslPort`, `SslCert`, `SslKey`, `ShowFSActiveTorr`, `StoreSettingsInJson`) |
| **Restarts the engine**: drops every active torrent, then builds a new client | `ForceEncrypt`, `EnableDebug`, `EnableIPv6`, `DisableTCP`, `DisableUTP`, `DisableUPNP`, `DisableDHT`, `DisablePEX`, `DisableUpload`, `DownloadRateLimit`, `UploadRateLimit`, `ConnectionsLimit`, `AggressivePeerManagement`, `PeersListenPort` |

A request that changes only in-place fields never touches the client. One that changes
any restart field rebuilds, as every settings save used to.

**Torrents that are already loaded.** A `CacheSize` change does **not** resize the cache
of a torrent that is already loaded. It applies to torrents loaded after the change: a
torrent keeps the cache it was opened with until it is closed (idle expiry or a drop)
and loaded again. Its read-ahead is derived from that size, so it cannot outgrow it. A
`ReaderReadAHead` change applies to loaded torrents too, on their next read-ahead
update (about a second): it is a percentage of the cache they have, and no cache is
resized. `RetrackersMode` applies to torrents added afterwards. `ResponsiveMode` is latched
when a stream opens, so it applies to readers opened after the change.
A preload is a share of the cache the torrent has. `UseDisk` and
`TorrentsSavePath` do not move the warmup directory, which is fixed at start.

**Download speed.** The engine's torrent status reports `download_speed` as
the rate of useful data (what the torrent kept) and `download_speed_raw` as the
wire rate, which also counts protocol overhead, duplicates and waste. Both
are sampled once a second.

**Refusal under playback.** A change that would restart the engine while the daemon
counts a confirmed playing session (a media-server webhook confirmed it, no stop, and
a handle on the file is open) answers `409` and stores nothing:

```json
{"error": "settings change needs an engine restart (DisableDHT, PeersListenPort) and 1 playing session(s) would be interrupted; resend with force to restart anyway",
 "fields": ["DisableDHT", "PeersListenPort"], "sessions": 1}
```

Add `"force": true` to the request to restart anyway. A change to in-place fields is
never refused. The panel asks for confirmation before it applies a change to a restart
field, and sends `force` only after you confirm. A plain Apply sends none.

## Engine add semantics

An add through the engine (`POST /torrents`, `/stream`, `/torrent/upload`, and
the library's add and inspect, which call `POST /torrents`) starts from the
link or file it was given, and for a hash the engine already stores it
restores the stored metainfo and peers. Trackers follow hard rule 3:

- **Stored, not known public** (a private torrent, or a record without
  metainfo): the request's trackers are ignored and the record's own are used.
  The log line carries a count only, since tracker URLs carry passkeys.
- **Stored and known public:** the request's trackers are merged with the
  record's, as before.
- **Already held in memory and not known public:** the add gives the live
  torrent no tracker.
- **Re-adding with new trackers** is remove, then add.

## Stub contents and the stub scrub

A stub is a small JSON file: `url` (the engine's stream link, which carries the
info hash and the file index), `size`, `imdb` and `magnet`, written with sorted
keys. The `magnet` is always the bare `magnet:?xt=urn:btih:<40-hex info hash>`:
no `tr=`, `dn=` or `xs=`. A request's magnet, or the announce list of an
uploaded `.torrent`, goes to the engine and no further, because a private
tracker's announce URL carries the account's passkey. Nothing reads the stub's
magnet back (a wake takes the hash from the stream link and the engine's stored
record). The one indirect reader is the id lookup described under the scrub:
a stub with no `imdb` field gets its id from a regex over the whole file, so an
id that only an old `dn=` held is not carried by a new stub.
`library.WriteStub` takes the hash, not a magnet,
so no add path (magnet, `.torrent`, pack, replace) can write a tracker. Stubs
are written with mode 0644, as before.

Stubs written by older versions may still hold the full magnet. `flodni
scrub-stubs [--dry-run] [--allow-root] <source_path>` rewrites those once, on
request; the daemon never does it on its own. `source_path` is required and is
the directory the daemon is given as its source (it holds `movies/` and `tv/`),
not the mount path. It is not read from `config.json`, because the daemon's own
command-line source wins over the config and the two can differ. Flags go
before the path: a flag after it is refused, never ignored.

- **What it changes.** Only the `magnet` field, rebuilt from the hash the old
  one carried. Every other field and its bytes stay as written, because the
  rewrite goes through the stub encoder and a stub the encoder would not
  reproduce byte for byte is left alone and reported. A line-based legacy stub
  has only its magnet line replaced.
- **What it leaves.** A stub is clean only when its magnet is exactly
  `magnet:?xt=urn:btih:<40 lowercase hex>`, or empty; it is not written. A
  stub with no `imdb` field whose old magnet holds an IMDb-looking id
  (`tt` and 7 to 10 digits) is left alone and counted skipped, because the
  reader finds that stub's id by a regex over the whole file, magnet included,
  and a rewrite would lose it. A line-based stub whose third line is not a
  magnet is reported. Files that are not stubs (not JSON or line format with an
  `http(s)` link and a size, or larger than 64 KiB), symlinks and non-`.mkv`
  files are never written or followed.
- **mtime guarantee.** Each rewrite is one new file in the stub's directory
  that already has the stub's mode, owner, atime and mtime (set on the file
  before it replaces the stub), renamed over the stub after a last check that
  the stub is still the file that was read. The stub never exists with another
  mtime, which matters because the mount reports it to the media server and a
  change makes it rescan. Nothing touches the final path after the rename. The
  owner is set by the scrub, not by `atomicfile`: when it cannot be set (a
  non-root run over files of another user) that stub is reported, not rewritten.
- **Dry run.** `--dry-run` writes nothing; `rewritten` counts the stubs a real
  run would rewrite.
- **Output and exit codes.** One line, `scrub-stubs: scanned=N rewritten=N
  skipped=N errors=N`, and one `scrub-stubs: <path>: <reason>` line per error
  on stderr, with a fixed reason. It never prints a magnet or a URL. Exit 0:
  stubs were scanned and nothing failed. Exit 1: at least one stub was
  reported. Exit 2: a bad invocation (a flag after the path, no or several
  paths), neither `movies/` nor `tv/` under the path, a run as root without
  `--allow-root`, or no stub scanned at all (which also catches the mount path
  given by mistake).
- **Run it as the daemon's user.** As root it refuses unless `--allow-root` is
  given: a root run over a tree another user can write is the case to avoid. For
  example `docker exec --user <daemon uid> <container> flodni scrub-stubs
  /path/to/source`.
- **With the daemon running.** Safe, with one small window. A stub the daemon
  removed, replaced or rewrote since the scrub read it (a different inode, size
  or mtime) is skipped and never recreated or overwritten; that check runs
  immediately before the rename. The check and the rename are two steps, so a
  removal or a replace in between is still lost to the scrub. The only way to
  close it is to stop the daemon first, which is the recommended way for a
  library that is being changed.

The engine's own record of a torrent (its saved `config.db` entry) holds the
magnet or `.torrent` it was added with, trackers included: a closed torrent
wakes from it. After the scrub it is the only copy of a private torrent's
trackers, so back it up. No library route returns it.

## Removing a title

`POST /api/library/remove` takes a `path` or a `hash`. A hash removes every
stub whose own link carries that full 40-hex hash, whatever its file name: the
eight digits older tools put in the name (first eight for episodes, last eight
for movies) are not used, so a stub of another torrent that shares them is
left alone. The call reads each stub under both folders. A stub with no
readable hash is skipped and logged once per call. The torrent is dropped once
no stub links to it. A stub that cannot be read at all (an I/O error, or JSON that
does not parse) is not "no hash": it may be a sibling episode of a season pack,
so the torrent is kept and one line names the first such path. A stub that reads
fine and links to no hash is still skipped, as before.

## Replacing a release

**Implemented.** When an add of a TV release would replace episode stubs that
another release filed, the old stubs stay until the new release has shown it
delivers. The check reads the first 64 KiB of a bounded sample of the new
release's files: the file that replaces an existing stub, plus the first and
the last episode by season and episode number, three files at most. A sample
clip, an extra or a season-0 special is never chosen as first or last, and a
special is not read as the replaced file unless the release holds nothing else:
only the files the add would file are candidates. The reads run at the same time under one
60 s timeout, so a dead release costs one wait, not three. The replacement
goes ahead only if every sampled file reads. Otherwise the call answers 422,
the old stubs and registry rows stay, the new torrent is dropped, and the log
names the release. A release with one file is checked on that file; an add that
replaces nothing is not checked. A private torrent is read through its own
peer sources only.

The trade-off: the check samples, it does not prove. A pack whose middle
episodes are dead but whose sampled files read still replaces the old stubs,
and those episodes then fail on first play. Reading every file would delay
every replacement by the slowest episode of the pack.

## Listing stubs

`GET /api/library/list` (control port, `type=movies` or `type=tv`) reports each
stub with `path`, `fuse_path`, `size`, `hash`, `imdb`, `file_index`, `season` and
`episode`, and `linked`:

| `linked` | Meaning |
|---|---|
| `true` | A library symlink points at this stub's base name. |
| `false` | None does: nothing the media server sees uses this stub. |
| `null` | There is no linked set to compare against: no walk has finished, no `plex_path_map` target is configured, or the set is older than three walk intervals. |

The set is the one the play webhook uses to break a tie between candidate
stubs. It is kept in memory and refreshed every five minutes; `list` reads no
extra file. Report only: nothing is removed or changed because a stub is
unlinked, and a stub linked by a link outside the `plex_path_map` targets reads
as `false`.

## Auditing stubs against the engine's file list

`GET /api/library/audit` (control port, behind `api_token` like the other
routes) checks every stub's `index` against the file list saved with its
torrent's record, using the engine's own ids. It reads the stubs the list
route reads and the records without waking any torrent; it changes no stub,
record or state, and it runs only when asked, never at start.

```json
{"checked": 120, "mismatched_total": 1, "truncated": false,
 "mismatched": [{"path": "...", "hash": "...", "index": 3, "stub_size": 200,
                 "engine_length": 100, "reason": "length_differs"}],
 "no_file_list": {"stubs": 2, "torrents": 1, "hashes": ["..."]},
 "unreadable": 0,
 "orphan_records": [{"hash": "...", "name": "..."}]}
```

| Field | Meaning |
|---|---|
| `checked` | Stubs compared against a stored file list. |
| `reason` `no_such_file` | The index names no file in the list (`engine_length` is `null`). |
| `reason` `length_differs` | The index names a file of another length than the stub's `size`. |
| `no_file_list` | Stubs whose record holds no file list (saved before the metainfo arrived). They cannot be checked until one wake stores the list; the audit does not wake them. |
| `unreadable` | Stubs with no readable hash. |
| `orphan_records` | Torrent records the engine stores that no stub references, as `hash` (lower-case hex) and the stored `name`, sorted by hash and not capped. Nothing else of a record is shown, and a name that looks like a URL or magnet link is left empty. Report only: nothing is removed. An unreadable stub may reference one of them, so check `unreadable` first. `[]` when there are none. |

`mismatched` and `no_file_list.hashes` hold at most 200 entries each;
`mismatched_total` and `truncated` give the full count. Repair a reported
stub with `POST /api/library/remove` for its hash, then `POST /api/library/add`
again, which writes the stub from the engine's current list.

## File lifecycle API

Four additions to the library routes on the control port, decided in
[ADR 3](docs/architecture/decisions/0003-file-lifecycle-api.md). They ride on
the existing `api_token` bearer gate; `remove`, `list`, `inspect` and `audit`
are unchanged.

**A request with none of the new fields behaves as it did.** An add without
`replace` or `dry_run` returns the same body as v0.1.0: `hash`, `title`,
`type`, `files`, `already_present`, nothing else (a golden test pins it).

### Replace policy

`POST /api/library/add` takes `replace`:

| `replace` | When the registry already holds a different release of the episode |
|---|---|
| omitted, `always` | Replace it, after the readability read below. This is what an add did before. |
| `never` | Keep it; answer `kept`. |
| `if-better` | Replace only when the request's `quality_score` is greater than the stored one. A score of 0 is never better, so a caller that sends none cannot replace by accident. |

Flodni compares that one number and nothing else; which release is better
stays with the caller. A movie never replaces: a second release is a second
stub, whatever `replace` says. An unknown value is a 400.

The readability gate is not a policy: a replacing release that delivers no
data is a 422 under `always` and `if-better`, and the old files stay.

A single episode is decided under its episode lock before the engine is
contacted: a refused add makes no upload, no add, no metadata wait and no read,
so it never announces a private torrent. A season pack is decided per episode
once the torrent's file list is known, so a refused pack has paid the metadata
wait; it writes the allowed episodes and lists the refused ones.

A request that names `replace` or `dry_run` also gets:

| Field | Meaning |
|---|---|
| `decision` | `added`, `already_present`, `replaced` or `kept`. |
| `replaced` | The stubs this add replaced (or, in a dry run, would): `path`, `hash`, `file_index`. Omitted when empty. |
| `skipped` | Episodes the policy refused: `season`, `episode`, `reason`. Omitted when empty. |

**`files` lists only what this request wrote** (for `already_present`, what was
already there, as before): callers link every entry that has a `fuse_path`. A
refused episode is in `skipped`, never in `files`; a `kept` answer is 200 with
`"files": []`; a pack with every episode refused is the same. An empty `files`
means nothing was filed by this request. Status is 201 only when something was
written; `already_present`, `kept` and a dry run are 200.

### Dry run

`dry_run: true` answers the decision from the stubs on disk and the registry
and does nothing else: no engine call at all (no `.torrent` upload, no add, no
metadata wait, no read), no stub or registry row written, no warm on add, no
media-server scan. The answer carries `"dry_run": true` and
`"readability": "unchecked"`, because it never asks the swarm, so a plan can
still fail the real add's readability gate. No `files` entry has a `fuse_path`;
each names the path the stub would get, with the request's `file_index` and a
size of 0. It works for a single episode or a movie; a season pack is a 400,
because its episodes are known only from the torrent's file list.

### State of a file

`GET /api/library/file?hash=&file_index=` reads one file. It is a projection of
records that exist (the stub, the registry row, the daemon's file state); there
is no new store.

```json
{"hash": "...", "file_index": 3, "state": "played",
 "stub": {"path": "...", "fuse_path": "...", "size": 1234, "type": "tv",
          "season": 1, "episode": 2},
 "registry": {"episode_key": "...", "quality_score": 42, "source": "api"},
 "warm": {"readability": "warmed", "cache": "cached", "queued": false,
          "failed_attempts": 0},
 "played_at": "2026-11-14T08:00:00Z", "protected": true,
 "evicted": false, "confirmed": false}
```

The facts are independent (a file is warm and played at once); `state` is one
word for the first that applies: `confirmed` (a session is open on the file and
a webhook confirmed it), `evicted` (the windows were evicted), `played` (a play
inside the protection window), `warm` (readability `warmed` or `present` and
cache `cached`), `warming` (queued, no outcome yet), `added`. `registry` is
`null` for a movie. A file no stub links to is a 404: replaced and removed
files are not kept, their rows go with their torrent. The state reads three
stores written at different times, so a file can be `added` a moment before it
is `warm`.

### Confirming a play

`POST /api/library/confirm` with `{"hash": "...", "file_index": 3}` records a
play of the file by hand: it clears the file's eviction mark and writes the same
played mark a play webhook writes, so the file is protected from the first
rounds of quota eviction for `played_protection_days`. It answers with the
file's state. A repeat is a 200 and renews the played time. An unknown file is a 404.

It records the play and nothing else: it confirms no session and counts no
playback start, so the reader budget's hold on an unconfirmed session is not
lifted (only a webhook does that). `flodni_played_marks_total{source}` counts it
as `manual`, the webhook's marks as `webhook`.

**It fails closed.** With `api_token` unset the control port is open to anyone
who can reach it, and a play mark changes what eviction protects, so the route
answers 403 and records nothing. With a token, anyone holding it can protect
any file. (`remove` and the other routes keep working without a token.)

The gate is the token the control server was started with, the same value the
bearer middleware enforces, including the `FLODNI_API_TOKEN` override: never the
live config. With no token at start, `POST /api/config` refuses to set or
change `api_token` (403), so confirm stays 403 until the service is started
with a token. Other settings still save on an open port, so set `api_token`
before exposing it.

## Warm on add

**Experimental, off by default.** With `warm_on_add` set (and `warmup_dir`
set, see below), Flodni completes the head and tail windows of each file the
library API files (`POST /api/library/add`), in the background, so the first
play starts from the SSD instead of waiting for the swarm. It is implemented
and tested against fakes and, for one title, end to end: `make e2e` adds a
fixture with the switch on, waits for the fill, stops the seeder and reads both
windows back from the SSD. Pause, the wake of a closed torrent and removal
while queued are covered by unit tests only (the `catch-up` variant does wake a
closed torrent and reports the engine's state for it before the pass).

- **Cost.** It downloads the windows (by default 64 MiB of head and 16 MiB of
  tail per file, `warmup_head_size_mb` and `warmup_tail_size_mb`; the tail
  window is page-aligned: its start is rounded down to a 4 KiB boundary, so it
  is up to 4095 bytes longer, and a read of exactly the last N MiB of a file
  stays inside it) from the
  swarm for every file of every title added, played or not. A private tracker
  may count that download against the account; that is why it is off by
  default. A pack added episode by episode warms each episode. Catch-up (below)
  does the same for the titles already in the library, until the cache is full
  (about 190 files at the default 15 GB at a time; room freed by evictions is
  filled again), so it can cost a private tracker one download per not-warm
  title.
- **Switch.** `warm_on_add` is read at each add, so a change saved in the
  control panel applies to the next add. Switching it off drops the queue and
  stops a running fill at its next check. It works only with `warmup_dir` set
  (index mode, where nothing reaches the SSD before its pieces pass their
  checksum). With `warm_on_add` set and `warmup_dir` empty, Flodni logs one
  warning and warms nothing.
- **What it does.** An add that wrote stubs hands the new files to an
  in-memory queue (4096 files; over that a file is dropped and counted as
  `dropped_queue_full`). Two workers take one title at a time and fill each
  file's windows in 4 MiB chunks, writing only verified bytes. A restart
  forgets the queue; catch-up (below) feeds the files that were not warmed yet
  in a later pass.
- **Pause.** While a read-ahead pump runs, or a playback the media server
  confirmed (or that the read pattern shows) still has an open file, the
  warmer stops. A running fill stops at its next check, up to one fetch (8 s)
  later; the file goes back to the front of the queue and nothing is
  recorded. It resumes after playback was clear on two checks 5 s apart.
- **Removal.** Removing the torrent cancels its queued files and stops a
  running fill; a later add of the same hash starts again. A fill that
  outlived the removal has its cache files removed once more.
- **Room.** It checks for room before it fetches. It may evict only files
  never played, oldest first, and only when the state database is on and at
  least one play has been recorded. Without that evidence it evicts nothing:
  a file with no room is dropped (`cache: dropped_quota` or
  `dropped_no_evidence`) and counted as `no_room`.
- **Closed torrents.** A torrent the engine closed is woken from its hash
  alone, and only if the engine still stores it: the stored record brings
  back the metainfo, the private flag and the trackers it was added with, so
  a private torrent gets no DHT, PEX or public tracker from the wake. A hash
  the engine does not store, or stores without its metainfo, is refused: the
  job is skipped, counted as `wake_error`, and nothing is recorded. The check
  and the load of the record are one step against a removal, so a torrent
  removed meanwhile does not come back (see "A stub the engine does not
  store").
- **Outcomes.** One per file attempt, the `outcome` label of
  `flodni_warm_fills_total{trigger="add"}` and `{trigger="catchup"}`:

| Outcome | `readability` | `cache` | Per-torrent record |
|---|---|---|---|
| `warmed`: windows complete and a swarm delivery time is known | `warmed` | `cached` | `last_swarm_read_at` set to the delivery time, failure count reset |
| `present`: complete, no delivery time | `present` | `cached` | untouched |
| `already_warm`: the windows were complete | unchanged (`warmed` if this file's own earlier attempt delivered) | `cached` | stamped only in that case |
| `no_room` | unchanged | `dropped_quota` or `dropped_no_evidence`; catch-up records nothing | untouched |
| `failed`: a chunk did not verify in time | `failed`, `failed_attempts` +1 | unchanged | untouched |
| `evicted`: quota eviction took the windows during the fill | unchanged | `evicted`, not retried | untouched |
| `lost`: the windows vanished with no eviction mark | unchanged (`warmed` if a delivery was seen) | `lost` | stamped if `warmed` |
| `write_failed`: another error, such as a failed write | as `lost` | `write_failed` | as `lost` |
| `index_mismatch`: the engine's file at the stub's index has another length than the stub | unchanged | unchanged, not retried; only the attempt time is recorded (catch-up looks again after 20 hours) | untouched |
| `wake_error`, `dropped_queue_full` | nothing recorded | | |
| `skipped`: a file left behind after a `wake_error`, after a `failed` with no peers or no running torrent, or (catch-up) after a `no_room` of the same title | nothing recorded | | |

- **Delivery time.** The time kept is the latest swarm delivery seen across
  the attempts of a file, so a fill paused at 20:00 and finished at 23:00 from
  RAM carries 20:00. If a media server scan filled the windows from the swarm
  first, that scan recorded its own delivery, and an `already_warm` fill adds
  nothing.
- **Retries.** `lost` and `write_failed` are retried from memory after 10
  minutes, 1 hour and 6 hours, then left alone
  (`flodni_warm_cache_failures_total` makes a failing disk visible); catch-up
  tries such a file again after 20 hours. After a
  timeout with no peers, or a torrent the engine did not have running, the rest
  of that title's files are skipped without a record; warm on add does not
  retry them, and catch-up (below) picks them up in a later pass.
- **Log line.** Each attempt logs one line, `[Warm] ` followed by a JSON
  object: `hash` (full), `file` (index), `trigger` (`add` or `catchup`),
  `outcome` (also `stopped` and `cancelled` for fills that recorded nothing),
  `cache`, `reason`, `head_bytes`, `tail_bytes`, `delivered_pieces`, `delivered_at`,
  `evicted` (files evicted for room) and `duration_ms`. It carries no path.
- **Catch-up.** Experimental, tested against fakes and, for one title, end to
  end (`make e2e`, variant `catch-up`: off, then on after a restart, then a
  second restart that feeds nothing, then the windows read back with the seeder
  dead). The quota stop, the exclusion of evicted files, the overwrite of a
  stale eviction mark and the 20-hour rule are covered by unit tests only. It
  warms the titles that are already in the library: the
  ones filed before `warm_on_add` was on, and the ones whose warm failed or was
  dropped. It runs only with `warm_on_add` on, `warmup_dir` set and the state
  database attached (without the database an eviction mark would not survive a
  restart). With `warm_on_add` off it does nothing; with it on and `warmup_dir`
  or the database missing, Flodni logs one warning and does nothing. The first pass
  starts one interval after the daemon starts (3 minutes at the default
  pace), then a pass starts 6 hours after the previous one ended, from a fresh
  walk of the stubs; two passes never run at once.
  - **Pace.** `warm_catchup_per_hour` (default 20) is the files started per
    hour, read on each feed, so a change in the control panel applies to a
    running pass. `0` turns catch-up and its day-scale retries off; a negative
    value is logged and replaced by 20. A pace raised from 0, or `warm_on_add`
    switched on, takes effect at the next pass: up to 6 hours later, or at a
    restart. A pass feeds one title at a time, newest stub first, only while
    the warmer is idle, so it holds at most one of the two workers: an add is
    never queued behind a backlog, and a playback pauses it like any fill.
  - **Scope.** `warm_catchup_scope` is `"all"` (the default; the rule below
    unchanged) or `"retries"`. Use `"retries"` with `warm_on_add` on and no
    backlog to walk: catch-up then feeds only files warm on add attempted
    and did not finish, so a failure there is tried again and the titles
    filed before warm on add was on are left alone. The files are: a failed
    read (the same 20-hour wait and 4 failed attempts as below);
    `dropped_quota`, `dropped_no_evidence`, `lost` and `write_failed`; and a
    file warm on add queued over an hour ago with no outcome recorded, which
    covers a restart in the middle of a warm, a wake error and a queue that
    was full (`file_state.queued_at`, schema 16: written when warm on add
    queues the file, cleared when an attempt records any outcome, and read
    after a restart). A file never attempted by warm on add (no record, or
    only `cached`), a file warm on disk and an evicted file (except the
    re-warm below, which both scopes run) are never fed, and the gauge
    counts the never-attempted ones as `out_of_scope`. The
    rest of the rules below (20 hours, the cap, never evicting, the quota
    stop) apply in both scopes. An unknown value is logged and replaced by
    `"all"`. Evidence does not change with the scope: a retried read the
    swarm served, failed or not, counts as it does today, and
    `dropped_quota`, `write_failed`, `lost` and a refused wake never do and
    never touch the readability or the title's streak. The pass logs its
    scope (`"scope"` in both `[WarmCatchup]` lines) and
    `flodni_warm_catchup_scope{scope}` says which one the last pass ran.
    Recommended pace for `"retries"`: `warm_catchup_per_hour` 6. The
    retries are few, a handful of files a day, and each one wakes a torrent
    and reads a swarm that already failed or a cache that was full, so
    there is nothing to hurry; at 6 an hour the first pass starts 10 minutes
    after the daemon starts. The default 20 is sized for walking a backlog.
  - **Which files.** A file is skipped when both windows are complete on disk
    (the disk decides, not the record; a record without `cache: cached` is
    written, with no wake). It is not warmed when its `cache` is `evicted`,
    or it carries an eviction mark or time, including a file evicted before
    schema 14: eviction is left to its next play, except the re-warm below.
    A file that failed 4 times is given up on; only a read through the mount
    that the swarm serves, or an add again, makes it eligible. Once one file
    of a title has given up through failed swarm reads (`lost` and
    `write_failed` do not count, nor `not_stored`), the title's other files
    are given up too, so a dead pack costs one wake a day for four days, not
    for four days per file; any swarm delivery for a file of the title after
    that give-up makes them eligible again. Warm on add of a new file is not
    affected: an add still tries once. A file attempted under 20 hours ago
    waits, and so does its whole title when any file of it failed under 20
    hours ago (a no-peers failure skips the rest of a pack without a record).
    A title the engine refused to wake (`not_stored`, for example a stub the
    engine never stored) waits for a play. Everything else is a candidate: no
    record, `dropped_quota`, `dropped_no_evidence`, `lost`, `write_failed`,
    and a record that says `cached` while the disk does not.
  - **Re-warm after eviction.** An evicted file is fed again when a
    webhook-confirmed play of it is recorded within the last
    `played_protection_days` (the same setting that protects it from
    eviction; `0` means a play counts forever) and no attempt on it has been
    recorded since that play. It then goes through the same rules as any
    candidate (the 20-hour wait, the cap, the engine refusing the hash, a
    title given up as a whole, which a re-warm never overrides) and
    the same pace, and, like all catch-up, only into free room: a full
    cache leaves it, counted `no_room`, never evicting another file. A file
    never played, or last played longer ago than the protection time, stays
    evicted until its next play. A re-warm is an attempt, so a file warmed
    again and evicted again, or one whose re-warm failed, is not fed again
    until it is played again. Under scope `"retries"` the re-warm is fed too:
    the file was kept once, so it is a retry, not the cold backlog. There is
    no state of its own: such a file counts as `pending` in
    `flodni_warm_catchup_files` (and as `left_no_room` when the pass left it
    for room), and the `pass_start` line carries `"rewarm"`, how many of the
    pending files are re-warms. Files left evicted stay `evicted`.
  - **Never evicts.** It checks for room before it wakes anything. At a full
    cache the pass ends: the file is counted `no_room`, nothing is recorded, and
    `flodni_warm_catchup_files{state="left_no_room"}` holds what the pass left.
    A pass also ends after three cache-side failures (`lost`, `write_failed`)
    of its own files.
  - **Retries.** There is no new state: the 20 hours and the 4 failed
    attempts are computed from `last_attempt_at` and `failed_attempts`, so the
    6-hour pass picks a failed file up once its 20 hours are over. The
    in-memory retries of a cache-side loss (10 minutes, 1 hour, 6 hours) stay;
    after them only the 20-hour wait applies, with no cap.
  - **Closed torrents.** It wakes only through the same check as warm on add
    (the engine must still store the torrent), never from the stub's magnet.
  - **Log.** Its attempts log `[Warm]` lines with `"trigger":"catchup"`, and a
    pass logs one `[WarmCatchup]` JSON line at its start and one at its end,
    with counts only (no path).
- **Metrics.** `flodni_warm_fills_total{trigger,outcome}` (`add` and
  `catchup`, every outcome from 0),
  `flodni_warm_fill_bytes_total{trigger,window}`,
  `flodni_warm_cache_failures_total{reason}`, `flodni_warm_queue_files`,
  `flodni_warm_paused` and `flodni_warm_catchup_files{state}`, the backlog as
  of the last pass: `pending` (also what a pass left because it was stopped, the
  pace was set to 0 or its disk failed), `retry_wait`, `gave_up` (a file's own
  4 failed attempts, or a pack-level give-up), `evicted`, `not_stored`,
  `out_of_scope` (scope `retries` only) and `left_no_room`.
  Already warm files and files a pass found no longer candidates are not
  counted. `flodni_warm_catchup_scope{scope}` says which scope the last pass
  ran.
- **Limits.** The queue and the retries are in memory. The eviction time is
  kept in whole seconds. Without the state database the add warmer still fills,
  evicts nothing, and its records stay in memory and are not served; catch-up
  does not run. A wake that fails for a passing reason (the engine busy) also
  skips the title; catch-up feeds it again in a later pass, once its 20 hours
  are over if an attempt was recorded. A title removed and added again while
  `warm_on_add` was off keeps its removal mark until a restart: catch-up lists
  it as `pending` every pass and feeds nothing.

### Warm the next episode (built, off by default)

`warm_next_episode` (default `false`) fills the windows of the next episode
while one plays, so a binge starts at once. It is built and has not run against
real plays yet. It reuses warm on add's warmer, so it needs `warm_on_add` on and
`warmup_dir` set; without them Flodni logs one warning and does nothing. With
it off nothing is listed, queued or counted.

- **When.** A play the media server's webhook confirmed (the same single-file
  resolution that marks a file played). Flodni lists the library, finds that
  episode and takes the next one in the same show: the next episode of the
  season, else the first of the next season, by the season and episode in the
  stub names. The show is the stub's show directory. A file that is not a
  library episode (a movie) does nothing and counts nothing.
- **One at a time.** At most one next-episode fill is in flight per playing
  show; a second confirmed play of the show meanwhile counts `in_flight`. The
  fills run on their own worker, one after another, and a playback does not
  pause them, which is the point (every other fill pauses for a playback).
- **Never evicts.** Before it queues anything it asks the cache whether the
  windows are warm already (`already_warm`) and whether they fit without
  evicting (`no_room`, then it skips). The fill itself runs under the
  never-evict policy, is not retried after a cache failure and stops at a
  removal or a shutdown; the next confirmed play asks again.
- **A season pack is skipped.** When the next episode is a file of the torrent
  being played (a pack), it is skipped (`same_torrent`) unless
  `warm_next_episode_same_torrent` is `true` (default `false`): a second reader
  on the playing torrent splits the engine's read-ahead, and a playing stream
  that slows down costs more than the head start gains. Only a next episode in
  another torrent is filled by default.
- **Metrics.** `flodni_warm_next_episode_total{outcome}`: `queued`, `no_next`,
  `same_torrent`, `no_room`, `already_warm`, `in_flight` and `error`, every
  value present from 0. The fills themselves show as
  `flodni_warm_fills_total{trigger="next"}`.
- **What should decide switching it on.** Not measured yet. Switch it on for a
  trial when a series is watched episode after episode and the first read of
  the next one is slow: `flodni_session_first_byte_seconds` of a binge's
  later episodes. Then check that the playing stream does not slow down while
  a fill runs: `flodni_read_stalls_total{confirmed="true"}` should not rise
  against the same weeks with it off, and `flodni_warm_next_episode_total{outcome="no_room"}`
  should be small (a full cache is skipped, never evicted for this).

### A stub the engine does not store

A closed torrent is woken from the engine's stored record only, and a hash
with no record, or a record without the metainfo, is refused. This is the
strict wake (`docs/roadmap.md`, step 1): a wake from a bare magnet would add
the torrent with the DHT and the public trackers and no private flag known
until the metainfo arrived (hard rule 3). Such a stub, from a library add made
before the engine saved it, a lost `config.db` (see "Engine state and
backup") or a raw engine `rem` with the stub left, stops opening until it is
added again. At startup the daemon logs one `WARNING: N of M library
torrents have no stored engine record with metainfo` line, naming the first
five hashes, and exports `flodni_library_torrents_unstored` (counted once,
after the library walk), so read it before deploying the strict wake.

What a refusal looks like:

- An open of the stub answers `EIO` (`ENOENT` if the stub is already gone
  from disk) and logs `[VFS] wake refused at open ...`.
- A read on a handle that is already open, whose torrent the engine closed and
  then does not store, answers `EIO` with no fetch and no engine call after the
  first; the handle stays refused until it is reopened.
- Neither is a dead swarm. No failed read is marked and no dead-swarm verdict
  follows. The readability record gets its own reason, `not_stored` (see
  "Readability record"), and `flodni_wake_refused_total` counts it.
- A wake that comes before the engine is up is not a refusal. It is the
  separate, transient `ErrEngineNotReady`: an open or a read answers the same
  `EIO`, logs `engine not ready` (at most one line per 10 s), and writes
  nothing: no `not_stored` in the readability record, no latch on the handle
  or in warm on add, no `flodni_wake_refused_total`. The next read asks again.
  Start-up waits for the engine to come up (up to 60 s, then logs a WARNING and
  goes on) instead of a fixed 2 s.

The library adds the engine now saves at once (`save_to_db`), so only existing
stubs are affected. To repair one, add the hash to the engine with
`save_to_db` set (`POST /torrents` with
`{"action":"add","link":"magnet:?xt=urn:btih:<hash>","save_to_db":true}` on
the engine port; an explicit add, so the link's trackers are used and a
private torrent needs its `.torrent` instead), or run a library remove and
then a library add. A library add alone answers "already present" and changes
nothing.

Every path that starts a torrent for a hash, labelled:

| Path | Kind | What it does |
|---|---|---|
| A read or an open through the mount, warm on add, catch-up | wake | `native.Wake` loads the stored record (`torr.LoadStored`); the magnet is used for its hash only, and a link that is not a magnet is refused without being fetched. Refuses an unstored hash. |
| `/play/{hash}/{id}`, `/stream` with a stored hash, and the engine's own lookup of a stored torrent | wake | `torr.LoadStored`. `/play` takes a bare hash only (40 hex or 32 base32): 400 for anything else, an `http(s)` or `file:` value never being fetched or read, and 404 for an unstored hash or a record without metainfo; `/stream` wakes a stored hash from its record the same way. |
| `POST /torrents` (`add`), upload, and `/stream?link=` on a new link with a token set | explicit add | The caller's link or file starts the torrent, a stored record or not; the stored metainfo and trackers are merged in when there is one. |

A load, a save and a removal of one torrent are serialised in the engine, so a
torrent that was removed stays removed: a wake that was still adding it
finishes first and the removal then takes it, and a save by an idle close that
lost the race writes no record. The one case left is an explicit add of the
same hash after the removal, which is a new add. This is unit-tested under the
race detector, and end to end: the `removal` variant of `make e2e` removes
three titles back to back (two through the library API, one with `rm`) while a
fourth is read and a descriptor is open on one of them.

### A stub whose file index is stale

A stub names its file by the index the engine gave it when the stub was
written. Flodni writes the index and the size from the same engine record, so
its own stubs agree with the engine. A stub from elsewhere, or an engine that
numbered a pack's files another way, can leave the index naming a different
file. The ids are the engine's own (the list saved with the torrent's record
and `Status()` serve the same ids): not the order of the metainfo and not a
sort of the paths. An open compares the stub with that list by one rule: the
stub's index wins when the engine's file there has the stub's length; else the
file of that length (by name if several); else the open fails with `EIO`.

- **The counter.** `flodni_stub_index_mismatch_total` counts the paths whose
  stub index differs from the file an open resolved, or names no file of the
  stub's length, once per path per process. Each first open logs one
  `WARNING: stub index:` line naming the path. It is 0 from the start. A
  non-zero value means a stale stub exists; zero over a few weeks of use means
  none of the files opened has one.
- **To repair a stub:** run a library remove and then a library add of the
  title. A library add alone answers "already present" and changes nothing.
- **What is guarded.** The open reads the engine's list from the engine when
  the torrent is loaded, else from the record saved with it, without waking the
  torrent. A warm head under the stub's index is used only when the file there
  has the stub's length; otherwise the open is cold (the torrent is woken
  first) and resolves by the rule above. When no file has the stub's length the
  open fails with `EIO` and the counter moves: nothing is served for a stub
  that cannot be matched. A record saved before the metainfo has no list; then
  an open takes a warm head only if it is no longer than the stub's file. Warm
  on add and catch-up refuse a file whose engine length differs from the
  stub's size (outcome `index_mismatch`). Not guarded: a torrent with no
  stored list whose stale stub shares its index with another file's warm head
  of a plausible length; `GET /api/library/audit` lists such stubs.

## FUSE mount options

Startup-only: the mount is made once, so a save from the Control Panel changes
them at the next start. A timeout outside its range refuses the start (and the
save). The reasoning, and what Flodni leaves to the host, is
[ADR 0005](docs/architecture/decisions/0005-fuse-timeouts-splice-readahead.md).

| Key | Values | Default |
| --- | --- | --- |
| `attr_timeout_seconds` | seconds, 0 to 60 | `1` |
| `entry_timeout_seconds` | seconds, 0 to 60 | `1` |
| `negative_timeout_seconds` | seconds, 0 to 60 | `0` |
| `fuse_disable_splice` | `true`, `false` | `false` |

Flodni does not invalidate the kernel's cached names when a stub is removed or
replaced, so an `attr_timeout_seconds` or `entry_timeout_seconds` above 1 can
keep a removed stub visible for that long. Start logs a `[Config] WARNING` line
when either is above the default. The kernel's readahead is host policy: Flodni
never writes it.

## Logging

The daemon logs through `log/slog` (`internal/flog`). The default output is
the text it has always printed, one line per event: an optional `[Name] `
prefix, the time (`2006/01/02 15:04:05`), then the message. There is no
level token in the line; the level only filters. A line that carries
structured fields appends the ones its message does not already show, as
`key=value`.

| Key | Env | Values | Default |
| --- | --- | --- | --- |
| `log_format` | `MKV_PROXY_LOG_FORMAT` | `text`, `json` | `text` |
| `log_level` | `MKV_PROXY_LOG_LEVEL` | `DEBUG`, `INFO` (`WARN`, `ERROR`: reserved, behave like `INFO`) | `INFO` |

`log_format` `json` prints one JSON object per line instead: `time`,
`level`, `msg`, `logger` (the `[Name]` prefix, when there is one) and the
line's fields. `msg` is the same text the default format prints, so a
documented line keeps its wording and payload in both formats. An unknown
`log_format` falls back to `text` with a warning. Both keys apply at start
and on a save from the Control Panel.

`log_level` `DEBUG` shows the debug lines; any other value shows Info and
above. `WARN` and `ERROR` are accepted but reserved: they behave like `INFO`,
so a `log_level` never hides a contract line (`[Session]`, `[Warm]`,
`[WarmupEvict]`, `[Startup]`, `[Webhook]`, `[ReaderBudget]`, `[DeadSwarm]`). A line whose
text starts with `[DEBUG]` is Debug (hidden unless the level is `DEBUG`), one
with `WARNING` or `Warning` near its start is Warn, one with `ERROR` or
`[PANIC]` is Error, everything else is Info.

**Stable lines.** The text of these lines does not change without a note
here, in either format; match them by substring: `[DiskWarmup] Initial
size`, `[DiskWarmup] Active`, `[StateDB] Active:`, `[StateDB] Migration
complete:`, `[StateDB] Migration already completed`, `Failed to open
StateDB`, `InodeMap: Loaded`, `FUSE mounted at`, `[FUSE] mounted, bdi`, `Library
torrents with no
stored engine record with metainfo`, `Failed to parse`, `physical_source_path
... differs`, `CORRUPT CACHE`, `prune skipped`, `Startup GC: Pruned`, and,
from the GoStorm engine, `Successfully migrated`, `Settings file was corrupt`
and `Migrate torrent`. The container entrypoint's `is NOT on a mounted
volume` warning is printed by the shell and is not affected. The JSON
contract lines, `[Session]`, `[Warm]`, `[WarmupEvict]`, `[Startup] report` and
the `[Webhook]` status line, are described in their own sections and are byte-identical in
both formats; `[DeadSwarm]` and `[TTFF]` wording is not a contract. The
GoStorm engine's own log lines still use the standard logger, so they keep
their old shape in both formats.

**Tracker credentials in logs.** A private tracker's announce URL carries the
user's passkey, in its path, its query or its userinfo, and a magnet carries it
in `tr=` and `xs=`. Every log line that can hold a link, a magnet, an error built
from one or an announce URL goes through one redactor (`internal/redact`), which
keeps `scheme://host[:port]` and replaces the rest with `/<redacted>`: the add
line prints `add torrent https://tracker.example/<redacted>`, a failed announce
says which tracker failed and why. This covers the add API, the wake and
activation errors, the discovery boost's failure list and all of the engine's
own log lines, the debug ones included (the engine's `EnableDebug` setting
logs every announce, with its URL, redacted). A `torrs://` link is replaced
whole, since it packs the trackers.

The conversion from `log.Printf` is `scripts/slogify` (see its README); it
can be run again on a newer tree.

## Metrics

`GET /metrics/prometheus` on the control port serves the Prometheus text
format 0.0.4, next to the JSON at `/metrics` that the control panel reads.
It follows the token rule of the other control routes: open when no token is
set, the bearer header only when one is (no `?token=`). It answers `GET`
only, with 405 and `Allow: GET` otherwise. A scrape reads in-memory counters
and never calls the torrent engine.

```yaml
scrape_configs:
  - job_name: flodni
    metrics_path: /metrics/prometheus
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/flodni-token
    static_configs:
      - targets: ["flodni:9080"]
```

Drop `authorization` when no token is set. The port has no TLS, so keep the
scraper on the same host or Docker network.

| Series | Type | Labels | Meaning |
|---|---|---|---|
| `flodni_build_info` | gauge | `version` | Always 1. |
| `process_start_time_seconds` | gauge | none | Start of the process. |
| `flodni_session_first_byte_seconds` | histogram | none | Open to the first byte served, per real playback. |
| `flodni_session_first_deep_read_seconds` | histogram | none | Open to the first read at 8 MiB or later or in the tail, per real playback. |
| `flodni_seek_read_duration_seconds` | histogram | none | Duration of the read that followed a seek. |
| `flodni_read_duration_seconds` | histogram | `source` | Duration of each served read. Its `_count` is the number of reads each path answered. |
| `flodni_read_bytes_total` | counter | `source` | Bytes handed to the kernel. |
| `flodni_sessions_closed_total` | counter | `outcome` | Closed sessions: `served`, `probe`, `unserved`, `dead_swarm`. |
| `flodni_dead_swarm_verdicts_total` | counter | `reason`, `source` | Dead-swarm verdicts; `reason` as `last_failure_reason`, `source` is `mount`, or `wake` for `metainfo_timeout`. |
| `flodni_warmup_cache_bytes` | gauge | none | Approximate size of the warmup cache; only with the cache enabled. |
| `flodni_warmup_cache_quota_bytes` | gauge | none | Its size limit; only with the cache enabled. |
| `flodni_warmup_evictions_total` | counter | `window` | Files removed by quota eviction, `head` or `tail`. |
| `flodni_warmup_evicted_bytes_total` | counter | `window` | Their allocated size in bytes. |
| `flodni_warm_fills_total` | counter | `trigger`, `outcome` | Warm fills. `trigger="mount"`: per-file records the mount path wrote, `outcome` `warmed` (the engine holds a swarm delivery stamp) or `cached` (it does not); needs the state database and index mode. `trigger="add"`, `trigger="catchup"` and `trigger="next"`: warm on add, catch-up and next-episode attempts, `outcome` as in "Warm on add", every value present from 0. |
| `flodni_warm_next_episode_total` | counter | `outcome` | Confirmed plays of a library episode seen with `warm_next_episode` on: `queued`, `no_next`, `same_torrent`, `no_room`, `already_warm`, `in_flight`, `error`; every value present from 0. See "Warm the next episode". |
| `flodni_warm_fill_bytes_total` | counter | `trigger`, `window` | Bytes warm on add and catch-up wrote, `window` `head` or `tail`, also by fills that did not finish. |
| `flodni_warm_cache_failures_total` | counter | `reason` | Warm on add and catch-up attempts that lost a window or could not write it: `lost` or `write_failed`. |
| `flodni_warm_queue_files` | gauge | none | Files waiting for warm on add or catch-up. |
| `flodni_warm_paused` | gauge | none | 1 while warm on add and catch-up are paused for a playback. |
| `flodni_warm_catchup_files` | gauge | `state` | Library files catch-up has not warmed, as of the last pass: `pending`, `retry_wait`, `gave_up`, `evicted`, `not_stored`, `out_of_scope`, `left_no_room`; every state present from 0. See "Warm on add". |
| `flodni_warm_catchup_scope` | gauge | `scope` | The `warm_catchup_scope` the last catch-up pass ran with: 1 for `all` or `retries`, 0 for the other; both 0 before the first pass. |
| `flodni_warmup_files` | gauge | `window` | Media files with a warm window on disk now: `head` counts heads at or above the 1 MiB ready floor, `tail` counts tails fully covered. Only with the cache enabled. |
| `flodni_playback_starts_total` | counter | none | Playbacks the media server webhook confirmed (not playing to playing). |
| `flodni_played_marks_total` | counter | `source` | Plays recorded on a file, the mark that protects it from eviction: `webhook` (a play resolved to one file) or `manual` (`POST /api/library/confirm`). Both present from 0. |
| `flodni_torrent_idle_closes_total` | counter | `confirmed` | Torrents the engine closed because no reader had them for the idle timeout. `confirmed` is `true` when a file of the torrent had a webhook-confirmed playback; the label is refreshed once a second. Both values present from 0. With the keep-alive on, a torrent it holds is not counted; see Keep-alive. |
| `flodni_torrent_reopens_total` | counter | `confirmed` | Closed torrents a read on an open handle woke again; the wake at open is not counted. `confirmed` as above. |
| `flodni_wake_refused_total` | counter | none | Wakes the engine refused because it keeps no record of the torrent: one per handle at a read, one per open. Not a failed read and not a dead swarm. 0 from start. See "A stub the engine does not store". |
| `flodni_read_stalls_total` | counter | `confirmed` | Served reads that took over one second; failed reads are not counted. `confirmed` as above. Independent of sessions. |
| `flodni_keepalive_releases_total` | counter | `reason` | Holds that ended: `stop` (a stop webhook, or the playback state dropped) or `ceiling` (`playback_keepalive_minutes` after the last activity). The torrent then closes at its next idle check. Both values present from 0. See Keep-alive. |
| `flodni_keepalive_evicted_total` | counter | none | Confirmed torrents that lost their hold to `playback_keepalive_max_torrents`, once per episode. Present from 0. |
| `flodni_keepalive_held_torrents` | gauge | none | Idle torrents the keep-alive is keeping open now (a torrent with a reader is not counted). |
| `flodni_playback_webhooks_received_total` | counter | `matched` | Play and resume webhooks for a movie or an episode; `matched` is `true` when the webhook resolved to one file, else `false`. |
| `flodni_playback_webhook_match_rule_total` | counter | `rule` | The same webhooks by the rule that resolved them: `basename`, `path_map`, `imdb`, `fuzzy`, `linked` or `none`; every value present from 0. See Play webhook. |
| `flodni_playback_webhook_last_timestamp_seconds` | gauge | `matched` | Unix time of the last play or resume webhook for a movie or an episode, `matched` as above; 0 = none seen. Persisted hourly with the state database. See Play webhook. |
| `flodni_unconfirmed_read_last_session_timestamp_seconds` | gauge | none | Unix time of the last session that crossed `unconfirmed_read_margin_mb` and closed unconfirmed; 0 = none seen. |
| `flodni_warning` | gauge | `name` | 1 while an advisory warning holds, else 0. `no_play_webhook`: see Play webhook. Never affects `/api/health` status. |
| `flodni_unconfirmed_read_bytes_total` | counter | none | Bytes past the head and tail windows that readers took from files with no confirmed playback, whichever path served them. Not bytes downloaded. See Reader budget. |
| `flodni_unconfirmed_read_sessions_total` | counter | `outcome` | Sessions that crossed `unconfirmed_read_margin_mb`, each counted once when it closes: `confirmed_late` if a webhook confirmed the file afterwards, else `unconfirmed`. |
| `flodni_unconfirmed_read_refused_reads_total` | counter | none | Reads the reader budget refused with EIO. Moves only with `refuse_unconfirmed_reads` on. See Reader budget. |
| `flodni_unconfirmed_read_holds_total` | counter | `result` | Holds of a refused handle, when they end: `lifted` by a confirmation (or the switch going off), `refused` when the time ran out. Moves only with `refuse_unconfirmed_reads` on. |
| `flodni_unconfirmed_read_confirmed_after_refusal_total` | counter | none | Sessions with a refused read that a webhook then confirmed, once each (plays the budget would have broken). Moves only with `refuse_unconfirmed_reads` on. |
| `flodni_reader_budget_readahead_overshoot_bytes_total` | counter | none | Bytes the engine was asked to fetch as read-ahead past the head window edge, for a read that started inside the window of a file with no confirmed playback. Asked for, not downloaded. Stays 0 with `unconfirmed_readahead_clamp` on. See Reader budget. |
| `flodni_reader_budget_attributed_fill_bytes_total` | counter | none | Bytes the pump and the prefetches filled past the head and tail windows of a file with a session and no confirmed playback. Count toward the margin only with `attribute_fills_to_reader` on. |
| `flodni_warmup_cold_reopen_total` | counter | `previously_played` | Confirmed plays that opened cold on a file quota eviction had removed, `true` or `false` by whether it had been played before. |
| `flodni_warmup_unverified_drops_total` | counter | none | Warmup blocks, head or tail, never written because their pieces did not verify in time. |
| `flodni_warmup_write_queue_drops_total` | counter | none | Head chunks dropped because the warmup write queue was full. |
| `flodni_readability_torrents` | gauge | `state` | Library torrents by readability: `failed` has a failure streak, `ok` has none and a read served by the swarm, `never_read` is the rest (no record, or a streak reset by a metainfo without a read). Not exported when the state database is off. |
| `flodni_library_torrents_unstored` | gauge | none | Library torrents with no stored engine record holding metainfo, counted once at startup after the library walk and not refreshed. Not exported until counted. See "A stub the engine does not store". |
| `flodni_library_unlinked_stubs` | gauge | none | Stubs no library symlink points at (`linked` is `false` in `GET /api/library/list`), as of the last list of movies and of tv (a client list, the audit and a catch-up pass refresh it). Not exported, rather than 0, while there is no linked set or a kind has not been listed yet. See "Listing stubs". |
| `flodni_stub_index_mismatch_total` | counter | none | Library paths whose stub file index differs from the file an open resolved by size and name, once per path per process. Non-zero means a stale stub. See "A stub whose file index is stale". |
| `flodni_tracker_scrapes_total` | counter | `result` | Experimental. Tracker scrapes per torrent and tracker: `ok` (answered), `error` (the request failed or the answer left the torrent out), `skipped` (no BEP 48 scrape URL or an unsupported scheme). All values present from 0 once the scraper runs. See Tracker scrape. |
| `flodni_torrents_by_seeders` | gauge | `bucket` | Experimental. Torrents by the seeders of their latest scrape answer: `0`, `1-2`, `3+`. A torrent with an error entry is in none. |
| `flodni_tracker_scrape_pass_running` | gauge | none | Experimental. 1 while a scrape pass runs. |
| `flodni_tracker_scrape_last_pass_finished_timestamp_seconds` | gauge | none | Experimental. Unix time the last finished full pass ended; absent before the first. |
| `flodni_readahead_cache_bytes` | gauge | none | Bytes in the read-ahead cache. |
| `flodni_readahead_cache_active_bytes` | gauge | none | The part that is not stale. |
| `flodni_readahead_cache_entries` | gauge | none | Entries in the cache. |
| `flodni_readahead_cache_budget_bytes` | gauge | none | The cache's size budget. |
| `flodni_readahead_evictions_total` | counter | none | Chunks the cache dropped on a `Put` to stay under its budget. Chunks the stale sweep removes are not counted. 0 from start. |
| `flodni_warmup_head_fill_duration_seconds` | histogram | none | Time from the first chunk to the last of a head fill, per file whose head completed. Buckets 2, 5, 10, 15, 30, 60 and 120 s. |
| `flodni_fuse_short_reads_total` | counter | none | Reads served with fewer bytes than requested away from EOF. |
| `flodni_fuse_short_read_results_total` | counter | `result` | Outcome of the refill: `repaired` or `failed`. |
| `flodni_fuse_read_ahead_bytes` | gauge | none | Kernel read-ahead of the FUSE mount in bytes (`read_ahead_kb` times 1024), read from sysfs at each scrape. Flodni never writes it; the host may tune it (the start line `[FUSE] mounted, bdi <major:minor>, read_ahead_kb <n>` names the device). Absent where sysfs cannot be read. |
| `flodni_fetch_dedup_total` | counter | none | Concurrent fetches of one chunk that shared a single fetch. |
| `flodni_engine_short_reads_total` | counter | `path` | Engine reads that ended short and were reported as success: `stream` (the sequential reader) or `fetch` (the stateless block fetch behind the FUSE fallback). 0 from start. |
| `flodni_engine_short_read_repairs_total` | counter | `result` | Outcome of the stream reader's refill loop, per read that needed it: `repaired` after a reconnect, `unfilled` when a gap stayed. |
| `flodni_readahead_refetched_bytes_total` | counter | none | Bytes the engine fetched again for a piece that had completed and been evicted from the in-memory piece cache (kept per torrent while it is open). Counted, not acted on: the baseline for judging eviction. 0 from start. |
| `flodni_readahead_stranded_partial_bytes` | gauge | none | Bytes held by partially written pieces outside every reader's protected window, over open torrents. Eviction only takes complete pieces, so after a seek these bytes stay until the cache frees them, on its clean pass or its own 5 s timer (at most every 5 s), for a piece untouched for 20 s that the engine no longer wants and has nothing in flight for (it forgets the chunks first, so a later read downloads them again). Sampled at scrape under each cache's reader read lock; no engine lock. Alert on a value that stays high with no one playing. |
| `flodni_engine_peer_redials_total` | counter | none | Peers the opt-in redial (`redial_dropped_peer`) added back to a private torrent after its only connected peer dropped. 0 with the switch off. See Engine stall gaps. |
| `flodni_engine_db_write_errors_total` | counter | `op` | Engine database (`config.db`) commits that failed, by `op`: `set` (a record save), `rem` (a removal) or `clear`. Each is a record not saved or not removed; alert on any increase. All three present from 0. See Engine state and backup. |
| `flodni_engine_hedge_triggers_total` | counter | none | Tail-hedge duplicate requests the engine fired. A process counter: unlike `hedge_trigger_count` in `/metrics`, it does not fall when a torrent expires. |
| `flodni_engine_peer_ejects_total` | counter | `transport` | Outlier peers the engine dropped for a rate far under the swarm median, `tcp` or `utp`. Process counter. |
| `flodni_engine_peer_churn_drops_total` | counter | `transport` | Peers dropped by the warmup-probe churn, `tcp` or `utp`. Process counter. |
| `flodni_engine_gradient2_backoff_samples_total` | counter | none | Gradient2 limiter samples whose latency gradient was below 1, the loop reacting to rising latency. Stays 0 while the limiter is off, which is the default. Process counter. |
| `flodni_sessions_open` | gauge | none | Sessions open now, the length of the `open` list of `/api/sessions`. |
| `flodni_sessions_min_ahead_bytes` | gauge | none | The smallest `ahead_bytes` over open sessions that a play webhook confirmed and that opened more than 60 seconds ago (the first 60 seconds are left out, so a cold start does not flap it). No sample while no session qualifies: absent, not 0. Worked out at scrape. The alert to use: under 16 MiB for 60 s while a session is playing. |
| `flodni_session_ahead_bytes` | histogram | none | `ahead_bytes` of every open session that has one, sampled at scrape. Buckets 1, 4, 16, 32, 64, 128, 256, 512, 768 and 1024 MiB. Capped at 1 GiB, so the top bucket and `+Inf` hold sessions with 1 GiB or more ahead. |
| `go_goroutines` | gauge | none | Goroutines that currently exist. |
| `go_memstats_heap_inuse_bytes` | gauge | none | Bytes in in-use heap spans. |
| `go_memstats_sys_bytes` | gauge | none | Bytes the Go runtime obtained from the system. |
| `go_gc_cycles_total` | counter | none | Completed garbage collection cycles. |
| `process_open_fds` | gauge | none | Open file descriptors, counted from `/proc/self/fd`. Not exported where there is no `/proc`. |

`source` is `warmup_head`, `warmup_tail`, `racache_hit` or `fetch_block`.
Every label value is exported at 0 from the start. Durations are in seconds
and the buckets are fixed (1 ms to 10 s). Series names and labels are meant
to be stable; buckets and help text are not.

- Counters reset when Flodni restarts; use `rate()` or `increase()`.
- `flodni_session_first_byte_seconds` and
  `flodni_session_first_deep_read_seconds` cover only sessions that reach
  the `served` thresholds above, not scanner probes, and a session counts
  when it closes. `flodni_seek_read_duration_seconds` is recorded at the
  read itself in any open session, scanner probes included.
- `source` names the path that answered a read, not where the bytes came
  from: a `racache_hit` may hold bytes the swarm delivered a moment ago.
- The dead-swarm series stay 0 when the state database is off; nothing is
  recorded then.
- Durations are kept in microseconds and exported in seconds: a read under
  1 ms lands in `le="0.001"` and adds its fraction to `_sum`, and a read of
  1.5 ms is in `le="0.005"`. The JSON `/metrics/ttff` keeps whole
  milliseconds (`max` truncated, `avg` and the percentiles in ms).
- The engine counters (hedge, eject, churn, Gradient2) are atomics the
  engine bumps where each event happens, one set per process, so a scrape
  takes no engine lock. The JSON `/metrics` fields of the same name are
  still sums over active torrents and fall when one expires.
- `flodni_warmup_head_fill_duration_seconds` has the same buckets as the
  `warmup_duration_buckets_...` field of `/metrics`; the JSON buckets are
  open on the right (`<2s`), a histogram's `le` is closed, so they differ only
  for a fill of exactly a bound.
- `flodni_read_bytes_total` counts every served read, including reads after
  a session closed and reads with no session, so it differs from the
  `[Session]` lines.
- `flodni_warmup_cache_bytes` is a running total, resynced from the disk on
  each eviction walk. Writes and removals through the cache keep it in step
  (counted in allocated blocks, see the eviction section); it can still
  differ from the disk by files changed behind the cache's back, until the
  next walk.
- `flodni_warmup_files` reads the cache's in-memory index. A head removed
  behind the cache's back shows until the next lookup of it, a tail until
  the next restart. Warm bytes are `flodni_warmup_cache_bytes`. The series is a count of files, not of
  complete media files: a head counts once it holds 1 MiB, whatever the
  head window size.
- `flodni_readability_torrents` is recomputed every 60 s, so a scrape stays
  in memory. The library size is the number of distinct torrents
  looked up through the mount, so a new torrent counts once it appears
  there. Records of torrents that left the library are not subtracted (a
  startup prune removes them), and `never_read` stops at 0 rather than
  going negative.

## Viewing the model

`make check` validates the architecture model and `make view` serves it at
<http://localhost:8080/workspace/1>. Both need Docker. `make docs` checks
that the documentation agrees with the tree.

## Contributing

Pull requests are opened by the maintainer only. Issues are welcome; see
[CONTRIBUTING.md](CONTRIBUTING.md).

## Licence

GPL-3.0, see [LICENSE.md](LICENSE.md). Tiramisu is copyright Matteo
Rancilio; third-party components are listed in [NOTICE](NOTICE).
