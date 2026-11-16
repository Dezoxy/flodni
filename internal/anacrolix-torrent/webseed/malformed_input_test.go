package webseed

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// A malformed url-list entry in a torrent fails the request, not the process (adapted
// from upstream Tiramisu fd976eb).
func TestMalformedWebseedURLFailsTheRequest(t *testing.T) {
	cl := Client{HttpClient: http.DefaultClient, Url: "http://bad host/"}
	cl.SetInfo(&metainfo.Info{Name: "x", Length: 16384, PieceLength: 16384, Pieces: make([]byte, 20)})
	req := cl.NewRequest(RequestSpec{Start: 0, Length: 16384})
	select {
	case res := <-req.Result:
		if res.Err == nil {
			t.Fatal("a request to a malformed URL succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no result for a request to a malformed URL")
	}
}

// countingWriter records how much the client buffered.
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

// A 206 body larger than the requested range is refused before it is buffered.
func TestMalformedWebseedPartialBodyIsCapped(t *testing.T) {
	const want, sent = 16384, 8 << 20
	part := requestPart{e: RequestSpec{Start: 0, Length: want}, result: make(chan requestPartResult, 1)}
	part.result <- requestPartResult{resp: &http.Response{
		StatusCode: http.StatusPartialContent,
		Body:       io.NopCloser(io.MultiReader(bytes.NewReader(make([]byte, sent)), strings.NewReader(""))),
	}}
	var w countingWriter
	err := recvPartResult(context.Background(), &w, part)
	if err == nil {
		t.Fatal("an oversized 206 body was accepted")
	}
	if w.n > want+1024 {
		t.Fatalf("buffered %d bytes for a %d-byte range", w.n, want)
	}
}
