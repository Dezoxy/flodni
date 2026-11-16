# 4. Scope the API tokens and the panel sign-in

Date: 2026-11-16

## Status

Proposed on 2026-11-16, for v0.3.0, and revised the same day after a
security review of the first draft. Nothing in this record is built.

Builds on the gate described in the Auth section of
[3. Make the file lifecycle explicit in the library API](0003-file-lifecycle-api.md):
that record kept one bearer token and said a scoped mechanism
needs its own decision. This is that decision.

## Context

Flodni has one credential. With `api_token` unset both servers are open (the
control server on `metrics_port`, default 9080, and the engine's server on
8090). With it set, a single bearer token opens every route except the panel
page, `GET /api/health` and the engine's `GET /echo`. The same string is held
by the media server's webhook, the metrics scraper, the bridge that uploads
torrents, automation that adds and removes titles, and the person using the
panel. The threat model already names this as a known gap (T-14, "one shared
token with no scopes").

What that costs today, all from the tree:

- **The blast radius of a leaked token is the whole service.** A webhook URL
  carries `?token=` (T-05), so the credential most likely to land in a proxy
  log is the one that can also write `config.json`, restart the daemon and
  shut the engine down.
- **Open mode is the default and the only alternative to all-or-nothing.** The
  open-port hardening (Origin and Sec-Fetch-Site refusals, the Host allowlist,
  fail-closed `restart`, `confirm` and token writes) exists because there is
  nothing between "any client" and "the maintainer".
- **The token is stored in plaintext** in `config.json` or the environment, and
  the panel keeps it in the browser's local storage, where any script on the
  origin can read it.
- **A deployment behind an identity-aware proxy cannot use it.** The proxy
  already authenticates the person; Flodni still asks for the token.

### What the gate looks like today

`internal/apiauth` wraps both servers outside their routers: `Middleware`
(control, `main.go`) and `GoStorm` (engine, `internal/gostorm/web/server.go`
`newServer`) take the token as a string and call one `gate`. The engine's
`newServer` serves the plain-http listener and the optional TLS listener
(`settings.Ssl`), so both count as the engine port and get the same gate.

The gate cannot do everything alone. It runs before routing, so it cannot
know which route pattern a request will match: the control `mux` can be asked
(`mux.Handler(r)` returns the pattern), gin cannot be asked from outside. The
design therefore splits the work (see "Where each half runs"): the outer gate
authenticates and applies the transport rules on both ports; the scope check
runs where the pattern is known.

| Today | In this record |
|---|---|
| Open routes: `GET /control`, `GET /api/health` on control; `GET /echo` on the engine (`controlOpen`, `gostormOpen`) | Same, plus a cheap `GET /api/live`, which joins the same open set (including the cross-site exemption) |
| `?token=` on `/plex/webhook` and `/webhook` by exact path (`controlWebhook`) | Same two routes; a file token must be webhook-only to use it; stripped before any handler or log sees it |
| Constant-time compare of the raw token, length checked first (the length leaks) | SHA-256 of the presented token, constant-time compare against every stored hash, the legacy token included |
| With a token set the Origin and Sec-Fetch-Site checks are off, because the bearer header is the gate | Still off for a bearer request; a stricter set **on** for a request authenticated by a cookie or a proxy header, because a browser attaches those by itself |
| The engine's handlers know only `api_token_enforced` (`TokenEnforcedKey`), which lets `/stream` add and fetch | Set per request, only for a caller holding `admin` (see "Scopes") |
| `app.controlToken` decides the fail-closed routes (`restart`, `confirm`, `api_token` writes) | Same switch, now "any auth enforced" |
| `gin.Recovery` and `WebLogger` on the engine | Replaced or routed through the redactor (see "Metrics and logs") |

### The route inventory

Read from the two routers, not guessed. Control routes are registered
in `internal/daemon/main.go` on an own `http.ServeMux`; engine routes in
`internal/gostorm/web/api/route.go`. The `authorized` group there is the
engine's optional Basic auth (`settings.HttpAuth`), unrelated to this gate,
and stays as it is. The scope is what the route needs in the proposed model.
"Today" is the current gate with `api_token` set.

Control server (port 9080):

| Method | Path | Today | Scope |
|---|---|---|---|
| POST | `/plex/webhook` | token, header or `?token=` | `webhook` |
| POST | `/webhook` | token, header or `?token=` | `webhook` |
| GET | `/metrics` | token | `metrics` |
| GET | `/metrics/profiling` | token | `metrics` |
| GET | `/metrics/prometheus` | token | `metrics` |
| GET | `/metrics/ttff` | token | `metrics` |
| GET | `/api/sessions` | token | `read` |
| GET | `/api/torrents/health` | token | `read` |
| GET | `/api/startup` | token | `read` |
| GET | `/api/library/list` | token | `read` |
| GET | `/api/library/file` | token | `read` |
| GET | `/api/library/audit` | token | `read` |
| POST | `/api/library/add` | token | `library` |
| POST | `/api/library/remove` | token | `library` |
| POST | `/api/library/confirm` | token, 403 on an open port | `library` |
| POST | `/api/library/inspect` | token | `library` (it contacts the engine and so the swarm) |
| POST | `/api/torrents/scrape` | token | `scrape` |
| GET | `/api/torrents/scrape/status` | token | `scrape` |
| POST | `/api/warmup/evict` | token | `admin` (drops cached data) |
| GET | `/api/config` | token | `admin` (paths, URLs, host list) |
| POST | `/api/config` | token | `admin` |
| POST | `/api/gostorm/settings` | token | `admin` (the engine's settings set, proxied) |
| POST | `/api/restart` | token, 403 on an open port | `admin` |
| GET | `/api/health` | open | open |
| GET | `/control` | open (static page) | open |
| GET | `/api/live` | does not exist | open (new) |
| POST | `/control/login` | does not exist | session (new, scoped mode only) |
| POST | `/control/logout` | does not exist | session (new, scoped mode only) |

Engine server (port 8090, and its TLS port). `POST /settings` and
`POST /torrents` carry an `action` in the JSON body; the scope depends on it.

| Method | Path | Action | Today | Scope |
|---|---|---|---|---|
| GET | `/echo` | | open | open |
| POST | `/settings` | `get` | token | `engine` |
| POST | `/settings` | `set`, `def` | token | `admin` |
| POST | `/torrents` | `list`, `active`, `get` | token | `engine` |
| POST | `/torrents` | `add`, `set`, `rem`, `drop`, anything else | token | `admin` |
| POST | `/torrent/upload` | | token | `upload` |
| POST | `/cache` | `get` is the only action | token | `engine` |
| GET, HEAD | `/stream`, `/stream/*fname` | | token | `engine`, with the open-port rule unless `admin` |
| GET, HEAD | `/play/:hash/:id` | | token | `engine` |
| GET | `/download/:size` | | token | `engine` |
| GET | `/storage/settings` | | token | `engine` |
| POST | `/storage/settings` | | token | `admin` |
| POST | `/shutdown`, `/shutdown/*reason` | | token | `admin` |

Counts (rows; a method or an action counts once): 52 in all.

| Scope | Control | Engine | Total |
|---|---|---|---|
| `webhook` | 2 | 0 | 2 |
| `metrics` | 4 | 0 | 4 |
| `read` | 6 | 0 | 6 |
| `library` | 4 | 0 | 4 |
| `scrape` | 2 | 0 | 2 |
| `engine` | 0 | 13 | 13 |
| `upload` | 0 | 1 | 1 |
| `admin` | 5 | 9 | 14 |
| open or session | 5 | 1 | 6 |

Where this table departs from the owner's list of scopes, and why:

- `POST /api/library/inspect` is not named; it is `library`, not `read`,
  because it asks the engine for the torrent.
- `GET /api/config`, `POST /api/warmup/evict` and `POST /api/gostorm/settings`
  are not named. `GET /api/config` returns paths and the host list, so it is
  `admin`; evict deletes cache, so `admin`; the settings proxy is the engine's
  `settings set`, so `admin`.
- `GET /api/torrents/health` is `read` (same class as sessions).
- `POST /torrent/upload` gets its own `upload` scope, not `engine`. It adds
  and saves a torrent (`upload.go`), so a script that only lists must not
  hold it. The bridge holds `engine` and `upload`.
- The engine's `/stream`, `/play`, `/download` and `/storage/settings` GET are
  `engine`. A media player cannot send a header, so with a scoped
  configuration those routes are for scripts; that is how `/stream` behaves
  with a token set today. `/stream` is the one route where `engine` is
  deliberately weaker than a token today; see "Scopes".
- `/control` stays open (static page, no data) and `GET /echo` stays open
  (a version string). The owner's list has only `/api/health` and one
  liveness route; both extras are needed, see "Open routes".

## Decision drivers

- One maintainer, one deployment: the smallest model that limits a leaked
  token, not a policy engine.
- With no token configured, Flodni must behave as v0.2.6, open-port hardening
  included. The one deliberate exception is a `config.json` that cannot be
  read, which must stop the start (see "Compatibility").
- Fail closed: a malformed, unreadable or contradictory configuration must
  stop the start, never degrade to open mode.
- A new route must not be able to ship open by accident.
- No dependency without need (`CLAUDE.md`).
- Hard rule 1: no secret in the repository, in a log or in a metric label.

## Considered options

### A. Scoped tokens in the existing gate, a hashed token file (chosen)

Extend `internal/apiauth` with a route-to-scope table and a token store. One
file holds named tokens with scopes and SHA-256 hashes. Panel sign-in through
an optional verified proxy JWT, or a login cookie.

- Pro: one table enforces both ports; no new process; the change is additive
  and opt-in; stored hashes mean the file can be mounted read-only and backed
  up without leaking a credential.
- Con: a route table to keep correct (mitigated by a test, below); rotation is
  a file edit and a restart; one gin middleware on the engine.

### B. Keep one token, put the scopes in a reverse proxy

A proxy in front of 9080 and 8090 maps paths to credentials.

- Pro: nothing to build in Flodni.
- Con: the engine port is reached directly by the bridge and scripts; the
  action-in-body routes cannot be told apart by path; every deployment must
  rebuild the same table; the proxy becomes the security boundary and Flodni
  stays all-or-nothing behind it. Fails the "no accident" driver.

### C. OAuth2 / OIDC login in Flodni

Flodni becomes a relying party: redirects, code exchange, sessions.

- Pro: the standard answer for people.
- Con: a login flow, a client secret to store and a session store for a
  single-maintainer tool whose callers are mostly machines. An identity-aware
  proxy already does this and hands Flodni a signed assertion, which option A
  can verify with far less code. Rejected for now; the JWT mode is the
  integration point if this is ever wanted.

### D. mTLS for machine callers

- Pro: strong, no secret on the wire.
- Con: Flodni serves no TLS on the control port (T-04); certificate issuance
  and distribution to the media server and the bridge is more operational
  weight than the threat needs. Not rejected for good: a TLS proxy can
  terminate it in front.

## Decision

Option A. Scoped, named tokens held as hashes in a file; both servers enforce
them through one route table; an optional verified-JWT mode and a login
cookie let a person use the panel without a token in the browser.

### Scopes

| Scope | Grants |
|---|---|
| `webhook` | the two play webhooks (`POST`), the only routes that may take `?token=` |
| `metrics` | `GET /metrics*` on the control port |
| `read` | health detail, startup report, sessions, library list, file and audit |
| `library` | library add, remove, confirm and inspect |
| `scrape` | `POST /api/torrents/scrape` and its status |
| `engine` | the engine's torrent list, active and get, settings get, cache, streaming routes (read-only `/stream`, below) |
| `upload` | `POST /torrent/upload`, which adds a torrent |
| `admin` | everything above, plus config reads and writes, restart, evict, engine settings set, torrent add and removal on the engine, shutdown, and `/stream` that adds |

`admin` implies every other scope. No other scope implies another: a `library`
token cannot read sessions unless it also holds `read`, and an `engine` token
cannot upload.

**`/stream` and the add.** Today a token lets `/stream` add a hash the engine
does not hold, fetch an `http(s)` link and open a `file:` link (`stream.go`,
`ParseLink`); `TokenEnforcedKey` is what switches the open-port refusal off.
Scoped mode must not make that available to every `engine` token, so the key
is set **per request, and only for a caller holding `admin`** (and for the
in-process caller, below). An `engine` caller gets the open-port rule: it
plays or reports a hash the engine already holds, and refuses (403) any
other link, any `http(s)` or `file:` link before any fetch, and `save` and
`preload`. The earlier wording "an `engine` token cannot add by link" is true
only because of this rule, and it is the rule, not the scope, that makes it
so. A `library` token still reaches the engine's add through Flodni (see
"Residuals").

### The route-to-scope map is the single source of truth

One table in `internal/apiauth` (method, route pattern, optional engine
action, scope), written once and used on both ports and for the metric label.
Rules that make it hold:

1. **Unknown means `admin`.** A route or a method absent from the table is
   authorized as `admin`, so forgetting an entry costs the caller a refusal,
   never exposure. This includes a method no row lists for a known path
   (`POST /control`, `DELETE /api/health` need `admin`; the earlier idea of
   "the path's strictest scope" would have left them open).
2. **A test fails on any route registered without a row.** It compares
   patterns, never raw paths, so no hand-written matcher can disagree with the
   router: the engine side lists gin's `Routes()` and compares each registered
   path pattern to the table; the control side builds the mux through a small
   registrar that records each pattern as it registers (`handle(pattern,
   scope, handler)`; `http.ServeMux` has no enumeration). The registrar is a
   refactor of the inline block in `main.go`, one commit, no behaviour change.
3. **The router names the pattern.** On the control port the gate asks the mux
   (`mux.Handler(r)`), on the engine a gin middleware reads `c.FullPath()`. A
   request that matches no pattern is `unmatched`, which is `admin`. Because
   the router decides, differential cases (trailing slash, `//`, case,
   `/stream/` against `/stream`, a path with dot segments) cannot be
   classified differently from how they are served; the test includes them. On
   the engine `RedirectTrailingSlash` and `RedirectFixedPath` are turned off,
   so no redirect is produced before the middleware runs.
4. **A golden copy catches silent widening.** The matrix test derives its
   expectations from the table, which would pass whatever the table says. A
   reviewed `testdata/route_scopes` file holds the same rows by hand; any
   change to the table fails until the golden is edited in the same PR.

### Where each half runs

- **Control port.** The outer gate (`apiauth.Middleware`) does everything,
  because it can ask the mux for the pattern: Host allowlist, credential
  extraction, authentication, the scope check, the ambient-credential rules.
- **Engine port.** The outer gate (`apiauth.GoStorm`) does what needs no
  pattern: Host allowlist, extracting and **authenticating** the bearer
  token (which is the only credential this port reads), and refusing a bad or
  missing one. It stores the identity (scopes, token name) in the request
  context. The first middleware in `newRoute` (before `WebLogger`, so before
  anything reads a body) does the pattern-dependent part: the scope check
  from `c.FullPath()`, the body-action peek, and setting `TokenEnforcedKey`
  for an `admin` caller. Both listeners (plain and TLS) share `newServer`, so
  both are covered.

**The engine port never reads the cookie or the proxy header.** Browsers send
a cookie to every port on a hostname, so reading it on 8090 would give any
page on the panel's host a credential for the engine. 8090 accepts the bearer
header only (and the in-process caller).

### The body action

Engine routes with an `action` in the body need the body. A mismatch between
what the gate reads and what the handler reads would be an `engine` to
`admin` escalation, so the design is narrow:

1. **Authenticate first.** Nothing reads a body before the caller has
   authenticated and holds a scope that could apply. An unauthenticated caller
   never makes Flodni read anything.
2. **Same decoder, same struct.** The peek decodes with `encoding/json`'s
   `Decoder` into the same tagged struct the handler binds (`requestI`,
   `route.go`, extended with whatever field each handler's request type uses
   for `action`). It does not read the action out of a `map`: `encoding/json`
   matches struct keys case-insensitively and the last duplicate wins, so
   `{"action":"list","ACTION":"rem"}` is `rem` to the handler and `list` to a
   map reader. Trailing data is ignored by gin's own `Decode`, so the peek
   ignores it too (same behaviour, deliberately).
3. **Cap by reading.** The peek reads through `io.LimitedReader` with a 64 KiB
   cap, not by trusting `Content-Length` (a chunked body has none). Over the
   cap is 413 (reason `body`). A `Content-Encoding` other than identity is
   400 (neither side decodes gzip). An unparsable body is 400 (`body`). A
   missing or unknown action is `admin`.
4. **Replay the bytes.** The handler receives the buffered bytes, exactly the
   ones that were decoded.
5. **Cross-check in the handler.** The peeked action goes in the request
   context. The `torrents` and `settings` handlers refuse (400) when the
   context holds an action that differs from the `req.Action` they bound. In
   open mode nothing is peeked and the check does nothing.
6. **Read deadline.** The engine server sets only `ReadHeaderTimeout` and
   `IdleTimeout` (T-09), deliberately, since streams and `/torrent/upload`
   are long. A server-wide `ReadTimeout` would cut them, so the peek sets a
   10-second read deadline through `http.NewResponseController` for its read
   and clears it afterwards. That bounds a slow body on the two routes that
   read one, after authentication, and leaves the long routes alone.
   `/torrent/upload` is multipart and is classified by path alone: the gate
   never buffers an upload.

### The token file

JSON, because the config already is:

```json
{
  "tokens": [
    {"name": "plex-webhook", "scopes": ["webhook"],            "sha256": "<64 hex>"},
    {"name": "bridge",       "scopes": ["engine", "upload"],   "sha256": "<64 hex>"},
    {"name": "automation",   "scopes": ["library", "read"],    "sha256": "<64 hex>"},
    {"name": "prometheus",   "scopes": ["metrics"],            "sha256": "<64 hex>"},
    {"name": "operator",     "scopes": ["admin"],              "sha256": "<64 hex>"},
    {"name": "break-glass",  "scopes": ["admin"],              "sha256": "<64 hex>"}
  ]
}
```

- **Location.** `auth_tokens_file` in `config.json`, or
  `FLODNI_AUTH_TOKENS_FILE` (the variable wins, like the other secrets). The
  file is meant to be mounted read-only; Flodni never writes it.
- **Startup-only settings.** In scoped mode `api_token`, `auth_tokens_file`,
  `auth_jwt` and `auth_panel_https` are read at start and never changed
  through the API. A `POST /api/config` whose body would change any of them
  answers 403 and writes nothing; a save that does not touch them keeps the
  file's values, as a save keeps `api_token` today. This closes a persistence
  path the existing gate has: with a token enforced, a posted `api_token` is
  written to `config.json`, and a restart (which an `admin` may also request)
  then activates it, so a leaked admin token, a 12-hour cookie or an admitted
  proxy user could install a credential, or their own issuer and JWKS, that
  survives the owner's rotation. Legacy mode keeps today's behaviour.
- **Hash.** SHA-256 of the token, hex. Tokens are generated, 256 bits of
  randomness; at that entropy the hash only has to be one-way and fast. A
  slow KDF (argon2, bcrypt) exists to make guessing a low-entropy human
  password expensive; against 2^256 it buys nothing, and it would cost a KDF
  run on every webhook and every metrics scrape. The hash is only as strong as
  the token, so the only supported way to make one is `flodni auth
  new-token`; a hand-hashed short password is outside the model.
- **Token shape.** A token is `fl_` plus 43 base64url characters (32 random
  bytes from `crypto/rand`). The gate checks that shape before it looks
  anything up, and a presented value of any other shape never matches a file
  entry. The loader also refuses the hash of the empty string. Without both,
  `echo -n "$UNSET" | sha256sum` pasted into the file would be matched by an
  empty bearer. The prefix lets a secret scanner recognise a leaked token; the
  repository's gitleaks configuration gains a rule for
  `fl_[A-Za-z0-9_-]{43}`.
- **Loader.** Strict. Each of these is a refusal to start: unknown JSON
  fields, trailing data after the object, a file over 64 KiB, an empty,
  repeated or reserved name (`legacy`, `internal`), a name outside
  `[a-z0-9][a-z0-9_-]{0,31}` (names are logged and ride in the cookie), a
  repeated `sha256`, a `sha256` that is not 64 lower-case hex characters or is
  the hash of the empty string, a scope not in the list, an entry with no
  scopes, more than 64 tokens, and a file with zero tokens. A configured path
  that is unreadable or unparsable is a refusal to start, never open mode:
  "I pointed Flodni at a token file" is the owner's statement that auth is
  wanted. A file that is group- or world-writable logs a warning; it does not
  refuse, because container mounts show arbitrary modes.
- **CLI.** `flodni auth new-token --name NAME --scopes a,b` prints the token
  once and the JSON entry to add, and touches no file. It runs offline,
  starts no daemon, and prints the token to stdout only. Same pattern as
  `flodni trackers` and `flodni scrub-stubs`.
- **Rotation.** Edit the file (add the new entry, deploy the new token to the
  caller, remove the old entry) and restart. There is no reload signal: one
  code path that reads the file, once, at start. Keep one `admin` entry as a
  break-glass credential, stored offline: if the identity provider is down,
  the cookie login with that token still opens the panel.

### Lookup

Check the shape, hash the presented token once, then compare the hash to every
stored hash with `subtle.ConstantTimeCompare`, with no early exit, keeping
the matched entry through `subtle.ConstantTimeSelect`. The compared values
are fixed-length hashes, so no length check leaks anything, and the cost does
not depend on which entry matched. With at most 64 entries the loop is
negligible. **The legacy `api_token` is hashed at load and is one more entry
in the same loop** (named `legacy`, all scopes, exempt from the shape rule
because an owner chose it); today's code compares raw bytes after a length
check, which leaks the token's length.

### Transport

- `Authorization: Bearer <token>` on every route. A bearer value that is
  empty counts as no credential.
- **`?token=` only on a webhook route, and only for a webhook-only token.**
  A file token is accepted in the query only when its scopes are exactly
  `["webhook"]`; an `admin` or multi-scope token in a URL is refused
  (`query_not_allowed`), because a URL is the place credentials leak from
  (T-05) and only a credential that can do little should live there. The
  legacy token keeps the query form on the two webhook routes during
  migration (it is how the media server's URL works today), with one WARN
  per start naming it.
- **No validity oracle off the webhook routes.** On any other route in
  scoped mode a request that carries a `token` query parameter is refused with
  `query_not_allowed` **without hashing or comparing the value**, even when it
  also carries a valid header. The response is the same whether or not the
  value was a real token, so the refusal tells a prober nothing. (Legacy mode
  keeps today's behaviour: the parameter is ignored off the webhook routes.)
- **Stripped before anything sees it.** After authentication the gate removes
  `token` from `r.URL.RawQuery` and `r.RequestURI` before it calls the
  handler, so no downstream log, error text or handler reads it.
  Independently, `internal/redact` gets a `token=` and `access_token=` rule
  beside its `passkey=` net (it has none today), so a line built from a URL
  elsewhere is masked. What a proxy in front logs is out of Flodni's hands
  (T-05 stays).

### The engine port

The same gate wraps 8090 with the same tokens (see "Where each half runs").
No proxy is needed.

**Flodni's own calls to its engine.** The library client and the panel's
engine-settings proxy (both in `main.go`) send `gc().APIToken` today, to
`gc().GoStormBaseURL`, which the settings proxy re-reads on every request and
which a config save changes live. In scoped mode the file holds hashes, so
Flodni has nothing stored to send. A secret it invents for itself must not be
able to leave the machine; as the code stands a redirect or a changed
`gostorm_url` would carry it to an address an `admin` chose. Two designs, and
a recommendation:

- **Recommended: an in-process `RoundTripper`.** The two self-callers use an
  `http.Client` whose `Transport` hands the request straight to the engine's
  handler (below the gate), in the same process. There is no credential, no
  socket, no redirect and no `gostorm_url` involved: the request cannot go
  anywhere else. The handler is given an identity that sets `TokenEnforcedKey`
  (it is Flodni's own add). Cost: `web` exposes its handler, and the library
  client takes an injected client. This removes the secret entirely, which is
  better than guarding it.
- **Fallback if the handler cannot be exposed cleanly: a loopback secret.** A
  random 256-bit secret per start, in memory only, never a `Config` field
  (`Redacted` blanks only two fields, so anything on `Config` can leak
  through `GET /api/config` or a save). Self-calls dial a fixed loopback
  address, never `gostorm_url`, with redirects disabled. The engine gate
  accepts the secret only from a loopback peer, with `engine` and `admin`,
  on 8090 only; the control gate never accepts it. It is never logged,
  written or returned.

Either way, scoped mode **refuses to start when `gostorm_url` is not a
loopback host**: an embedded engine is always local, and a remote engine would
need a credential Flodni can send, which a hash file cannot supply (the legacy
token keeps working against a remote engine in legacy mode).

`GET /echo` stays open: `checkEngine` in the health route calls it without a
header, and the container healthcheck depends on that chain.

### Open routes

The owner's rule is that only `/api/health` and a cheap liveness route are
unauthenticated. This record adds one route and keeps two:

- `GET /api/live` (new): answers 200 and `ok` with no dependency check, for a
  container or load balancer that only needs to know the process serves. It
  exists because `GET /api/health` runs the mount, engine and database
  checks (`health.go`) and so is not cheap, and it returns a `warnings` array
  that tells an unauthenticated caller whether webhooks arrive (T-12). It is
  registered in every mode, so it is also an additive v0.2.6 route, and it
  joins the open set that `controlOpen` and the cross-site exemption use.
- `GET /api/health` stays open as today. `flodni healthcheck` uses it.
- `GET /control` stays open: the static page holds no data, and in the cookie
  mode a person cannot sign in to a page they cannot load.
- `GET /echo` on 8090 stays open for the reason above.

### The panel behind an identity-aware proxy

Credentials, tried in this order per request, first present one decides and a
wrong one is refused without falling through:

1. **Bearer header.** A non-empty `Authorization: Bearer`. The panel today
   sends `Authorization: Bearer ` (empty) on every fetch (`settings.html`),
   which would refuse every cookie or JWT request under this rule. The panel
   is changed to send the header only when it holds a token, and the gate
   treats an empty bearer as absent, so an old panel still works.
2. **Session cookie** (below), control port only.
3. **Proxy JWT**, when configured, control port only.

A request authenticated by 2 or 3 is an **ambient-credential request**: a
browser attaches the cookie and the proxy attaches the assertion to a forged
form post alike. For a non-GET request (every state-changing control route
takes POST) the gate requires all three of:

- an `Origin` whose **host and port** equal the request's `Host`, scheme
  ignored. Port normalisation: a `Host` or `Origin` with no port means the
  scheme's default for that side, so a TLS tunnel (Host `panel.example`,
  Origin `https://panel.example`) matches, and `https://panel.example:8443`
  against Host `panel.example:8443` matches, but another port on the same
  host does not. A missing `Origin` is refused (browsers send it on a POST;
  scripts use a bearer token);
- `Sec-Fetch-Site: same-origin` when the header is present; a missing header
  passes (older clients), `same-site`, `cross-site` and `none` are refused;
- `Content-Type: application/json` (parameters allowed), because the handlers
  parse JSON whatever the type, and a `text/plain` post needs no preflight.

The hostname-only check of today's open port is not enough here: cookies and
`SameSite` ignore ports, so a page served from another port on the same
hostname is "same site" and sends the cookie. A GET with an ambient credential
keeps the existing cross-site refusal. This is the "keep the Origin check"
requirement made precise, and stricter than the open-port rule. Refusals are
counted with reason `csrf`.

**Proxy JWT mode (optional).** Configuration in `config.json`, all required
when the block exists except `allowed_emails`:

```json
"auth_jwt": {
  "issuer":   "https://<team>.cloudflareaccess.com",
  "audience": "<application audience tag>",
  "jwks_url": "https://<team>.cloudflareaccess.com/cdn-cgi/access/certs",
  "header":   "Cf-Access-Jwt-Assertion",
  "allowed_emails": []
}
```

(The values show the shape for Cloudflare Access; any provider that signs an
assertion and publishes a JWKS fits, such as an oauth2-proxy deployment.
`<team>` and the audience tag are the deployment's own.) The mode is off when
the block is absent. Each of these refuses to start: an incomplete block, a
`jwks_url` that is not `https`, `header` equal to `Authorization` in any case
(it would collide with the bearer rule), and **`auth_jwt` without
`auth_tokens_file`**: the JWT is a way into scoped mode, and a configuration
that names it and no token file is a mistake to stop, not a mode to guess.

Verification, in order, any failure refusing with the reason in parentheses.
A request with no JWT header is simply "no credential" (`missing`); the
`jwt_*` reasons are for a header that is present.

1. Exactly one header value (a repeated header is `jwt_malformed`) with three
   segments of **strict unpadded base64url** (no `=`, no whitespace, no
   standard-alphabet characters). Verification is over the received bytes
   `header.payload`, never over a re-encoding.
2. The protected header's `alg` is `RS256` or `ES256` and nothing else
   (`jwt_alg`); `none`, `HS*`, `PS*`, `ES384` and an absent `alg` are
   refused. `crit` present at all is refused (`jwt_malformed`). `jwk`, `jku`,
   `x5u` and `x5c` are ignored, never used to find or trust a key. A `kid` is
   required (`jwt_unknown_kid` when missing or unknown).
3. The algorithm is chosen by the key, not the token: an RSA key verifies only
   `RS256`, a P-256 key only `ES256`. This is what stops an `HS256` token
   "signed" with the public key.
4. The signature verifies (`jwt_signature`). An ES256 signature must be
   exactly 64 bytes (`r||s`), checked before any maths.
5. `iss` equals `issuer` exactly (`jwt_issuer`); `aud` (a string or an array)
   contains `audience` (`jwt_audience`); `exp` is present and in the future
   (`jwt_expired`); `nbf`, when present, is not in the future
   (`jwt_not_yet_valid`). Skew allowed: 60 seconds each way. A missing or
   mistyped claim is `jwt_claims`, as is a token whose `email` is not in
   `allowed_emails` when that list is set.

A verified token grants `admin`, on the control port only. A proxy header on
8090 is ignored. What the signature does not cover, and so must hold by
configuration: the audience must be this application's own, or any user the
proxy admits to another application is an admin here; an assertion is a bearer
credential until `exp` and can be replayed by whoever copies it (the CSRF
rules above do not stop a replay by a script, only a forged browser request);
and `allowed_emails` is the optional second list for a proxy policy that is
wider than "the maintainer", off by default.

**Keys.** Fetched over HTTPS from `jwks_url` with a 5-second timeout, a 64 KiB
body cap and **no redirects at all** (a "same host" rule still allows an https
to http downgrade). Parsing is strict:

- only RSA keys of at least 2048 bits, and EC keys on P-256 whose point is
  verified to be on the curve;
- a key's `use`, when present, must be `sig`, and its `alg`, when present,
  must match the algorithm the key type allows;
- a key with no `kid`, or a `kid` repeated in the set, makes the fetch fail;
- an **empty key set counts as a failed fetch**, so a provider that serves an
  empty document cannot wipe the cache.

The set is cached in memory. A token with an unknown `kid` triggers one refresh,
**with a single refresh in flight** (concurrent requests wait for it) and at
most one per minute, so a caller sending random key ids cannot make Flodni
hammer the provider. A successful fetch replaces the set; keys otherwise
expire after 24 hours. A failed fetch keeps the last good set and backs off 1,
2, 4 minutes up to 15. **A key the provider has revoked stays trusted until
the next successful refresh, up to 24 hours**; that window is accepted and is
a residual.

**When the JWKS is unreachable:** JWT authentication fails closed with
`jwt_keys_unavailable` for any token whose key is not already cached; tokens
and the cookie are unaffected, and the daemon starts anyway (the first fetch
never blocks the start). The startup report shows `jwt: true`; the refusal
metric shows the outage. The break-glass token is the way in.

**Dependency.** Recommendation: the Go standard library plus a verifier of
about 150 lines in `internal/apiauth` (JWS compact parse, `crypto/rsa`
`VerifyPKCS1v15`, `crypto/ecdsa` `VerifyASN1` after converting the JWS's raw
`r||s`, `encoding/json` for the header, claims and JWKS). The alternative is
`github.com/golang-jwt/jwt/v5`.

- For `golang-jwt/jwt/v5`: audited, widely used, handles claims and
  algorithm allow-listing (`WithValidMethods`), fewer lines of our own
  crypto-adjacent code.
- Against: it does not fetch or cache a JWKS, handle `kid` refresh, or parse
  and vet a JWK, so the part that needs the most care is ours either way; the
  library saves the 100-odd lines that are the most testable. It is a new
  module in `go.mod`, to track and audit on each bump, in a repository whose
  rule is no dependency without need, for a verifier that must accept exactly
  two algorithms and nothing else.
- The risk of the stdlib route is a hand-rolled verifier with a subtle bug.
  The mitigation is that the primitives are the standard library's and the
  failure matrix in the test plan is the review. If the owner prefers a
  library, `jwt/v5` is the one; the rest of this record does not change.

**Session cookie (the generic fallback).** Control port only, and only in
scoped mode:

- `POST /control/login` with `{"token":"..."}`. The token must hold `admin`;
  one without answers 403, a wrong one 401. Both count in the refusal metric
  (`invalid`, `scope`). Success sets the cookie and answers 204. Login is
  itself an ambient-credential-style request for CSRF purposes: it needs the
  same Origin, Sec-Fetch-Site and Content-Type rules.
- The cookie `flodni_session` holds `base64url(name|expiry|generation)` and an
  HMAC-SHA256 tag over it. `HttpOnly`, `SameSite=Strict`, `Path=/`, an
  absolute expiry of 12 hours (no sliding window, so a stolen cookie has a
  bound). `Secure` is set when the request arrived over TLS, when the login
  request's `Origin` is `https` (a proxy that omits `X-Forwarded-Proto` would
  otherwise yield a cookie without it), when the proxy said so
  (`X-Forwarded-Proto: https`), or when `auth_panel_https` is true.
- The HMAC key is **random at each start** (32 bytes from `crypto/rand`, in
  memory). A restart logs every panel session out, which is what a rotation
  wants. Deriving it from the token file would let sessions survive a restart
  and tie the key to a file other parties may read.
- **Logout revokes.** A process-wide **generation counter** is part of the MAC
  input; `POST /control/logout` increments it, so every cookie issued before
  it stops verifying, a stolen one included. With one maintainer, signing out
  everywhere is the right meaning; per-session revocation would need a store
  this record rejects. A cookie also names its token entry and is re-checked
  against the store on each request.
- The panel (`settings.html`) loses the token field and the local-storage
  token in this mode: a 401 shows a sign-in prompt that posts to
  `/control/login`; the browser's same-origin fetches carry the cookie.
- **Which prompt.** `/control/login` and `/control/logout` exist only in
  scoped mode (404 in legacy and open mode). A 401 in scoped mode carries the
  response header `Flodni-Auth: login`; the panel shows the sign-in form when
  it sees it and the existing bearer prompt (token in local storage) when it
  does not, which is how legacy mode keeps working unchanged. The header names
  no path and no secret.
- No lockout or rate limit on `/control/login`: with the token shape enforced
  a generated token cannot be guessed, and the refusal counter is the alert.
  An owner-chosen legacy token is the weakness (T-08, unchanged).

### Metrics and logs

- `flodni_auth_refusals_total{route,scope,reason}`. `route` is the route
  **pattern** from the table, not the raw path (`/play/:hash/:id`, never a
  hash), and `unmatched` for a path in no row, so cardinality is bounded by
  the 52 rows plus one. `scope` is the scope the route requires, not the
  presented token's, so no token name or identity can reach a label. `reason`
  is one of:
  - `missing`: no credential of any kind was presented on a route that needs
    one (this includes an absent JWT header);
  - `invalid`: a credential was presented and is wrong (unknown token,
    malformed shape, bad cookie tag);
  - `scope`: a valid token without the needed scope;
  - `query_not_allowed`: a `token` query parameter where it is not allowed;
  - `body`: the engine action peek refused the body (too large, unparsable,
    encoded);
  - `csrf`: the Origin, Sec-Fetch-Site or Content-Type rule for an
    ambient-credential request;
  - `session`: a cookie that is expired, tampered or from before a logout;
  - `jwt_malformed`, `jwt_alg`, `jwt_unknown_kid`, `jwt_keys_unavailable`,
    `jwt_signature`, `jwt_issuer`, `jwt_audience`, `jwt_expired`,
    `jwt_not_yet_valid`, `jwt_claims`: a JWT header was present and failed;
  - `host`, `origin`, `cross_site`: the existing refusals of the same gate,
    counted in the same family in **every mode, open mode included**.
  Emitted by the daemon's Prometheus text writer (`metrics_prom.go`);
  `apiauth` reports each refusal through an observer option so it imports no
  metrics code.
- `flodni_auth_legacy_requests_total`, no labels: requests authenticated by
  the legacy token while a tokens file is configured (and in `legacy_token`
  mode). "Remove `api_token` last" becomes observable: when it stays at zero
  for a while, the legacy token is unused.
- A refusal is logged as today: method, path **quoted with `%q`** (a path can
  carry control characters, and token names now make log integrity matter),
  remote address, and the reason. Never the query, a token, a cookie or a
  header value. A success on a mutating route (library writes, config,
  restart, engine settings) logs the **token name** at INFO: a name is a
  label chosen by the owner, not a secret, and it closes the repudiation gap
  in T-14 for the writes that matter.
- **The engine's loggers.** `WebLogger` prints `RawQuery` and the body; its
  line goes through the redactor, and the redactor has the `token=` rule.
  `gin.Recovery` dumps the request line and every header (masking only
  `Authorization`), which would print a cookie, the proxy header and
  `/stream` links; it is replaced by a recovery middleware that logs the panic
  value and the stack and no request. The control server logs no query today
  (its refusals print the path only, and neither server sets `ErrorLog`); a
  test pins that.

### Compatibility

| Configuration | Behaviour |
|---|---|
| No `api_token`, no tokens file, `config.json` absent or readable | **v0.2.6**: both servers open, the open-port hardening in force. The additions are `GET /api/live`, the refusal metric family (which also counts the existing Host, Origin and cross-site refusals), and the `auth` section of the startup report. |
| `config.json` exists but cannot be read or parsed | **Refuses to start, in every mode.** The one deliberate departure from v0.2.6. |
| `api_token` only | Today's behaviour: one token, every scope, `?token=` on the two webhook routes. Reported as `legacy_token`. |
| `auth_tokens_file` only | Scoped mode: deny by default. |
| Both | Scoped mode; `api_token` is a token named `legacy` with every scope, a WARN at start and `legacy_admin: true`. |
| `auth_jwt` without `auth_tokens_file` (with or without `api_token`) | Refuses to start. |
| `auth_tokens_file` set and the file missing, unreadable or invalid | Refuses to start. |
| Scoped mode and a `gostorm_url` that is not loopback | Refuses to start. |
| `auth_jwt` with `header: Authorization`, a non-https `jwks_url`, or an incomplete block | Refuses to start. |

**Why an unreadable `config.json` refuses in every mode, not only when it
names auth.** Today a file that exists but cannot be read yields defaults
silently (`config.go`, no else branch), and one that fails to parse logs and
continues with defaults (surfacing only as a startup alarm). Either way
`auth_tokens_file` is lost and the daemon starts open. A start that cannot read
its configuration cannot know whether the file names auth settings, so the
rule cannot be conditional on content it has not read. An absent file
(not-exist) stays valid: defaults, as today. The cost is that an owner with a
broken file and no auth now gets a refusal where v0.2.6 started open and
logged a warning; the panel already refuses to save over such a file (409),
which shows the intent. The environment variable `FLODNI_AUTH_TOKENS_FILE`
is read independently, so a set variable with an unreadable `config.json`
still refuses (the file may name `auth_jwt`).

On the both-set case, recommended over refusing to start: the point of a
migration is that the existing callers (the media server's webhook URL, the
bridge, automation) keep working while each gets its own scoped token, one at
a time. A refusal forces every caller to change in the same restart. Treating
`api_token` as `admin` adds no privilege (it already had everything), keeps
the `?token=` webhook URL working, and the cost is that an owner who forgets
to remove it keeps a plaintext all-power token; the WARN, the report's
`legacy_admin` (which also joins the report's existing alarms), the legacy
request counter and the roadmap's last step ("remove it") are the guard.
Refusal is the other defensible choice and the owner may take it (open
question 1).

**Migration is one switch, then a clean-up.** Create the file with
`flodni auth new-token`, set `auth_tokens_file`, restart. Callers are then
moved to scoped tokens at leisure; the last step removes `api_token`. Rolling
back is deleting the setting and restarting. A mistake in the file stops the
start and names the line, which is the failure mode the owner wants.

**The startup report.** `[Startup] report` stays byte-identical to
`GET /api/startup`; the report is built once and both are that one body. A new
top-level key is additive and `schema` stays at 1:

```json
"auth": {
  "mode": "scoped",
  "tokens": 6,
  "scopes": ["admin", "engine", "library", "metrics", "read", "upload", "webhook"],
  "jwt": false,
  "legacy_admin": false
}
```

`mode` is `open`, `legacy_token` or `scoped`; `tokens` counts entries
(`legacy_token`: 1; `open`: 0; a legacy token beside a file counts); `scopes`
is the sorted union in use (all eight for the legacy token); `jwt` is whether
the mode is configured. It carries no name, no hash, no path and no key. A
contract test pins that every key the v0.2.6 report had is still present with
the same type, and that the line and the endpoint body are the same bytes in
all three modes.

### Not in scope

- Rate limiting, lockout, or a server-side session store.
- Per-user identity beyond a token name; user accounts of any kind.
- TLS in Flodni's control port (T-04 stays; a TLS proxy remains the answer).
- Hot reload of the token file; fine-grained per-route or per-title scopes.
- Authenticating the engine's own optional Basic auth differently; it stays.
- Authentication of the FUSE mount (B3).

## Consequences

Positive:

- A leaked webhook URL or metrics scraper credential can no longer write
  configuration, restart or shut down the daemon.
- No credential in plaintext on disk (except an owner-kept legacy token), and
  the token file is safe to mount read-only and to back up.
- The panel in a browser holds no bearer token in script-readable storage.
- A new route cannot go public by omission, and the table makes the whole
  permission model one reviewable page.
- Behind an identity proxy, nobody types a token to use the panel.

Negative and accepted:

- **Two more things to keep correct**: the route table (guarded by the
  completeness test and a golden file) and the verifier.
- **Rotation is a restart**, and a restart logs panel sessions out.
- **The engine action peek** reads a body on two routes in scoped mode, and a
  handler cross-check reads the context on them.
- **Logout signs out everywhere**, because revocation is one counter.
- **A hashed token cannot be recovered**: a lost token is replaced.
- **An unreadable `config.json` now stops the start** where v0.2.6 started.
- **More than one way to be misconfigured**, which the loader's strictness
  and the startup report are there to expose.

## Risks

- **Fail open on a bad file.** The worst possible bug here. Mitigated by the
  loader contract (any problem with a configured file stops the start), the
  unreadable-config rule, and a test that every malformed-file case ends in
  an error and never in `open`.
- **An unlisted route or method ships open.** Cannot happen: unknown means
  `admin`.
- **A JWT accepted for the wrong application.** Mitigated by exact `iss`,
  `aud` and a pinned algorithm per key type; the operator's duty to use a
  dedicated audience is in the documentation.
- **The JWKS fetch as a probe.** The URL is configuration only and not
  changeable through the API in scoped mode, https only, no redirects,
  capped, rate-limited. It is still a blind fetch to an address the owner
  named; a mistyped host is the owner's to see in the refusal metric.
- **A body-action mismatch.** An `engine` to `admin` escalation if the gate
  and the handler disagree. Mitigated by the same decoder and struct, bytes
  replayed, and the context cross-check refusing a difference.
- **Private torrents.** No change to peer discovery: the gate decides who may
  call the add, upload, inspect and library routes. Scoped mode makes the
  rule tighter, not looser: `engine` gets the open-port `/stream` rule, and
  `upload` is separate so a list-only script cannot add. The privacy tests
  stay as they are. Not yet verified: whether a `library` add can pass an
  `http` link to the engine (the hash pattern in `gostorm_client.go` is
  unanchored); step 3 checks this before `library` is documented as
  non-fetching.

## Threat model changes

Rows that change in the implementing PR (this record edits none of them):

| Row | Change |
|---|---|
| T-01 | Spoofing: the compare is over hashes of every entry, the legacy token included; a token is bound to scopes. A 403 for insufficient scope joins the 401. |
| T-02 | The default is still open, and still documented. The gap "no mitigation other than a token" gains the scoped mode; the open-port refusals remain for the unconfigured case. The GoStorm `POST /shutdown` gap closes in scoped mode (it needs `admin`). |
| T-03 | A stored token is no longer plaintext in the new file. A passive reader on the wire still sees a bearer token (T-04). |
| T-05 | Rewritten: a file token is accepted in `?token=` only if its scopes are exactly `webhook`, so the credential in a URL can forge a play event and do nothing else; the legacy token keeps the query form during migration, with a WARN. Stripped from every Flodni log. A proxy's own log is still out of scope. |
| T-06 | A separate webhook credential now exists, and it is the only kind that rides in a URL; a forged event needs it. |
| T-10, T-11 | Config and library writes need `admin` and `library`; a `webhook`, `metrics`, `read`, `scrape` or `upload` token can do neither. In scoped mode the auth settings cannot be changed through the API at all. |
| T-12 | Health stays open; `GET /api/live` is the cheap probe that leaks no warning. |
| T-14 | Per-caller identity: each mutating success logs a token name. Still no audit log. |
| T-15 | Rewritten. For a request authenticated by a cookie or a proxy header the primary defence is no longer the hostname-only Origin check, which a page on another port of the same host passes, and which `SameSite` does not stop (the same host is the same site): a non-GET needs an `Origin` whose host **and port** equal `Host`, `Sec-Fetch-Site: same-origin` when present, and `Content-Type: application/json`. The engine gate never reads the cookie. The open-port gaps for an unconfigured deployment are unchanged. |
| T-08 | Residual unchanged (no rate limit); the refusal metric makes guessing visible. |
| New rows | (a) the stolen-token blast radius per scope; (b) the proxy-JWT path: header trust, audience, replay, JWKS outage and fetch; (c) the session cookie: theft, generation revocation, CSRF. |

Residuals that remain after this change, to be written into the threat model:

- Tokens live at the callers: in compose environment and `docker inspect`, in
  the media server's stored webhook URL, in shell history.
- `config.json` and its backups stay plaintext while `api_token` and the
  Plex token remain.
- Per-scope reach: `library` reaches the engine's add through Flodni; an
  `engine` token can stream any title the engine holds.
- The cookie is sent to every port on the host; only the control gate reads
  it, and only with the CSRF rules above.
- A JWT can be replayed until `exp`, and a revoked JWKS key is trusted for up
  to 24 hours.
- The JWKS URL is a blind fetch to an address the owner named.
- The in-process secret, if the fallback design is built instead of the
  `RoundTripper`.
- No TLS (T-04), no rate limit (T-08), a legacy token that is a
  human-chosen secret if the owner keeps it, and an `admin` token or an
  admitted proxy user being able to do everything `admin` does.

## Migration: small pull requests

Each step is one PR off `main`, behaviour-preserving in open mode, with its
tests (test first, shown red, then green).

1. **Route table and registrar.** The scope table, the registrar for the
   control mux, `GET /api/live`. No enforcement. Tests: completeness (gin and
   mux), differential path cases, the golden file, `/api/live`, open mode
   unchanged. Also the unreadable-`config.json` refusal.
2. **Token file, loader, CLI, report.** `auth_tokens_file`, the strict loader,
   `flodni auth new-token`, the startup `auth` section, the refusals in the
   compatibility table. Tests: every malformed-file case refuses to start;
   `new-token` round trip; report contract in all three modes.
3. **Scoped gate on both ports.** Lookup, shape check, scope check on both
   halves, the body peek and its handler cross-check, query tokens, the
   refusal metric, legacy-as-admin, per-request `TokenEnforcedKey`, the
   startup-only settings, the self-call path. Tests: the matrix, 8090
   enforcing the same tokens, `/stream` refusals for `engine`, the body
   mismatch cases, bounded metric labels.
4. **Logs and redaction.** `token=` in `internal/redact`, `WebLogger` through
   it, the replaced recovery middleware, quoted paths, a guard that the
   control server logs no query. Tests: no log, metric or refusal line holds a
   token, a cookie or the proxy header, including after a panic.
5. **Proxy JWT.** Verifier, JWKS cache, `auth_jwt`. Tests: the failure matrix,
   the outage, and bearer and cookie still working during it.
6. **Panel cookie.** `/control/login`, `/control/logout`, the generation
   counter, the panel's sign-in prompt and its bearer fix, the CSRF rules.
   Tests: attributes, expiry, tamper, restart and logout invalidate, the CSRF
   cases, `Secure` cases.
7. **Documentation and the threat model.** README, `SECURITY.md`, the changed
   rows above, a gitleaks rule for the token shape, an end-to-end scoped
   start, and the ADR moved to Accepted. The owner then moves each caller to
   its own token and removes `api_token`.

## Test plan

- **Scope matrix.** For every row of the route table on both ports (the
  engine's TLS listener included), against each of: no credential, a wrong
  token, each of the eight scopes alone, a two-scope token, the legacy token,
  and a cookie (which the engine must ignore). The expected result is written
  by hand in the golden file, not computed from the table under test.
  Handlers are stubs that record being reached, so a refused request is shown
  never to reach one.
- **Completeness and routing.** gin `Routes()` and the mux registrar each
  against the table; differential cases (trailing slash, `//`, case, dot
  segments, unlisted methods) classified by the router's own pattern.
- **Query tokens.** A webhook-only token accepted on the two webhook routes
  and stripped from what the handler sees; an `admin` or multi-scope file
  token refused there; the legacy token accepted with a WARN; refused with
  `query_not_allowed` everywhere else and on 8090, and shown to do no hashing
  (a counter on the lookup); a captured log buffer, the engine log, the
  refusal and success logs, the redactor output and the metrics text never
  contain the token.
- **Body action.** `{"action":"list","ACTION":"rem"}`, duplicates, unicode
  escapes in the key, trailing data, over the cap by chunked body with no
  `Content-Length`, gzip, a slow body hitting the deadline, a body never read
  before authentication; the handler refuses when the context action differs.
- **`/stream`.** An `engine` token refused for an unheld hash, an `http(s)`
  link, `file:`, `save` and `preload`, and allowed for a held hash; an `admin`
  token adds. The existing open-port tests pass unmodified.
- **Settings are startup-only.** In scoped mode a posted `api_token`,
  `auth_tokens_file`, `auth_jwt` or `auth_panel_https` that differs gets 403
  and writes nothing; an unrelated save keeps the file's values.
- **JWT.** Valid RS256 and ES256; wrong audience (string and array form);
  wrong issuer; expired; not yet valid; skew inside and outside 60 s; bad
  signature; tampered payload; no header (`missing`); `alg: none`; `HS256`
  signed with the public key; `crit`; `jwk`, `jku` and `x5c` ignored; a
  repeated header; padded base64url; a 63-byte ES256 signature; an off-curve
  point; a 1024-bit RSA key; `use: enc`; a mismatched JWK `alg`; no `kid` and
  an unknown `kid` (one refresh, then refusal, no second fetch within a
  minute, one fetch in flight under concurrency); an empty key set; a
  redirecting JWKS; JWKS unreachable (fail closed, last good set kept,
  backoff); oversize and non-HTTPS JWKS; `header: Authorization`;
  `allowed_emails`; the header on 8090 ignored.
- **Loader.** Each malformed file refuses to start and never reaches open
  mode: bad JSON, unknown field, trailing data, duplicate name, duplicate
  hash, reserved or badly-charactered name, empty scopes, unknown scope,
  short or non-hex hash, the hash of the empty string, zero tokens,
  unreadable path, over 64 tokens, over 64 KiB. An unreadable or unparsable
  `config.json` refuses; an absent one starts. `auth_jwt` without a file
  refuses. A non-loopback `gostorm_url` in scoped mode refuses.
- **Lookup.** The loop visits every entry regardless of the match; an empty
  or wrongly shaped bearer never matches; the legacy token goes through the
  same loop.
- **Self-calls.** The library client and the settings proxy reach the engine
  with no credential in any request, log or `GET /api/config`; a changed
  `gostorm_url` or a redirect cannot carry anything off the machine.
- **Compatibility.** With neither setting, the v0.2.6 test files
  (`apiauth_test.go`, `origin_test.go`, `hosts_test.go`, the daemon's
  open-port tests) pass unmodified; the startup contract holds in every mode.
- **Cookie and CSRF.** Attributes, expiry, a tampered tag, a restart or
  logout invalidating it, the `Secure` cases including an https `Origin`; a
  POST from another port of the same host, with `text/plain`, with
  `Sec-Fetch-Site: same-site`, and with no `Origin` refused; the TLS-tunnel
  case (no port on `Host`, https `Origin`) accepted; a token without `admin`
  refused at login; the panel sends no empty bearer.
- **Race.** `go test -race` over the gate with concurrent requests, a JWKS
  refresh and a logout in flight.

## Open questions

Recommendations in bold; the owner may override any. Questions 1 to 3, 6 and
12 need an owner decision; the rest are defaults the owner may leave.

1. **`api_token` and a tokens file together.** **Treat `api_token` as an
   `admin` token named `legacy`, with a WARN, `legacy_admin: true` and a use
   counter.** Alternative: refuse to start. Staged migration versus a forced
   flag day.
2. **Self-calls.** **An in-process `RoundTripper`**, no secret. Alternative:
   the loopback secret with the constraints above. Needs the engine's
   handler exposed from `web`.
3. **An unreadable `config.json` refuses to start in every mode.**
   **Yes.** Alternative: only when the environment or a readable part names
   auth, which cannot be known for a file that is unreadable.
4. **Is `/control` open?** **Yes, static page only.**
5. **`GET /echo` and a liveness route.** **Keep `/echo` open and add
   `/api/live`.**
6. **Email allow-list for the JWT.** **Optional, off by default.** The
   proxy's policy is the primary list; this is insurance if it is ever wide.
7. **Cookie signing key from the file?** **No: random at start**, plus a
   generation counter so logout revokes everywhere.
8. **An engine on another host.** **Not supported in scoped mode** (refuses
   to start); legacy mode keeps working with it.
9. **`POST /torrents` actions for the bridge.** **`list`, `active` and `get`
   are `engine`; `add`, `set`, `rem`, `drop` are `admin`; upload is its own
   scope.**
10. **Log the token name on mutating successes.** **Yes**, names only.
11. **Count Host, Origin and cross-site refusals in the same metric.**
    **Yes**, in every mode.
12. **A break-glass `admin` token.** **Yes, one, stored offline**, listed in
    the file like any other; it is the way in when the identity provider is
    down. The owner decides where it is kept.
13. **Reload on a signal.** **No.** One read at start; restart to rotate.

## Related

- Requirements: none recorded.
- Architecture views: Daemon (control server, engine server).
- Other ADRs: [3. Make the file lifecycle explicit in the library API](0003-file-lifecycle-api.md),
  whose Auth section this extends.
- Threat model: [control API and its token](../security/threat-model.md)
  (T-01 to T-06, T-08, T-10 to T-12, T-14, T-15).
- Roadmap: "Authentication (v0.3.0)" in the [roadmap](../../roadmap.md).
