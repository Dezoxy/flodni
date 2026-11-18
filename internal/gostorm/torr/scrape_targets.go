package torr

import (
	"errors"
	"net/url"
	"strings"

	sets "flodni/internal/gostorm/settings"

	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// ScrapeTarget is a stored torrent and the trackers its scrape may ask: the
// announce URLs of its stored record, nothing else. The URLs hold passkeys, so
// callers must not log or return them.
type ScrapeTarget struct {
	Hash     string // lower-case hex info hash
	Trackers []string
}

// StoredScrapeTargets reads the stored records only (no torrent is woken, no
// lock of the engine is taken beyond the settings read).
func StoredScrapeTargets() []ScrapeTarget {
	return scrapeTargetsOf(sets.ListTorrent(), publicTrackers)
}

// StoredScrapeTarget reads one record, for a scrape of a single torrent: the
// other records are not loaded. Nil when the hash is not stored.
func StoredScrapeTarget(hashHex string) []ScrapeTarget {
	hash, ok := parseHash(hashHex)
	if !ok {
		return nil
	}
	db := sets.GetTorrent(hash)
	if db == nil {
		return nil
	}
	return scrapeTargetsOf([]*sets.TorrentDB{db}, publicTrackers)
}

// StoredHashes is the set of stored records, by lower-case hex hash. Keys only: no
// record is read or parsed.
func StoredHashes() map[string]bool {
	keys := sets.TorrentHashes()
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[strings.ToLower(k)] = true
	}
	return out
}

// ScrapeReady says whether the stored records can be read yet: the settings
// database opens in the engine's start-up, a moment after the daemon starts.
func ScrapeReady() bool { return sets.TorrentsReady() }

// InfoBytesPrivate is the stored-record form of the private test: metadata that is
// absent, undecodable or flagged private counts as private, the same verdict
// scrapeTargetsOf reaches. The offline tracker audit uses it.
func InfoBytesPrivate(infoBytes []byte) bool {
	if len(infoBytes) == 0 {
		return true
	}
	var info metainfo.Info
	if bencode.Unmarshal(infoBytes, &info) != nil {
		return true
	}
	return !infoPublic(&info)
}

func trackerHost(tr string) string {
	u, err := url.Parse(tr)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// scrapeTargetsOf is the pure part. Hard rule 3: a torrent that is private, or
// whose metadata is not known yet and so might be, is scraped at its own
// trackers only. The retrackers are appended as whole tiers, so for such a
// torrent every tier holding a host of any list we add on our own account
// (built-ins, the remote list, trackers.txt) is dropped whole, matched by
// lower-case host, not by string. That covers an entry rotated out of a list
// whose host still has another entry, another path, scheme or case; the list is
// read once per call. No DHT, PEX or public fallback exists in this path. A
// public torrent keeps its stored trackers as they are.
//
// Residual gap: a record saved before the retracker gate keeps no record of
// where a tier came from, so a legacy tier in which no entry's host is in any
// current list cannot be told apart from the torrent's own.
func scrapeTargetsOf(list []*sets.TorrentDB, public func() []string) []ScrapeTarget {
	var publicHosts map[string]bool
	hosts := func() map[string]bool {
		if publicHosts == nil {
			publicHosts = map[string]bool{}
			for _, tr := range public() {
				if h := trackerHost(tr); h != "" {
					publicHosts[h] = true
				}
			}
		}
		return publicHosts
	}
	var out []ScrapeTarget
	for _, db := range list {
		if db == nil || db.TorrentSpec == nil {
			continue
		}
		var info *metainfo.Info
		if len(db.InfoBytes) > 0 {
			var i metainfo.Info
			if bencode.Unmarshal(db.InfoBytes, &i) == nil {
				info = &i
			}
		}
		strict := !infoPublic(info)
		seen := map[string]bool{}
		var trackers []string
	tiers:
		for _, tier := range db.Trackers {
			if strict {
				for _, tr := range tier {
					if hosts()[trackerHost(tr)] {
						continue tiers
					}
				}
			}
			for _, tr := range tier {
				if tr == "" || seen[tr] {
					continue
				}
				seen[tr] = true
				trackers = append(trackers, tr)
			}
		}
		out = append(out, ScrapeTarget{Hash: db.InfoHash.HexString(), Trackers: trackers})
	}
	return out
}

// ScrapeNet is how the engine reaches HTTP trackers, for the scrape to do the
// same: through its proxy, with its user agent, so a passkey never leaves from
// the real address when a proxy is set.
type ScrapeNet struct {
	Proxy     *url.URL // nil: direct
	UserAgent string
	// UDP is false when a proxy is configured: UDP cannot go through an HTTP proxy.
	UDP bool
}

// ScrapeNetwork reads the engine's proxy settings. An invalid proxy is an error:
// the scrape must not run direct then.
func ScrapeNetwork() (ScrapeNet, error) {
	n := ScrapeNet{UserAgent: trackerUserAgent, UDP: true}
	if sets.Args == nil || sets.Args.ProxyURL == "" {
		return n, nil
	}
	n.UDP = false
	u, err := url.Parse(sets.Args.ProxyURL)
	if err != nil || u.Host == "" {
		return n, errors.New("invalid proxy URL")
	}
	switch u.Scheme {
	case "socks5", "socks5h", "http", "https":
	default:
		return n, errors.New("proxy protocol not supported by the scrape")
	}
	// Mode "peers" leaves the engine's tracker requests direct; so are ours.
	if sets.Args.ProxyMode != "peers" {
		n.Proxy = u
	}
	return n, nil
}
