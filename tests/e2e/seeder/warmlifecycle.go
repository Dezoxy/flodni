package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// The warm-lifecycle scenario files title A at once and keeps seeding four titles
// while run.sh holds A open. run.sh asks for the others by creating add-b, add-c and
// add-p in the shared directory: B and C are private and filed while the warmer is
// paused for A's playback, P is public and is the positive control of the canary
// tracker. The canary stands for a public tracker: Flodni learns its address from
// trackers.txt, which only a public torrent may use, and every announce it gets is
// appended to canary.log (the info hash in hex). The private titles must never
// reach it.
const (
	warmLifecycleSize   = 40<<20 + 4321 // above the stub floor, past the head and tail windows
	warmLifecyclePublic = 24<<20 + 99
	canaryPort          = 6970
)

type lifecycleTitle struct {
	letter string
	public bool
	mi     *metainfo.MetaInfo
	hash   string
}

func (t *lifecycleTitle) releaseName() string {
	return "Flodni.E2E.WarmLifecycle." + t.letter + ".2026.1080p.WEB-DL"
}

func runWarmLifecycle(out, flodni string, peerPort, trackerPort int) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	dataDir, err := os.MkdirTemp("", "seeder-data")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(dataDir) }() // scratch space

	var titles []*lifecycleTitle
	for i, letter := range []string{"A", "B", "C", "P"} {
		t := &lifecycleTitle{letter: letter, public: letter == "P"}
		size := int64(warmLifecycleSize)
		if t.public {
			size = warmLifecyclePublic
		}
		path := filepath.Join(dataDir, t.releaseName()+".mkv")
		if err := writeRandom(path, byte(31+i), size, sha256.New()); err != nil {
			return fmt.Errorf("%s: %w", letter, err)
		}
		if t.mi, err = buildMetaInfoFlag(path, !t.public); err != nil {
			return fmt.Errorf("%s: %w", letter, err)
		}
		t.hash = t.mi.HashInfoBytes().HexString()
		log.Printf("%s: info hash %s, public %v", letter, t.hash, t.public)
		titles = append(titles, t)
	}

	ip, err := ownIPv4()
	if err != nil {
		return err
	}
	stopTracker := startTracker(trackerPort, announceHandler(ip, peerPort, nil))
	defer stopTracker()
	// canary.log exists from the start, so an empty file is a measured zero.
	canaryLog := filepath.Join(out, "canary.log")
	if err := os.WriteFile(canaryLog, nil, 0o644); err != nil {
		return err
	}
	stopCanary := startTracker(canaryPort, canaryHandler(canaryLog))
	defer stopCanary()

	cl, err := newClient(dataDir, peerPort)
	if err != nil {
		return err
	}
	defer cl.Close()
	for _, t := range titles {
		tt, err := cl.AddTorrent(t.mi)
		if err != nil {
			return fmt.Errorf("add torrent %s: %w", t.letter, err)
		}
		<-tt.GotInfo()
		tt.VerifyData()
		if err := waitComplete(ctx, tt, 2*time.Minute); err != nil {
			return fmt.Errorf("%s: %w", t.letter, err)
		}
		log.Printf("seeding %s: all %d bytes verified", t.letter, tt.Length())
	}
	for _, u := range []string{":8090/echo", ":9080/control"} {
		if err := waitHTTP(ctx, "http://"+flodni+u, 2*time.Minute); err != nil {
			return err
		}
	}

	for _, t := range titles {
		l := strings.ToLower(t.letter)
		if t.letter != "A" {
			if err := waitForFile(ctx, filepath.Join(out, "add-"+l)); err != nil {
				return err
			}
		}
		path, err := step("add "+t.letter, func() (string, error) { return t.add(ctx, flodni) })
		if err != nil {
			return err
		}
		if err := writeAtomic(filepath.Join(out, l+".hash"), t.hash); err != nil {
			return err
		}
		// The path last: run.sh waits on it.
		if err := writeAtomic(filepath.Join(out, l+".path"), path); err != nil {
			return err
		}
	}

	<-ctx.Done()
	log.Printf("stopping")
	return nil
}

func (t *lifecycleTitle) add(ctx context.Context, flodni string) (string, error) {
	var file bytes.Buffer
	if err := t.mi.Write(&file); err != nil {
		return "", fmt.Errorf("write metainfo: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	return addAccepted(ctx, flodni, map[string]any{
		"type":          "movie",
		"hash":          t.hash,
		"torrent_file":  file.Bytes(),
		"title":         "Flodni E2E WarmLifecycle " + t.letter,
		"year":          2026,
		"release_title": t.releaseName(),
		"metadata_wait": 60,
	})
}

// waitForFile blocks until path exists.
func waitForFile(ctx context.Context, path string) error {
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// canaryHandler is a public tracker that gives no peers and records every announce.
func canaryHandler(logPath string) http.Handler {
	empty, err := bencode.Marshal(map[string]any{"interval": 30, "peers": ""})
	if err != nil {
		log.Fatalf("FAIL: canary body: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/announce", func(w http.ResponseWriter, r *http.Request) {
		hash := hex.EncodeToString([]byte(r.URL.Query().Get("info_hash")))
		log.Printf("CANARY announce from %s hash=%.8s", r.RemoteAddr, hash)
		if f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			_, _ = fmt.Fprintln(f, hash)
			_ = f.Close()
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write(empty)
	})
	return mux
}
