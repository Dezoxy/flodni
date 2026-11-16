package jwt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"sync"
	"time"

	"flodni/internal/flog"
)

const (
	keyTTL      = 24 * time.Hour
	minRefresh  = time.Minute
	maxBackoff  = 15 * time.Minute
	fetchLimit  = 64 << 10
	fetchTimout = 5 * time.Second
)

// keyCache holds the last good key set and rations refreshes: at most one in
// flight, at most one per minute (more after failures: 1, 2, 4 ... 15 min).
type keyCache struct {
	url    string
	now    func() time.Time
	client *http.Client
	log    *slog.Logger

	mu      sync.Mutex
	set     map[string]key
	fetched time.Time
	failed  bool          // the last fetch failed
	next    time.Time     // no fetch before this
	backoff time.Duration // the wait after the next failure
	flight  chan struct{} // closed when the fetch in flight ends
}

func (c *keyCache) init(url string, now func() time.Time) {
	c.url, c.now, c.backoff, c.log = url, now, minRefresh, flog.Std
	c.client = &http.Client{
		Timeout:       fetchTimout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// get returns the key for kid, refreshing once if it is unknown.
func (c *keyCache) get(kid string) (key, error) {
	waited := false
	for {
		c.mu.Lock()
		now := c.now()
		if c.set != nil && now.Sub(c.fetched) > keyTTL {
			c.set = nil
		}
		if k, ok := c.set[kid]; ok {
			c.mu.Unlock()
			return k, nil
		}
		if ch := c.flight; ch != nil {
			c.mu.Unlock()
			<-ch
			waited = true
			continue
		}
		if waited || now.Before(c.next) {
			failed := c.failed || c.set == nil
			c.mu.Unlock()
			if failed {
				return key{}, fail(ReasonKeysUnavailable)
			}
			return key{}, fail(ReasonUnknownKid)
		}
		ch := make(chan struct{})
		c.flight = ch
		c.mu.Unlock()

		set, err := c.fetch()

		c.mu.Lock()
		now = c.now()
		c.failed = err != nil
		if err == nil {
			c.set, c.fetched, c.backoff, c.next = set, now, minRefresh, now.Add(minRefresh)
		} else {
			// The class only: the URL (path, query) and the error text stay out of the log.
			c.log.Warn("[Auth] jwks fetch failed", "class", errClass(err))
			c.next = now.Add(c.backoff)
			c.backoff = min(c.backoff*2, maxBackoff)
		}
		c.flight = nil
		close(ch)
		c.mu.Unlock()
		waited = true
	}
}

// fetch reads the JWKS: no redirects, a timeout and a size cap. Any problem,
// an empty or unusable set included, is a failure.
func (c *keyCache) fetch() (map[string]key, error) {
	resp, err := c.client.Get(c.url)
	if err != nil {
		return nil, classed(netClass(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, classed("status")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, fetchLimit+1))
	if err != nil {
		return nil, classed("network")
	}
	if len(body) > fetchLimit {
		return nil, classed("size")
	}
	return parseJWKS(body)
}

// classed is a fetch failure known by its class alone.
type classed string

func (e classed) Error() string { return "jwks: " + string(e) }

func errClass(err error) string {
	var c classed
	if errors.As(err, &c) {
		return string(c)
	}
	return "network"
}

// netClass names a transport failure without its text, which holds the URL.
func netClass(err error) string {
	var cv *tls.CertificateVerificationError
	var rh tls.RecordHeaderError
	var ne net.Error
	switch {
	case errors.As(err, &cv), errors.As(err, &rh):
		return "tls"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	}
	return "network"
}

type jwk struct {
	Kty, Kid, Use, Alg, Crv, N, E, X, Y string
}

func parseJWKS(body []byte) (map[string]key, error) {
	var doc struct{ Keys []jwk }
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, classed("format")
	}
	set := map[string]key{}
	seen := map[string]bool{}
	for _, j := range doc.Keys {
		if j.Kid == "" || seen[j.Kid] {
			return nil, classed("duplicate or missing kid")
		}
		seen[j.Kid] = true
		if k, ok := j.key(); ok {
			set[j.Kid] = k
		}
	}
	if len(set) == 0 {
		return nil, classed("no usable key")
	}
	return set, nil
}

// key vets one JWK; a key it cannot vouch for is dropped, not used.
func (j jwk) key() (key, bool) {
	if j.Use != "" && j.Use != "sig" {
		return key{}, false
	}
	switch j.Kty {
	case "RSA":
		n, ok1 := b64(j.N)
		e, ok2 := b64(j.E)
		if !ok1 || !ok2 || (j.Alg != "" && j.Alg != "RS256") {
			return key{}, false
		}
		N, E := new(big.Int).SetBytes(n), new(big.Int).SetBytes(e)
		if N.BitLen() < 2048 || !E.IsInt64() || E.Int64() < 3 || E.Int64() > 1<<31-1 || E.Bit(0) == 0 {
			return key{}, false
		}
		return key{alg: "RS256", rsa: &rsa.PublicKey{N: N, E: int(E.Int64())}}, true
	case "EC":
		x, ok1 := b64(j.X)
		y, ok2 := b64(j.Y)
		if !ok1 || !ok2 || j.Crv != "P-256" || len(x) != 32 || len(y) != 32 || (j.Alg != "" && j.Alg != "ES256") {
			return key{}, false
		}
		pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
		if err != nil { // not on the curve
			return key{}, false
		}
		return key{alg: "ES256", ec: pub}, true
	}
	return key{}, false
}
