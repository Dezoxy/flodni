package library

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/anacrolix/torrent/metainfo"
)

// TorrentSource is what a release carries beyond its hash: the trackers its indexer
// announces it on and, when the indexer served a .torrent, the file itself. The
// file has the metadata, so the engine never waits for the swarm to send it, and its
// trackers may carry the user's own passkey.
type TorrentSource struct {
	Hash     string
	Trackers []string
	File     []byte
	// Private is the info dict's private flag (BEP 27); only a .torrent carries it.
	Private bool
}

// ParseTorrentFile reads a .torrent: its info hash and its announce list.
func ParseTorrentFile(data []byte) (TorrentSource, error) {
	mi, err := metainfo.Load(bytes.NewReader(data))
	if err != nil {
		return TorrentSource{}, fmt.Errorf("parse .torrent: %w", err)
	}
	info, err := mi.UnmarshalInfo()
	if err != nil {
		return TorrentSource{}, fmt.Errorf("parse .torrent info: %w", err)
	}
	var trackers []string
	for _, tier := range mi.UpvertedAnnounceList() {
		trackers = append(trackers, tier...)
	}
	return TorrentSource{
		Hash:     mi.HashInfoBytes().HexString(),
		Trackers: MergeTrackers(trackers),
		File:     data,
		Private:  info.Private != nil && *info.Private,
	}, nil
}

// ParseMagnetSource reads a magnet: its info hash and its tr= trackers.
func ParseMagnetSource(magnet string) (TorrentSource, error) {
	hash := HashFromMagnet(magnet)
	if hash == "" {
		return TorrentSource{}, fmt.Errorf("magnet without an info hash")
	}
	var trackers []string
	if u, err := url.Parse(magnet); err == nil {
		trackers = u.Query()["tr"]
	}
	return TorrentSource{Hash: hash, Trackers: MergeTrackers(trackers)}, nil
}

// releaseMagnet builds the magnet an add hands the engine when the request has none.
// The public fallback trackers go only on a torrent known to be public, which takes
// its .torrent: from a bare hash the library cannot tell, and a private torrent must
// not be announced to a public tracker (hard rule 3). The magnet then carries just the
// release's own trackers, and the engine adds public ones once the metadata shows the
// torrent is public.
func releaseMagnet(hash, title string, file TorrentSource) string {
	if len(file.File) > 0 && !file.Private {
		return BuildMagnet(hash, title, MergeTrackers(DefaultTrackers(), file.Trackers))
	}
	return BuildMagnet(hash, title, file.Trackers)
}

// MergeTrackers joins tracker lists in order, dropping blanks and repeats.
func MergeTrackers(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, tr := range list {
			tr = strings.TrimSpace(tr)
			if tr == "" || seen[tr] {
				continue
			}
			seen[tr] = true
			out = append(out, tr)
		}
	}
	return out
}

// releaseFile checks a request's .torrent against the hash the request names; an
// empty file is no file.
func releaseFile(data []byte, hash string) (TorrentSource, error) {
	if len(data) == 0 {
		return TorrentSource{}, nil
	}
	src, err := ParseTorrentFile(data)
	if err != nil {
		return TorrentSource{}, errf(http.StatusBadRequest, "torrent_file: %v", err)
	}
	if canonicalHashKey(src.Hash) != canonicalHashKey(hash) {
		return TorrentSource{}, errf(http.StatusBadRequest, "torrent_file is release %s, the request names %s", src.Hash, hash)
	}
	return src, nil
}

// torrentUploader is the engine side that takes a .torrent; *GoStormClient
// has it.
type torrentUploader interface {
	UploadTorrent(ctx context.Context, data []byte, title string) (string, error)
}

// uploadReleaseFile hands the .torrent to the engine just before the add, which then
// finds the metadata and the file's trackers in place. A failed upload fails the add:
// from a bare magnet the engine would not know a private torrent is private until its
// metadata arrived, and would put it on the DHT meanwhile.
func (m *Manager) uploadReleaseFile(ctx context.Context, src TorrentSource, title string) error {
	up, ok := m.cfg.GoStorm.(torrentUploader)
	if len(src.File) == 0 || !ok {
		return nil
	}
	if _, err := up.UploadTorrent(ctx, src.File, title); err != nil {
		return errf(http.StatusBadGateway, ".torrent upload for %s failed: %v", src.Hash, err)
	}
	return nil
}
