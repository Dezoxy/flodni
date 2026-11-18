package scrape

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDeriveBEP48(t *testing.T) {
	cases := []struct {
		in, scrape, host string
		err              error
	}{
		{"http://tracker.example.org/announce", "http://tracker.example.org/scrape", "http://tracker.example.org", nil},
		{"HTTPS://Tracker.Example.org:8443/a/b/announce?k=v", "https://Tracker.Example.org:8443/a/b/scrape?k=v", "https://tracker.example.org:8443", nil},
		{"http://tracker.example.org/announce.php?x=1&y=2", "http://tracker.example.org/scrape.php?x=1&y=2", "http://tracker.example.org", nil},
		{"http://tracker.example.org/key%2Fabc/announce", "http://tracker.example.org/key%2Fabc/scrape", "http://tracker.example.org", nil},
		{"http://tracker.example.org/announce/extra", "", "", ErrNoScrape},
		{"http://tracker.example.org/", "", "", ErrNoScrape},
		{"http://tracker.example.org", "", "", ErrNoScrape},
		{"http://tracker.example.org/x/tracker", "", "", ErrNoScrape},
		{"udp://tracker.example.org:6969/announce", "tracker.example.org:6969", "udp://tracker.example.org:6969", nil},
		{"udp://tracker.example.org/announce", "", "", ErrUnsupported},
		{"wss://tracker.example.org/announce", "", "", ErrUnsupported},
		{"not a url", "", "", ErrUnsupported},
	}
	for _, c := range cases {
		ep, err := Derive(c.in)
		if err != c.err {
			t.Errorf("%s: err = %v, want %v", c.in, err, c.err)
			continue
		}
		if err == nil && (ep.Key != c.scrape || ep.Host != c.host) {
			t.Errorf("%s: key %q host %q, want %q %q", c.in, ep.Key, ep.Host, c.scrape, c.host)
		}
	}
}

func TestHTTPScrapeMultiHashAndPartialAnswer(t *testing.T) {
	a, b, c := h20(1), h20(2), h20(3)
	f := newFakeHTTP(t, map[[20]byte]Counts{a: {5, 7}, c: {0, 1}}) // b unknown: omitted
	got, retry, e := HTTPScrape(context.Background(), newHTTPClient(2*time.Second, nil, false), f.srv.URL+"/scrape", [][20]byte{a, b, c}, DefaultMaxBody, "flodni")
	if e != nil {
		t.Fatal(e)
	}
	if len(f.requests()) != 1 || len(f.requests()[0].hashes) != 3 {
		t.Fatalf("want one request carrying 3 hashes, got %+v", f.requests())
	}
	if got[a] != (Counts{5, 7}) || got[c] != (Counts{0, 1}) {
		t.Fatalf("answers = %v", got)
	}
	if _, ok := got[b]; ok {
		t.Fatal("an omitted hash must be absent, not zero")
	}
	if retry != 0 {
		t.Fatalf("retry = %v", retry)
	}
}

func TestHTTPScrapeFailures(t *testing.T) {
	a := h20(1)
	cases := []struct {
		name   string
		answer func(w http.ResponseWriter, r *http.Request, hs [][20]byte)
		want   string
		retry  time.Duration
	}{
		{"failure reason", func(w http.ResponseWriter, r *http.Request, _ [][20]byte) {
			_, _ = w.Write([]byte("d14:failure reason9:not today5:flagsd20:min_request_intervali900eee"))
		}, "tracker: not today", 900 * time.Second},
		{"status 500", func(w http.ResponseWriter, r *http.Request, _ [][20]byte) { http.Error(w, "x", 500) }, "http status 500", 0},
		{"redirect", func(w http.ResponseWriter, r *http.Request, _ [][20]byte) {
			http.Redirect(w, r, "http://elsewhere.invalid/", 302)
		}, "http status 302", 0},
		{"not bencode", func(w http.ResponseWriter, r *http.Request, _ [][20]byte) { _, _ = w.Write([]byte("<html>")) }, "malformed response", 0},
		{"no files", func(w http.ResponseWriter, r *http.Request, _ [][20]byte) { _, _ = w.Write([]byte("de")) }, "malformed response", 0},
		{"too deep", func(w http.ResponseWriter, r *http.Request, _ [][20]byte) {
			_, _ = w.Write([]byte(strings.Repeat("l", 50) + strings.Repeat("e", 50)))
		}, "malformed response", 0},
		{"oversized", func(w http.ResponseWriter, r *http.Request, _ [][20]byte) {
			_, _ = w.Write([]byte("d5:filesd" + strings.Repeat("x", 4000) + "ee"))
		}, "response too large", 0},
	}
	for _, c := range cases {
		f := newFakeHTTP(t, nil)
		f.answer = c.answer
		_, retry, e := HTTPScrape(context.Background(), newHTTPClient(2*time.Second, nil, false), f.srv.URL+"/scrape", [][20]byte{a}, 1024, "flodni")
		if e == nil || e.Msg != c.want {
			t.Errorf("%s: err = %v, want %q", c.name, e, c.want)
			continue
		}
		if c.retry != 0 && e.RetryAfter != c.retry {
			t.Errorf("%s: retry-after = %v, want %v", c.name, e.RetryAfter, c.retry)
		}
		_ = retry
	}
}

func TestHTTPScrapeHangTimesOutWithoutURLInError(t *testing.T) {
	f := newFakeHTTP(t, nil)
	f.answer = func(w http.ResponseWriter, r *http.Request, _ [][20]byte) { time.Sleep(time.Second) }
	_, _, e := HTTPScrape(context.Background(), newHTTPClient(100*time.Millisecond, nil, false), f.srv.URL+"/scrape", [][20]byte{h20(1)}, 1024, "flodni")
	if e == nil || e.Msg != "timeout" {
		t.Fatalf("err = %v, want timeout", e)
	}
}

func TestUDPScrape(t *testing.T) {
	a, b, c := h20(1), h20(2), h20(3)
	f := newFakeUDP(t, map[[20]byte]Counts{a: {4, 9}, b: {0, 0}})
	addr := strings.TrimPrefix(f.host(), "udp://")
	got, e := UDPScrape(context.Background(), addr, [][20]byte{a, b, c}, 2*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	// the fake stops early at c: a partial answer covers a and b only
	if got[a] != (Counts{4, 9}) || got[b] != (Counts{0, 0}) {
		t.Fatalf("answers = %v", got)
	}
	if _, ok := got[c]; ok {
		t.Fatal("hash beyond a short answer must be absent")
	}
	if _, e := UDPScrape(context.Background(), addr, make([][20]byte, MaxUDPHashes+1), time.Second); e == nil {
		t.Fatal("more than 70 hashes in one packet must be refused")
	}
	for mode, want := range map[string]string{"error": "tracker: unregistered torrent", "badtx": "malformed response", "big": "response too large"} {
		f.mu.Lock()
		f.mode = mode
		f.mu.Unlock()
		_, e := UDPScrape(context.Background(), addr, [][20]byte{a}, 2*time.Second)
		if e == nil || e.Msg != want {
			t.Errorf("mode %s: err = %v, want %q", mode, e, want)
		}
	}
}

func TestUDPScrapeTimeout(t *testing.T) {
	f := newFakeUDP(t, nil)
	_ = f.conn.Close() // nobody answers
	_, e := UDPScrape(context.Background(), strings.TrimPrefix(f.host(), "udp://"), [][20]byte{h20(1)}, 150*time.Millisecond)
	if e == nil {
		t.Fatal("want an error")
	}
}

func TestBencodeBounds(t *testing.T) {
	for _, in := range []string{"", "i12", "5:ab", "d1:ae", "l", "i1ex", "99999999999:x", "d3:abci1e"} {
		if _, err := decodeBencode([]byte(in)); err == nil {
			t.Errorf("%q: want an error", in)
		}
	}
	if _, err := decodeBencode([]byte("d1:ai1e1:bl3:xyzee")); err != nil {
		t.Fatal(err)
	}
}
