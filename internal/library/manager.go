package library

import (
	"context"
	"encoding/json"
	"errors"
	"flodni/internal/flog"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"flodni/internal/metadb"
)

const (
	defaultMetadataWait = 60
	maxMetadataWait     = 300
)

// replacementReadBytes is how much of a replacing release's first file must arrive
// before the stubs it replaces may go. 64 KiB is a few pieces: enough to show the
// swarm delivers, small enough to cost seconds on a live one.
const replacementReadBytes = 64 << 10

// replacementSampleSize is how many files of a replacing pack the check reads: the
// one being replaced, the first and the last episode by name, never more. A bound
// on cost, not a proof: a dead file outside the sample can still pass.
const replacementSampleSize = 3

// replacementReadTimeout bounds that read, for the whole sample together. A release with no seeds would otherwise
// hold the request open until the client gives up; a minute matches the metadata
// wait. A variable so tests can shrink it.
var replacementReadTimeout = 60 * time.Second

var reInfoHash = regexp.MustCompile(`^[a-f0-9]{40}$|^[a-z2-7]{32}$`)

// EpisodeRegistry is the subset of the state DB the manager writes to. A TV stub that
// is not registered is deleted by the next TV sync as orphaned, so this is required for
// TV adds.
type EpisodeRegistry interface {
	UpsertEpisode(key string, entry metadb.EpisodeEntry) error
	GetEpisode(key string) (*metadb.EpisodeEntry, bool, error)
	DeleteEpisode(key string) error
	EpisodesByFilePath(path string) ([]metadb.EpisodeEntry, error)
}

// Config wires the manager to the on-disk layout and the engine.
type Config struct {
	MoviesDir  string
	TVDir      string
	GoStormURL string
	GoStorm    GoStorm
	Registry   EpisodeRegistry
	// InvalidatePath, when set, drops the FUSE layer's cached state for a removed stub.
	InvalidatePath func(string)
	// Blacklist, when set, records a removed release the way the FUSE unlink handler
	// does, so the title is not filed back by mistake. Called only when the caller asks.
	Blacklist func(path, hash string) error
	// Gaps, when set, reports the episodes the reaper removed and has not been able to
	// replace. Read-only: what to do about a hole is a decision for the client, which
	// can ask the user; the engine only says which ones are open.
	Gaps func() ([]Gap, error)
	// StoredFiles, when set, is the file list saved with a torrent's record (engine
	// ids), read without waking the torrent; nil when there is none. GET
	// /api/library/audit needs it.
	StoredFiles func(hash string) []TorrentFile
	// StoredRecords, when set, lists the torrent records the engine stores (hash in
	// lower-case hex and the stored name). GET /api/library/audit reports the ones no
	// stub references as orphan_records.
	StoredRecords func() []StoredRecord
	// Linked, when set, is the daemon's set of stub base names some library symlink
	// points at, nil while there is no usable set (no walk yet, no root, too old). It
	// feeds the linked field of the list. It must be cheap and must not touch the disk.
	Linked func() map[string]struct{}
	// FileState, when set, reports the daemon's per-file facts (warm record, played and
	// eviction marks, an open session) for GET /api/library/file; nil reads as none.
	FileState func(hash string, fileIndex int) FileFacts
	// Confirm, when set, records a play of the file the way a play webhook does for the
	// file it resolves to (POST /api/library/confirm). TokenSet says whether the control
	// port has an api_token; the route answers 403 unless it does.
	Confirm  func(hash string, fileIndex int)
	TokenSet func() bool
	Logger   *slog.Logger
	// MediaServer, when set, is asked to rescan the section a stub was added to or
	// removed from. MovieSection and TVSection are its library ids.
	MediaServer  MediaServer
	MovieSection int
	TVSection    int
	// RefreshDelay is how long refreshes are coalesced for; 0 means the default.
	RefreshDelay time.Duration
	// OnAdded, when set, is called once after an add that wrote stubs, not for a
	// title that was already present and not for a failure. Each file carries the
	// engine's file id. It runs under the hash locks, so it must only enqueue:
	// memory only, never blocking.
	OnAdded func(hash string, files []AddedFile)
}

// Manager adds and removes library entries on behalf of external clients: it does what
// the catalogue sync used to do for one title, without any of the discovery.
type Manager struct {
	cfg Config

	mu        sync.Mutex
	hashLocks *keyLocks // one lock per info hash
	showLocks *keyLocks // one per show: exclusive for packs, shared for single episodes

	// unlinked is the count of stubs with linked=false in the last list of each kind
	// (movies, tv); nil until listed, or when that list had no linked set.
	unlinked [2]atomic.Pointer[int]

	refreshPending map[int]bool
	refreshDirty   map[int]bool
}

func New(cfg Config) *Manager {
	if cfg.Logger == nil {
		cfg.Logger = flog.New(os.Stdout, "", true)
	}
	cfg.GoStormURL = strings.TrimRight(cfg.GoStormURL, "/")
	return &Manager{cfg: cfg, hashLocks: newKeyLocks(), showLocks: newKeyLocks()}
}

// Error carries the HTTP status the handler should answer with.
type Error struct {
	Status  int
	Message string
	// Err is an optional sentinel a caller can route on with errors.Is, while
	// Status stays what a client is told.
	Err error
}

func (e *Error) Error() string { return e.Message }

// Unwrap lets one value carry both an HTTP status and a sentinel, so a caller
// can route on errors.Is while a client still gets a status.
func (e *Error) Unwrap() error { return e.Err }

func errf(status int, format string, args ...interface{}) *Error {
	return &Error{Status: status, Message: fmt.Sprintf(format, args...)}
}

// AddRequest describes one title to file into the library. Either Hash or Magnet is
// required; everything else shapes the filename and the stub.
type AddRequest struct {
	Type         string `json:"type"` // "movie" (default) or "tv"
	Hash         string `json:"hash"`
	Magnet       string `json:"magnet"`
	Title        string `json:"title"`
	ReleaseTitle string `json:"release_title"`
	ReleaseDate  string `json:"release_date"`
	Year         int    `json:"year"`
	// IMDB is written into a movie stub. Episode stubs carry none: see addEpisodes.
	IMDB string `json:"imdb"`
	// Is4K overrides the resolution read out of ReleaseTitle.
	Is4K         *bool  `json:"is_4k"`
	Season       int    `json:"season"`
	Episode      int    `json:"episode"` // 0 with Season set means "season pack"
	FirstAirDate string `json:"first_air_date"`
	// FileIndex picks one file inside the torrent; 0 means the largest video file.
	FileIndex int `json:"file_index"`
	// TorrentFile is the release's .torrent (base64 in JSON), when the caller fetched
	// one: the engine gets the metadata at once and keeps the file's own trackers.
	TorrentFile []byte `json:"torrent_file,omitempty"`
	// QualityScore is stored in the TV registry. Left at 0, the next TV sync is free
	// to replace the episode with any release it scores above zero.
	QualityScore int `json:"quality_score"`
	// Replace says what to do when the registry already holds a different release
	// of the same episode: "never", "if-better" (only when QualityScore is greater
	// than the stored one; 0 is never better) or "always". Omitted means "always",
	// which is what an add did before the field existed. A movie never replaces.
	Replace string `json:"replace"`
	// DryRun answers what an add would decide from the registry and the stubs on disk,
	// and does nothing else: no engine call (the torrent is never added, no metadata
	// wait, no read), nothing written, no OnAdded, no scan. A single episode or a movie
	// only: a pack's episodes are known only from the torrent's file list.
	DryRun       bool `json:"dry_run"`
	MetadataWait int  `json:"metadata_wait"`
	// AcceptFiles vets the torrent's file list once metadata arrives; an error drops
	// the torrent and becomes a 422. Not part of the HTTP contract.
	AcceptFiles func(paths []string) error `json:"-"`
}

// AddedFile is one stub written to disk.
type AddedFile struct {
	Path string `json:"path"`
	// FusePath is empty, and so omitted, in a dry run: nothing was written, and callers
	// link every entry that has one.
	FusePath  string `json:"fuse_path,omitempty"`
	Size      int64  `json:"size"`
	FileIndex int    `json:"file_index"`
	Season    int    `json:"season,omitempty"`
	Episode   int    `json:"episode,omitempty"`
}

// Replace policies and add decisions (ADR 3).
const (
	ReplaceNever    = "never"
	ReplaceIfBetter = "if-better"
	ReplaceAlways   = "always"

	DecisionAdded          = "added"
	DecisionAlreadyPresent = "already_present"
	DecisionReplaced       = "replaced"
	DecisionKept           = "kept"

	// ReadabilityUnchecked is what a dry run reports: it never asks the swarm.
	ReadabilityUnchecked = "unchecked"
)

// ReplacedFile is a stub an add replaced: its path, the release it linked to and the
// file index in that release.
type ReplacedFile struct {
	Path      string `json:"path"`
	Hash      string `json:"hash"`
	FileIndex int    `json:"file_index"`
}

// SkippedFile is an episode the replace policy refused. It was not written, so it is
// never in Files.
type SkippedFile struct {
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Reason  string `json:"reason"`
}

// AddResponse is the answer to an add. Files lists only the stubs this request wrote
// (or, for already_present, the ones already there): callers link each entry that
// carries a fuse_path. Decision, Replaced and Skipped are answered only to a request
// that carries the replace field; any other request gets the v0.1.0 body.
type AddResponse struct {
	Hash           string         `json:"hash"`
	Title          string         `json:"title"`
	Type           string         `json:"type"`
	Files          []AddedFile    `json:"files"`
	AlreadyPresent bool           `json:"already_present"`
	Decision       string         `json:"decision,omitempty"`
	Replaced       []ReplacedFile `json:"replaced,omitempty"`
	Skipped        []SkippedFile  `json:"skipped,omitempty"`
	// DryRun and Readability are set only by a dry run: nothing was written or asked.
	DryRun      bool   `json:"dry_run,omitempty"`
	Readability string `json:"readability,omitempty"`
}

// addResult is what filing the episodes of one torrent produced.
type addResult struct {
	files    []AddedFile
	replaced []ReplacedFile
	skipped  []SkippedFile
}

type RemoveRequest struct {
	// Type is "movie" or "tv", or omitted: video requests predate it.
	Type string `json:"type"`
	Path string `json:"path"`
	Hash string `json:"hash"`
	// Blacklist keeps the release out: without it a later add is free to file the
	// title back, which is what you want when removing to upgrade
	// and not what you want when removing for good.
	Blacklist bool `json:"blacklist"`
}

// Gap is an episode removed because its release died and nothing live replaced it.
type Gap struct {
	EpisodeKey string `json:"episode_key"`
	Show       string `json:"show"`
	Season     int    `json:"season"`
	ShowIMDB   string `json:"show_imdb,omitempty"`
	Path       string `json:"path"`
	DeadHash   string `json:"dead_hash"`
	RemovedAt  int64  `json:"removed_at"`
	// LastAttempt is when the engine last re-searched this hole, zero when it never
	// has. No omitempty: the client needs to tell "never tried" from "tried at 0".
	LastAttempt int64 `json:"last_attempt"`
}

// maxGaps caps one listing: the client pages through nothing, it acts on what it
// sees, and an unbounded backlog should not become an unbounded response.
const maxGaps = 500

// ListGaps returns the open holes, oldest first, along with how many there are in
// total: the response is capped, and a client cannot tell a full page from a
// truncated one by its length alone.
func (m *Manager) ListGaps() ([]Gap, int, error) {
	if m.cfg.Gaps == nil {
		return []Gap{}, 0, nil
	}
	gaps, err := m.cfg.Gaps()
	if err != nil {
		// The driver error names the database file: it is logged, not returned, since
		// this endpoint carries no authentication.
		m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: cannot read the episode gaps: %v", err), "err", err)
		return nil, 0, errf(http.StatusInternalServerError, "cannot read the episode gaps")
	}
	if gaps == nil {
		gaps = []Gap{}
	}
	total := len(gaps)
	if len(gaps) > maxGaps {
		gaps = gaps[:maxGaps]
	}
	return gaps, total, nil
}

type RemoveResponse struct {
	Removed []string `json:"removed"`
	// Warnings names removals asked for good whose release could not be blacklisted:
	// the sync may add those titles back.
	Warnings []string `json:"warnings,omitempty"`
}

// Item is one stub as reported by List.
type Item struct {
	Path      string `json:"path"`
	FusePath  string `json:"fuse_path"`
	Size      int64  `json:"size"`
	Hash      string `json:"hash"`
	IMDB      string `json:"imdb,omitempty"`
	FileIndex int    `json:"file_index"`
	Season    int    `json:"season,omitempty"`
	Episode   int    `json:"episode,omitempty"`
	// ModTime is the stub's mtime, which orders the catch-up feeder (newest first). It
	// is not part of the API answer.
	ModTime time.Time `json:"-"`
	// Linked is true when a library symlink points at this stub's base name, false when
	// none does, and null (nil) when there is no linked set to say.
	Linked *bool `json:"linked"`
}

// StoredRecord is a torrent record the engine stores: its hash (lower-case hex) and
// stored name. Nothing else of the record is carried.
type StoredRecord struct {
	Hash string `json:"hash"`
	Name string `json:"name"`
}

var (
	reStubHash  = regexp.MustCompile(`link=([a-fA-F0-9]{40})`)
	reStubIndex = regexp.MustCompile(`index=(\d+)`)
	reStubIMDB  = regexp.MustCompile(`tt\d{7,10}`)
)

// cleanupCtx is what the rollback paths use: the request context may already be
// cancelled (the client hung up during the metadata wait) and the cleanup still has to
// reach the engine, or the torrent stays hydrated with no stub pointing at it.
func cleanupCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
}

// dropTorrent reports whether the engine removed the torrent.
func (m *Manager) dropTorrent(ctx context.Context, hash string) bool {
	cctx, cancel := cleanupCtx(ctx)
	defer cancel()
	removeErr := m.cfg.GoStorm.RemoveTorrent(cctx, hash)
	if removeErr != nil {
		m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: cannot remove torrent %s: %v", hash, removeErr), "hash", hash, "remove_err", removeErr)
	}
	// The failure counter outlives the torrent otherwise: the same release added again
	// later would arrive already condemned, and the reaper would drop it on sight.
	if cf, ok := m.cfg.Registry.(interface{ ClearMetadataFailure(string) error }); ok {
		if err := cf.ClearMetadataFailure(hash); err != nil {
			m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: cannot clear the failure counter for %s: %v", hash, err), "hash", hash, "err", err)
		}
	}
	return removeErr == nil
}

// Add registers the torrent with GoStorm, waits for its file list, and writes the stub
// files the FUSE layer serves. On any failure after the torrent was added it is removed
// again, so a failed call leaves nothing hydrated.
func (m *Manager) Add(ctx context.Context, req AddRequest) (*AddResponse, error) {
	// A request that names none of the lifecycle fields gets the v0.1.0 body.
	optedIn := strings.TrimSpace(req.Replace) != "" || req.DryRun
	resp, err := m.add(ctx, req)
	if resp != nil && !optedIn {
		resp.Decision, resp.Replaced, resp.Skipped = "", nil, nil
	}
	return resp, err
}

func (m *Manager) add(ctx context.Context, req AddRequest) (*AddResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, errf(http.StatusRequestTimeout, "request cancelled: %v", err)
	}
	// Registered first so it runs after the unlocks below: dropping a torrent takes its
	// own hash lock, and nesting that inside the one this add holds is how two adds
	// replacing each other's episodes would deadlock. Every drop is reference-checked -
	// an earlier add of the same hash may still have stubs streaming from it.
	dropped := map[string]bool{}
	defer func() {
		// The engine keys torrents by hex; a base32 request spelling would match nothing.
		for h := range dropped {
			m.dropTorrentIfUnused(ctx, canonicalHashKey(h))
		}
	}()

	kind, hash, err := m.validate(&req)
	if err != nil {
		return nil, err
	}

	// Two clients asking for the same release at once would otherwise both pay the
	// metadata wait and write two stubs; the second one waits here and finds the
	// title already present.
	unlock := m.lockHash(hash)
	defer unlock()

	// Two releases of the same episode arriving at once carry different hashes, so the
	// lock above does not cover them: they would both write and race on the registry
	// key, leaving the loser's stub unregistered and its torrent running.
	// Two releases of the same episode carry different hashes, so the lock above does
	// not cover them: they would both write and race on the registry key. A pack takes
	// the show exclusively, because it writes episodes for seasons it discovers in the
	// file names and the requested season does not bound them; a single episode only
	// shares the show and locks its own key, so filing a season episode by episode
	// still runs in parallel.
	if kind == "tv" {
		show := "show:" + EpisodeKey(req.Title, 0, 0)
		if req.Episode > 0 {
			defer m.showLocks.RLock(show)()
			// The legacy row may be written too (see episodeKey), so its lock is taken
			// as well, always in key order.
			keys := []string{EpisodeKey(req.Title, req.Season, req.Episode)}
			if legacy := legacyEpisodeKey(req.Title, req.Season, req.Episode); legacy != keys[0] {
				keys = append(keys, legacy)
				sort.Strings(keys)
			}
			for _, k := range keys {
				defer m.hashLocks.Lock("ep:" + k)()
			}
		} else {
			defer m.showLocks.Lock(show)()
		}
	}

	// The stub already on disk is the cheap answer: it saves the metadata wait, and
	// re-adding would duplicate the title under a second name. For TV only an exact
	// episode counts, because one multi-season pack is a single hash behind several
	// seasons: matching by hash alone would report season 2 as already filed.
	if existing, err := m.alreadyPresent(kind, hash, req); err != nil {
		return nil, err
	} else if len(existing) > 0 {
		resp := &AddResponse{Hash: hash, Title: req.Title, Type: kind, Files: existing, AlreadyPresent: true,
			Decision: DecisionAlreadyPresent}
		if req.DryRun {
			resp.DryRun, resp.Readability = true, ReadabilityUnchecked
			for i := range resp.Files {
				resp.Files[i].FusePath = ""
			}
		}
		return resp, nil
	}

	if req.DryRun {
		return m.dryRun(req, kind, hash)
	}

	// A single episode is decided here, under its lock and before the engine is
	// contacted: a refused add costs no metadata wait and touches no swarm (a private
	// torrent is not announced). A pack's episodes are only known after metadata, so a
	// pack fetches it once before its verdicts, even when every episode is then refused.
	if kind == "tv" && req.Episode > 0 && req.Replace != ReplaceAlways {
		key, err := m.episodeKey(req.Title, req.Season, req.Episode)
		if err != nil {
			return nil, errf(http.StatusServiceUnavailable, "cannot read the registry entry for %s: %v",
				EpisodeKey(req.Title, req.Season, req.Episode), err)
		}
		v, err := m.replaceVerdict(req.Replace, key, m.singleEpisodePath(req, hash), req.QualityScore)
		if err != nil {
			return nil, err
		}
		if v.refused {
			m.cfg.Logger.Info(fmt.Sprintf("[LibraryAPI] Kept %s S%02dE%02d: %s", req.Title, req.Season, req.Episode, v.reason), "title", req.Title, "season", req.Season, "episode", req.Episode, "reason", v.reason)
			return &AddResponse{Hash: hash, Title: req.Title, Type: kind, Files: []AddedFile{}, Decision: DecisionKept,
				Skipped: []SkippedFile{{Season: req.Season, Episode: req.Episode, Reason: v.reason}}}, nil
		}
	}

	file, err := releaseFile(req.TorrentFile, hash)
	if err != nil {
		return nil, err
	}
	magnet := req.Magnet
	if magnet == "" {
		magnet = releaseMagnet(hash, req.Title, file)
	}

	requestedHash := hash
	if err := m.uploadReleaseFile(ctx, file, req.Title); err != nil {
		return nil, err
	}
	addedHash, err := m.cfg.GoStorm.AddTorrent(ctx, magnet, req.Title)
	if err != nil || addedHash == "" {
		// An interrupted request may still have been taken, and the engine saves it;
		// dropTorrentIfUnused spares a torrent another stub already uses.
		dropped[requestedHash] = true
		return nil, errf(http.StatusBadGateway, "gostorm rejected the torrent: %v", err)
	}
	// The filename builders slice the hash, so a malformed answer must stop here.
	hash = strings.ToLower(strings.TrimSpace(addedHash))
	if !reInfoHash.MatchString(hash) {
		// The torrent is hydrated under the hash we asked for; drop it rather than
		// leaving it running with no stub pointing at it.
		dropped[requestedHash] = true
		return nil, errf(http.StatusBadGateway, "gostorm returned a malformed info hash %q", addedHash)
	}
	// A base32 magnet comes back in hex: everything from here on is keyed on the
	// spelling the engine reported, so the lock has to cover it too, or a cleanup
	// elsewhere sees the hash free and removes the torrent from under this add.
	if hash != requestedHash {
		defer m.lockHash(hash)()
	}

	wait := req.MetadataWait
	if wait <= 0 {
		wait = defaultMetadataWait
	} else if wait > maxMetadataWait {
		wait = maxMetadataWait
	}

	info, err := m.cfg.GoStorm.GetTorrentInfo(ctx, hash, wait)
	if err != nil || info == nil {
		dropped[hash] = true
		return nil, errf(http.StatusGatewayTimeout, "no metadata after %ds: %v", wait, err)
	}

	if req.AcceptFiles != nil {
		paths := make([]string, 0, len(info.FileStats))
		for _, f := range info.FileStats {
			paths = append(paths, f.Path)
		}
		if err := req.AcceptFiles(paths); err != nil {
			dropped[hash] = true
			return nil, &Error{Status: http.StatusUnprocessableEntity, Message: err.Error(), Err: err}
		}
	}

	var res addResult
	if kind == "tv" {
		res, err = m.addEpisodes(ctx, req, hash, magnet, info, dropped)
	} else {
		res.files, err = m.addMovie(req, hash, magnet, info)
	}
	if err != nil {
		dropped[hash] = true
		return nil, err
	}
	if len(res.files) == 0 && len(res.skipped) > 0 {
		// Every episode of the pack was refused: nothing was written, so nothing is
		// announced and the torrent goes unless another stub uses it. The metadata fetch
		// already happened: only a single episode is refused before any engine contact.
		dropped[hash] = true
		m.cfg.Logger.Info(fmt.Sprintf("[LibraryAPI] Kept %s: every episode of %s refused by replace=%s", req.Title, hash[:8], req.Replace), "title", req.Title, "hash", hash[:8], "replace", req.Replace)
		return &AddResponse{Hash: hash, Title: req.Title, Type: kind, Files: []AddedFile{},
			Decision: DecisionKept, Skipped: res.skipped}, nil
	}
	files := res.files

	if m.cfg.OnAdded != nil {
		m.cfg.OnAdded(hash, files)
	}
	m.scheduleRefresh(m.section(kind))
	m.cfg.Logger.Info(fmt.Sprintf("[LibraryAPI] Added %s %q (%s): %d file(s)", kind, req.Title, hash[:8], len(files)), "kind", kind, "title", req.Title, "hash", hash[:8])
	decision := DecisionAdded
	if len(res.replaced) > 0 {
		decision = DecisionReplaced
	}
	return &AddResponse{Hash: hash, Title: req.Title, Type: kind, Files: files,
		Decision: decision, Replaced: res.replaced, Skipped: res.skipped}, nil
}

// alreadyPresent reports the stubs this request would create that are already on disk.
// A TV pack is never short-circuited: which episodes it holds is only known once the
// file list arrives.
func (m *Manager) alreadyPresent(kind, hash string, req AddRequest) ([]AddedFile, error) {
	if kind != "tv" {
		return m.findByHash(kind, hash)
	}
	if req.Episode <= 0 {
		return nil, nil
	}
	path := filepath.Join(m.cfg.TVDir, ShowFolderName(req.Title, req.FirstAirDate),
		fmt.Sprintf("Season.%02d", req.Season),
		EpisodeFilename(req.Title, req.Season, req.Episode, hash[:8]))
	if _, err := os.Stat(path); err != nil {
		return nil, nil
	}
	// The name carries only eight digits of the hash: the stub's link says whose it is.
	st, _ := readStub(path)
	if st.Hash != canonicalHashKey(hash) {
		return nil, nil
	}
	return []AddedFile{{
		Path: path, FusePath: m.fusePath(path), Size: st.Size, FileIndex: st.FileIndex,
		Season: req.Season, Episode: req.Episode,
	}}, nil
}

// lockHash serialises work on one info hash.
func (m *Manager) lockHash(hash string) func() { return m.hashLocks.Lock(hash) }

// validate normalises the request and returns the content kind and the info hash.
func (m *Manager) validate(req *AddRequest) (string, string, error) {
	kind := strings.ToLower(strings.TrimSpace(req.Type))
	switch kind {
	case "", "movie", "movies", "film":
		kind = "movie"
	case "tv", "show", "series", "episode":
		kind = "tv"
	default:
		return "", "", errf(http.StatusBadRequest, "unknown type %q: use movie or tv", req.Type)
	}

	req.Replace = strings.ToLower(strings.TrimSpace(req.Replace))
	switch req.Replace {
	case "":
		req.Replace = ReplaceAlways
	case ReplaceNever, ReplaceIfBetter, ReplaceAlways:
	default:
		return "", "", errf(http.StatusBadRequest, "unknown replace %q: use never, if-better or always", req.Replace)
	}

	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		return "", "", errf(http.StatusBadRequest, "title is required")
	}
	if req.ReleaseTitle == "" {
		req.ReleaseTitle = req.Title
	}
	if req.ReleaseDate == "" && req.Year > 0 {
		req.ReleaseDate = fmt.Sprintf("%d", req.Year)
	}

	hash := strings.ToLower(strings.TrimSpace(req.Hash))
	if req.Magnet != "" {
		if h := HashFromMagnet(req.Magnet); h != "" {
			hash = h
		} else if hash == "" {
			return "", "", errf(http.StatusBadRequest, "magnet carries no info hash")
		}
	}
	if hash == "" {
		return "", "", errf(http.StatusBadRequest, "hash or magnet is required")
	}
	if !reInfoHash.MatchString(hash) {
		return "", "", errf(http.StatusBadRequest, "malformed info hash %q", hash)
	}

	if kind == "tv" {
		if req.Episode <= 0 && req.FileIndex > 0 {
			return "", "", errf(http.StatusBadRequest,
				"file_index needs an episode: a season pack files every file it can name")
		}
		if req.Season <= 0 {
			return "", "", errf(http.StatusBadRequest, "season is required for tv")
		}
		if req.DryRun && req.Episode <= 0 {
			return "", "", errf(http.StatusBadRequest,
				"dry_run needs an episode: a season pack's episodes are only known from the torrent's file list, which a dry run never fetches")
		}
		if m.cfg.Registry == nil {
			return "", "", errf(http.StatusServiceUnavailable,
				"the episode registry is unavailable: a tv stub added now would be deleted by the next sync")
		}
		if m.cfg.TVDir == "" {
			return "", "", errf(http.StatusServiceUnavailable, "no tv directory configured")
		}
	} else if m.cfg.MoviesDir == "" {
		return "", "", errf(http.StatusServiceUnavailable, "no movies directory configured")
	}
	return kind, hash, nil
}

func (m *Manager) addMovie(req AddRequest, hash, magnet string, info *TorrentStats) ([]AddedFile, error) {
	file, err := m.pickFile(req.FileIndex, info.FileStats)
	if err != nil {
		return nil, err
	}

	path := m.moviePath(req, hash)
	if err := WriteStub(path, m.streamURL(hash, file.ID), file.Length, magnet, req.IMDB); err != nil {
		return nil, errf(http.StatusInternalServerError, "cannot write %s: %v", filepath.Base(path), err)
	}
	return []AddedFile{{
		Path: path, FusePath: m.fusePath(path), Size: file.Length, FileIndex: file.ID,
	}}, nil
}

// moviePath is where an add of this release writes its stub; the name needs no file
// list, so a dry run can say it.
func (m *Manager) moviePath(req AddRequest, hash string) string {
	is4K := reIs4K.MatchString(req.ReleaseTitle)
	if req.Is4K != nil {
		is4K = *req.Is4K
	}
	name := BuildMovieFilename(MovieName{
		Title: req.Title, ReleaseDate: req.ReleaseDate, ReleaseTitle: req.ReleaseTitle,
		Is4K: is4K, Hash: hash,
	})
	return filepath.Join(m.cfg.MoviesDir, name)
}

var reIs4K = regexp.MustCompile(`(?i)2160p|\buhd\b|\b4k\b`)

// pickFile returns the requested file, or the largest video file when no index is given.
func (m *Manager) pickFile(index int, files []FileStat) (*FileStat, error) {
	if index > 0 {
		for i := range files {
			if files[i].ID == index {
				if !IsVideoFile(files[i].Path) {
					return nil, errf(http.StatusBadRequest, "file %d (%s) is not a video file", index, files[i].Path)
				}
				return &files[i], nil
			}
		}
		return nil, errf(http.StatusBadRequest, "the torrent has no file %d", index)
	}

	var best *FileStat
	skipped := 0
	for i := range files {
		if m.usable(files[i]) && (best == nil || files[i].Length > best.Length) {
			best = &files[i]
		} else if IsVideoFile(files[i].Path) && isSample(files[i]) {
			skipped++
		}
	}
	if best == nil {
		if skipped > 0 {
			return nil, errf(http.StatusUnprocessableEntity, "the torrent holds only a sample")
		}
		return nil, errf(http.StatusUnprocessableEntity, "the torrent holds no video file")
	}
	return best, nil
}

// replacementSample returns the indexes into the sorted episode list whose files
// the check reads: the one being replaced, then the first and the last regular
// episode. seasons holds each entry's season. Season-0 specials are never chosen as
// first or last, nor as the replaced one; the list holds only files that name an
// episode, so samples and extras never appear. A single-file release yields one
// index. A release of specials alone has nothing else to read, so the replaced
// special is read.
func replacementSample(seasons []int, replacing int) []int {
	var idx []int
	if seasons[replacing] > 0 {
		idx = append(idx, replacing)
	}
	first, last := -1, -1
	for i, s := range seasons {
		if s > 0 {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	for _, j := range []int{first, last} {
		if j >= 0 && len(idx) < replacementSampleSize && !slices.Contains(idx, j) {
			idx = append(idx, j)
		}
	}
	if len(idx) == 0 {
		idx = append(idx, replacing)
	}
	return idx
}

// readReplacementSample reads the head of every sampled file at once, under one
// shared timeout. Concurrent, so the worst case is one timeout, as for a single
// file, rather than three; at most replacementSampleSize reads run. The first
// failure cancels the others and is returned.
func (m *Manager) readReplacementSample(ctx context.Context, hash string, files []FileStat) error {
	ctx, cancel := context.WithTimeout(ctx, replacementReadTimeout)
	defer cancel()
	errs := make(chan error, len(files))
	for _, f := range files {
		go func() {
			errs <- m.cfg.GoStorm.ReadFileHead(ctx, hash, f.ID, min(replacementReadBytes, f.Length))
		}()
	}
	var first error
	for range files {
		if err := <-errs; err != nil && first == nil {
			first = err
			cancel()
		}
	}
	return first
}

func (m *Manager) addEpisodes(ctx context.Context, req AddRequest, hash, magnet string, info *TorrentStats, dropped map[string]bool) (addResult, error) {
	type episodeFile struct {
		file    FileStat
		season  int
		episode int
		key     string
	}
	var wanted []episodeFile

	if req.Episode > 0 {
		file, err := m.pickFileForEpisode(req, info.FileStats)
		if err != nil {
			return addResult{}, err
		}
		wanted = append(wanted, episodeFile{file: *file, season: req.Season, episode: req.Episode})
	} else {
		for _, f := range info.FileStats {
			if !m.usable(f) {
				continue
			}
			// Season 0 with an episode is an explicit S00Exx special: it keeps season 0
			// and is filed under Season.00, never under the requested season.
			season, episode := PackEpisode(f.Path, req.Season)
			if episode == 0 {
				continue
			}
			wanted = append(wanted, episodeFile{file: f, season: season, episode: episode})
		}
		if len(wanted) == 0 {
			return addResult{}, errf(http.StatusUnprocessableEntity, "no episode file could be named in this torrent")
		}
	}

	// One episode, one stub: a pack carrying a proper next to the original would
	// otherwise write the same path twice, and a rollback could only half-undo it.
	best := map[[2]int]episodeFile{}
	for _, w := range wanted {
		k := [2]int{w.season, w.episode}
		if cur, ok := best[k]; !ok || w.file.Length > cur.file.Length {
			best[k] = w
		}
	}
	wanted = wanted[:0]
	for _, w := range best {
		wanted = append(wanted, w)
	}

	sort.Slice(wanted, func(i, j int) bool {
		if wanted[i].season != wanted[j].season {
			return wanted[i].season < wanted[j].season
		}
		return wanted[i].episode < wanted[j].episode
	})

	// The registry key of each episode, read once: it is the folded key unless the
	// show was registered under the older form (see episodeKey).
	for i := range wanted {
		key, err := m.episodeKey(req.Title, wanted[i].season, wanted[i].episode)
		if err != nil {
			return addResult{}, errf(http.StatusServiceUnavailable, "cannot read the registry entry for %s: %v",
				EpisodeKey(req.Title, wanted[i].season, wanted[i].episode), err)
		}
		wanted[i].key = key
	}

	showDir := filepath.Join(m.cfg.TVDir, ShowFolderName(req.Title, req.FirstAirDate))

	// The replace policy, per episode: an episode the registry already holds from a
	// different release, and the policy refuses to replace, is not written. It goes in
	// skipped, never in the files this request wrote. Nothing is read from the swarm
	// for it: the readability gate below runs only for the episodes that replace.
	var skipped []SkippedFile
	if req.Replace != ReplaceAlways {
		allowed := wanted[:0]
		for _, w := range wanted {
			path := filepath.Join(showDir, fmt.Sprintf("Season.%02d", w.season),
				EpisodeFilename(req.Title, w.season, w.episode, hash[:8]))
			v, err := m.replaceVerdict(req.Replace, w.key, path, req.QualityScore)
			if err != nil {
				return addResult{}, err
			}
			if v.refused {
				skipped = append(skipped, SkippedFile{Season: w.season, Episode: w.episode, Reason: v.reason})
				continue
			}
			allowed = append(allowed, w)
		}
		wanted = allowed
		if len(wanted) == 0 {
			return addResult{skipped: skipped}, nil
		}
	}

	// The new release wins, but only once it has shown it can deliver bytes: a dead
	// swarm must not cost the files that played. Nothing is written or removed
	// before this check passes, so a failure needs no rollback. Re-adding a stub at
	// its own path replaces nothing and is not checked.
	for i, w := range wanted {
		path := filepath.Join(showDir, fmt.Sprintf("Season.%02d", w.season),
			EpisodeFilename(req.Title, w.season, w.episode, hash[:8]))
		prev, ok, err := m.cfg.Registry.GetEpisode(w.key)
		if err != nil {
			return addResult{}, errf(http.StatusServiceUnavailable, "cannot read the registry entry for %s: %v", w.key, err)
		}
		if !ok || prev == nil || prev.FilePath == "" || prev.FilePath == path {
			continue
		}
		seasons := make([]int, len(wanted))
		for k := range wanted {
			seasons[k] = wanted[k].season
		}
		var sample []FileStat
		for _, j := range replacementSample(seasons, i) {
			sample = append(sample, wanted[j].file)
		}
		if err := m.readReplacementSample(ctx, hash, sample); err != nil {
			m.cfg.Logger.Info(fmt.Sprintf("[LibraryAPI] replacement %s for %q delivered no data, existing files kept: %v", hash[:8], req.Title, err), "hash", hash[:8], "title", req.Title, "err", err)
			return addResult{}, errf(http.StatusUnprocessableEntity,
				"replacement release delivered no data (%v, limit %s); existing files kept", err, replacementReadTimeout)
		}
		break // one sample proves the release; later episodes need no second check
	}

	var out []AddedFile
	// What this call found already in place. The stubs it overwrote must survive a
	// rollback, and the episodes a previous release left elsewhere are only deleted
	// once the whole pack is written: doing it as we go would leave a failure halfway
	// with neither the old episode nor the new one.
	type previous struct {
		entry    metadb.EpisodeEntry
		hadEntry bool
		hadStub  bool
		path     string
		key      string
	}
	var prior []previous

	rollback := func() {
		for i, f := range out {
			if !prior[i].hadStub {
				if err := m.deleteStub(ctx, f.Path); err != nil {
					m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: cannot undo %s: %v", f.Path, err), "path", f.Path, "err", err)
				}
			}
			key := prior[i].key
			var err error
			if prior[i].hadEntry {
				err = m.cfg.Registry.UpsertEpisode(key, prior[i].entry)
			} else {
				err = m.cfg.Registry.DeleteEpisode(key)
			}
			if err != nil {
				m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: cannot undo the registry entry for %s: %v", key, err), "key", key, "err", err)
			}
		}
	}

	for _, w := range wanted {
		seasonDir := filepath.Join(showDir, fmt.Sprintf("Season.%02d", w.season))
		path := filepath.Join(seasonDir, EpisodeFilename(req.Title, w.season, w.episode, hash[:8]))
		key := w.key

		// Read the previous state before touching anything: a registry we cannot read
		// is a registry we cannot restore, so the add stops here rather than
		// overwriting a row blind.
		p := previous{path: path, key: key}
		prev, ok, err := m.cfg.Registry.GetEpisode(key)
		if err != nil {
			rollback()
			return addResult{}, errf(http.StatusServiceUnavailable, "cannot read the registry entry for %s: %v", key, err)
		}
		if ok && prev != nil {
			p.entry, p.hadEntry = *prev, true
		}
		// The policy read and this one must agree: the row about to be replaced is
		// judged again, so a change between them cannot turn into a replacement the
		// policy refused.
		if req.Replace != ReplaceAlways {
			if v, verr := m.replaceVerdict(req.Replace, key, path, req.QualityScore); verr != nil || v.refused {
				rollback()
				return addResult{}, errf(http.StatusConflict, "the registry entry for %s changed during the add", key)
			}
		}
		if _, err := os.Stat(path); err == nil {
			p.hadStub = true
		}

		// Episode stubs carry no imdb id, as the TV sync writes them: the webhook
		// matcher pairs a Plex episode event with an open file by looking at states
		// whose id is empty, and a series id here takes the episode out of it.
		if err := WriteStub(path, m.streamURL(hash, w.file.ID), w.file.Length, magnet, ""); err != nil {
			rollback()
			return addResult{}, errf(http.StatusInternalServerError, "cannot write the stub for S%02dE%02d: %v", w.season, w.episode, err)
		}
		out = append(out, AddedFile{
			Path: path, FusePath: m.fusePath(path), Size: w.file.Length,
			FileIndex: w.file.ID, Season: w.season, Episode: w.episode,
		})
		prior = append(prior, p)

		// UpsertEpisode replaces the row, so the show id a previous sync stored would
		// be wiped by an add through the API. It is carried over instead: the id is
		// how a dead release is traced back to TMDB, and this path has none of its own.
		// Reuse the row already read above: reading again would cost a query per
		// episode and, on error, blank the id this very block exists to preserve.
		showIMDB := p.entry.ShowIMDB
		if err := m.cfg.Registry.UpsertEpisode(key, metadb.EpisodeEntry{
			EpisodeKey: key, QualityScore: req.QualityScore, Hash: hash,
			FilePath: path, Source: "api", Created: time.Now().Unix(), ShowIMDB: showIMDB,
		}); err != nil {
			rollback()
			return addResult{}, errf(http.StatusInternalServerError, "cannot register S%02dE%02d: %v", w.season, w.episode, err)
		}
		// The episode is back on disk: an open gap for it is stale, and a client that
		// read the gap list and added it here would otherwise keep seeing its own hole.
		if gc, ok := m.cfg.Registry.(interface{ ClearEpisodeGap(string) error }); ok {
			// A gap stored under the other key form is this episode's too.
			keys := []string{key}
			if folded := EpisodeKey(req.Title, w.season, w.episode); folded != key {
				keys = append(keys, folded)
			} else if legacy := legacyEpisodeKey(req.Title, w.season, w.episode); legacy != key {
				keys = append(keys, legacy)
			}
			for _, k := range keys {
				if err := gc.ClearEpisodeGap(k); err != nil {
					m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: cannot clear the gap for %s: %v", k, err), "k", k, "err", err)
				}
			}
		}
	}

	// The pack is complete: now the releases it replaced can go.
	var replaced []ReplacedFile
	for _, p := range prior {
		if !p.hadEntry || p.entry.FilePath == "" || p.entry.FilePath == p.path {
			continue
		}
		old := ReplacedFile{Path: p.entry.FilePath, Hash: p.entry.Hash, FileIndex: stubIndex(p.entry.FilePath)}
		_, statErr := os.Stat(p.entry.FilePath)
		stubWasThere := statErr == nil
		if err := m.deleteStub(ctx, p.entry.FilePath); err != nil {
			// Leaving it there keeps its torrent alive, which is the safe half of the
			// failure, but it is a leak either way.
			m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: cannot remove the replaced stub %s: %v", p.entry.FilePath, err), "file_path", p.entry.FilePath, "err", err)
			continue
		}
		if stubWasThere { // a row whose stub was already gone replaced no file
			replaced = append(replaced, old)
		}
		if p.entry.Hash != "" && p.entry.Hash != hash {
			dropped[p.entry.Hash] = true
		}
	}
	return addResult{files: out, replaced: replaced, skipped: skipped}, nil
}

// replaceDecision is the registry's answer for one episode.
type replaceDecision struct {
	refused   bool
	reason    string               // why it was refused
	replacing *metadb.EpisodeEntry // the row an allowed add would replace, nil when none
}

// replaceVerdict decides one episode against the registry: when a row holds a release
// stored at another path than this add's, policy says whether it may be replaced.
// never refuses; if-better replaces only for a score greater than the stored one (0 is
// never better, so a caller that sends no score cannot replace by accident); always
// replaces. No row, an empty row and the same stub are not a replacement. It reads the
// registry only: no engine call, so it is safe before one, and a dry run is exactly
// this.
func (m *Manager) replaceVerdict(policy, key, path string, score int) (replaceDecision, error) {
	prev, ok, err := m.cfg.Registry.GetEpisode(key)
	if err != nil {
		return replaceDecision{}, errf(http.StatusServiceUnavailable, "cannot read the registry entry for %s: %v", key, err)
	}
	if !ok || prev == nil || prev.FilePath == "" || prev.FilePath == path {
		return replaceDecision{}, nil
	}
	// A row whose stub is gone holds no release: there is nothing to keep or replace.
	if _, serr := os.Stat(prev.FilePath); os.IsNotExist(serr) {
		return replaceDecision{}, nil
	}
	switch policy {
	case ReplaceNever:
		return replaceDecision{refused: true, reason: "replace=never: the episode is already filed"}, nil
	case ReplaceIfBetter:
		if score <= 0 || score <= prev.QualityScore {
			return replaceDecision{refused: true, reason: fmt.Sprintf(
				"replace=if-better: quality_score %d is not greater than the stored %d", score, prev.QualityScore)}, nil
		}
	}
	return replaceDecision{replacing: prev}, nil
}

// dryRun answers a dry-run add from the stubs and the registry alone. It makes no
// engine call, writes nothing and calls no hook, so a private torrent is not announced
// by asking what an add would do. Files names where the stubs would go, with no
// fuse_path (nothing exists to link); the size and file index are what the request
// said, because the file list is not fetched.
func (m *Manager) dryRun(req AddRequest, kind, hash string) (*AddResponse, error) {
	resp := &AddResponse{Hash: hash, Title: req.Title, Type: kind, Files: []AddedFile{},
		DryRun: true, Readability: ReadabilityUnchecked, Decision: DecisionAdded}
	if kind == "movie" {
		resp.Files = append(resp.Files, AddedFile{Path: m.moviePath(req, hash), FileIndex: req.FileIndex})
		return resp, nil
	}
	// A base32 spelling slices differently in alreadyPresent's stub name than the hex
	// the engine reports, so a filed release can slip past it: look again by the hex
	// spelling, for the preview only (the real add keeps v0.1.0's behaviour).
	if existing, err := m.alreadyPresent(kind, canonicalHashKey(hash), req); err != nil {
		return nil, err
	} else if len(existing) > 0 {
		for i := range existing {
			existing[i].FusePath = ""
		}
		resp.Files, resp.AlreadyPresent, resp.Decision = existing, true, DecisionAlreadyPresent
		return resp, nil
	}
	key, err := m.episodeKey(req.Title, req.Season, req.Episode)
	if err != nil {
		return nil, errf(http.StatusServiceUnavailable, "cannot read the registry entry for %s: %v",
			EpisodeKey(req.Title, req.Season, req.Episode), err)
	}
	path := m.singleEpisodePath(req, hash)
	v, err := m.replaceVerdict(req.Replace, key, path, req.QualityScore)
	if err != nil {
		return nil, err
	}
	if v.refused {
		resp.Decision = DecisionKept
		resp.Skipped = []SkippedFile{{Season: req.Season, Episode: req.Episode, Reason: v.reason}}
		return resp, nil
	}
	resp.Files = append(resp.Files, AddedFile{Path: path, FileIndex: req.FileIndex, Season: req.Season, Episode: req.Episode})
	if v.replacing != nil {
		resp.Decision = DecisionReplaced
		resp.Replaced = []ReplacedFile{{Path: v.replacing.FilePath, Hash: v.replacing.Hash,
			FileIndex: stubIndex(v.replacing.FilePath)}}
	}
	return resp, nil
}

// singleEpisodePath is where a single-episode add of this release would write its stub.
// The tag is the hex spelling, as the engine reports it, so a base32 request names the
// stub the real add writes. Only the pre-engine check and the dry run use it.
func (m *Manager) singleEpisodePath(req AddRequest, hash string) string {
	hash = canonicalHashKey(hash)
	return filepath.Join(m.cfg.TVDir, ShowFolderName(req.Title, req.FirstAirDate),
		fmt.Sprintf("Season.%02d", req.Season),
		EpisodeFilename(req.Title, req.Season, req.Episode, hash[:8]))
}

// deleteStub removes one stub and drops the FUSE layer's cached state for it.
func (m *Manager) deleteStub(_ context.Context, path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if m.cfg.InvalidatePath != nil {
		m.cfg.InvalidatePath(path)
	}
	return nil
}

// dropTorrentIfUnused removes a torrent only once no stub points at it any more: a
// season pack is one torrent behind many episodes, and removing one episode must not
// make the others unstreamable.
//
// It holds the hash lock while it looks and decides. Without it an Add of the same hash,
// which holds that lock across the whole metadata wait with its stub not yet written,
// would look absent here and end up registered against a torrent this call had just
// removed; two concurrent Removes would both see the last stub gone and drop twice.
// It reports whether the torrent was dropped.
func (m *Manager) dropTorrentIfUnused(ctx context.Context, hash string) bool {
	// A stub that carries no hash identifies no torrent: the engine would be asked to
	// remove the empty hash.
	if hash == "" {
		return false
	}
	// Never wait: whoever holds this hash is adding that same release right now, so
	// either it needs the torrent or its own cleanup will drop it.
	unlock, ok := m.hashLocks.TryLock(hash)
	if !ok {
		return false
	}
	defer unlock()

	for _, kind := range []string{"movie", "tv"} {
		found, unreadable, err := m.scanByHash(kind, hash)
		if err != nil {
			m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: keeping torrent %s, cannot check its stubs: %v", hash, err), "hash", hash, "err", err)
			return false
		}
		if len(found) > 0 {
			return false
		}
		// A stub that could not be read may be the one that links here (another
		// episode of a pack): the torrent stays until it can be read.
		if len(unreadable) > 0 {
			m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: keeping torrent %s, %d stub(s) could not be read and may use it (first: %s)", hash, len(unreadable), unreadable[0]), "hash", hash, "unreadable", len(unreadable), "first_unreadable", unreadable[0])
			return false
		}
	}
	return m.dropTorrent(ctx, hash)
}

// pickFileForEpisode prefers the file whose name carries the requested episode number;
// a single-episode torrent that does not name it falls back to the largest video file
// that names no episode.
// Of several files that name it, the largest wins: a release's Sample/ clip names the
// episode too, and when the torrent listed it first it was filed as the episode
// (Dezoxy fork). Season packs already keep the largest file per episode.
func (m *Manager) pickFileForEpisode(req AddRequest, files []FileStat) (*FileStat, error) {
	if req.FileIndex > 0 {
		return m.pickFile(req.FileIndex, files)
	}
	var best *FileStat
	for i := range files {
		if !m.usable(files[i]) {
			continue
		}
		if season, episode := ParseSeasonEpisode(filepath.Base(files[i].Path)); episode == req.Episode &&
			season == req.Season && (best == nil || files[i].Length > best.Length) {
			best = &files[i]
		}
	}
	if best != nil {
		return best, nil
	}
	// Nothing names the episode: the largest video file that names no episode at all.
	// One that names another episode (or a special) is that episode's, never this one.
	var unnamed []FileStat
	for _, f := range files {
		if _, episode := ParseSeasonEpisode(filepath.Base(f.Path)); episode == 0 {
			unnamed = append(unnamed, f)
		}
	}
	file, err := m.pickFile(0, unnamed)
	if err != nil {
		return nil, errf(http.StatusUnprocessableEntity, "no file in the torrent names S%02dE%02d or names no episode: %v",
			req.Season, req.Episode, err)
	}
	return file, nil
}

func (m *Manager) section(kind string) int {
	if kind == "tv" {
		return m.cfg.TVSection
	}
	return m.cfg.MovieSection
}

// kindOf tells which media directory a path belongs to.
func (m *Manager) kindOf(path string) string {
	if m.cfg.TVDir != "" {
		if rel, err := filepath.Rel(filepath.Clean(m.cfg.TVDir), path); err == nil && !strings.HasPrefix(rel, "..") {
			return "tv"
		}
	}
	return "movie"
}

func (m *Manager) streamURL(hash string, fileID int) string {
	return fmt.Sprintf("%s/stream?link=%s&index=%d&play", m.cfg.GoStormURL, hash, fileID)
}

// fusePath is the stub's path as seen under the FUSE mount: the media directory's own
// name plus whatever is below it.
func (m *Manager) fusePath(path string) string {
	for _, dir := range m.mediaDirs() {
		if rel, err := filepath.Rel(dir, path); err == nil && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(filepath.Join(filepath.Base(dir), rel))
		}
	}
	return filepath.ToSlash(path)
}

func (m *Manager) mediaDirs() []string {
	var dirs []string
	if m.cfg.MoviesDir != "" {
		dirs = append(dirs, filepath.Clean(m.cfg.MoviesDir))
	}
	if m.cfg.TVDir != "" {
		dirs = append(dirs, filepath.Clean(m.cfg.TVDir))
	}
	return dirs
}

// findByHash returns the stubs already on disk that link to this info hash. It matches
// the full 40-hex hash in each stub's own link, never the file name: older tools tagged
// movie stubs with the last 8 hash chars and episode stubs with the first 8, a
// deployment holds both kinds in either folder, and another torrent's stub can share
// the eight digits.
//
// Cost: one read of every .mkv stub under the folder, a few hundred bytes each, where
// the name match read none. A stub that does not parse, or links to no hash, is
// skipped and counted; the walk logs the count once.
func (m *Manager) findByHash(kind, hash string) ([]AddedFile, error) {
	out, _, err := m.scanByHash(kind, hash)
	return out, err
}

// scanByHash is findByHash that also returns the stubs it could not read (an I/O error
// or unparseable content), which are neither matches nor known not to match. Only the
// "is this torrent still used" check needs them; a stub that reads fine and carries no
// hash is in neither list.
func (m *Manager) scanByHash(kind, hash string) (out []AddedFile, unreadable []string, err error) {
	dir := m.cfg.MoviesDir
	if kind == "tv" {
		dir = m.cfg.TVDir
	}
	// readStub lowercases the link's hash; compare it with the hex spelling, which is
	// also what a base32 request means.
	hash = canonicalHashKey(hash)
	skipped, firstSkipped := 0, ""
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			// A directory we cannot read would silently shrink the answer, and a
			// missing stub here means a duplicate add.
			return err
		}
		if info.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}
		st, rerr := readStub(path)
		if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) { // a stub that vanished uses nothing
			unreadable = append(unreadable, path)
		}
		if st.Hash == "" {
			if skipped == 0 {
				firstSkipped = path
			}
			skipped++
			return nil
		}
		if st.Hash != hash {
			return nil
		}
		season, episode := ParseSeasonEpisode(filepath.Base(path))
		out = append(out, AddedFile{
			Path: path, FusePath: m.fusePath(path), Size: st.Size,
			FileIndex: st.FileIndex, Season: season, Episode: episode,
		})
		return nil
	})
	if skipped > 0 {
		m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: skipped %d stub(s) under %s with no readable hash (first: %s)", skipped, dir, firstSkipped), "skipped", skipped, "dir", dir, "first_skipped", firstSkipped)
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, errf(http.StatusInternalServerError, "cannot read %s: %v", dir, err)
	}
	return out, unreadable, nil
}

// Remove deletes the stubs for a title, the torrent behind them, and the registry
// entries pointing at them.
func (m *Manager) Remove(ctx context.Context, req RemoveRequest) (*RemoveResponse, error) {
	switch strings.ToLower(strings.TrimSpace(req.Type)) {
	case "", "movie", "movies", "film", "tv", "show", "series", "episode":
	default:
		return nil, errf(http.StatusBadRequest, "unknown type %q: use movie or tv", req.Type)
	}
	var targets []string

	switch {
	case req.Path != "":
		path, err := m.resolveInsideMediaDirs(req.Path)
		if err != nil {
			return nil, err
		}
		targets = []string{path}
	case req.Hash != "":
		hash := strings.ToLower(strings.TrimSpace(req.Hash))
		if !reInfoHash.MatchString(hash) {
			return nil, errf(http.StatusBadRequest, "malformed info hash %q", hash)
		}
		for _, kind := range []string{"movie", "tv"} {
			found, err := m.findByHash(kind, hash)
			if err != nil {
				return nil, err
			}
			for _, f := range found {
				targets = append(targets, f.Path)
			}
		}
		if len(targets) == 0 {
			return nil, errf(http.StatusNotFound, "no stub carries hash %s", hash)
		}
	default:
		return nil, errf(http.StatusBadRequest, "path or hash is required")
	}

	resp := &RemoveResponse{Removed: []string{}}
	var failed []string
	hashes := map[string]bool{}
	for _, path := range targets {
		st, _ := readStub(path)
		hash := st.Hash
		if err := m.deleteStub(ctx, path); err != nil {
			m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: cannot delete %s: %v", path, err), "path", path, "err", err)
			failed = append(failed, path)
			continue
		}
		if hash != "" {
			hashes[hash] = true
		}
		if req.Blacklist {
			if w := m.blacklist(path, hash); w != "" {
				m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: %s", w), "w", w)
				resp.Warnings = append(resp.Warnings, w)
			}
		}
		m.forgetEpisode(path)
		resp.Removed = append(resp.Removed, path)
	}

	// Only now, with the stubs gone, is it safe to decide which torrents nobody needs.
	for hash := range hashes {
		m.dropTorrentIfUnused(ctx, hash)
	}

	for _, path := range resp.Removed {
		m.scheduleRefresh(m.section(m.kindOf(path)))
	}
	m.cfg.Logger.Info(fmt.Sprintf("[LibraryAPI] Removed %d stub(s)", len(resp.Removed)))
	if len(failed) > 0 {
		return nil, errf(http.StatusInternalServerError,
			"removed %d stub(s), cannot delete: %s", len(resp.Removed), strings.Join(failed, ", "))
	}
	return resp, nil
}

// blacklist records a removal for good; a non-empty result says why it could not.
func (m *Manager) blacklist(path, hash string) string {
	switch {
	case m.cfg.Blacklist == nil:
		return fmt.Sprintf("%s removed but not blacklisted: no blacklist is configured", path)
	case hash == "":
		return fmt.Sprintf("%s removed but not blacklisted: its stub carries no info hash", path)
	}
	if err := m.cfg.Blacklist(path, hash); err != nil {
		return fmt.Sprintf("%s removed but not blacklisted: %v", path, err)
	}
	return ""
}

func (m *Manager) forgetEpisode(path string) {
	if m.cfg.Registry == nil {
		return
	}
	entries, err := m.cfg.Registry.EpisodesByFilePath(path)
	if err != nil {
		m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: registry lookup for %s: %v", path, err), "path", path, "err", err)
		return
	}
	for _, e := range entries {
		if err := m.cfg.Registry.DeleteEpisode(e.EpisodeKey); err != nil {
			m.cfg.Logger.Warn(fmt.Sprintf("[LibraryAPI] WARNING: cannot unregister %s: %v", e.EpisodeKey, err), "episode_key", e.EpisodeKey, "err", err)
		}
	}
}

// resolveInsideMediaDirs accepts an absolute path, a path relative to a media
// directory, or the fuse_path the add response returned ("movies/x.mkv"), and refuses
// anything that escapes the configured directories.
func (m *Manager) resolveInsideMediaDirs(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errf(http.StatusBadRequest, "empty path")
	}
	candidates := []string{path}
	if !filepath.IsAbs(path) {
		for _, dir := range m.mediaDirs() {
			candidates = append(candidates,
				filepath.Join(filepath.Dir(dir), path),
				filepath.Join(dir, path))
		}
	}

	inBounds := ""
	for _, c := range candidates {
		clean := filepath.Clean(c)
		if !filepath.IsAbs(clean) || !m.insideMediaDirs(clean) {
			continue
		}
		if info, err := os.Stat(clean); err == nil {
			if info.IsDir() || !strings.HasSuffix(strings.ToLower(clean), ".mkv") {
				return "", errf(http.StatusBadRequest, "%s is not a stub file", path)
			}
			return clean, nil
		}
		if inBounds == "" {
			inBounds = clean
		}
	}
	if inBounds != "" {
		return "", errf(http.StatusNotFound, "no stub at %s", path)
	}
	return "", errf(http.StatusBadRequest, "%s is outside the media directories", path)
}

func (m *Manager) insideMediaDirs(clean string) bool {
	for _, dir := range m.mediaDirs() {
		if rel, err := filepath.Rel(dir, clean); err == nil &&
			rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// List reports the stubs on disk. It is how a client with no filesystem access knows
// what the library already holds.
func (m *Manager) List(kind string) ([]Item, error) {
	dir := m.cfg.MoviesDir
	if strings.HasPrefix(strings.ToLower(kind), "tv") {
		dir = m.cfg.TVDir
	}
	items := []Item{}
	if dir == "" {
		return items, nil
	}
	slot := 0
	if dir == m.cfg.TVDir && dir != m.cfg.MoviesDir {
		slot = 1
	}
	var linked map[string]struct{}
	if m.cfg.Linked != nil {
		linked = m.cfg.Linked()
	}
	unlinked := 0
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(strings.ToLower(path), ".mkv") {
			return nil
		}
		st, _ := readStub(path)
		season, episode := ParseSeasonEpisode(filepath.Base(path))
		it := Item{
			Path: path, FusePath: m.fusePath(path), Size: st.Size, Hash: st.Hash,
			IMDB: st.IMDB, FileIndex: st.FileIndex, Season: season, Episode: episode,
			ModTime: info.ModTime(),
		}
		if linked != nil {
			_, ok := linked[filepath.Base(path)]
			it.Linked = &ok
			if !ok {
				unlinked++
			}
		}
		items = append(items, it)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, errf(http.StatusInternalServerError, "cannot read %s: %v", dir, err)
	}
	if linked != nil {
		m.unlinked[slot].Store(&unlinked)
	} else {
		m.unlinked[slot].Store(nil)
	}
	return items, nil
}

// UnlinkedStubs is the number of stubs no library symlink points at, as of the last
// list of movies and the last list of tv. It reads no disk. It reports false until both
// have been listed with a linked set to compare against.
func (m *Manager) UnlinkedStubs() (int, bool) {
	a, b := m.unlinked[0].Load(), m.unlinked[1].Load()
	if a == nil || b == nil {
		return 0, false
	}
	return *a + *b, true
}

// stub is what a virtual .mkv on disk says about itself.
type stub struct {
	Size      int64
	Hash      string
	IMDB      string
	FileIndex int
}

// stubIndex is the file index a stub carries, 0 when it cannot be read.
func stubIndex(path string) int {
	st, _ := readStub(path)
	return st.FileIndex
}

// readStub reads a stub file. Legacy line-based stubs carry the stream URL on the first
// line and nothing else. It returns the zero stub and the error when the file cannot be
// read or is JSON that does not parse: such a stub is unknown, which is not the same as
// a stub that reads fine and links to no hash (a nil error with an empty Hash).
func readStub(path string) (stub, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return stub{}, err
	}
	content := strings.TrimSpace(string(data))

	url := ""
	out := stub{}
	if strings.HasPrefix(content, "{") {
		var obj struct {
			URL  string `json:"url"`
			Size int64  `json:"size"`
			IMDB string `json:"imdb"`
		}
		if err := json.Unmarshal(data, &obj); err != nil {
			return stub{}, err
		}
		url, out.Size, out.IMDB = obj.URL, obj.Size, obj.IMDB
	} else {
		// Legacy line-based stub: URL, size, magnet, imdb id, one per line. The size
		// is there like in the JSON form, so reading only the URL would report every
		// one of these as a zero-byte entry with no id.
		lines := strings.Split(content, "\n")
		url = strings.TrimSpace(lines[0])
		if len(lines) > 1 {
			out.Size, _ = strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64)
		}
		if len(lines) > 3 && strings.HasPrefix(strings.TrimSpace(lines[3]), "tt") {
			out.IMDB = strings.TrimSpace(lines[3])
		} else if m := reStubIMDB.FindString(content); m != "" {
			out.IMDB = m
		}
	}

	// Older builds wrote the URL verbatim, hash in caps included, so normalise it:
	// everything downstream compares lowercase, and an empty hash would leave the
	// torrent in the engine and blacklist the release without its id.
	if m := reStubHash.FindStringSubmatch(url); len(m) > 1 {
		out.Hash = strings.ToLower(m[1])
	}
	if m := reStubIndex.FindStringSubmatch(url); len(m) > 1 {
		out.FileIndex, _ = strconv.Atoi(m[1])
	}
	return out, nil
}

// minVideoSize is the size below which a file named as a sample is dropped: a fixed
// 50 MiB, since release samples run to a few tens of MiB. It is separate from the VFS's
// own size floor for video stubs (internal/vfs/metadata_test.go), which guards opening.
const minVideoSize int64 = 50 << 20

var reSampleFolder = regexp.MustCompile(`(?i)^samples?$`)

var reSampleName = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])samples?(?:$|[^a-z0-9])`)

// isSample reports a file that is a sample by name (its own name or its immediate parent
// folder; not the root folder, which usually carries the title) and below minVideoSize.
// Samples were only dropped when a larger file named the same episode; a lone one was
// filed as the episode.
func isSample(f FileStat) bool {
	if f.Length >= minVideoSize {
		return false
	}
	p := filepath.ToSlash(f.Path)
	if reSampleName.MatchString(path.Base(p)) {
		return true
	}
	dir := path.Dir(p)
	parent := path.Base(dir)
	// A folder named just "Sample" is one anywhere. Any other parent counts only below
	// the root folder, which is the release's own name.
	if reSampleFolder.MatchString(parent) {
		return true
	}
	return strings.Contains(dir, "/") && reSampleName.MatchString(parent)
}

// usable reports whether f is a video file the add may file, logging a sample it skips.
func (m *Manager) usable(f FileStat) bool {
	if !IsVideoFile(f.Path) {
		return false
	}
	if isSample(f) {
		m.cfg.Logger.Info(fmt.Sprintf("[LibraryAPI] ignored sample %s (%d bytes)", f.Path, f.Length), "path", f.Path, "length", f.Length)
		return false
	}
	return true
}

// episodeKey returns the registry key to use for an episode of title. It is the folded
// key, except when the show was registered under the older form: its row is then
// replaced in place, not duplicated or re-keyed. The old key drops accented letters, so
// it is also another show's proper key (Elite's "lite" is Lite's). The legacy row is
// reused only when its stub sits in a show folder that folds to this title's key; any
// other row is someone else's and the folded key is used, which at worst leaves a
// duplicate, never a deleted stub.
func (m *Manager) episodeKey(title string, season, episode int) (string, error) {
	key := EpisodeKey(title, season, episode)
	legacy := legacyEpisodeKey(title, season, episode)
	if legacy == key {
		return key, nil
	}
	prev, ok, err := m.cfg.Registry.GetEpisode(legacy)
	if err != nil {
		return key, err
	}
	if ok && prev != nil && EpisodeKey(ShowNameFromEpisodePath(prev.FilePath), season, episode) == key {
		return legacy, nil
	}
	return key, nil
}
