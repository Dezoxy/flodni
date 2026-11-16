package library

import (
	"encoding/base32"
	"encoding/hex"
	"net/http"
	"strings"
)

// CanonicalHash spells an info hash in lowercase hex, decoding the 32-character
// base32 form a magnet may carry; anything else comes back trimmed and lowercased.
func CanonicalHash(hash string) string { return canonicalHashKey(hash) }

// canonicalHashKey folds a valid base32 info hash onto its 40-hex spelling so
// one torrent has one lock key and one ownership identity.
func canonicalHashKey(hash string) string {
	h := strings.ToLower(strings.TrimSpace(hash))
	if len(h) != 32 {
		return h
	}
	raw, err := base32.StdEncoding.DecodeString(strings.ToUpper(h))
	if err != nil || len(raw) != 20 {
		return h
	}
	return hex.EncodeToString(raw)
}

// resolveIdentity reconciles the hash and magnet a request carries. The magnet's
// own BTIH stays authoritative when no hash is supplied, but a hash that disagrees with
// it is refused: the alternative is waking and mutating a torrent the caller never named.
// Comparison uses the canonical spelling so a base32 magnet and its hex hash are one
// torrent, not a mismatch.
func resolveIdentity(reqHash, reqMagnet string) (hash, magnet string, err error) {
	hash = strings.ToLower(strings.TrimSpace(reqHash))
	magnet = strings.TrimSpace(reqMagnet)
	if err := CheckMagnet(magnet); err != nil {
		return "", "", err
	}
	fromMagnet := ""
	if magnet != "" {
		fromMagnet = HashFromMagnet(magnet)
	}
	if fromMagnet != "" && hash != "" && canonicalHashKey(fromMagnet) != canonicalHashKey(hash) {
		return "", "", errf(http.StatusBadRequest,
			"hash_magnet_mismatch: hash %s but magnet carries %s", hash, fromMagnet)
	}
	switch {
	case fromMagnet != "":
		hash = fromMagnet
	case magnet != "" && hash == "":
		return "", "", errf(http.StatusBadRequest, "magnet carries no info hash")
	}
	if hash == "" {
		return "", "", errf(http.StatusBadRequest, "hash or magnet is required")
	}
	if !reInfoHash.MatchString(hash) {
		return "", "", errf(http.StatusBadRequest, "malformed info hash %q", hash)
	}
	return hash, magnet, nil
}
