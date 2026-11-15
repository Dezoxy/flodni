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
