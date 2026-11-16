# 4. Scope the API tokens and the panel sign-in

Date: 2026-11-16

## Status

Proposed on 2026-11-16, for v0.3.0. Nothing in this record is built.

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
`newServer`) take the token as a string and call one `gate`. That is the right
placement. The gate runs on the raw `http.Handler`, before the control `mux`
and before gin, so one scope lookup there covers both ports and no route can
register itself around it. Both servers get the Host allowlist from the same
option.

Properties the design keeps or changes:

| Today | In this record |
|---|---|
| Open routes: `GET /control`, `GET /api/health` on control; `GET /echo` on the engine (`controlOpen`, `gostormOpen`) | Same, plus a cheap `GET /api/live` (see "Open routes") |
| `?token=` on `/plex/webhook` and `/webhook` by exact path (`controlWebhook`) | Same two routes, but only for a token with the `webhook` scope, and stripped before any handler or log sees it |
| Constant-time compare of the raw token, length checked first | SHA-256 of the presented token, constant-time compare against every stored hash |
| With a token set the Origin and Sec-Fetch-Site checks are off, because the bearer header is the gate | Still off for a bearer request; **on** for a request authenticated by a cookie or a proxy header, because those are ambient credentials a browser attaches by itself |
| The engine's handlers know only `api_token_enforced` (`TokenEnforcedKey`), used by `/stream` to refuse adds on an open port | Unchanged; set when any token or the internal secret is enforced |
| `app.controlToken` decides the fail-closed routes (`restart`, `confirm`, `api_token` writes) | Same switch, now "any auth enforced" |

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
| POST | `/control/login` | does not exist | session (new) |
| POST | `/control/logout` | does not exist | session (new) |

Engine server (port 8090). `POST /settings` and `POST /torrents` carry an
`action` in the JSON body; the scope depends on it.

| Method | Path | Action | Today | Scope |
|---|---|---|---|---|
| GET | `/echo` | | open | open |
| POST | `/settings` | `get` | token | `engine` |
| POST | `/settings` | `set`, `def` | token | `admin` |
| POST | `/torrents` | `list`, `active`, `get` | token | `engine` |
| POST | `/torrents` | `add`, `set`, `rem`, `drop`, anything else | token | `admin` |
| POST | `/torrent/upload` | | token | `engine` |
| POST | `/cache` | `get` is the only action | token | `engine` |
| GET, HEAD | `/stream`, `/stream/*fname` | | token | `engine` |
| GET, HEAD | `/play/:hash/:id` | | token | `engine` |
| GET | `/download/:size` | | token | `engine` |
| GET | `/storage/settings` | | token | `engine` |
| POST | `/storage/settings` | | token | `admin` |
| POST | `/shutdown`, `/shutdown/*reason` | | token | `admin` |

Counts (rows, a method or an action counts once): 52 in all.

| Scope | Control | Engine | Total |
|---|---|---|---|
| `webhook` | 2 | 0 | 2 |
| `metrics` | 4 | 0 | 4 |
| `read` | 6 | 0 | 6 |
| `library` | 4 | 0 | 4 |
| `scrape` | 2 | 0 | 2 |
| `engine` | 0 | 14 | 14 |
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
- The engine's `/stream`, `/play`, `/download` and `/storage/settings` GET are
  `engine`. A media player cannot send a header, so with a scoped
  configuration those routes are for scripts; that is how `/stream` behaves
  with a token set today.
- `/control` stays open (static page, no data) and `GET /echo` stays open
  (a version string). The owner's list has only `/api/health` and one
  liveness route; both extras are needed, see "Open routes".

## Decision drivers

- One maintainer, one deployment: the smallest model that limits a leaked
  token, not a policy engine.
- With no token configured, Flodni must behave exactly as v0.2.6, open-port
  hardening included.
- Fail closed: a malformed or unreadable configuration must stop the start,
  never degrade to open mode.
- A new route must not be able to ship open by accident.
- No dependency without need (`CLAUDE.md`).
- Hard rule 1: no secret in the repository, in a log or in a metric label.

## Considered options

### A. Scoped tokens in the existing gate, a hashed token file (chosen)

Extend `internal/apiauth` with a route-to-scope table and a token store. One
file holds named tokens with scopes and SHA-256 hashes. Panel sign-in through
an optional verified proxy JWT, or a login cookie.

- Pro: one place enforces both ports; no new process; the change is additive
  and opt-in; stored hashes mean the file can be mounted read-only and backed
  up without leaking a credential.
- Con: a route table to keep correct (mitigated by a test, below); rotation is
  a file edit and a restart.

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
- Con: Flodni serves no TLS (T-04); certificate issuance and distribution to
  the media server and the bridge is more operational weight than the threat
  needs. Not rejected for good: a TLS proxy can terminate it in front.

## Decision

Option A. Scoped, named tokens held as hashes in a file; both servers enforce
them through one extended `apiauth` gate; an optional verified-JWT mode and a
login cookie let a person use the panel without a token in the browser.

### Scopes

| Scope | Grants |
|---|---|
| `webhook` | the two play webhooks (`POST`), the only routes that may take `?token=` |
| `metrics` | `GET /metrics*` on the control port |
| `read` | health detail, startup report, sessions, library list, file and audit |
| `library` | library add, remove, confirm and inspect |
| `scrape` | `POST /api/torrents/scrape` and its status |
| `engine` | the engine's torrent list, active and get, `/torrent/upload`, settings get, streaming routes |
| `admin` | everything above, plus config writes and reads, restart, evict, engine settings set, torrent add and removal on the engine, shutdown |

`admin` implies every other scope. No other scope implies another: a `library`
token cannot read sessions unless it also holds `read`. Scopes are a flat set;
there is no inheritance to reason about beyond `admin`.

### The route-to-scope map is the single source of truth

One table in `internal/apiauth` (method, path pattern, optional engine
action, scope), written once, used by both gates, and by the metric label.
Three rules make it hold:

1. **Unknown routes need `admin`.** A route absent from the table is
   authorized as `admin`, so forgetting an entry costs the caller a refusal,
   never exposure. A method no row lists for a known path takes that path's
   strictest scope (so a wrong-method probe reveals nothing without it).
2. **A test fails on any route registered without a row.** The engine uses
   gin, whose `Engine.Routes()` lists everything; the test builds `newRoute()`
   and checks each route against the table. The control mux cannot be listed
   (`http.ServeMux` has no enumeration), so the mux is built through a small
   registrar that records each pattern as it registers (`handle(pattern,
   scope, handler)`); the test builds the real mux and compares. The
   registrar is a refactor of the inline block in `main.go`, one commit, no
   behaviour change, and it is the only part of this record that touches code
   outside the gate.
3. **A golden copy catches silent widening.** The matrix test derives its
   expectations from the table, which would pass whatever the table says. A
   reviewed `testdata/route_scopes` file holds the same rows by hand; any
   change to the table fails until the golden is edited in the same PR, so a
   scope change is always a visible diff.

Engine routes with an `action` need the body. The gate reads at most 64 KiB
of a `POST /settings` or `POST /torrents` body, parses `action`, restores the
body for the handler, and refuses a body it cannot parse (400, counted as
`invalid`) or an oversize one (413). A missing or unknown action is `admin`.
This reads the body twice in a process that already reads it once (the
engine's request logger); it happens only in scoped mode and only on those two
routes. `/torrent/upload` is multipart and is classified by path alone, so the
upload body is never buffered by the gate.

### The token file

JSON, because the config already is:

```json
{
  "tokens": [
    {"name": "plex-webhook", "scopes": ["webhook"],            "sha256": "<64 hex>"},
    {"name": "bridge",       "scopes": ["engine"],             "sha256": "<64 hex>"},
    {"name": "automation",   "scopes": ["library", "read"],    "sha256": "<64 hex>"},
    {"name": "prometheus",   "scopes": ["metrics"],            "sha256": "<64 hex>"},
    {"name": "operator",     "scopes": ["admin"],              "sha256": "<64 hex>"}
  ]
}
```

- **Location.** `auth_tokens_file` in `config.json`, or
  `FLODNI_AUTH_TOKENS_FILE` (the variable wins, like the other secrets). The
  file is meant to be mounted read-only; Flodni never writes it. The path is
  a startup input only: `POST /api/config` cannot change it (403, even with
  `admin`), and a panel save keeps the file's value, as it keeps `api_token`
  today.
- **Hash.** SHA-256 of the token, hex. Tokens are generated, 256 bits of
  randomness; at that entropy the hash only has to be one-way and fast. A
  slow KDF (argon2, bcrypt) exists to make guessing a low-entropy human
  password expensive; against 2^256 it buys nothing, and it would cost a KDF
  run on every webhook and every metrics scrape.
  Consequence to state plainly: the hash is only as strong as the token, so
  the only supported way to make one is `flodni auth new-token`. A
  hand-hashed short password is outside the model.
- **Loader.** Strict: unknown JSON fields, a name that is empty or repeated, a
  scope not in the list, an entry with no scopes, a `sha256` that is not 64 hex
  characters, more than 64 tokens, and a file with zero tokens are each a
  refusal to start. A configured path that is unreadable or unparsable is a
  refusal to start, never open mode: "I pointed Flodni at a token file" is
  the owner's statement that auth is wanted. A file that is group- or
  world-writable logs a warning; it does not refuse, because container mounts
  show arbitrary modes.
- **CLI.** `flodni auth new-token --name NAME --scopes a,b` prints the token
  once and the JSON entry to add, and touches no file. The token looks like
  `fl_` plus 43 base64url characters (32 random bytes from `crypto/rand`);
  the prefix lets a secret scanner recognise a leaked one. It runs offline,
  starts no daemon, and prints the token to stdout only. Same pattern as
  `flodni trackers` and `flodni scrub-stubs`.
- **Rotation.** Edit the file (add the new entry, deploy the new token to the
  caller, remove the old entry) and restart. There is no reload signal: one
  code path that reads the file, once, at start, and the restart already
  logs everyone out of panel cookie sessions, which is what a rotation wants.

### Lookup

Hash the presented token once, then compare it to every stored hash with
`subtle.ConstantTimeCompare`, with no early exit, keeping the matched entry
through `subtle.ConstantTimeSelect`. The compared values are fixed-length
hashes, so no length check leaks anything, and the loop's cost does not depend
on which entry matched. With at most 64 entries the loop is negligible. (An
early exit would leak at most which entry matched to a caller who already
holds a valid token; the full loop costs nothing, so it is done.)

### Transport

- `Authorization: Bearer <token>` on every route.
- `?token=` only on a route whose scope is `webhook` (today's two paths), and
  only for a token holding `webhook` (or `admin`). On any other route in
  scoped mode a request that carries a `token` query parameter is refused
  with reason `query_not_allowed`, even when it also carries a valid header:
  a client that puts credentials in URLs is told so, rather than silently
  working. The legacy-token mode keeps today's behaviour (ignored there).
- **Stripped before anything sees it.** After authentication the gate removes
  `token` from `r.URL.RawQuery` and `r.RequestURI` before it calls the
  handler, so no downstream log, error text or handler reads it.
  Independently, `internal/redact` gets a rule for `token=` (and
  `access_token=`) alongside its existing `passkey=` net, so a line built from
  a URL elsewhere is masked, and the engine's `WebLogger` (`internal/gostorm/log`),
  which prints `RawQuery`, is routed through it. The control server logs no
  query today (checked: its request logging and refusals print the path only);
  a test pins that, so a future logger cannot reintroduce it. What a proxy in
  front logs is out of Flodni's hands (T-05 stays).

### The engine port

The same gate wraps 8090 with the same tokens: `apiauth.GoStorm` takes the
same store. No proxy and no per-router change is needed; the gate runs before
gin, and gin's `Routes()` feeds the completeness test.

Flodni itself calls its own engine (the library client and the panel's
engine-settings proxy, both in `main.go`, both now sending `gc().APIToken`).
In scoped mode the file holds hashes, so Flodni cannot send a stored token. It
generates a random 256-bit **internal secret at each start**, in memory only,
and sends that on those two paths. The engine gate accepts it as a token with
the `engine` and `admin` scopes, on 8090 only; the control gate never accepts
it. It is never logged, written or returned. This covers an embedded engine
on `gostorm_url` (the default, `http://127.0.0.1:8090`). An engine reached on
another host would need a stored credential Flodni can read; scoped mode does
not support that split (open question 6).

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
  registered in every mode, so it is also an additive v0.2.6 route.
- `GET /api/health` stays open as today. `flodni healthcheck` uses it.
- `GET /control` stays open: the static page holds no data, and in the cookie
  mode a person cannot sign in to a page they cannot load. A page that loads
  and then shows a login form is the only workable design.
- `GET /echo` on 8090 stays open for the reason above.

### The panel behind an identity-aware proxy

Two ways in, tried in this order per request, first match wins:

1. **Bearer header.** An `Authorization: Bearer` that is present is the
   credential. If it is wrong the request is refused; it never falls through
   to a cookie or a JWT.
2. **Session cookie** (below).
3. **Proxy JWT**, when configured.

A request authenticated by 2 or 3 is subject to the Origin and Sec-Fetch-Site
refusals (`crossOrigin`, `crossSite`), hostname-only exactly as today. That is
the "keep the Origin check" requirement made precise: a bearer header cannot
be attached by a cross-site page, but a cookie and a header an identity
proxy adds on every request both can ride along on a forged form post. For
the proxy case the check matters most: the proxy adds the assertion to every
request that reaches it, whoever's page caused it. `SameSite=Strict` on the
cookie is the second layer, not a replacement.

**Proxy JWT mode (optional).** Configuration in `config.json`, all required
when the block exists:

```json
"auth_jwt": {
  "issuer":   "https://<team>.cloudflareaccess.com",
  "audience": "<application audience tag>",
  "jwks_url": "https://<team>.cloudflareaccess.com/cdn-cgi/access/certs",
  "header":   "Cf-Access-Jwt-Assertion"
}
```

(The values above show the shape for Cloudflare Access; any provider that
signs an assertion and publishes a JWKS fits, such as an oauth2-proxy
deployment. `<team>` and the audience tag are the deployment's own.) The mode
is off when the block is absent. A block present but incomplete, or a
`jwks_url` that is not `https`, refuses to start.

Verification, in order, any failure refusing with the reason in parentheses:

1. The header is present and has three base64url segments (`jwt_missing`,
   `jwt_malformed`).
2. The protected header's `alg` is `RS256` or `ES256` and nothing else
   (`jwt_alg`). `none`, `HS*`, `PS*`, `ES384` and an absent `alg` are refused.
   The algorithm is chosen by the key, not trusted from the token: an RSA key
   verifies only `RS256`, a P-256 key only `ES256`. This is what stops the
   classic confusion where an `HS256` token is "signed" with the public key.
3. `kid` selects a key from the cached set (`jwt_unknown_kid` when none
   matches, after one refresh; see below).
4. The signature verifies (`jwt_signature`).
5. `iss` equals `issuer` exactly (`jwt_issuer`); `aud` (a string or an array)
   contains `audience` (`jwt_audience`); `exp` is present and in the future
   (`jwt_expired`); `nbf`, when present, is not in the future
   (`jwt_not_yet_valid`). Clock skew allowed: 60 seconds each way.
   (`jwt_claims` for a missing or mistyped claim.)

A verified token grants `admin`, on the control port only. A proxy header on
8090 is ignored: the engine port is for scripts and is not behind the
proxy. The audience is what ties an assertion to this application; the
operator must give Flodni its own, not a shared one, or any user the proxy
admits to another application is an admin here. Per-user restriction beyond
the proxy's own policy (an allow-list of emails) is not built (open question
4).

**Keys.** Fetched over HTTPS from `jwks_url` with a 5-second timeout, 64 KiB
body cap, no redirects to another host, and cached in memory. A token whose
`kid` is unknown triggers one refresh; refreshes are rate-limited to one per
minute, so a caller sending random key ids cannot make Flodni hammer the
provider. A successful fetch replaces the set; keys otherwise expire after 24
hours. A failed fetch keeps the last good set (a provider outage does not log
the operator out within the day) and backs off 1, 2, 4 minutes up to 15.
Only RSA and P-256 keys are loaded.

**When the JWKS is unreachable at start or later:** JWT authentication fails
closed with `jwt_keys_unavailable` for any token whose key is not already
cached; tokens and the cookie are unaffected, and the daemon starts anyway.
The JWKS is never fetched at start in a way that blocks it. The startup
report shows `jwt: true`; the refusal metric shows the outage.

**Dependency.** Recommendation: the Go standard library plus a verifier of
about 150 lines in `internal/apiauth` (JWS compact parse, `crypto/rsa`
`VerifyPKCS1v15`, `crypto/ecdsa` `VerifyASN1` after converting the JWS's raw
`r||s`, `encoding/json` for the header, claims and JWKS). The alternative is
`github.com/golang-jwt/jwt/v5`.

- For `golang-jwt/jwt/v5`: audited, widely used, handles claims and
  algorithm allow-listing (`WithValidMethods`), fewer lines of our own
  crypto-adjacent code.
- Against: it does not fetch or cache a JWKS, handle `kid` refresh, or parse
  JWK, so the part that needs the most care (key management and the fetch)
  is ours either way; the library saves the 100-odd lines that are the most
  testable. It is a new module in `go.mod`, to track in Renovate and to
  audit on each bump, in a repository whose rule is no dependency without
  need, for a verifier that must accept exactly two algorithms and nothing
  else.
- The risk of the stdlib route is a hand-rolled verifier with a subtle bug.
  The mitigation is that the primitives are the standard library's and the
  failure matrix in the test plan (including `alg: none`, an `HS256` token
  keyed with the public key, a truncated signature and an unknown `kid`) is
  the review. If the owner prefers a library to hand-written verification
  code, `jwt/v5` is the one; the rest of this record does not change.

**Session cookie (the generic fallback).** For a deployment with no
identity proxy, or for the person who prefers it:

- `POST /control/login` with `{"token":"..."}`. The token must hold `admin`;
  one without answers 403, a wrong one 401. Both count in the refusal metric
  (`invalid`, `scope`). Success sets the cookie and answers 204.
- The cookie `flodni_session` holds `base64url(name|expiry)` and an
  HMAC-SHA256 tag. `HttpOnly`, `SameSite=Strict`, `Path=/`, an absolute expiry
  of 12 hours (no sliding window, so a stolen cookie has a bound).
  `Secure` is set when the request arrived over TLS, when the proxy said so
  (`X-Forwarded-Proto: https`; a spoofed value only makes the cookie
  unusable over http, which hurts the spoofer), or when `auth_panel_https` is
  true in the configuration.
- The HMAC key is **random at each start** (32 bytes from `crypto/rand`, in
  memory). A restart logs every panel session out. Deriving it from the token
  file would make sessions survive a restart, but also survive a revoked
  token until the file changes, and would tie the key to a file that other
  parties may read; the restart that rotates tokens is exactly when sessions
  should end. A cookie names its token entry and is re-checked against the
  store on each request, so a token removed from the file and picked up by a
  restart never has a live session.
- `POST /control/logout` expires the cookie (`Max-Age=0`). A stateless cookie
  cannot be revoked server-side before it expires; the 12-hour bound and the
  restart are the revocation, and a stolen cookie is accepted as a residual
  risk (below).
- CSRF follows from `SameSite=Strict`, the Origin and Sec-Fetch-Site
  refusals, and the fact that no state-changing control route accepts GET.
  The login route itself is cross-site-protected the same way.
- The panel (`settings.html`) loses the token field and the local-storage
  token for this mode: a 401 shows a sign-in prompt that posts to
  `/control/login`; the browser's own same-origin fetches carry the cookie.
  The bearer-in-local-storage path stays as it is for the legacy token.
  Neither login nor logout exists in open mode.
- No lockout or rate limit on `/control/login`: a generated token cannot be
  guessed, and an owner-chosen legacy token is the weakness (T-08, unchanged).
  The refusal counter is the alert.

### Metrics and logs

- `flodni_auth_refusals_total{route,scope,reason}`. `route` is the route
  **pattern** from the table, not the raw path (`/play/:hash/:id`, never a
  hash), and `unmatched` for a path in no row, so cardinality is bounded by
  the 52 rows plus one. `scope` is the scope the route requires, not the
  presented token's, so no token name or identity can reach a label. `reason`
  is one of `missing`, `invalid`, `scope`, `query_not_allowed`, `session`,
  `jwt_missing`, `jwt_malformed`, `jwt_alg`, `jwt_unknown_kid`,
  `jwt_keys_unavailable`, `jwt_signature`, `jwt_issuer`, `jwt_audience`,
  `jwt_expired`, `jwt_not_yet_valid`, `jwt_claims`. The existing Host,
  Origin and Sec-Fetch-Site refusals use the same family with `host`,
  `origin` and `cross_site`, since they leave the same trace. Emitted by the
  daemon's Prometheus text writer (`metrics_prom.go`); `apiauth` reports each
  refusal through an observer option so it imports no metrics code.
- A refusal is logged as today: method, path (never the query), remote
  address, and now the reason. It never contains a token, a cookie or a
  header value. A success on a mutating route (library writes, config,
  restart, engine settings) logs the **token name** at INFO: a name is a
  label chosen by the owner, not a secret, and it closes the repudiation gap
  in T-14 for the writes that matter.

### Compatibility

| Configuration | Behaviour |
|---|---|
| No `api_token`, no tokens file | **Exactly v0.2.6**: both servers open, the open-port hardening in force, no new refusal path. The only additions are `GET /api/live`, the metric (zero) and the `auth` section of the startup report. |
| `api_token` only | Today's behaviour: one token, every scope, `?token=` on the two webhook routes, no hashing change in what a client sees. Reported as `legacy_token`. |
| `auth_tokens_file` only | Scoped mode: deny by default. |
| Both | Scoped mode; `api_token` is treated as a token named `legacy` with the `admin` scope; one WARN at start and in the report (`legacy_admin: true`). |

On the both-set case, recommended over refusing to start: the point of a
migration is that the existing callers (the media server's webhook URL, the
bridge, automation) keep working while each gets its own scoped token, one at a
time. A refusal forces every caller to change in the same restart. Treating
`api_token` as `admin` adds no privilege (it already had everything), keeps
the `?token=` webhook URL working, and the cost is that an owner who forgets
to remove it keeps a plaintext all-power token; the WARN, the report field
and the roadmap's last step ("remove it") are the guard. Refusal is the
other defensible choice and the owner may take it (open question 1).

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
  "tokens": 5,
  "scopes": ["admin", "engine", "library", "metrics", "read", "webhook"],
  "jwt": false,
  "legacy_admin": false
}
```

`mode` is `open`, `legacy_token` or `scoped`; `tokens` counts entries
(`legacy_token`: 1; `open`: 0); `scopes` is the sorted union in use (all seven
for the legacy token); `jwt` is whether the mode is configured. It carries no
name, no hash, no path and no key. A contract test pins that every key the
v0.2.6 report had is still present with the same type, and that the line and
the endpoint body are the same bytes in all three modes.

### Not in scope

- Rate limiting, lockout, or a server-side session store.
- Per-user identity beyond a token name; user accounts of any kind.
- TLS in Flodni (T-04 stays; a TLS proxy remains the answer).
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
- **The engine action peek** reads a body in the gate on two routes, in scoped
  mode only.
- **A stateless cookie cannot be revoked** before its 12 hours end or the
  next restart.
- **A hashed token cannot be recovered**: a lost token is replaced, not
  looked up.
- **The scoped model has more than one way to be misconfigured**, which the
  loader's strictness and the startup report are there to expose.

## Risks

- **Fail open on a bad file.** The worst possible bug here. Mitigated by the
  loader contract (any problem with a configured file stops the start) and a
  test that every malformed-file case ends in an error and never in `open`.
- **An unlisted route ships open.** Cannot happen: unknown means `admin`.
  The reverse (a route that should be reachable by a scoped token but is
  not) shows up as a 403 in the matrix test, not in production.
- **A JWT accepted for the wrong application.** Mitigated by exact `iss`,
  `aud` and a pinned algorithm per key type; the operator's duty to use a
  dedicated audience is in the documentation.
- **The JWKS fetch as a probe.** The URL is configuration only (an `admin`
  cannot change it through the API), HTTPS only, capped, rate-limited.
- **The internal secret leaks.** In memory, never sent to the control port,
  accepted on 8090 only; a process with memory access already owns the
  daemon.
- **Private torrents.** No change: the gate decides who may call the add,
  upload, inspect and library routes; it adds no peer discovery. The privacy
  tests stay as they are. Scoping `engine` away from `admin` means a bridge
  token can upload a torrent but not add one by link, which is the narrower
  reach the bridge needs.

## Threat model changes

Rows that change in the implementing PR (this record edits none of them):

| Row | Change |
|---|---|
| T-01 | Spoofing: the compare is now over hashes of every entry; a token is bound to scopes. A 403 for insufficient scope joins the 401. |
| T-02 | The default is still open, and still documented. The gap "no mitigation other than a token" gains the scoped mode; the open-port refusals remain for the unconfigured case. The GoStorm `POST /shutdown` gap closes in scoped mode (it needs `admin`). |
| T-03 | A stored token is no longer plaintext. A passive reader on the wire still sees a bearer token (T-04); the file on disk does not hold one. |
| T-05 | The leaked query token is a `webhook`-scope token, with no config write or restart reach; stripped from every Flodni log. A proxy's own log is still out of scope. |
| T-06 | A separate webhook credential now exists, and it is the only one that rides in a URL; a forged event needs it. |
| T-10, T-11 | Config and library writes need `admin` and `library`; the holder of a `webhook`, `metrics`, `read` or `scrape` token cannot do either. |
| T-12 | Health stays open; `GET /api/live` is the cheap probe that leaks no warning. |
| T-14 | Per-caller identity: each mutating success logs a token name. Still no audit log. |
| T-15 | The Origin check applies to cookie and proxy-header requests. Residual unchanged. |
| T-08 | Residual unchanged (no rate limit); the refusal metric makes guessing visible. |
| New rows | (a) the stolen-token blast radius per scope, as the table above; (b) the proxy-JWT path: header trust, audience, JWKS outage and fetch; (c) the session cookie: theft, no server-side revoke, CSRF. |

What remains after this change: no TLS (T-04), no rate limit (T-08), the
Host and Origin gaps of T-15 for an unconfigured deployment, a legacy token
that is a human-chosen secret if the owner keeps it, a stolen cookie valid up
to 12 hours, and an `admin` token or an admitted proxy user being able to do
everything `admin` does. Secrets on disk: the token file holds hashes only, so
reading it yields no credential, but a read-write mount would let the same
reader add one; hence read-only.

## Migration: small pull requests

Each step is one PR off `main`, behaviour-preserving in open mode, with its
tests (test first, shown red, then green).

1. **Route table and registrar.** The scope table, the registrar for the
   control mux, `GET /api/live`. No enforcement. Tests: completeness (gin and
   mux), the golden file, `/api/live`, open mode byte-for-byte unchanged.
2. **Token file, loader, CLI, report.** `auth_tokens_file`, the strict loader,
   `flodni auth new-token`, the startup `auth` section, the internal secret.
   Tests: every malformed-file case refuses to start; `new-token` round trip;
   report contract (line equals endpoint, old keys intact) in all three modes.
3. **Scoped gate on both ports.** Lookup, scope check, the engine action
   peek, query tokens on webhook routes only and their stripping, the refusal
   metric, legacy-as-admin, the fail-closed switches. Tests: the matrix,
   query redaction, 8090 enforcing the same tokens, constant-time lookup
   shape, metric labels bounded.
4. **Redaction.** `token=` in `internal/redact`, the engine's `WebLogger`
   routed through it, a guard that the control server logs no query. Tests:
   a line with a `token` URL, the engine log with a query token, refusal and
   success logs, the metrics text.
5. **Proxy JWT.** Verifier, JWKS cache, `auth_jwt`. Tests: the failure
   matrix, the outage, and bearer and cookie still working during it.
6. **Panel cookie.** `/control/login`, `/control/logout`, the panel's sign-in
   prompt, the Origin check on ambient credentials. Tests: attributes,
   expiry, tamper, restart invalidates, CSRF refusals, `Secure` cases.
7. **Docs, threat model, ADR to Accepted.** README, `SECURITY.md`, the rows
   above, an end-to-end variant (scoped start, refused webhook on `admin`
   path). Run on the maintainer's Docker host, not in CI.

## Test plan

- **Scope matrix.** For every row of the route table on both ports, against
  each of: no credential, a wrong token, each of the seven scopes alone, a
  two-scope token, the legacy token, the internal secret (engine only), and
  a cookie. The expected result is written by hand in the golden file, not
  computed from the table under test. Handlers are stubs that record being
  reached, so a refused request is shown never to reach one.
- **Completeness.** gin `Routes()` and the mux registrar each against the
  table; a route added without a row fails the build of the test.
- **Query tokens.** Accepted on the two webhook routes and stripped from what
  the handler sees; refused with `query_not_allowed` everywhere else and on
  8090; a captured log buffer, the engine log, the refusal and success logs,
  the redactor output and the metrics text never contain the token.
- **JWT.** Valid RS256 and ES256; wrong audience (string and array form);
  wrong issuer; expired; not yet valid; skew inside and outside 60 s; bad
  signature; tampered payload; missing header; `alg: none`; `HS256` signed
  with the public key; an unsupported curve; unknown `kid` (one refresh,
  then refusal, and no second fetch within a minute); JWKS unreachable
  (fail closed, last good set kept, backoff); oversize and non-HTTPS JWKS;
  the header on 8090 ignored; a verified JWT on a cross-origin POST refused.
- **Loader.** Each malformed file refuses to start and never reaches open
  mode: bad JSON, unknown field, duplicate name, empty scopes, unknown scope,
  short or non-hex hash, zero tokens, unreadable path, over 64 tokens.
- **Constant time.** The lookup visits every entry regardless of the match
  (an observed loop count), and compares fixed-length values.
- **Compatibility.** With neither setting, the v0.2.6 test files
  (`apiauth_test.go`, `origin_test.go`, `hosts_test.go`, the daemon's
  open-port tests) pass unmodified; the startup contract holds in every mode.
- **Cookie.** Attributes, expiry, a tampered tag, a restart invalidating it,
  the `Secure` cases, logout, a token without `admin` refused at login.
- **Race.** `go test -race` over the gate with concurrent requests and a
  JWKS refresh in flight.

## Open questions

Recommendations in bold; the owner may override any.

1. **`api_token` and a tokens file together.** **Treat `api_token` as an
   `admin` token named `legacy`, with a WARN and `legacy_admin: true`.**
   Alternative: refuse to start. Staged migration versus a forced flag day.
2. **Is `/control` open?** **Yes, static page only.** A page that needs a
   credential to load cannot ask for one.
3. **`GET /echo` and a liveness route.** **Keep `/echo` open and add
   `/api/live`.** The health route's engine check and the image's healthcheck
   depend on the first; the second keeps the probe cheap and quiet.
4. **Restrict the JWT to named users.** **Not in v0.3.0.** The proxy's own
   policy is the allow-list; an email or subject allow-list in Flodni is a
   second place to forget to update. Revisit if a shared audience is ever
   unavoidable.
5. **Cookie signing key from the file?** **No: random at start.** Sessions end
   on restart, which is when a rotation happens.
6. **An engine on another host.** **Not supported in scoped mode.** The
   internal secret exists only in this process; the legacy token keeps working
   against a remote engine because it is plaintext on both sides. A stored
   plaintext `engine`-scope credential for Flodni's own calls would work but
   is a plaintext secret on disk, the thing this record removes.
7. **`POST /torrents` actions for the bridge.** **`list`, `active` and `get`
   are `engine`; `add`, `set`, `rem`, `drop` are `admin`.** The bridge adds by
   `/torrent/upload` or the library API; a plain add by link needs `admin`.
8. **Log the token name on mutating successes.** **Yes**, names only.
9. **Count Host, Origin and cross-site refusals in the same metric.**
   **Yes**, three extra `reason` values; they are refusals of the same gate.
10. **Reload on a signal.** **No.** One read at start; restart to rotate.

## Related

- Requirements: none recorded.
- Architecture views: Daemon (control server, engine server).
- Other ADRs: [3. Make the file lifecycle explicit in the library API](0003-file-lifecycle-api.md),
  whose Auth section this extends.
- Threat model: [control API and its token](../security/threat-model.md)
  (T-01 to T-06, T-08, T-10 to T-12, T-14, T-15).
- Roadmap: "Authentication (v0.3.0)" in the [roadmap](../../roadmap.md).
