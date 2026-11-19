## Architecture Overview

### Purpose

Flodni presents torrents as ordinary media files. A media server scans and
plays them like any other library, and the bytes are fetched from the swarm
when they are read. Nothing is downloaded to disk in full.

### Context

A media server (Plex) reads files from a FUSE mount that Flodni
provides. Each file is backed by a small stub on the host that names a
torrent and a file inside it. When the media server reads a byte range, Flodni
asks its torrent engine for the pieces that hold it.

Titles are added, replaced and removed through the control API, by the
operator or by automation outside this repository. Choosing which release to
add is not Flodni's job.

![System context view: what Flodni depends on, and who depends on it](embed:SystemContext)

### What runs

Flodni is one Go process. It owns three kinds of state on the host: the stub
library, the engine's stores and one configuration file.

![Container view: the daemon and the stores it owns](embed:Containers)

### Inside the daemon

A read enters through the virtual filesystem. Head and index bytes come from
the on-disk index cache; everything else comes from the torrent engine, which
talks to the swarm. The library manager owns the stub files. When warm on add
is switched on, the warmer fills the cache's windows for each newly filed file
in the background, and a slow catch-up pass does the same for the files
already in the library, never evicting. The control API is the only way in
besides the mount.

![Component view: the parts of the daemon](embed:Daemon)

### Who may call it

One auth gate wraps both HTTP servers, the control API and the engine's
server on port 8090. Every request is checked against one route-to-scope
table, and a route with no registered scope is refused. Callers hold named
tokens, kept as hashes in a token file that is read at start. A token carries
only the scopes its caller needs, and `admin` implies all of them. The panel
signs in with a login cookie, which is CSRF-checked on writes, or, optionally,
through an identity-aware proxy that signs a JWT assertion for each panel
user. The static panel page and the health and liveness probes are open, and
only the two webhook routes accept `?token=`. With no token configured the
gate is open and applies only the open-port refusals, which is meant for a
trusted network; a single legacy `api_token` still works as one token with
every scope. The details are in
[ADR 0004](../decisions/0004-scoped-authentication.md).

![Component view: the auth gate, the token file and who calls the control API](embed:Authentication)

### Status

Implemented, inherited from Tiramisu and running. Authentication is opt-in
and scoped (see "Who may call it"); with no token configured the control API
and the engine's server are open and meant to be reachable only from the host
and a trusted network. Planned changes are in the repository's `docs/roadmap.md`;
ideas not yet planned are in `docs/ideas.md`.
