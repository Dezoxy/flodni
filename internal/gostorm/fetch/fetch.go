package fetch

import (
	"errors"
	"net/http"
	"strings"
	"time"
)

// maxRedirects is how many redirects an outbound fetch follows, all on the host it
// started at.
const maxRedirects = 3

// NewClient is the one client for the engine's outbound http(s) fetches (a linked
// .torrent, a poster check): a total timeout, at most maxRedirects redirects, and none to
// another host. A redirect to a magnet: link is let through: it is never fetched, the
// caller reads it from the resulting error.
func NewClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: sameHostRedirect}
}

func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if strings.EqualFold(req.URL.Scheme, "magnet") {
		return nil
	}
	if len(via) > maxRedirects {
		return errors.New("redirect refused: too many redirects")
	}
	if !strings.EqualFold(req.URL.Host, via[0].URL.Host) {
		return errors.New("redirect refused: it leaves the host")
	}
	return nil
}
