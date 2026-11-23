package native

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// PipeResponseWriter carries torr.Stream's HTTP response into an io.Pipe. A non-2xx status
// (http.NotFound, a 403, the 416 http.ServeContent answers for a range past the end) writes its
// error text as the body; if that text reached the pipe the reader would take it for file bytes.
// The status has to reach the reader as an error and the body not at all.

// pipeThrough runs serve against a PipeResponseWriter the way the stream goroutines do (a
// deferred Close once the handler returns) and reads the pipe to the end.
func pipeThrough(t *testing.T, serve func(w http.ResponseWriter)) ([]byte, error) {
	t.Helper()
	pr, pw := io.Pipe()
	w := &PipeResponseWriter{writer: pw, header: make(http.Header)}
	go func() {
		defer pw.Close()
		serve(w)
	}()
	done := make(chan struct{})
	var data []byte
	var err error
	go func() {
		defer close(done)
		data, err = io.ReadAll(pr)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reader blocked: the writer left the pipe open")
	}
	return data, err
}

func TestPipeResponseWriterRefusesNon2xx(t *testing.T) {
	cases := []struct {
		name   string
		status string
		serve  func(w http.ResponseWriter)
	}{
		{"NotFound", "404", func(w http.ResponseWriter) {
			http.NotFound(w, httptest.NewRequest("GET", "/stream", nil))
		}},
		{"Error403", "403", func(w http.ResponseWriter) {
			http.Error(w, "file size exceeded max allowed", http.StatusForbidden)
		}},
		{"ExplicitHeaderThenBody", "500", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("boom"))
		}},
		{"ServeContentRangePastEnd", "416", func(w http.ResponseWriter) {
			req := httptest.NewRequest("GET", "/stream", nil)
			req.Header.Set("Range", "bytes=100-")
			http.ServeContent(w, req, "f.mkv", time.Time{}, bytes.NewReader(make([]byte, 100)))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := pipeThrough(t, tc.serve)
			if len(data) != 0 {
				t.Fatalf("reader got %d body bytes %q, want none", len(data), data)
			}
			if err == nil || !strings.Contains(err.Error(), tc.status) {
				t.Fatalf("reader error = %v, want one naming status %s", err, tc.status)
			}
		})
	}
}

// The deferred pw.Close() runs after the handler returns; io.Pipe keeps the first close error,
// so a read after that Close must still see the status error, not EOF.
func TestPipeResponseWriterCloseDoesNotMaskTheStatus(t *testing.T) {
	pr, pw := io.Pipe()
	w := &PipeResponseWriter{writer: pw, header: make(http.Header)}
	w.WriteHeader(http.StatusNotFound)
	pw.Close() // the deferred Close, before anything reads

	buf := make([]byte, 16)
	for i := 0; i < 2; i++ { // and it stays that way on a second read
		n, err := pr.Read(buf)
		if n != 0 || err == nil || err == io.EOF || !strings.Contains(err.Error(), "404") {
			t.Fatalf("read %d = (%d, %v), want (0, error naming 404)", i, n, err)
		}
	}
}

func TestPipeResponseWriterWriteAfterNon2xxWritesNothing(t *testing.T) {
	pr, pw := io.Pipe()
	defer pr.Close()
	w := &PipeResponseWriter{writer: pw, header: make(http.Header)}
	w.WriteHeader(http.StatusForbidden)
	n, err := w.Write([]byte("error text"))
	if n != 0 || err == nil {
		t.Fatalf("Write after 403 = (%d, %v), want (0, error)", n, err)
	}
}

func TestPipeResponseWriterPassesSuccessBodies(t *testing.T) {
	body := bytes.Repeat([]byte("0123456789"), 100)
	cases := []struct {
		name  string
		serve func(w http.ResponseWriter)
	}{
		{"ImplicitOK", func(w http.ResponseWriter) {
			_, _ = w.Write(body)
		}},
		{"ExplicitOK", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		}},
		{"ExplicitPartialContent", func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body)
		}},
		{"ServeContentRange", func(w http.ResponseWriter) {
			req := httptest.NewRequest("GET", "/stream", nil)
			req.Header.Set("Range", "bytes=0-")
			http.ServeContent(w, req, "f.mkv", time.Time{}, bytes.NewReader(body))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := pipeThrough(t, tc.serve)
			if err != nil {
				t.Fatalf("reader error = %v, want none", err)
			}
			if !bytes.Equal(data, body) {
				t.Fatalf("reader got %d bytes, want the %d-byte body unchanged", len(data), len(body))
			}
		})
	}
}

// As in net/http, the first WriteHeader wins; a later one cannot turn an error into data or
// data into an error.
func TestPipeResponseWriterFirstWriteHeaderWins(t *testing.T) {
	t.Run("ErrorThenOK", func(t *testing.T) {
		data, err := pipeThrough(t, func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusNotFound)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("body"))
		})
		if len(data) != 0 || err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("got (%q, %v), want no bytes and an error naming 404", data, err)
		}
	})
	t.Run("OKThenError", func(t *testing.T) {
		data, err := pipeThrough(t, func(w http.ResponseWriter) {
			w.WriteHeader(http.StatusOK)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte("body"))
		})
		if err != nil || string(data) != "body" {
			t.Fatalf("got (%q, %v), want the body and no error", data, err)
		}
	})
	t.Run("WriteThenError", func(t *testing.T) {
		// A Write is an implicit 200, so a WriteHeader after it changes nothing.
		data, err := pipeThrough(t, func(w http.ResponseWriter) {
			_, _ = w.Write([]byte("body"))
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("more"))
		})
		if err != nil || string(data) != "bodymore" {
			t.Fatalf("got (%q, %v), want both writes and no error", data, err)
		}
	})
}

// fakeStartStream stands in for startStream: the same pipe wiring, with serve as the response.
func fakeStartStream(serve func(w http.ResponseWriter)) func(*NativeReader, int64) error {
	return func(r *NativeReader, off int64) error {
		pr, pw := io.Pipe()
		r.pipeReader = pr
		r.pipeReaderAtomic.Store(pr)
		r.pipeWriter = pw
		r.offset = off
		w := &PipeResponseWriter{writer: pw, header: make(http.Header)}
		go func() {
			defer pw.Close()
			serve(w)
		}()
		return nil
	}
}

// ReadAt (the sequential, smart-seek and hard-seek paths all end in an io.ReadFull on the
// pipe) must not hand an error response to the pump as file bytes.
func TestReadAtSurfacesAnErrorResponse(t *testing.T) {
	old := startStreamFn
	t.Cleanup(func() { startStreamFn = old })
	startStreamFn = fakeStartStream(func(w http.ResponseWriter) {
		http.NotFound(w, httptest.NewRequest("GET", "/stream", nil))
	})

	r := &NativeReader{hash: "h", fileID: 1}
	defer r.Close()
	shortBefore, _ := ShortReadCounts()
	p := make([]byte, 64)
	n, err := r.ReadAt(p, 0)
	if n != 0 || err == nil || err == io.EOF || !strings.Contains(err.Error(), "404") {
		t.Fatalf("ReadAt = (%d, %v), want (0, error naming 404)", n, err)
	}
	if shortAfter, _ := ShortReadCounts(); shortAfter != shortBefore {
		t.Fatalf("an error response was counted as a short read")
	}
}

func TestReadAtStillServesASuccessBody(t *testing.T) {
	old := startStreamFn
	t.Cleanup(func() { startStreamFn = old })
	body := bytes.Repeat([]byte("x"), 64)
	startStreamFn = fakeStartStream(func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body)
	})

	r := &NativeReader{hash: "h", fileID: 1}
	defer r.Close()
	p := make([]byte, 64)
	n, err := r.ReadAt(p, 0)
	if n != 64 || err != nil || !bytes.Equal(p, body) {
		t.Fatalf("ReadAt = (%d, %v), want the 64-byte body", n, err)
	}
}

// FetchAhead reaches the stream through streamRangeFn, so the same response can be fed to its
// head read. FetchBlock has no such seam (it calls torr.PeekTorrent itself); it ends in the same
// io.ReadFull, so the type-level tests above cover its input.
func TestFetchAheadSurfacesAnErrorResponse(t *testing.T) {
	old := streamRangeFn
	t.Cleanup(func() { streamRangeFn = old })
	streamRangeFn = func(_ context.Context, _ string, _ int, _, _ int64, pw *io.PipeWriter) error {
		w := &PipeResponseWriter{writer: pw, header: make(http.Header)}
		go func() {
			defer pw.Close()
			http.Error(w, "file size exceeded max allowed", http.StatusForbidden)
		}()
		return nil
	}

	type fill struct {
		n    int
		done bool
		err  error
	}
	fills := make(chan fill, 4)
	c := &NativeClient{}
	buf, dest := make([]byte, 128), make([]byte, 64)
	n, err := c.FetchAhead("h", 1, 0, buf, dest, func(n int, done bool, err error) {
		fills <- fill{n, done, err}
	})
	if n != 0 || err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("FetchAhead = (%d, %v), want (0, error naming 403)", n, err)
	}
	select {
	case f := <-fills:
		if !f.done || f.n != 0 || f.err == nil || !strings.Contains(f.err.Error(), "403") {
			t.Fatalf("onFill = %+v, want done with the 403 error and no bytes", f)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("onFill never reported done")
	}
}
