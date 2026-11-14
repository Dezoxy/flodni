// Command seeder is the far side of the end-to-end stream test (tests/e2e).
//
// It writes a deterministic two-file fixture, builds a private torrent for it
// whose only tracker is its own announce handler, seeds it over TCP, and then
// files the torrent into a running Flodni through the library API. It leaves
// the expected sha256 and the FUSE path in a shared directory for run.sh.
//
// With -mode replacement it files three single-file episode releases instead:
// see replacement.go. With -mode removal it files four movies: see removal.go. With
// -mode warm-lifecycle it files one private movie at once and three more titles on
// request: see warmlifecycle.go.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

const (
	releaseName = "Flodni.E2E.2026.1080p.WEB-DL"
	// The video is deliberately not a multiple of the piece size nor of 4096, and
	// larger than the stub floor (20 MiB), the engine cache (128 MiB) and the
	// 64 MiB head window.
	videoSize  = 150<<20 + 12345
	readmeSize = 3 << 10
	pieceSize  = 1 << 20
	announce   = "http://seeder:6969/announce"
)

// engineDefaultAlloc leaves MaxAllocPeerRequestDataPerConn at the engine default.
var engineDefaultAlloc bool

func main() {
	out := flag.String("out", "/e2e", "shared directory for expected.sha256 and fuse_path")
	flodni := flag.String("flodni", "flodni", "hostname of the Flodni container")
	peerPort := flag.Int("peer-port", 51413, "TCP peer port")
	trackerPort := flag.Int("tracker-port", 6969, "HTTP announce port")
	mode := flag.String("mode", "stream", "stream (one movie), replacement (three episode releases) or removal (four movies) or warm-lifecycle (titles filed on request)")
	flag.BoolVar(&engineDefaultAlloc, "engine-default-alloc", false,
		"keep the engine's 1 MiB per-peer request allowance (a slow real peer) instead of 64 MiB")
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[seeder] ")

	var err error
	switch *mode {
	case "stream":
		err = run(*out, *flodni, *peerPort, *trackerPort)
	case "replacement":
		err = runReplacement(*out, *flodni, *peerPort, *trackerPort)
	case "removal":
		err = runRemoval(*out, *flodni, *peerPort, *trackerPort)
	case "warm-lifecycle":
		err = runWarmLifecycle(*out, *flodni, *peerPort, *trackerPort)
	default:
		err = fmt.Errorf("unknown mode %q", *mode)
	}
	if err != nil {
		log.Fatalf("FAIL: %v", err)
	}
}

func run(out, flodni string, peerPort, trackerPort int) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	dataDir, err := os.MkdirTemp("", "seeder-data")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dataDir) }() // scratch space

	sum, err := writeFixture(filepath.Join(dataDir, releaseName))
	if err != nil {
		return fmt.Errorf("fixture: %w", err)
	}
	mi, err := buildMetaInfo(filepath.Join(dataDir, releaseName))
	if err != nil {
		return fmt.Errorf("metainfo: %w", err)
	}
	hash := mi.HashInfoBytes()
	log.Printf("info hash %s, video sha256 %s", hash.HexString(), sum)

	ip, err := ownIPv4()
	if err != nil {
		return err
	}
	log.Printf("own address %s:%d", ip, peerPort)

	// Announce first, so the tracker is up before any peer asks.
	stopTracker := startTracker(trackerPort, announceHandler(ip, peerPort, nil))
	defer stopTracker()

	cl, err := newClient(dataDir, peerPort)
	if err != nil {
		return err
	}
	defer cl.Close()

	t, err := cl.AddTorrent(mi)
	if err != nil {
		return fmt.Errorf("add torrent: %w", err)
	}
	<-t.GotInfo()
	t.VerifyData()
	if err := waitComplete(ctx, t, 2*time.Minute); err != nil {
		return err
	}
	log.Printf("seeding: all %d bytes verified", t.Length())

	if err := waitHTTP(ctx, fmt.Sprintf("http://%s:8090/echo", flodni), 2*time.Minute); err != nil {
		return err
	}
	if err := waitHTTP(ctx, fmt.Sprintf("http://%s:9080/control", flodni), 2*time.Minute); err != nil {
		return err
	}

	if mb := os.Getenv("E2E_GOSTORM_CACHE_MB"); mb != "" {
		if err := setGoStormCache(ctx, flodni, mb); err != nil {
			return err
		}
	}

	var torrentFile bytes.Buffer
	if err := mi.Write(&torrentFile); err != nil {
		return err
	}
	if apiToken() != "" {
		// Auth on: the same add without the header must be refused first.
		status, _, err := postAddAs(ctx, flodni, "", map[string]any{"type": "movie", "hash": hash.HexString()})
		if err != nil {
			return err
		}
		if status != http.StatusUnauthorized {
			return fmt.Errorf("unauthenticated library add: status %d, want 401", status)
		}
		log.Printf("unauthenticated library add refused: status %d", status)
		if err := checkGoStormAuth(ctx, flodni); err != nil {
			return err
		}
	}
	fusePath, err := addToLibrary(ctx, flodni, hash.HexString(), torrentFile.Bytes())
	if err != nil {
		return err
	}

	// The warm-on-add variant reads the first 8 MiB and the last 4 MiB back and
	// compares them to these, and names the torrent by hash.
	if err := writeRangeHashes(out, hash.HexString(), filepath.Join(dataDir, releaseName, releaseName+".mkv")); err != nil {
		return err
	}
	// fuse_path last: run.sh waits for both files, and this order means it never
	// reads a half-written pair.
	if err := writeAtomic(filepath.Join(out, "expected.sha256"), sum); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(out, "fuse_path"), fusePath); err != nil {
		return err
	}
	log.Printf("READY expected.sha256=%s fuse_path=%s", sum, fusePath)

	<-ctx.Done()
	// Piece data sent over the whole run, against the video size: above 1.0
	// the leecher fetched pieces more than once (evicted, then requested again).
	st := t.Stats()
	sent := st.BytesWrittenData.Int64()
	log.Printf("upload stats: piece data sent %d bytes, video %d bytes, ratio %.3f",
		sent, int64(videoSize), float64(sent)/float64(videoSize))
	log.Printf("stopping")
	return nil
}

// writeFixture writes the release directory and returns the hex sha256 of the
// video. The content is pseudo-random from fixed seeds, so every run is the same.
func writeFixture(dir string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := writeRandom(filepath.Join(dir, "a-readme.txt"), 1, readmeSize, nil); err != nil {
		return "", err
	}
	h := sha256.New()
	if err := writeRandom(filepath.Join(dir, releaseName+".mkv"), 2, videoSize, h); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeRangeHashes leaves the info hash and the sha256 of the video's first 8 MiB
// and last 4 MiB in out, for the warm-on-add variant.
func writeRangeHashes(out, hash, video string) error {
	f, err := os.Open(video)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // read-only
	sum := func(off, n int64) (string, error) {
		h := sha256.New()
		if _, err := io.Copy(h, io.NewSectionReader(f, off, n)); err != nil {
			return "", err
		}
		return hex.EncodeToString(h.Sum(nil)), nil
	}
	head, err := sum(0, 8<<20)
	if err != nil {
		return err
	}
	tail, err := sum(videoSize-4<<20, 4<<20)
	if err != nil {
		return err
	}
	for name, content := range map[string]string{"hash": hash, "head8m.sha256": head, "tail4m.sha256": tail} {
		if err := writeAtomic(filepath.Join(out, name), content); err != nil {
			return err
		}
	}
	return nil
}

func writeRandom(path string, seed byte, size int64, tee io.Writer) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	var key [32]byte
	key[0] = seed
	var w io.Writer = f
	if tee != nil {
		w = io.MultiWriter(f, tee)
	}
	if _, err := io.CopyN(w, rand.NewChaCha8(key), size); err != nil {
		_ = f.Close() // the copy error is the one reported
		return err
	}
	return f.Close()
}

func buildMetaInfo(root string) (*metainfo.MetaInfo, error) {
	return buildMetaInfoFlag(root, true)
}

// buildMetaInfoFlag is buildMetaInfo with the BEP 27 private flag chosen.
func buildMetaInfoFlag(root string, private bool) (*metainfo.MetaInfo, error) {
	info := metainfo.Info{PieceLength: pieceSize, Private: &private}
	if err := info.BuildFromFilePath(root); err != nil {
		return nil, err
	}
	b, err := bencode.Marshal(info)
	if err != nil {
		return nil, err
	}
	mi := &metainfo.MetaInfo{InfoBytes: b, Announce: announce}
	mi.SetDefaults()
	return mi, nil
}

// startTracker serves the announce handler and returns the function that stops it.
func startTracker(port int, h http.Handler) (stop func()) {
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("FAIL: announce server: %v", err)
		}
	}()
	return func() { _ = srv.Close() }
}

// newClient is the seeding client: TCP only, no discovery beyond the tracker.
func newClient(dataDir string, peerPort int) (*torrent.Client, error) {
	cfg := torrent.NewDefaultClientConfig()
	cfg.DataDir = dataDir
	cfg.DefaultStorage = storage.NewFileWithCompletion(dataDir, storage.NewMapPieceCompletion())
	cfg.ListenPort = peerPort
	cfg.NoDHT = true
	cfg.DisablePEX = true
	cfg.NoDefaultPortForwarding = true
	cfg.DisableIPv6 = true
	cfg.DisableUTP = true
	cfg.Seed = true
	// The 1 MiB default drops most of the leecher's pipelined requests, which then
	// wait out a ~35 s request timeout: reads crawled at 600 kB/s with stalls.
	// The slow-seeder variant keeps it, to exercise exactly that.
	if !engineDefaultAlloc {
		cfg.MaxAllocPeerRequestDataPerConn = 64 << 20
	}
	cl, err := torrent.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("client: %w", err)
	}
	return cl, nil
}

func ownIPv4() (net.IP, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.To4(), nil
		}
	}
	return nil, errors.New("no non-loopback IPv4 address")
}

// announceHandler answers an announce with the seeder itself as the only peer,
// whatever the event or numwant, in the compact form (BEP 23). With a non-nil
// known set, only those info hashes (hex) get the peer; any other gets none.
func announceHandler(ip net.IP, port int, known map[string]bool) http.Handler {
	peers := make([]byte, 6)
	copy(peers, ip)
	binary.BigEndian.PutUint16(peers[4:], uint16(port))
	body, err := bencode.Marshal(map[string]any{
		"interval": 30,
		"peers":    string(peers),
	})
	if err != nil {
		log.Fatalf("FAIL: announce body: %v", err)
	}
	empty, err := bencode.Marshal(map[string]any{"interval": 30, "peers": ""})
	if err != nil {
		log.Fatalf("FAIL: announce body: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/announce", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		hash := hex.EncodeToString([]byte(q.Get("info_hash")))
		log.Printf("announce from %s hash=%.8s event=%q", r.RemoteAddr, hash, q.Get("event"))
		w.Header().Set("Content-Type", "text/plain")
		if known != nil && !known[hash] {
			_, _ = w.Write(empty)
			return
		}
		_, _ = w.Write(body)
	})
	return mux
}

func waitComplete(ctx context.Context, t *torrent.Torrent, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for t.BytesCompleted() < t.Length() {
		if time.Now().After(deadline) {
			return fmt.Errorf("seed verification: %d of %d bytes after %s", t.BytesCompleted(), t.Length(), limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	return nil
}

func waitHTTP(ctx context.Context, url string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	client := &http.Client{Timeout: 5 * time.Second}
	var last error
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				log.Printf("%s is up", url)
				return nil
			}
			err = fmt.Errorf("status %d", resp.StatusCode)
		}
		last = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("%s not ready after %s: %v", url, limit, last)
}

// addToLibrary files the torrent through POST /api/library/add and returns the
// fuse_path of the first file the response lists.
func addToLibrary(ctx context.Context, flodni, hash string, torrentFile []byte) (string, error) {
	return addAccepted(ctx, flodni, map[string]any{
		"type":          "movie",
		"hash":          hash,
		"torrent_file":  torrentFile,
		"title":         "Flodni E2E",
		"year":          2026,
		"release_title": releaseName,
		"metadata_wait": 60,
	})
}

// addAccepted posts an add request that must succeed and returns the fuse_path of
// the first file the response lists.
func addAccepted(ctx context.Context, flodni string, req map[string]any) (string, error) {
	status, raw, err := postAdd(ctx, flodni, req)
	if err != nil {
		return "", err
	}
	if status < 200 || status > 299 {
		return "", fmt.Errorf("library add: status %d: %s", status, raw)
	}
	log.Printf("library add: status %d: %s", status, raw)
	var ar struct {
		Files []struct {
			FusePath string `json:"fuse_path"`
		} `json:"files"`
	}
	if err := json.Unmarshal(raw, &ar); err != nil {
		return "", fmt.Errorf("library add: response: %w: %s", err, raw)
	}
	if len(ar.Files) == 0 || ar.Files[0].FusePath == "" {
		return "", fmt.Errorf("library add: no files[0].fuse_path in %s", raw)
	}
	return ar.Files[0].FusePath, nil
}

// postAdd sends one add request and returns the status and body, whatever the status.
func postAdd(ctx context.Context, flodni string, req map[string]any) (int, []byte, error) {
	return postAddAs(ctx, flodni, apiToken(), req)
}

// checkGoStormAuth pins the engine's server on 8090 under the same token: torrent
// control without the header is refused, the readiness probe stays open.
func checkGoStormAuth(ctx context.Context, flodni string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	do := func(method, path, body string) (int, error) {
		req, err := http.NewRequestWithContext(ctx, method, "http://"+flodni+":8090"+path, strings.NewReader(body))
		if err != nil {
			return 0, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, err
		}
		resp.Body.Close()
		return resp.StatusCode, nil
	}
	status, err := do(http.MethodPost, "/torrents", `{"action":"list"}`)
	if err != nil {
		return err
	}
	if status != http.StatusUnauthorized {
		return fmt.Errorf("unauthenticated POST :8090/torrents: status %d, want 401", status)
	}
	log.Printf("unauthenticated POST :8090/torrents refused: status %d", status)
	status, err = do(http.MethodGet, "/echo", "")
	if err != nil {
		return err
	}
	if status != http.StatusOK {
		return fmt.Errorf("unauthenticated GET :8090/echo: status %d, want 200", status)
	}
	log.Printf("unauthenticated GET :8090/echo open: status %d", status)
	return nil
}

// setGoStormCache sets GoStorm's CacheSize (MB given, bytes stored) through its
// settings API: get the whole object, change one field, set it back. A set
// replaces the struct, drops torrents and reconnects the client, so it must run
// before the library add; /echo is awaited again afterwards.
func setGoStormCache(ctx context.Context, flodni, mb string) error {
	n, err := strconv.ParseInt(mb, 10, 64)
	if err != nil || n <= 0 {
		return fmt.Errorf("E2E_GOSTORM_CACHE_MB %q: want a positive integer", mb)
	}
	post := func(body []byte) ([]byte, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			"http://"+flodni+":8090/settings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if tok := apiToken(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("POST :8090/settings: status %d: %s", resp.StatusCode, raw)
		}
		return raw, nil
	}
	raw, err := post([]byte(`{"action":"get"}`))
	if err != nil {
		return err
	}
	var cur map[string]json.RawMessage
	if err := json.Unmarshal(raw, &cur); err != nil {
		return fmt.Errorf("settings get: %w", err)
	}
	log.Printf("GoStorm CacheSize before: %s bytes", cur["CacheSize"])
	cur["CacheSize"] = json.RawMessage(strconv.FormatInt(n*1024*1024, 10))
	body, err := json.Marshal(map[string]any{"action": "set", "sets": cur})
	if err != nil {
		return err
	}
	if _, err := post(body); err != nil {
		return err
	}
	if err := waitHTTP(ctx, "http://"+flodni+":8090/echo", 2*time.Minute); err != nil {
		return err
	}
	raw, err = post([]byte(`{"action":"get"}`))
	if err != nil {
		return err
	}
	var after map[string]json.RawMessage
	if err := json.Unmarshal(raw, &after); err != nil {
		return fmt.Errorf("settings get: %w", err)
	}
	log.Printf("GoStorm CacheSize after: %s bytes", after["CacheSize"])
	return nil
}

// apiToken is the control API token the stack runs with; empty means auth is off.
func apiToken() string { return os.Getenv("FLODNI_API_TOKEN") }

// postAddAs is postAdd with an explicit bearer token; "" sends no header.
func postAddAs(ctx context.Context, flodni, token string, req map[string]any) (int, []byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return 0, nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://%s:9080/api/library/add", flodni), bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if token != "" {
		hreq.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(hreq)
	if err != nil {
		return 0, nil, fmt.Errorf("library add: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, raw, nil
}

func writeAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
