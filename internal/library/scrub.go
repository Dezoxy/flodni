package library

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
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
	ScrubNotMagnet    = "magnet line is not a magnet"
)

// scrubBeforeRename is a test hook: it runs after the temp file is complete and
// before the final re-check and rename.
var scrubBeforeRename = func(path string) {}

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
// Each rewrite is one new file that already carries the stub's mode, owner, atime
// and mtime, renamed over the stub after a last check that the stub is still the
// file that was read (same inode, size and mtime). A stub the daemon has removed,
// replaced or rewritten since is left as it is and counted skipped. The check and
// the rename are not one step, so the instant between them is the only window left;
// stopping the daemon closes it.
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
	data, err := readNoFollow(path, before)
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
	case scrubErrNotMagnet:
		fail(ScrubNotMagnet)
		return
	}
	if dryRun {
		res.Rewritten++
		return
	}
	switch err := replaceStub(path, out, before, owner); err {
	case nil:
		res.Rewritten++
	case errChangedUnderUs:
		res.Skipped++
	default:
		fail(err.Error())
	}
}

// readNoFollow reads a stub without following a symlink swapped in since the Lstat,
// and refuses a file that is no longer the one that was examined.
func readNoFollow(path string, want os.FileInfo) ([]byte, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only
	if fi, err := f.Stat(); err != nil || !os.SameFile(fi, want) {
		return nil, errors.New("changed")
	}
	return io.ReadAll(io.LimitReader(f, maxStubBytes+1))
}

var errChangedUnderUs = errors.New("stub changed while scrubbing")

// replaceStub puts data in place of the stub as one new file that already has the
// stub's mode, owner, atime and mtime: write a temp file in the same directory, set
// all four on it, re-check that the stub is still the file that was read, rename over
// it, fsync the directory. Nothing touches the final path after the rename, so a
// lookup from the mount never sees another mtime and no path-based chown or chtimes
// can follow a link swapped in later. A stub that changed, was replaced or vanished
// since the read gives errChangedUnderUs and is left as it is.
func replaceStub(path string, data []byte, before os.FileInfo, o ownerTimes) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*") // O_EXCL, never ends in .mkv
	if err != nil {
		return errors.New(ScrubWriteFailed)
	}
	tmpName := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = tmp.Close() // best effort; the temp file is being discarded
			_ = os.Remove(tmpName)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		return errors.New(ScrubWriteFailed)
	}
	if err = tmp.Chmod(before.Mode().Perm()); err != nil {
		return errors.New(ScrubWriteFailed)
	}
	if err = chownFile(tmp, o); err != nil {
		return errors.New(ScrubOwnerFailed)
	}
	if err = tmp.Sync(); err != nil {
		return errors.New(ScrubWriteFailed)
	}
	if err = tmp.Close(); err != nil {
		return errors.New(ScrubWriteFailed)
	}
	// The temp file is this call's own, created with O_EXCL a moment ago.
	if err = os.Chtimes(tmpName, o.atime, before.ModTime()); err != nil {
		return errors.New(ScrubTimesFailed)
	}
	scrubBeforeRename(path)
	now, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, now) || now.Size() != before.Size() || !now.ModTime().Equal(before.ModTime()) {
		return errChangedUnderUs
	}
	if err = os.Rename(tmpName, path); err != nil {
		return errors.New(ScrubWriteFailed)
	}
	renamed = true
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync() // best effort: the rename has happened
		_ = d.Close()
	}
	return nil
}

type scrubOutcome int

const (
	scrubRewrite scrubOutcome = iota
	scrubClean
	scrubNotStub
	scrubErrNoHash
	scrubErrLayout
	scrubErrMagnet
	scrubErrNotMagnet
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
	// With no imdb field the id is found by a regex over the whole stub, magnet
	// included; rewriting would lose an id that only the old magnet holds.
	var imdb string
	_ = json.Unmarshal(fields["imdb"], &imdb) // absent or not a string: no id
	if imdb == "" && reStubIMDB.MatchString(magnet) {
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
	if magnet == "" || magnetIsBare(magnet) {
		return nil, scrubClean
	}
	if !strings.HasPrefix(strings.ToLower(magnet), "magnet:") {
		return nil, scrubErrNotMagnet
	}
	// readStub takes the id from line 4, else by a regex over the whole stub.
	if !(len(lines) > 3 && strings.HasPrefix(strings.TrimSpace(lines[3]), "tt")) && reStubIMDB.MatchString(magnet) {
		return nil, scrubClean
	}
	clean, ok := scrubbedMagnet(magnet, strings.TrimSpace(lines[0]))
	if !ok {
		return nil, scrubErrNoHash
	}
	lines[2] = strings.Replace(lines[2], magnet, clean, 1)
	return []byte(strings.Join(lines, "\n")), scrubRewrite
}

func isHTTPURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

var reBareMagnet = regexp.MustCompile(`^magnet:\?xt=urn:btih:[a-f0-9]{40}$`)

// magnetIsBare reports exactly the form StubMagnet writes: nothing to scrub.
func magnetIsBare(magnet string) bool { return reBareMagnet.MatchString(magnet) }

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
