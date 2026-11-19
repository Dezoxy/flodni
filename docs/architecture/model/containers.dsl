// Flodni is one Go process. The daemon is the only running container; the rest
// are the stores it owns on the host. Its internal structure is modelled as
// components, because nothing inside it deploys on its own.

flodni = softwareSystem "Flodni" "Presents torrents as ordinary media files and streams their bytes on demand, without downloading whole files to disk." {

    daemon = container "Flodni Daemon" "Single Go binary: mounts the virtual library, runs the torrent engine and serves the control API." "Go 1.27, cgo FUSE" "Layer Runtime" {
        vfs = component "Virtual Filesystem" "Serves reads on stub files by asking the engine for the byte range. internal/vfs." "Go, libfuse3" "Layer Runtime"
        engine = component "Torrent Engine" "GoStorm on a vendored anacrolix/torrent: sessions, piece priority, read-ahead and the piece cache. Its HTTP server on port 8090 is gated by the API auth gate, with the bearer header only. internal/gostorm." "Go" "Layer Runtime"
        library = component "Library Manager" "Turns a torrent into named stub files, and replaces or removes them. internal/library." "Go" "Layer Runtime"
        control = component "Control API and Panel" "HTTP API and control panel for configuration, library operations and metrics. Every route but the panel page and the health and liveness probes sits behind the API auth gate. internal/daemon, internal/library." "Go net/http" "Layer Runtime,Web UI"
        warmup = component "Index Cache" "Keeps each file's head and index bytes on disk so a scan or a seek answers before the swarm does. internal/warmup." "Go" "Layer Runtime"
        authGate = component "API Auth Gate" "Wraps both HTTP servers and checks every request against one route-to-scope table: a route with no registered scope is refused. A named token holds scopes (webhook, metrics, read, library, scrape, engine, upload, admin, which implies all); a verified proxy assertion or a panel login cookie is admin on the control port only, and a cookie write is CSRF-checked. With no token configured it is open, for a trusted network, and applies only the open-port refusals; a single legacy api_token counts as one token with every scope. Counts refusals in flodni_auth_refusals_total. internal/apiauth, ADR 0004." "Go net/http" "Layer Runtime"
        warmer = component "Warm on Add and Catch-up" "Fills the head and tail windows of newly filed files in the background, and slowly of the files already in the library without ever evicting, pausing while a playback is open. Off by default. internal/warmer, internal/daemon/warm_catchup.go." "Go" "Layer Runtime"
    }

    stubs = container "Stub Library" "Small files on the host, one per title, that name a torrent and a file inside it." "Files on disk" "Layer Data,Storage"
    state = container "State Stores" "Torrent database, metadata index and the on-disk index cache." "bbolt, SQLite, files" "Layer Data,Database"
    tokenFile = container "Token File" "Named, scoped API tokens, stored as SHA-256 hashes and never as the token itself. Read once at start, never written by Flodni." "JSON file (auth_tokens_file)" "Layer Data,Storage"
    config = container "Configuration" "One JSON file with every setting. Holds tracker credentials, the legacy single api_token and the path of the token file." "config.json" "Layer Data,Storage"

    daemon.vfs -> daemon.engine "Requests byte ranges from" "In-process"
    daemon.vfs -> daemon.warmup "Reads head and index bytes from" "In-process"
    daemon.vfs -> stubs "Lists and resolves" "File I/O"
    daemon.vfs -> daemon.library "Removes stubs through, when a file is deleted from the mount" "In-process"
    daemon.library -> stubs "Writes, replaces and removes" "File I/O"
    daemon.library -> daemon.engine "Adds and drops torrents in" "In-process"
    daemon.library -> daemon.warmer "Hands newly filed files, and later the stubs it lists for catch-up, to" "In-process"
    daemon.warmer -> daemon.warmup "Fills the windows of files in" "In-process"
    daemon.warmer -> state "Records per-file warm outcomes in" "In-process"
    daemon.warmer -> daemon.engine "Reads pieces from, and wakes stored torrents in" "In-process"
    daemon.control -> daemon.library "Runs library operations through" "In-process"
    daemon.control -> daemon.engine "Reads session and health state from" "HTTP on loopback"
    daemon.control -> config "Reads and writes" "File I/O"
    daemon.control -> daemon.authGate "Has every request checked against the route-to-scope table by" "In-process"
    daemon.engine -> daemon.authGate "Has every request checked against the route-to-scope table by" "In-process"
    daemon.authGate -> tokenFile "Reads the token hashes at start from" "File I/O"
    daemon.engine -> state "Stores torrents and metadata in" "File I/O"
    daemon.warmup -> state "Stores index bytes in" "File I/O"
    daemon.engine -> swarm "Exchanges pieces with peers and announces to trackers of" "BitTorrent, HTTP(S)/UDP" "Inbound across trust boundary"
}

mediaServer -> flodni.daemon.vfs "Scans and reads media files from" "FUSE mount"
operator -> flodni.daemon.control "Configures and monitors through" "HTTP"
idProxy -> flodni.daemon.control "Forwards panel requests with a signed assertion to" "HTTP, signed JWT"
automation -> flodni.daemon.control "Adds, replaces and removes titles through" "HTTP/JSON"
