package httpTracker

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type Client struct {
	hc   *http.Client
	url_ *url.URL
}

type (
	ProxyFunc       func(*http.Request) (*url.URL, error)
	DialContextFunc func(ctx context.Context, network, addr string) (net.Conn, error)
)

type NewClientOpts struct {
	Proxy          ProxyFunc
	DialContext    DialContextFunc
	ServerName     string
	AllowKeepAlive bool
	// InsecureSkipVerify turns off verification of an https tracker's certificate. The default
	// verifies it.
	InsecureSkipVerify bool
}

func NewClient(url_ *url.URL, opts NewClientOpts) Client {
	return Client{
		url_: url_,
		hc: &http.Client{
			CheckRedirect: sameOriginRedirect,
			Transport: &http.Transport{
				DialContext: opts.DialContext,
				Proxy:       opts.Proxy,
				TLSClientConfig: &tls.Config{
					InsecureSkipVerify: opts.InsecureSkipVerify,
					ServerName:         opts.ServerName,
				},
				// This is for S3 trackers that hold connections open.
				DisableKeepAlives: !opts.AllowKeepAlive,
			},
		},
	}
}

func (cl Client) Close() error {
	cl.hc.CloseIdleConnections()
	return nil
}

// maxRedirects is how many redirects an announce follows.
const maxRedirects = 3

// sameOriginRedirect follows a redirect only when it keeps the scheme and host:port of the
// request it came from, up to maxRedirects. Anything else (an https to http downgrade, another
// host) stops with the redirect's own response, which the announce reports as a bad status:
// the announce URL's query, which can carry a passkey, is never sent to the new place.
func sameOriginRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > maxRedirects {
		return http.ErrUseLastResponse
	}
	prev := via[len(via)-1].URL
	if req.URL.Scheme != prev.Scheme || !strings.EqualFold(req.URL.Host, prev.Host) {
		return http.ErrUseLastResponse
	}
	return nil
}
