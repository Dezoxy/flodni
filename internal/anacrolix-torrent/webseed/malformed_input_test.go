package webseed

import (
	"net/http"
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
