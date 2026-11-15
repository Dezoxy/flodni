## Threat model: control API and its token

### Scope and status

This models the network surface of the daemon: the control server, the
engine's server, the FUSE mount and the play webhook. It is a STRIDE pass
over what the code does today (observed implementation, read from the
files named below). It does not cover the torrent swarm, peer discovery or
the privacy of private torrents; that is hard rule 3 in `CLAUDE.md`, pinned
by tests. Nothing here is a measurement or a penetration test.

The token is opt-in: with `api_token` and `FLODNI_API_TOKEN` both empty,
`internal/apiauth` returns the handler unchanged and every route is open.
The design assumes a single maintainer on a trusted network, not a hostile
one.

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
| Maintainer | Holds the token; uses the panel and automation. Trusted. |
| Media server | Posts play webhooks; reads the mount. Trusted, but cannot send headers on webhooks. |
| LAN client | Can reach the published ports; holds no token. Untrusted when a token is set. |
| Browser on the network | A web page the maintainer or a household member opens, on any site. Holds no token and sends requests from their machine to the open ports, so it reaches a loopback or private address the page's own server could not. Sends an `Origin` header on cross-site requests. |
| Internet client | Reaches the ports only if they are forwarded or published. Untrusted. |
| Local user on the host | Can read the mount (`AllowOther` is on) and the files on disk. Not defended against here. |

### Trust boundaries

| Boundary | Listener | Authentication |
|---|---|---|
| B1 control server | `:<metrics_port>`, default 9080, all interfaces (`internal/daemon/main.go`) | Opt-in bearer token (`internal/apiauth`) |
| B2 GoStorm server | engine port, default 8090 (`internal/gostorm/web/server.go`) | Same token, header only |
| B3 FUSE mount | kernel to daemon, `AllowOther` | Filesystem permissions only |
| B4 play webhook | `/plex/webhook` and `/webhook` on B1 | Token via header or `?token=` |

### Threats

IDs are stable; each row names the boundary it belongs to.

| ID | Boundary | STRIDE | Threat | Mitigation today | Residual |
|---|---|---|---|---|---|
| T-01 | B1, B2 | Spoofing | A client without the token calls a control route | Bearer token, constant-time compare, length check first (`apiauth.go`); 401 logged with method, path and address, never the credential | None when the token is set; nothing at all when it is not (T-02) |
| T-02 | B1, B2, B4 | Spoofing, Elevation | No token configured, so every route is open, including config write, restart, library add and remove | The default is documented in `SECURITY.md`; `.example` config carries a loopback `gostorm_url`. With no token enforced, `POST /api/config` refuses to set or change `api_token` and `POST /api/restart` is refused, both 403 (`control_api.go`), so a client cannot choose or erase the next start's token or restart the daemon through the control port | Gap: every other route stays open, including other config keys and library add and remove; GoStorm's port answers `POST /shutdown` without a token, which ends the process (`os.Exit(0)`), so a client can still stop the service and whether it comes back depends on the supervisor; the daemon starts without warning of an open control port beyond one log line when auth is on |
| T-03 | B1, B2 | Information disclosure | The token or its traffic is read on the wire | The token is never logged or returned: `Config.Redacted` blanks it for `GET /api/config` | Gap: no TLS on the control port (T-04) |
| T-04 | B1 | Information disclosure, Tampering | Passive or active attacker on the path sees the token and every payload | None in flodni; `SECURITY.md` says to front it with a TLS proxy | Gap: no TLS on B1. GoStorm can serve TLS when its own settings enable it; the control port cannot |
| T-05 | B4 | Information disclosure | `?token=` on a webhook URL lands in proxy and access logs and in the media server's own configuration | Only the two webhook paths accept a query token, matched on the exact path (`controlWebhook` in `apiauth.go`); no other route does | Gap: the token in a URL is logged by any hop that logs URLs; use a dedicated proxy that strips it, or accept it |
| T-06 | B4 | Spoofing | Anyone who can reach the port posts a forged play event | Needs the token when set; the payload is matched against registered files only (`internal/daemon/webhook.go`), so an unknown title does nothing | Gap: with `api_token` empty the webhook is unauthenticated (the open-port refusals in T-02 do not cover it; there is no separate webhook secret); a forged event can hold a registered torrent open |
| T-07 | B4 | Denial of service | An oversized or malformed webhook exhausts memory | Form posts are capped at 10 MiB with `MaxBytesReader`; JSON posts at 10 MiB with `LimitReader` (`webhook.go`) | A 10 MiB body is still held in memory per request |
| T-08 | B1 | Denial of service | Slow or oversized requests tie up the server | `ReadHeaderTimeout` 10 s, `ReadTimeout` 60 s, `IdleTimeout` 120 s, `WriteTimeout` 600 s (`main.go`); library bodies capped at 1 MiB (`internal/library/handler.go`); evict body at 1 KiB (`warmup_evict.go`) | No rate limiting or lockout; the token can be guessed at full network speed |
| T-09 | B2 | Denial of service | Slow headers or idle connections on the engine port | `ReadHeaderTimeout` 10 s, `IdleTimeout` 120 s (`internal/gostorm/web/server.go`) | No read or write timeout, deliberately: streams are long-lived |
| T-10 | B1 | Tampering, Elevation | A caller rewrites `config.json` or restarts the daemon | Token required when set; writes go through `atomicfile.WriteFile`; the token is preserved when the panel posts an empty one (`ResolveAPIToken`) | Config posted by a token holder is trusted as given; paths are not validated |
| T-11 | B1 | Tampering | A caller adds or removes titles, or points an add at an unexpected source | Token required when set; add and remove are the only routes that change the library | A token holder can add any magnet or `.torrent`; there is no allow-list |
| T-12 | B1 | Information disclosure | Unauthenticated health leaks state | `GET /api/health` returns status words only; `GET /control` serves the static panel, which holds no data | Its `warnings` array tells a caller whether play webhooks are arriving |
| T-13 | B3 | Information disclosure | A local user on the host reads media through the mount | None beyond file modes on the stubs | Out of scope: the host is trusted, and `AllowOther` is needed for the media server |
| T-14 | B1, B2 | Repudiation | No record of who changed what | 401s are logged; routine calls are not | Gap: one shared token, no per-caller identity, no audit log |
| T-15 | B1, B2 | Spoofing, Tampering | With no token, a page in a browser on the network POSTs to the open port (a `text/plain` body needs no preflight, and the handlers parse JSON without checking Content-Type), so config write, library add and remove run with no token | With no token, `internal/apiauth` answers 403 to any request whose `Origin` hostname differs from its `Host` hostname, logged with method, path and address; `Origin: null` and an unparsable `Origin` are refused; the engine sends no CORS headers (`Access-Control-Allow-Origin: *` and private-network access were removed) | Gap: only hostnames are compared, because a TLS tunnel delivers plain http with the original `Host`, so a page on the same hostname but another port (another service on the host) is not stopped. Browsers omit `Origin` on navigations and on `<img>` and `<script>` GETs, so a GET reaches the gate with none and passes: on the engine port `GET /play/:hash/:id` and `GET /stream` on a stored hash wake that torrent from its record (a record without metainfo gets 404). `GET`/`HEAD /stream` no longer adds on an open port: it plays only a hash the engine already holds, and answers 403 to any other link, to `http(s)` and `file:` links before any fetch (so no SSRF or local file read) and to `save` and `preload`; with a token set it adds only a hash the engine does not hold. `/shutdown` and the Plex webhook were GET-reachable and now take POST only. DNS rebinding passes: Origin and Host both carry the attacker's name, so the page is same-origin to the browser and can read responses (list, then remove torrents). A client with no `Origin` is the T-02 case. With a token set the check is off and the token is the gate. A reverse proxy in front of the control port must keep the `Host` header, or the panel's own POSTs are refused |
| T-16 | B1, B2 | Tampering, Information disclosure | **Tracker injection (hard rule 3), fixed.** An add (magnet, `torrs://`, `.torrent` upload, or the library add and inspect, which reach the engine through `POST /torrents`) for a hash the engine stores or holds carried the request's trackers into the live torrent and, through the saved record, into every later wake, so one add announced a private torrent's info hash to a foreign tracker for good | `addUnderLock` (`internal/gostorm/torr/lifecycle.go`) merges a request's trackers only into a record that is known public; otherwise the record's trackers replace them. `NewTorrent` (`torrent.go`) hands `AddTorrentSpec` no trackers for a torrent already held unless its metainfo shows it public. Unknown metainfo counts as private. One log line carries a count, never a URL. Pinned in `retrack_test.go` | Re-adding with new trackers is now remove, then add. A first add of a hash with no record and not held takes its own link's trackers, as it must |

### Known gaps

Ordered by what to fix first. None is scheduled here; see
[the roadmap](../../roadmap.md) for what is planned.

1. No TLS on the control port (T-04). The token and every payload travel in
   clear text; mitigate with a TLS proxy on any untrusted network.
2. The token is opt-in (T-02, T-06). An empty `api_token` leaves the other
   config keys, the library routes and the webhook open; setting the token
   needs the environment or the file, and a restart needs the service
   manager. GoStorm's `POST /shutdown` still stops the process without a token.
3. `?token=` on a webhook URL is logged by proxies (T-05). Plex cannot send
   headers, so the query form exists on those two paths only.
4. No rate limiting or lockout (T-08).
5. One shared token with no scopes (T-14): the webhook caller, the panel and
   automation all hold the same credential.
6. The open-port Origin refusal compares hostnames only (T-15): a page on
   the same hostname but another port, DNS rebinding, any client that sends no
   `Origin`, and the GET routes `/play/:hash/:id` and `/stream` on the
   engine port (a page can still wake a stored hash; neither adds) still
   reach an open port.

### Assumptions

- The control and engine ports are reachable from the host and a trusted
  network only. If that is false, T-02 and T-04 are the first things to
  change.
- The media server is not hostile. A forged webhook is treated as a
  nuisance, not as a route to the filesystem: its payload is matched, never
  executed or used as a path.

### Evidence

Read, not run: `internal/apiauth/apiauth.go`, `internal/daemon/main.go`,
`internal/daemon/webhook.go`, `internal/gostorm/web/server.go`,
`internal/library/handler.go`, `internal/config/config.go`. The token
behaviour is covered by `internal/apiauth/apiauth_test.go`; the Origin refusal by
`internal/apiauth/origin_test.go` and the engine's missing CORS headers by
`internal/gostorm/web/server_test.go`.
