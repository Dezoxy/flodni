package utils

import (
	"bytes"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"flodni/internal/gostorm/fetch"
	"flodni/internal/gostorm/torrshash"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

func ParseFile(file multipart.File) (*torrent.TorrentSpec, error) {
	minfo, err := metainfo.Load(file)
	if err != nil {
		return nil, err
	}
	info, err := minfo.UnmarshalInfo()
	if err != nil {
		return nil, err
	}

	// mag := minfo.Magnet(info.Name, minfo.HashInfoBytes())
	mag := minfo.Magnet(nil, &info)
	return &torrent.TorrentSpec{
		InfoBytes:   minfo.InfoBytes,
		Trackers:    [][]string{mag.Trackers},
		DisplayName: info.Name,
		InfoHash:    minfo.HashInfoBytes(),
	}, nil
}

func ParseLink(link string) (*torrent.TorrentSpec, error) {
	urlLink, err := url.Parse(link)
	if err != nil {
		return nil, err
	}

	switch strings.ToLower(urlLink.Scheme) {
	case "magnet":
		return fromMagnet(urlLink.String())
	case "http", "https":
		return fromHttp(urlLink.String())
	case "":
		return fromMagnet("magnet:?xt=urn:btih:" + urlLink.Path)
	default:
		err = fmt.Errorf("unknown scheme: %s", urlLink.Scheme)
	}
	return nil, err
}

// maxTorrsToken caps a torrs token before it is base62-decoded and inflated.
const maxTorrsToken = 8 << 10

// StrictHash returns the lower-case hex of an info hash given as 40 hex digits or
// 32 base32 characters (RFC 4648 alphabet, no padding), and nothing else. The
// vendored magnet parser panics on padded base32, so every caller checks first.
func StrictHash(s string) (string, bool) {
	switch len(s) {
	case 40:
		b, err := hex.DecodeString(s)
		if err != nil {
			return "", false
		}
		return hex.EncodeToString(b), true
	case 32:
		b, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(s))
		if err != nil || len(b) != 20 {
			return "", false
		}
		return hex.EncodeToString(b), true
	}
	return "", false
}

// magnetHash checks the first xt of a magnet the way the parser takes it (a btih)
// and returns its hash, before the parser sees it.
func magnetHash(link string) (string, bool) {
	u, err := url.Parse(link)
	if err != nil || !strings.EqualFold(u.Scheme, "magnet") {
		return "", false
	}
	xts := u.Query()["xt"]
	if len(xts) == 0 || len(xts[0]) < 9 || !strings.EqualFold(xts[0][:9], "urn:btih:") {
		return "", false
	}
	return StrictHash(xts[0][9:])
}

// LinkHash returns the info hash a link names, from its text alone: a magnet, a
// torrs:// token or a bare hash. It never fetches or reads anything. An http(s)
// link has no hash until it is fetched, so ok is false for it.
func LinkHash(link string) (string, bool) {
	if strings.HasPrefix(link, "torrs://") || (len(link) > 45 && torrshash.IsBase62(link)) {
		spec, _, err := ParseTorrsHash(link)
		if err != nil {
			return "", false
		}
		return spec.InfoHash.HexString(), true
	}
	u, err := url.Parse(link)
	if err != nil {
		return "", false
	}
	switch strings.ToLower(u.Scheme) {
	case "magnet":
		return magnetHash(u.String())
	case "":
		return StrictHash(u.Path)
	}
	return "", false
}

func fromMagnet(link string) (*torrent.TorrentSpec, error) {
	if _, ok := magnetHash(link); !ok {
		return nil, errors.New("magnet: no valid btih info hash")
	}
	mag, err := metainfo.ParseMagnetUri(link)
	if err != nil {
		return nil, err
	}

	var trackers [][]string
	if len(mag.Trackers) > 0 {
		trackers = [][]string{mag.Trackers}
	}

	return &torrent.TorrentSpec{
		InfoBytes:   nil,
		Trackers:    trackers,
		DisplayName: mag.DisplayName,
		InfoHash:    mag.InfoHash,
	}, nil
}

func ParseTorrsHash(token string) (*torrent.TorrentSpec, *torrshash.TorrsHash, error) {
	if strings.HasPrefix(token, "torrs://") {
		token = strings.TrimPrefix(token, "torrs://")
	}
	if len(token) > maxTorrsToken {
		return nil, nil, errors.New("torrs token too long")
	}
	th, err := torrshash.Unpack(token)
	if err != nil {
		return nil, nil, err
	}

	var trackers [][]string
	if len(th.Trackers()) > 0 {
		trackers = [][]string{th.Trackers()}
	}

	return &torrent.TorrentSpec{
		InfoBytes:   nil,
		Trackers:    trackers,
		DisplayName: th.Title(),
		InfoHash:    metainfo.NewHashFromHex(th.Hash),
	}, th, nil
}

// maxHTTPBody caps the .torrent an http(s) link may deliver. The upload route has no cap
// of its own to match, so this is 10 MiB: a .torrent of a very large multi-file release
// is a few MiB.
const maxHTTPBody = 10 << 20

// httpFetchTimeout bounds the whole fetch: connect, redirects and body.
var httpFetchTimeout = 60 * time.Second

func fromHttp(link string) (*torrent.TorrentSpec, error) {
	req, err := http.NewRequest("GET", link, nil)
	if err != nil {
		return nil, err
	}

	client := fetch.NewClient(httpFetchTimeout)
	req.Header.Set("User-Agent", "DWL/1.1.1 (Torrent)")

	resp, err := client.Do(req)
	if er, ok := err.(*url.Error); ok {
		if strings.HasPrefix(er.URL, "magnet:") {
			return fromMagnet(er.URL)
		}
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, errors.New(resp.Status)
	}

	// One byte past the cap tells a body that fits from one that does not.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxHTTPBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxHTTPBody {
		return nil, fmt.Errorf("torrent file too large: over %d bytes", maxHTTPBody)
	}
	minfo, err := metainfo.Load(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	info, err := minfo.UnmarshalInfo()
	if err != nil {
		return nil, err
	}
	// mag := minfo.Magnet(info.Name, minfo.HashInfoBytes())
	mag := minfo.Magnet(nil, &info)

	return &torrent.TorrentSpec{
		InfoBytes:   minfo.InfoBytes,
		Trackers:    [][]string{mag.Trackers},
		DisplayName: info.Name,
		InfoHash:    minfo.HashInfoBytes(),
	}, nil
}
