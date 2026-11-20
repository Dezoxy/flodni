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

// trickleContent hands out one byte every gap until total has passed since the first read, then
// ends (EOF) or, with stall set, blocks until the context ends. With gap 0 it blocks from the start.
// Seek claims a file far larger than any test streams.
type trickleContent struct {
	ctx   context.Context
	gap   time.Duration
	total time.Duration
	stall bool
	zero  bool // return (0, nil) until the context ends, then its error
	start time.Time
}

func (c *trickleContent) Read(p []byte) (int, error) {
	if c.start.IsZero() {
		c.start = time.Now()
	}
	if c.zero {
		if c.start.IsZero() {
			c.start = time.Now()
		}
		if time.Since(c.start) > time.Second {
			return 0, io.EOF // the idle deadline never fired: let the test fail, not hang
		}
		select {
		case <-c.ctx.Done():
			return 0, c.ctx.Err()
		default:
			return 0, nil
		}
	}
	if c.gap == 0 || time.Since(c.start) >= c.total {
		if c.gap != 0 && !c.stall {
			return 0, io.EOF
		}
		<-c.ctx.Done()
		return 0, c.ctx.Err()
	}
	select {
	case <-time.After(c.gap):
		p[0] = 'x'
		return 1, nil
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	}
}

func (c *trickleContent) Seek(off int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		return 1 << 30, nil
	}
	return off, nil
}

// streamCase is one request through serveStream, the function Torrent.Stream serves with.
type streamCase struct {
	method  string
	rng     string
	bounded bool
	mk      func(context.Context) *trickleContent
}

// run returns how the stream ended (by the counter), how long it took, and whether its context
// carried an absolute deadline.
func (c streamCase) run(t *testing.T, d time.Duration) (reason int, took time.Duration, absolute bool) {
	t.Helper()
	req := httptest.NewRequest(c.method, "/stream", nil)
	if c.rng != "" {
		req.Header.Set("Range", c.rng)
	}
	before := StreamEndCounts()
	begin := time.Now()
	serveStream(httptest.NewRecorder(), req, "a.mkv", time.Unix(0, 0), d, c.bounded,
		func(ctx context.Context) io.ReadSeeker {
			_, absolute = ctx.Deadline()
			return c.mk(ctx)
		})
	took = time.Since(begin)
	after := StreamEndCounts()
	reason = -1
	for i := range after {
		if after[i]-before[i] == 1 {
			reason = i
		}
	}
	return reason, took, absolute
}

func serveWithTimeout(t *testing.T, d time.Duration, mk func(context.Context) *trickleContent) (int, time.Duration) {
	t.Helper()
	r, took, abs := streamCase{method: http.MethodGet, mk: mk}.run(t, d)
	if abs {
		t.Error("an open-ended GET got an absolute deadline")
	}
	return r, took
}

// An open-ended stream that keeps delivering bytes is not cut at the timeout.
func TestIdleStreamSurvivesProgress(t *testing.T) {
	reason, took := serveWithTimeout(t, 100*time.Millisecond, func(ctx context.Context) *trickleContent {
		return &trickleContent{ctx: ctx, gap: 30 * time.Millisecond, total: 400 * time.Millisecond}
	})
	if reason != streamEndEOF {
		t.Errorf("reason = %s, want eof", StreamEndNames[reason])
	}
	if took < 400*time.Millisecond {
		t.Errorf("stream ended after %v, want at least 400ms", took)
	}
}

// An open-ended stream that delivers nothing ends after about the timeout, counted as timeout,
// also after it made progress first.
func TestIdleStreamEndsWithoutProgress(t *testing.T) {
	for name, mk := range map[string]func(context.Context) *trickleContent{
		"never any bytes": func(ctx context.Context) *trickleContent { return &trickleContent{ctx: ctx} },
		"stalls after progress": func(ctx context.Context) *trickleContent {
			return &trickleContent{ctx: ctx, gap: 30 * time.Millisecond, total: 150 * time.Millisecond, stall: true}
		},
	} {
		t.Run(name, func(t *testing.T) {
			reason, took := serveWithTimeout(t, 100*time.Millisecond, mk)
			if reason != streamEndTimeout {
				t.Errorf("reason = %s, want timeout", StreamEndNames[reason])
			}
			if took < 90*time.Millisecond || took > 2*time.Second {
				t.Errorf("ended after %v, want about the timeout", took)
			}
		})
	}
}

// A bounded range and a HEAD keep the absolute deadline, even while bytes keep coming; a bounded
// range is not counted as a stream end, and neither is a HEAD.
func TestBoundedAndHeadKeepAbsoluteDeadline(t *testing.T) {
	trickle := func(ctx context.Context) *trickleContent {
		return &trickleContent{ctx: ctx, gap: 30 * time.Millisecond, total: time.Hour}
	}
	for name, c := range map[string]streamCase{
		"bounded GET":  {method: http.MethodGet, rng: "bytes=0-99", bounded: true, mk: trickle},
		"open HEAD":    {method: http.MethodHead, mk: trickle},
		"bounded HEAD": {method: http.MethodHead, rng: "bytes=0-99", bounded: true, mk: trickle},
	} {
		t.Run(name, func(t *testing.T) {
			before := StreamEndCounts()
			_, _, absolute := c.run(t, 100*time.Millisecond)
			if !absolute {
				t.Error("the stream context has no absolute deadline")
			}
			if StreamEndCounts() != before {
				t.Error("the end was counted")
			}
		})
	}
	// the bounded GET really is cut while trickling
	done := make(chan time.Duration, 1)
	go func() {
		_, took, _ := streamCase{method: http.MethodGet, rng: "bytes=0-99", bounded: true, mk: trickle}.run(t, 100*time.Millisecond)
		done <- took
	}()
	select {
	case took := <-done:
		if took < 90*time.Millisecond || took > 400*time.Millisecond {
			t.Errorf("bounded GET ended after %v, want about 100ms", took)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bounded GET trickling past the timeout was not cut")
	}
}

// Reads that return no bytes are no progress: an open-ended stream spinning on (0, nil) still
// ends at the idle timeout, counted as timeout.
func TestIdleStreamZeroByteReadsAreNotProgress(t *testing.T) {
	reason, took := serveWithTimeout(t, 100*time.Millisecond, func(ctx context.Context) *trickleContent {
		return &trickleContent{ctx: ctx, zero: true}
	})
	if reason != streamEndTimeout {
		t.Errorf("reason = %s, want timeout", StreamEndNames[reason])
	}
	if took < 90*time.Millisecond || took > 2*time.Second {
		t.Errorf("ended after %v, want about the timeout", took)
	}
}

// A client that goes away ends an idle stream as cancel, not timeout.
func TestIdleStreamClientGone(t *testing.T) {
	parent, gone := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, gone)
	req := httptest.NewRequest(http.MethodGet, "/stream", nil).WithContext(parent)
	before := StreamEndCounts()
	serveStream(httptest.NewRecorder(), req, "a.mkv", time.Unix(0, 0), time.Hour, false,
		func(ctx context.Context) io.ReadSeeker { return &trickleContent{ctx: ctx} })
	after := StreamEndCounts()
	if after[streamEndCancel]-before[streamEndCancel] != 1 || after[streamEndTimeout] != before[streamEndTimeout] {
		t.Errorf("counts before %v after %v, want one cancel", before, after)
	}
}

func TestStreamEndReasonIdleCause(t *testing.T) {
	// the read saw context.Canceled, the stream's cause is the idle deadline
	e := &endRecorder{readErr: context.Canceled}
	if got := e.reason(context.DeadlineExceeded); got != streamEndTimeout {
		t.Errorf("reason = %s, want timeout", StreamEndNames[got])
	}
}
