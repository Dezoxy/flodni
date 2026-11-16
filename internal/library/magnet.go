package library

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

var reMagnetHash = regexp.MustCompile(`(?i)xt=urn:btih:([a-z0-9]{32,40})`)

// BuildMagnet creates a magnet URL from an info hash and optional trackers.
func BuildMagnet(infoHash, name string, trackers []string) string {
	magnet := fmt.Sprintf("magnet:?xt=urn:btih:%s", infoHash)
	if name != "" {
		magnet += fmt.Sprintf("&dn=%s", url.QueryEscape(name))
	}
	for _, tr := range trackers {
		magnet += fmt.Sprintf("&tr=%s", url.QueryEscape(tr))
	}
	return magnet
}

// DefaultTrackers returns the fallback tracker list.
func DefaultTrackers() []string {
	return []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"udp://open.stealth.si:80/announce",
		"udp://tracker.torrent.eu.org:451/announce",
		"udp://exodus.desync.com:6969/announce",
		"udp://tracker.openbittorrent.com:6969/announce",
	}
}

// HashFromMagnet extracts the lowercase info hash from a magnet URI, or "".
func HashFromMagnet(magnet string) string {
	m := reMagnetHash.FindStringSubmatch(magnet)
	if len(m) < 2 {
		return ""
	}
	return strings.ToLower(m[1])
}

// reBtih is the whole value of a magnet's first xt: a 40-hex or 32-base32 btih and
// nothing after it.
var reBtih = regexp.MustCompile(`(?i)^urn:btih:([0-9a-f]{40}|[a-z2-7]{32})$`)

// CheckMagnet refuses anything that is not a magnet link whose first xt is exactly
// urn:btih: plus a 40-hex or 32-base32 hash (ADR 0004). It runs before any engine
// call because the engine reads a link by scheme: it fetches http(s) and opens
// file:, and the library's own calls into it carry the in-process admin identity.
// An empty magnet passes; the hash field is the other way to name a torrent.
//
// The hash must also be the one HashFromMagnet reports (which stays an unanchored
// search), both as written and after the engine's &amp; to & rewrite, so the
// library keys the add on the torrent the engine will take.
func CheckMagnet(magnet string) error {
	if magnet == "" {
		return nil
	}
	first, err := btihOf(magnet)
	if err != nil {
		return err
	}
	if unesc := strings.ReplaceAll(magnet, "&amp;", "&"); unesc != magnet {
		again, err := btihOf(unesc)
		if err != nil || canonicalHashKey(again) != canonicalHashKey(first) {
			return errf(http.StatusBadRequest, "magnet names two different info hashes")
		}
	}
	if canonicalHashKey(HashFromMagnet(magnet)) != canonicalHashKey(first) {
		return errf(http.StatusBadRequest, "magnet names two different info hashes")
	}
	return nil
}

// btihOf returns the hash of the first xt of a magnet link.
func btihOf(magnet string) (string, error) {
	u, err := url.Parse(magnet)
	if err != nil || !strings.EqualFold(u.Scheme, "magnet") || u.Opaque != "" || u.Host != "" || u.Path != "" {
		return "", errf(http.StatusBadRequest, "only a magnet: link is accepted, not a URL or a path")
	}
	xts := u.Query()["xt"]
	if len(xts) == 0 {
		return "", errf(http.StatusBadRequest, "magnet carries no info hash")
	}
	m := reBtih.FindStringSubmatch(xts[0])
	if m == nil {
		return "", errf(http.StatusBadRequest, "magnet's xt must be urn:btih: and a 40-hex or 32-base32 info hash")
	}
	return strings.ToLower(m[1]), nil
}
