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
	if m, err := mt.MimeTypeByPath(filePath); err == nil && (m.IsVideo() || m.IsAudio()) {
		h.Set("Content-Type", m.String())
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
	if name == "" || name == "." || name == "/" {
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
	if torrstor.BoundedReadahead {
		if end, ok := boundedRangeEnd(req.Header.Get("Range"), file.Length()); ok {
			reader.SetRangeEnd(end)
		}
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
	// Create a context with timeout if configured
	ctx := req.Context()
	if streamTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(streamTimeout)*time.Second)
		defer cancel()
	}
	// Update request with new context
	req = req.WithContext(ctx)
	// Handle client disconnections better
	wrappedResp := &contextResponseWriter{
		ResponseWriter: resp,
		ctx:            ctx,
	}
	serveFileContent(wrappedResp, req, file.Path(), time.Unix(t.Timestamp, 0),
		streamContent(&ctxReader{Reader: reader, ctx: ctx}, req.Header.Get("Range")))

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
