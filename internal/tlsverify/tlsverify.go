// Package tlsverify counts and logs the failures of a tracker's TLS certificate check. A
// tracker's URL carries its passkey, so nothing here ever prints an error's text or a URL:
// only host:port and a fixed reason.
package tlsverify

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/url"
	"sync"
	"sync/atomic"
	"time"
)

// Reasons are the bounded values of the counter's label.
var reasons = [...]string{"unknown_authority", "hostname", "expired", "other"}

var counts [len(reasons)]atomic.Int64

// Warn receives the one-line warning. The daemon sets it; nil logs nothing.
var Warn func(string)

var now = time.Now

const (
	// warnEvery is the least gap between two warnings for one host:port.
	warnEvery = time.Hour
	// maxHosts bounds the rate-limit table; past it the table starts over.
	maxHosts = 1024
)

var (
	mu       sync.Mutex
	lastWarn = map[string]time.Time{}
)

// Classify says whether err is a certificate verification failure and, if so, its counter
// label and its wording in the warning. It reads error types only, never err.Error().
func Classify(err error) (label, text string, ok bool) {
	if err == nil {
		return "", "", false
	}
	var inv x509.CertificateInvalidError
	if errors.As(err, &inv) {
		if inv.Reason == x509.Expired {
			if inv.Cert != nil && inv.Cert.NotBefore.After(now()) {
				return "expired", "not yet valid", true
			}
			return "expired", "expired", true
		}
		return "other", "other", true
	}
	var host x509.HostnameError
	if errors.As(err, &host) {
		return "hostname", "hostname mismatch", true
	}
	var unk x509.UnknownAuthorityError
	if errors.As(err, &unk) {
		return "unknown_authority", "unknown authority", true
	}
	var cve *tls.CertificateVerificationError
	if errors.As(err, &cve) {
		return "other", "other", true
	}
	return "", "", false
}

// Report counts err when it is a certificate failure of the tracker at hostport and warns
// at most once an hour per hostport and path. via names the path that failed, "announce"
// or "scrape". Any other error is ignored.
func Report(hostport, via string, err error) {
	label, text, ok := Classify(err)
	if !ok {
		return
	}
	for i, r := range reasons {
		if r == label {
			counts[i].Add(1)
		}
	}
	mu.Lock()
	t := now()
	key := hostport + "|" + via
	if last, seen := lastWarn[key]; seen && t.Sub(last) < warnEvery {
		mu.Unlock()
		return
	}
	if len(lastWarn) >= maxHosts {
		lastWarn = map[string]time.Time{}
	}
	lastWarn[key] = t
	mu.Unlock()
	if w := Warn; w != nil {
		w("[Tracker] TLS verify failed for " + hostport + " (" + via + "): " + text)
	}
}

// Counts is the failures counted since start, by reason; every reason is present.
func Counts() map[string]int64 {
	out := make(map[string]int64, len(reasons))
	for i, r := range reasons {
		out[r] = counts[i].Load()
	}
	return out
}

func resetForTest() {
	mu.Lock()
	lastWarn = map[string]time.Time{}
	mu.Unlock()
	for i := range counts {
		counts[i].Store(0)
	}
}

// HostPort is host:port of a tracker URL, the scheme's default port filled in, and nothing
// else of the URL (no path, query or userinfo).
func HostPort(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" || u.Scheme == "wss" {
			port = "443"
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}
