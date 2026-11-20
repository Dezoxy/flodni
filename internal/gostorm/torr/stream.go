package torr

import (
	// "context"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/anacrolix/missinggo/v2/httptoo"
	"github.com/anacrolix/torrent"

	mt "flodni/internal/gostorm/mimetype"
	sets "flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr/state"
	"flodni/internal/gostorm/torr/storage/torrstor"
)

// Add atomic counter for concurrent streams
var activeStreams int32

type contextResponseWriter struct {
	http.ResponseWriter
	ctx context.Context
}

func (w *contextResponseWriter) Write(p []byte) (n int, err error) {
	// Check context before each write
	select {
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	default:
		return w.ResponseWriter.Write(p)
	}
}

// How an open-ended engine stream ended (flodni_engine_stream_ends_total). The pump's stream is
// the one that asks for "bytes=N-"; a bounded range is a fetch, and its end is not a stream end.
const (
	streamEndEOF       = iota // served to the end of the file
	streamEndTimeout          // the stream timeout ran out
	streamEndReadError        // the engine could not read
	streamEndCancel           // the client went away or cancelled
	numStreamEnds
)

// StreamEndNames are the reason label values of StreamEndCounts, in index order.
var StreamEndNames = [numStreamEnds]string{"eof", "timeout", "read_error", "cancel"}

var streamEnds [numStreamEnds]atomic.Int64

// StreamEndCounts is the number of open-ended streams that ended, by reason, since the process
// started. For /metrics/prometheus.
func StreamEndCounts() (out [numStreamEnds]int64) {
	for i := range out {
		out[i] = streamEnds[i].Load()
	}
	return out
}

// endRecorder keeps the first read error a stream saw. http.ServeContent drops the errors of its
// copy once the header is out, so the content wrapper below is the only place they show. A client
// that went away shows as the request's context, cancelled by the server. One goroutine serves a
// stream, so there is no lock.
type endRecorder struct{ readErr error }

func (e *endRecorder) noteRead(err error) {
	if e != nil && err != nil && err != io.EOF && e.readErr == nil {
		e.readErr = err
	}
}

// reason names how the stream ended; ctxErr is context.Cause of the stream's context. An engine
// read error that is not the context's is a read_error whatever the context did after; a context
// that ended with nothing recorded cut the stream short (it is only cancelled when the handler
// returns, after this runs), a client that went away included. Everything else ran to its end.
func (e *endRecorder) reason(ctxErr error) int {
	byCtx := func(err error) int {
		if errors.Is(err, context.DeadlineExceeded) {
			return streamEndTimeout
		}
		return streamEndCancel
	}
	isCtx := func(err error) bool {
		return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	}
	switch {
	case e.readErr != nil && !isCtx(e.readErr):
		return streamEndReadError
	case e.readErr != nil:
		// A read that met the idle deadline sees context.Canceled; the context's cause says why.
		if errors.Is(ctxErr, context.DeadlineExceeded) {
			return streamEndTimeout
		}
		return byCtx(e.readErr)
	case ctxErr != nil:
		return byCtx(ctxErr)
	}
	return streamEndEOF
}

// recordingContent notes the first read error of the content it wraps.
type recordingContent struct {
	io.ReadSeeker
	rec *endRecorder
}

func (c *recordingContent) Read(p []byte) (int, error) {
	n, err := c.ReadSeeker.Read(p)
	c.rec.noteRead(err)
	return n, err
}

// RefetchedRangeSince reports whether a piece covering [off, off+n) of file fileID completed
// again, after an eviction, no earlier than the second since falls in (see
// torrstor.Cache.RefetchedSince). A missing torrent, cache or range gives false. It lives here,
// with the other stream-side helpers, and reads one snapshot of the cache like DeliveredAt.
func (t *Torrent) RefetchedRangeSince(fileID int, off, n int64, since time.Time) bool {
	if t == nil {
		return false
	}
	cache := t.cache
	if cache == nil {
		return false
	}
	_, first, last, ok := t.rangePieces(fileID, off, n)
	return ok && cache.RefetchedSince(first, last, since.Unix())
}

// countStreamEnd counts the end of a GET stream that asked for an open-ended range.
func countStreamEnd(openEnded bool, method string, rec *endRecorder, ctxErr error) {
	if !openEnded || method != http.MethodGet {
		return
	}
	streamEnds[rec.reason(ctxErr)].Add(1)
}

// streamDeadline is the idle deadline of an open-ended stream: a timer that cancels the stream's
// context once no byte has been read for d, and that every read of n > 0 pushes back.
type streamDeadline struct {
	timer *time.Timer
	d     time.Duration
}

// progress pushes the deadline back. A nil deadline (a bounded stream) does nothing. Reset on a
// running or fired AfterFunc timer is safe from any goroutine; a late fire only cancels again.
func (s *streamDeadline) progress() {
	if s != nil {
		s.timer.Reset(s.d)
	}
}

// newStreamContext bounds a stream by d; d <= 0 means unbounded. An
// idle stream (the pump's open-ended GET) ends d after the last byte read from the engine; any
// other stream ends d after it started. Both cancel with the cause context.DeadlineExceeded, so
// countStreamEnd reads context.Cause and counts them as timeout. stop releases the timer and
// the context, and must run when the handler returns.
func newStreamContext(parent context.Context, d time.Duration, idle bool) (context.Context, *streamDeadline, func()) {
	if d <= 0 {
		ctx, cancel := context.WithCancel(parent)
		return ctx, nil, cancel
	}
	if !idle {
		ctx, cancel := context.WithTimeout(parent, d)
		return ctx, nil, cancel
	}
	ctx, cancel := context.WithCancelCause(parent)
	dl := &streamDeadline{d: d}
	dl.timer = time.AfterFunc(d, func() { cancel(context.DeadlineExceeded) })
	return ctx, dl, func() {
		dl.timer.Stop()
		cancel(context.Canceled)
	}
}

// serveStream serves the content of engine under the stream's deadline and counts how an
// open-ended GET ended. An open-ended GET (the pump's stream) ends when the engine delivers
// nothing for timeout; a bounded range or a HEAD ends that long after it started. engine builds
// the context-bound engine reader; its context is the one the deadline cancels.
func serveStream(resp http.ResponseWriter, req *http.Request, filePath string, modTime time.Time,
	timeout time.Duration, bounded bool, engine func(context.Context) io.ReadSeeker) {
	openEnded := !bounded
	ctx, idle, stop := newStreamContext(req.Context(), timeout, openEnded && req.Method == http.MethodGet)
	defer stop()
	req = req.WithContext(ctx)
	rec := &endRecorder{}
	wrappedResp := &contextResponseWriter{ResponseWriter: resp, ctx: ctx}
	content := &recordingContent{ReadSeeker: &progressContent{ReadSeeker: engine(ctx), dl: idle}, rec: rec}
	serveFileContent(wrappedResp, req, filePath, modTime, streamContent(content, req.Header.Get("Range")))
	countStreamEnd(openEnded, req.Method, rec, context.Cause(ctx))
}

// progressContent resets the idle deadline on every read that returned bytes. It wraps the
// engine reader, below any buffer, so the deadline follows what the engine delivered.
type progressContent struct {
	io.ReadSeeker
	dl *streamDeadline
}

func (c *progressContent) Read(p []byte) (int, error) {
	n, err := c.ReadSeeker.Read(p)
	if n > 0 {
		c.dl.progress()
	}
	return n, err
}

// ctxReader binds a context to torrstor.Reader's blocking Read, so http.ServeContent's internal
// reads are bounded by the stream timeout instead of blocking forever on a stalled swarm (Read()
// alone calls context.Background(), which never unblocks on a piece that never arrives).
type ctxReader struct {
	*torrstor.Reader
	ctx context.Context
}

func (r *ctxReader) Read(p []byte) (int, error) {
	return r.Reader.ReadContext(r.ctx, p)
}

// serveFileContent serves torrent file bytes. The file name and the bytes come from the
// swarm, so nothing here may be rendered by a browser on the engine's origin: only video and
// audio keep their real type, everything else (images and SVG included) is an opaque
// download, and nosniff plus a sandbox CSP stop a mislabelled file from running anyway.
func serveFileContent(resp http.ResponseWriter, req *http.Request, filePath string, modTime time.Time, content io.ReadSeeker) {
	h := resp.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox")
	if m, ok := mt.StreamType(filePath); ok {
		h.Set("Content-Type", m)
	} else {
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Content-Disposition", attachmentDisposition(filePath))
	}
	http.ServeContent(resp, req, filePath, modTime, content)
}

// attachmentDisposition builds a Content-Disposition from the last path element: control
// characters are dropped, the length is capped, and mime.FormatMediaType quotes or encodes
// the rest. It never fails; an unusable name gives a bare "attachment".
func attachmentDisposition(filePath string) string {
	name := path.Base(strings.ReplaceAll(filePath, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	if len(name) > 200 {
		name = strings.ToValidUTF8(name[:200], "")
	}
	if name == "" || name == "." || name == ".." || name == "/" {
		return "attachment"
	}
	if d := mime.FormatMediaType("attachment", map[string]string{"filename": name}); d != "" {
		return d
	}
	return "attachment"
}

func (t *Torrent) Stream(fileID int, req *http.Request, resp http.ResponseWriter) error {
	// Increment active streams counter
	streamID := atomic.AddInt32(&activeStreams, 1)
	defer atomic.AddInt32(&activeStreams, -1)
	// Stream disconnect timeout (same as torrent)
	streamTimeout := sets.BTsets.TorrentDisconnectTimeout
	// V800: In STRICT mode (non-responsive), pieces must complete SHA-1 verification before serving.
	// This takes longer than responsive (per-chunk) mode, so the fixed 15-30s absolute timeout
	// expires mid-piece on slow swarms, killing the stream and forcing reconnect thrashing.
	// Doubling the timeout for STRICT mode gives pieces enough time to complete without affecting
	// TorrentDisconnectTimeout's use elsewhere (torrent expiry, idle detection, AI Tuner).
	if !torrstor.IsResponsive() {
		streamTimeout *= 2
	}

	if !t.GotInfo() {
		http.NotFound(resp, req)
		return errors.New("torrent doesn't have info yet")
	}
	// Get file information
	st := t.Status()
	var stFile *state.TorrentFileStat
	for _, fileStat := range st.FileStats {
		if fileStat == nil {
			continue
		}
		if fileStat.Id == fileID {
			stFile = fileStat
			break
		}
	}
	if stFile == nil {
		return fmt.Errorf("file with id %v not found", fileID)
	}
	// Find the actual torrent file
	files := t.Files()
	var file *torrent.File
	for _, tfile := range files {
		if tfile.Path() == stFile.Path {
			file = tfile
			break
		}
	}
	if file == nil {
		return fmt.Errorf("file with id %v not found", fileID)
	}
	// Check file size limit
	if int64(sets.MaxSize) > 0 && file.Length() > int64(sets.MaxSize) {
		err := fmt.Errorf("file size exceeded max allowed %d bytes", sets.MaxSize)
		log.Printf("File %s size (%d) exceeded max allowed %d bytes", file.DisplayPath(), file.Length(), sets.MaxSize)
		http.Error(resp, err.Error(), http.StatusForbidden)
		return err
	}
	// Create reader with context for timeout
	reader := t.NewReader(file)
	if reader == nil {
		return errors.New("cannot create torrent reader")
	}

	// Ensure reader is always closed
	defer t.CloseReader(reader)

	// V303: Use thread-safe AdaptiveShield check
	if torrstor.IsResponsive() {
		reader.SetResponsive()
	}
	// A bounded range (FetchBlock, FetchAhead) needs no read-ahead past its end; the pump
	// asks for an open-ended one and keeps the full window (Dezoxy fork).
	end, bounded := boundedRangeEnd(req.Header.Get("Range"), file.Length())
	if bounded {
		reader.SetBounded() // for the metrics, with the read-ahead clamp on or off
	}
	if torrstor.BoundedReadahead && bounded {
		reader.SetRangeEnd(end)
	}
	// Log connection
	host, port, clerr := net.SplitHostPort(req.RemoteAddr)

	if sets.BTsets.EnableDebug {
		if clerr != nil {
			log.Printf("[Stream:%d] Connect client (Active streams: %d)", streamID, atomic.LoadInt32(&activeStreams))
		} else {
			log.Printf("[Stream:%d] Connect client %s:%s (Active streams: %d)",
				streamID, host, port, atomic.LoadInt32(&activeStreams))
		}
	}

	// Set response headers
	resp.Header().Set("Connection", "close")
	// Add timeout header if configured
	if streamTimeout > 0 {
		resp.Header().Set("X-Stream-Timeout", fmt.Sprintf("%d", streamTimeout))
	}
	// Add ETag
	etag := hex.EncodeToString([]byte(fmt.Sprintf("%s/%s", t.Hash().HexString(), file.Path())))
	resp.Header().Set("ETag", httptoo.EncodeQuotedString(etag))
	// DLNA headers
	resp.Header().Set("transferMode.dlna.org", "Streaming")
	// DLNA Seek (hardcoded to drop anacrolix/dms dependency)
	if req.Header.Get("getContentFeatures.dlna.org") != "" {
		resp.Header().Set("contentFeatures.dlna.org",
			"DLNA.ORG_OP=11;DLNA.ORG_CI=0;DLNA.ORG_FLAGS=01700000000000000000000000000000")
	}
	// Add support for range requests
	if req.Header.Get("Range") != "" {
		resp.Header().Set("Accept-Ranges", "bytes")
	}
	serveStream(resp, req, file.Path(), time.Unix(t.Timestamp, 0),
		time.Duration(streamTimeout)*time.Second, bounded,
		func(ctx context.Context) io.ReadSeeker { return &ctxReader{Reader: reader, ctx: ctx} })

	if sets.BTsets.EnableDebug {
		if clerr != nil {
			log.Printf("[Stream:%d] Disconnect client", streamID)
		} else {
			log.Printf("[Stream:%d] Disconnect client %s:%s", streamID, host, port)
		}
	}
	return nil
}

// boundedRangeEnd returns the exclusive end of a single "bytes=a-b" range, clamped to the
// file. Open-ended, suffix and multi-part ranges are not bounded.
func boundedRangeEnd(header string, length int64) (int64, bool) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(header), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, false
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok || a == "" || b == "" {
		return 0, false
	}
	start, err1 := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	last, err2 := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
	if err1 != nil || err2 != nil || start < 0 || last < start {
		return 0, false
	}
	end := last + 1
	if length > 0 && end > length {
		end = length
	}
	return end, true
}
