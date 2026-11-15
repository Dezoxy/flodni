# Security policy

Flodni is a torrent engine behind a FUSE virtual filesystem, run by its
maintainer at home. Its configuration file holds tracker and API
credentials; it holds no other personal data.

## Reporting a vulnerability

Report it privately through GitHub: **Security** tab, then **Report a
vulnerability**. Please do not open a public issue. Expect an answer within
a week.

In scope: this repository's code, the container image, CI workflows and
anything that could leak a secret. Issues in the vendored
`anacrolix/torrent` are best reported to that project as well.

## Known limits

Authentication on the control server (`metrics_port`, default 9080) is
opt-in. Set `api_token` in `config.json` or `FLODNI_API_TOKEN` and every
route except the static panel page (`GET /control`) and `GET /api/health`
(status words only, no paths or values; its `warnings` array tells an
unauthenticated caller whether play webhooks are arriving) needs
`Authorization: Bearer <token>`; the two webhook routes also accept
`?token=<token>`, because Plex cannot send headers. Unset, the
API is open: bind it to loopback or a private network, never to an address
other people can reach. Even then, `POST /api/config` refuses to set or change
`api_token` and `POST /api/restart` is refused (403), so a client cannot pick
or erase the next start's token; set it through the environment or
`config.json`. GoStorm's `POST /shutdown` still stops the process without a
token.

The same token also covers GoStorm's own server on port 8090 (torrent
control, settings, `/stream`, shutdown): every route except `GET /echo`, the
readiness probe, needs the `Authorization: Bearer` header, and there is no
query-token form. External clients of 8090 must send it when it is set;
flodni's own calls to the engine already do. With no token set, 8090 is open
as before, so keep it off any network others can reach. On an open port
both servers answer 403 to a request whose `Origin` header names a different
hostname than its `Host` (scheme and port are ignored, because a TLS tunnel
delivers plain http with the original `Host`; `Origin: null` is refused), so
a page in a browser on the network cannot send a cross-site POST. Requests
without `Origin` (scripts, Plex, and the GET navigations and `<img>` loads a
browser sends without one) pass, so some GETs on 8090 (`/stream?link=` adds a
torrent, `/play/:hash/:id` wakes a stored one) stay open to a page, and so does
DNS rebinding. A page on the same hostname but another port is not stopped. A
reverse proxy in front of the control port must keep the `Host` header, or the
panel's saves get 403. The engine sends no CORS headers; `/shutdown` and the
Plex webhook take POST only. GoStorm's server
bounds header reads (10 s) and idle connections (120 s); it sets no read or
write timeout, because streams and uploads are long-lived.

Flodni does not provide TLS, so the token travels in clear text: put the
control port behind a TLS proxy on any untrusted network. The token is
compared in constant time and never logged, but there is no rate limiting or
lockout.

The full analysis (assets, trust boundaries, threats and the gaps above) is
in the [threat model](docs/architecture/security/threat-model.md).

## Tracker passkeys

A private tracker's announce URL carries the account's passkey. Stubs no longer
store it: every new stub's `magnet` is the bare info hash. Stubs written by
older versions may still hold the request's magnet with its trackers, mode 0644.
Run `flodni scrub-stubs --dry-run <source_path>` to count them and
`flodni scrub-stubs <source_path>` to rewrite them, as the daemon's user (see
"Stub contents and the stub scrub" in the README for the flags, the exit codes
and the stubs it leaves alone). Each rewrite keeps the stub's mode, owner, mtime
and atime, set on the new file before it replaces the stub.

What this does not cover:

- Copies of the library directory made before the scrub (backups, snapshots)
  still hold the old magnets.
- The engine's own stored record of a torrent (`config.db`) keeps its trackers,
  because a wake needs them. After the scrub it is the only copy of a private
  torrent's trackers: back it up. Its file mode follows the umask.
- The engine can print a link or tracker URL in its own log lines. Redacting
  those is separate work and not done here.

## Foreign trackers in private torrents

A private torrent's own `.torrent` file can carry a second tracker that is not
the torrent's (a cross-upload, with someone else's key). The engine stores it in
the torrent's record in `config.db` and announces to it on every wake, which
sends the info hash to a tracker outside the torrent's own. Find them with
`flodni trackers audit` and remove a host with
`flodni trackers remove-host <host>` (try `--dry-run` first). Stop flodni and copy
`config.db` before either; see "Foreign trackers in stored records" in the README
for the flags and the approximate own-domain rule. The commands print hosts and
info hashes only, never a tracker URL or passkey. Removal is owner-triggered; the
daemon never does it by itself, and a removed host stays removed unless the
torrent is added again with it.

## Secrets

**Logs.** A private tracker's announce URL is the user's account, and logs
get shared when asking for help, so tracker credentials never reach a log.
`internal/redact` is the one redactor. It works on whole strings (errors from
`net/http` embed the full URL), keeps `scheme://host[:port]` of every
http(s), udp and ws(s) URL and replaces userinfo, path and query with
`/<redacted>`, replaces the `tr=` and `xs=` values of a magnet by the host of
their URL, replaces a `torrs://` link whole, and masks stray
`passkey=`-style values. It is applied at the add API (the link and its parse
and add errors), the wake and activation failure lines, the discovery boost's
announce failures and, through the engine client's logger, every line the
engine writes, including the debug lines `EnableDebug` turns on. The Plex
library refresh puts `X-Plex-Token` in its request URL, and `net/http` errors embed
the URL, so that error is redacted where it is built and again where it is logged.
The engine's `Client config:` line is redacted too (`BlockListURL` can hold a key).
The tracker scrape has its own cleaner for what a tracker sends back. Not covered: lines
of code that does not go through these paths, such as a new `log` call that
prints a link directly; the redactor is a net, not a license to log URLs.

The repository holds no secrets; `config.json` is ignored by git and
`config.json.example` carries placeholders. `config.json` holds `api_token` and the Plex token in clear text: a save from the panel keeps the file's existing permission bits (a new file is created 0600), so keep it unreadable to other users, or pass the secrets as `FLODNI_API_TOKEN` and `FLODNI_PLEX_TOKEN` instead. CI runs gitleaks on the full
history, and GitHub secret scanning with push protection is enabled.
