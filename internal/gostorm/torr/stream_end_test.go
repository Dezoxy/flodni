package torr

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestStreamEndReason(t *testing.T) {
	boom := errors.New("boom")
	for _, tc := range []struct {
		name string
		read error
		ctx  error
		want int
	}{
		{"clean", nil, nil, streamEndEOF},
		{"deadline in a read", context.DeadlineExceeded, context.DeadlineExceeded, streamEndTimeout},
		{"deadline wrapped", errors.Join(boom, context.DeadlineExceeded), nil, streamEndTimeout},
		{"canceled in a read", context.Canceled, context.Canceled, streamEndCancel},
		{"engine error", boom, nil, streamEndReadError},
		{"engine error beats a late cancel", boom, context.Canceled, streamEndReadError},
		{"unexpected eof is an error", io.ErrUnexpectedEOF, nil, streamEndReadError},
		{"context ended with nothing recorded: timeout", nil, context.DeadlineExceeded, streamEndTimeout},
		{"client gone: the request context is cancelled", nil, context.Canceled, streamEndCancel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &endRecorder{readErr: tc.read}
			if got := e.reason(tc.ctx); got != tc.want {
				t.Errorf("reason = %s, want %s", StreamEndNames[got], StreamEndNames[tc.want])
			}
		})
	}
}

// failingReader serves data, then fails with err. Seek reports the file twice as long as data,
// so the server asks for more than there is and meets the error.
type failingReader struct {
	data []byte
	err  error
	pos  int
}

func (f *failingReader) Read(p []byte) (int, error) {
	if f.pos >= len(f.data) {
		return 0, f.err
	}
	n := copy(p, f.data[f.pos:])
	f.pos += n
	return n, nil
}

func (f *failingReader) Seek(off int64, whence int) (int64, error) {
	switch whence {
	case io.SeekStart:
		f.pos = int(off)
	case io.SeekEnd:
		return int64(2*len(f.data)) + off, nil
	default:
		f.pos += int(off)
	}
	return int64(f.pos), nil
}

func serveRecorded(t *testing.T, content io.ReadSeeker, w http.ResponseWriter, ctx context.Context) *endRecorder {
	t.Helper()
	rec := &endRecorder{}
	req := httptest.NewRequest(http.MethodGet, "/stream", nil).WithContext(ctx)
	serveFileContent(&contextResponseWriter{ResponseWriter: w, ctx: ctx}, req, "a.mkv", time.Unix(0, 0),
		&recordingContent{ReadSeeker: content, rec: rec})
	return rec
}

// Through the real serve path: a read that fails mid-stream, a client that is gone and a
// stream that runs to its end each leave the matching reason.
func TestStreamEndRecordedThroughServe(t *testing.T) {
	data := bytes.Repeat([]byte("x"), 1<<16)
	t.Run("eof", func(t *testing.T) {
		rec := serveRecorded(t, bytes.NewReader(data), httptest.NewRecorder(), context.Background())
		if got := rec.reason(nil); got != streamEndEOF {
			t.Errorf("reason = %s, want eof", StreamEndNames[got])
		}
	})
	t.Run("read error", func(t *testing.T) {
		rec := serveRecorded(t, &failingReader{data: data, err: errors.New("piece lost")}, httptest.NewRecorder(), context.Background())
		if got := rec.reason(nil); got != streamEndReadError {
			t.Errorf("reason = %s, want read_error", StreamEndNames[got])
		}
	})
	t.Run("read deadline", func(t *testing.T) {
		rec := serveRecorded(t, &failingReader{data: data, err: context.DeadlineExceeded}, httptest.NewRecorder(), context.Background())
		if got := rec.reason(nil); got != streamEndTimeout {
			t.Errorf("reason = %s, want timeout", StreamEndNames[got])
		}
	})
}

// Only the open-ended stream is counted, and only a GET.
func TestCountStreamEnd(t *testing.T) {
	before := StreamEndCounts()
	countStreamEnd(false, http.MethodGet, &endRecorder{}, nil) // a bounded range: not counted
	countStreamEnd(true, http.MethodHead, &endRecorder{}, nil) // not a stream
	if StreamEndCounts() != before {
		t.Fatal("a bounded range or a HEAD was counted")
	}
	countStreamEnd(true, http.MethodGet, &endRecorder{readErr: context.Canceled}, nil)
	after := StreamEndCounts()
	if after[streamEndCancel]-before[streamEndCancel] != 1 {
		t.Errorf("cancel delta = %d, want 1", after[streamEndCancel]-before[streamEndCancel])
	}
}

// Open-ended means no usable closed range.
func TestOpenEndedRange(t *testing.T) {
	for h, want := range map[string]bool{
		"":                 true,
		"bytes=100-":       true,
		"bytes=-500":       true,
		"bytes=0-99":       false,
		"bytes=0-99,200-3": true,
	} {
		_, bounded := boundedRangeEnd(h, 1000)
		if got := !bounded; got != want {
			t.Errorf("open-ended(%q) = %v, want %v", h, got, want)
		}
	}
}

func TestRefetchedRangeSinceNilTorrent(t *testing.T) {
	var tr *Torrent
	if tr.RefetchedRangeSince(1, 0, 10, time.Now()) {
		t.Fatal("a missing torrent reported a completed piece")
	}
}
