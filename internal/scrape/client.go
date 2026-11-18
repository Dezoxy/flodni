package scrape

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"flodni/internal/tlsverify"
)

// MaxUDPHashes is BEP 15's practical limit of hashes per scrape packet.
const MaxUDPHashes = 70

const (
	// DefaultMaxBody bounds an HTTP scrape answer. 100 hashes need under 10 KB.
	DefaultMaxBody = 512 << 10
	maxInterval    = 24 * time.Hour
	maxCount       = 1 << 31
)

// Counts is one tracker's answer for one hash.
type Counts struct{ Seeders, Leechers int }

// Error is a failed request. Msg is a failure class or scrubbed tracker text,
// never a URL. RetryAfter is the tracker's own minimum interval, when it sent one.
type Error struct {
	Msg        string
	RetryAfter time.Duration
}

func (e *Error) Error() string { return e.Msg }

func fail(msg string) *Error { return &Error{Msg: msg} }

// classify maps a transport error to a class. The error text of net/http
// embeds the URL, so it is never used.
func classify(err error) *Error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return fail("timeout")
	}
	return fail("connection failed")
}

func capInterval(sec int64) time.Duration {
	if sec <= 0 {
		return 0
	}
	if sec > int64(maxInterval/time.Second) {
		return maxInterval
	}
	return time.Duration(sec) * time.Second
}

func newHTTPClient(timeout time.Duration, proxy *url.URL) *http.Client {
	var pf func(*http.Request) (*url.URL, error)
	if proxy != nil {
		pf = func(*http.Request) (*url.URL, error) { return proxy, nil }
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:             pf,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// HTTPScrape asks one scrape URL about every hash in a single request (BEP 48)
// and returns the answers it got: a hash the tracker omitted is absent from the
// map. The second result is the tracker's requested minimum interval.
func HTTPScrape(ctx context.Context, c *http.Client, scrapeURL string, hashes [][20]byte, maxBody int64, userAgent string) (map[[20]byte]Counts, time.Duration, *Error) {
	var q strings.Builder
	sep := "?"
	if strings.Contains(scrapeURL, "?") {
		sep = "&"
	}
	q.WriteString(scrapeURL)
	for i, h := range hashes {
		if i == 0 {
			q.WriteString(sep)
		} else {
			q.WriteByte('&')
		}
		q.WriteString("info_hash=")
		for _, b := range h {
			fmt.Fprintf(&q, "%%%02x", b)
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, q.String(), nil)
	if err != nil {
		return nil, 0, fail("bad tracker URL")
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.Do(req)
	if err != nil {
		if u, perr := url.Parse(scrapeURL); perr == nil {
			tlsverify.Report(tlsverify.HostPort(u), err)
		}
		return nil, 0, classify(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fail(fmt.Sprintf("http status %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, 0, classify(err)
	}
	if int64(len(body)) > maxBody {
		return nil, 0, fail("response too large")
	}
	return parseHTTPAnswer(body, hashes)
}

func intOf(m map[string]any, key string) (int, bool) {
	n, ok := m[key].(int64)
	if !ok || n < 0 || n >= maxCount {
		return 0, false
	}
	return int(n), true
}

func parseHTTPAnswer(body []byte, hashes [][20]byte) (map[[20]byte]Counts, time.Duration, *Error) {
	v, err := decodeBencode(body)
	top, ok := v.(map[string]any)
	if err != nil || !ok {
		return nil, 0, fail("malformed response")
	}
	var retry time.Duration
	for _, k := range []string{"min interval", "interval"} {
		if n, ok := top[k].(int64); ok {
			retry = max(retry, capInterval(n))
		}
	}
	if flags, ok := top["flags"].(map[string]any); ok {
		if n, ok := flags["min_request_interval"].(int64); ok {
			retry = max(retry, capInterval(n))
		}
	}
	if reason, ok := top["failure reason"].([]byte); ok {
		return nil, retry, &Error{Msg: "tracker: " + string(reason), RetryAfter: retry}
	}
	files, ok := top["files"].(map[string]any)
	if !ok {
		return nil, retry, &Error{Msg: "malformed response", RetryAfter: retry}
	}
	want := make(map[string]bool, len(hashes))
	for _, h := range hashes {
		want[string(h[:])] = true
	}
	out := make(map[[20]byte]Counts, len(hashes))
	for k, raw := range files {
		if !want[k] {
			continue
		}
		e, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		seeders, ok1 := intOf(e, "complete")
		leechers, ok2 := intOf(e, "incomplete")
		if !ok1 || !ok2 {
			continue
		}
		var h [20]byte
		copy(h[:], k)
		out[h] = Counts{Seeders: seeders, Leechers: leechers}
	}
	return out, retry, nil
}

const udpMagic = 0x41727101980

func rand32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// UDPScrape does BEP 15's connect and then one scrape packet for up to
// MaxUDPHashes hashes, within timeout. A short answer gives the hashes it
// covers; a longer one than the request allows is refused.
func UDPScrape(ctx context.Context, addr string, hashes [][20]byte, timeout time.Duration) (map[[20]byte]Counts, *Error) {
	if len(hashes) == 0 || len(hashes) > MaxUDPHashes {
		return nil, fail("bad batch")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", addr)
	if err != nil {
		return nil, classify(err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}

	exchange := func(pkt []byte, wantAction uint32, tx uint32, maxLen int) ([]byte, *Error) {
		if _, err := conn.Write(pkt); err != nil {
			return nil, classify(err)
		}
		buf := make([]byte, maxLen+1)
		n, err := conn.Read(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fail("timeout")
			}
			return nil, classify(err)
		}
		if n > maxLen {
			return nil, fail("response too large")
		}
		buf = buf[:n]
		if n < 8 || binary.BigEndian.Uint32(buf[4:8]) != tx {
			return nil, fail("malformed response")
		}
		switch a := binary.BigEndian.Uint32(buf[0:4]); a {
		case wantAction:
			return buf, nil
		case 3:
			return nil, fail("tracker: " + string(buf[8:]))
		}
		return nil, fail("malformed response")
	}

	tx := rand32()
	req := make([]byte, 16)
	binary.BigEndian.PutUint64(req[0:8], udpMagic)
	binary.BigEndian.PutUint32(req[8:12], 0)
	binary.BigEndian.PutUint32(req[12:16], tx)
	resp, e := exchange(req, 0, tx, 512)
	if e != nil {
		return nil, e
	}
	if len(resp) < 16 {
		return nil, fail("malformed response")
	}
	connID := binary.BigEndian.Uint64(resp[8:16])

	tx = rand32()
	req = make([]byte, 16, 16+20*len(hashes))
	binary.BigEndian.PutUint64(req[0:8], connID)
	binary.BigEndian.PutUint32(req[8:12], 2)
	binary.BigEndian.PutUint32(req[12:16], tx)
	for _, h := range hashes {
		req = append(req, h[:]...)
	}
	resp, e = exchange(req, 2, tx, 1024)
	if e != nil {
		return nil, e
	}
	if len(resp) > 8+12*len(hashes) {
		return nil, fail("response too large")
	}
	if (len(resp)-8)%12 != 0 {
		return nil, fail("malformed response")
	}
	out := make(map[[20]byte]Counts, len(hashes))
	for i := 0; i < (len(resp)-8)/12; i++ {
		o := 8 + 12*i
		out[hashes[i]] = Counts{
			Seeders:  capCount(binary.BigEndian.Uint32(resp[o : o+4])),
			Leechers: capCount(binary.BigEndian.Uint32(resp[o+8 : o+12])),
		}
	}
	return out, nil
}

// capCount keeps a UDP count below maxCount so it cannot wrap an int on a 32-bit build.
func capCount(v uint32) int { return int(min(v, maxCount-1)) }
