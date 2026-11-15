package library

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"flodni/internal/atomicfile"
)

// maxStubBytes bounds what the scrub reads: a stub is a few hundred bytes, so a
// bigger .mkv is somebody's real file and is never opened.
const maxStubBytes = 64 << 10

// Scrub error reasons. Fixed strings: a reason never carries file content, so
// nothing the scrub prints can echo a magnet or a URL.
const (
	ScrubReadFailed   = "read failed"
	ScrubWalkFailed   = "directory not readable"
	ScrubLayout       = "not in the written layout, left alone"
	ScrubNoHash       = "no info hash to rebuild the magnet from"
	ScrubWriteFailed  = "write failed"
	ScrubTimesFailed  = "rewritten but mtime/atime not restored"
	ScrubOwnerFailed  = "rewritten but owner not restored"
	ScrubMagnetFormat = "magnet field is not a string"
)

// ScrubError names a stub the scrub could not finish and why (one of the
// Scrub* reasons).
type ScrubError struct{ Path, Reason string }

// ScrubResult counts what ScrubStubs saw. With a dry run Rewritten is what a real
// run would rewrite.
type ScrubResult struct {
	Scanned   int // .mkv files opened and looked at
	Rewritten int
	Skipped   int // not a stub, already clean, or changed while scrubbing
	Errors    []ScrubError
}

// ScrubStubs rewrites the magnet field of every stub under roots to StubMagnet,
// dropping tracker URLs (and the passkeys in them) that older versions wrote. Every
// other field stays as written, the file keeps its mode, owner, mtime and atime (the
// FUSE layer reports the mtime to the media server, which rescans on a change), and a
// dry run only counts. A stub already without trackers, and any file that is not a
// stub, is not written.
//
// It is safe beside a running daemon: each rewrite is atomic, and a stub that changes
// between the read and the write is skipped, not overwritten. The one window left is
// the instant between that check and the rename, so do not run it during an add.
func ScrubStubs(roots []string, dryRun bool) ScrubResult {
	var res ScrubResult
	for _, root := range roots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if path == root && os.IsNotExist(err) {
					return nil // a library with no tv (or no movies) directory yet
				}
				res.Errors = append(res.Errors, ScrubError{path, ScrubWalkFailed})
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.IsDir() || !d.Type().IsRegular() {
				return nil
			}
			name := strings.ToLower(d.Name())
			if len(name) <= len(".mkv") || !strings.HasSuffix(name, ".mkv") {
				return nil
			}
			scrubOne(path, dryRun, &res)
			return nil
		})
		if err != nil {
			res.Errors = append(res.Errors, ScrubError{root, ScrubWalkFailed})
		}
	}
	return res
}

func scrubOne(path string, dryRun bool, res *ScrubResult) {
	fail := func(reason string) { res.Errors = append(res.Errors, ScrubError{path, reason}) }
	before, err := os.Lstat(path)
	if err != nil {
		fail(ScrubReadFailed)
		return
	}
	if !before.Mode().IsRegular() || before.Size() > maxStubBytes {
		res.Skipped++
		return
	}
	owner := statOwnerTimes(before) // before the read moves the atime
	data, err := os.ReadFile(path)
	if err != nil {
		fail(ScrubReadFailed)
		return
	}
	res.Scanned++
	out, outcome := scrubStubBytes(data)
	switch outcome {
	case scrubNotStub, scrubClean:
		res.Skipped++
		return
	case scrubErrNoHash:
		fail(ScrubNoHash)
		return
	case scrubErrLayout:
		fail(ScrubLayout)
		return
	case scrubErrMagnet:
		fail(ScrubMagnetFormat)
		return
	}
	if dryRun {
		res.Rewritten++
		return
	}
	// A stub rewritten by the daemon since the read must not be put back.
	if now, err := os.Lstat(path); err != nil || now.Size() != before.Size() || !now.ModTime().Equal(before.ModTime()) {
		res.Skipped++
		return
	}
	if err := atomicfile.WriteFileKeepMode(path, out, before.Mode().Perm()); err != nil {
		fail(ScrubWriteFailed)
		return
	}
	res.Rewritten++
	if err := restoreOwnerTimes(path, owner, before.ModTime()); err != nil {
		fail(err.Error())
	}
}

type scrubOutcome int

const (
	scrubRewrite scrubOutcome = iota
	scrubClean
	scrubNotStub
	scrubErrNoHash
	scrubErrLayout
	scrubErrMagnet
)

// scrubStubBytes returns the stub with its magnet replaced, or says why it did not.
func scrubStubBytes(data []byte) ([]byte, scrubOutcome) {
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("{")) {
		return scrubJSONStub(data)
	}
	return scrubLegacyStub(data)
}

func scrubJSONStub(data []byte) ([]byte, scrubOutcome) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, scrubNotStub
	}
	var streamURL string
	if json.Unmarshal(fields["url"], &streamURL) != nil || !isHTTPURL(streamURL) {
		return nil, scrubNotStub
	}
	if _, ok := fields["size"]; !ok {
		return nil, scrubNotStub
	}
	raw, ok := fields["magnet"]
	if !ok || string(raw) == "null" {
		return nil, scrubClean
	}
	var magnet string
	if json.Unmarshal(raw, &magnet) != nil {
		return nil, scrubErrMagnet
	}
	if magnet == "" || magnetIsBare(magnet) {
		return nil, scrubClean
	}
	// The rewrite goes through the stub encoder, so it is only trusted when the
	// encoder reproduces the file as it is: any other layout would change bytes
	// besides the magnet.
	if again, err := marshalStub(fields); err != nil || !bytes.Equal(again, data) {
		return nil, scrubErrLayout
	}
	clean, ok := scrubbedMagnet(magnet, streamURL)
	if !ok {
		return nil, scrubErrNoHash
	}
	fields["magnet"], _ = json.Marshal(clean)
	out, err := marshalStub(fields)
	if err != nil {
		return nil, scrubErrLayout
	}
	return out, scrubRewrite
}

// scrubLegacyStub handles the line-based stub: URL, size, magnet, imdb id.
func scrubLegacyStub(data []byte) ([]byte, scrubOutcome) {
	lines := strings.Split(string(data), "\n")
	if len(lines) < 3 || !isHTTPURL(strings.TrimSpace(lines[0])) {
		return nil, scrubNotStub
	}
	if _, err := strconv.ParseInt(strings.TrimSpace(lines[1]), 10, 64); err != nil {
		return nil, scrubNotStub
	}
	magnet := strings.TrimSpace(lines[2])
	if !strings.HasPrefix(strings.ToLower(magnet), "magnet:") || magnetIsBare(magnet) {
		return nil, scrubClean
	}
	clean, ok := scrubbedMagnet(magnet, strings.TrimSpace(lines[0]))
	if !ok {
		return nil, scrubErrNoHash
	}
	cr := ""
	if strings.HasSuffix(lines[2], "\r") {
		cr = "\r"
	}
	lines[2] = clean + cr
	return []byte(strings.Join(lines, "\n")), scrubRewrite
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// magnetIsBare reports a magnet whose only parameter is xt: nothing to scrub.
func magnetIsBare(magnet string) bool {
	_, q, ok := strings.Cut(magnet, "?")
	if !ok {
		return false
	}
	vals, err := url.ParseQuery(q)
	return err == nil && len(vals) == 1 && len(vals["xt"]) == 1
}

// scrubbedMagnet rebuilds the magnet from the hash the old one carried, or from the
// stream link when the old one has none.
func scrubbedMagnet(magnet, streamURL string) (string, bool) {
	if h := HashFromMagnet(magnet); h != "" {
		if m, ok := StubMagnet(h); ok {
			return m, true
		}
	}
	if u, err := url.Parse(streamURL); err == nil {
		if m, ok := StubMagnet(u.Query().Get("link")); ok {
			return m, true
		}
	}
	return "", false
}

// ownerTimes is what a rewrite must give back to the new file.
type ownerTimes struct {
	atime    time.Time
	uid, gid int
	hasOwner bool
}
