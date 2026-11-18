package torr

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func serveForTest(t *testing.T, name string, body []byte, rangeHeader string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/stream/x", nil)
	if rangeHeader != "" {
		req.Header.Set("Range", rangeHeader)
	}
	rec := httptest.NewRecorder()
	serveFileContent(rec, req, name, time.Unix(0, 0), bytes.NewReader(body))
	return rec.Result()
}

// A torrent's file name and bytes come from the swarm. Anything a browser could render
// must come back as a download that no script can run from.
func TestServeFileContentNeverRendersNonMedia(t *testing.T) {
	html := []byte("<html><script>alert(1)</script></html>")
	svg := []byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`)
	cases := []struct {
		name string
		body []byte
	}{
		{"a.html", html},
		{"dir/a.htm", html},
		{"a.xhtml", html},
		{"a.svg", svg},
		{"a.SVG", svg},
		{"a.png", html},
		{"a.jpg", html},
		{"a.txt", html},
		{"a.srt", html},
		{"a.js", []byte("alert(1)")},
		{"a.xml", []byte("<x/>")},
		{"a.pdf", []byte("%PDF-1.4")},
		{"noext", html},
		{"a.html.part", html},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := serveForTest(t, c.name, c.body, "")
			if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
				t.Errorf("Content-Type = %q, want application/octet-stream", got)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := resp.Header.Get("Content-Security-Policy"); got != "sandbox" {
				t.Errorf("Content-Security-Policy = %q, want sandbox", got)
			}
			disp, _, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition"))
			if err != nil || disp != "attachment" {
				t.Errorf("Content-Disposition = %q (%v), want attachment", resp.Header.Get("Content-Disposition"), err)
			}
		})
	}
}

func TestServeFileContentKeepsMediaTypesAndRange(t *testing.T) {
	body := []byte("0123456789abcdef")
	cases := map[string]string{
		"a.mkv":      "video/x-matroska",
		"a.mp4":      "video/mp4",
		"a.mp3":      "audio/mpeg",
		"dir/a.flac": "audio/flac",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			resp := serveForTest(t, name, body, "")
			if got := resp.Header.Get("Content-Type"); got != want {
				t.Errorf("Content-Type = %q, want %q", got, want)
			}
			if resp.Header.Get("Content-Disposition") != "" {
				t.Errorf("media must not be an attachment, got %q", resp.Header.Get("Content-Disposition"))
			}
			if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Security-Policy") != "sandbox" {
				t.Errorf("media still needs nosniff and CSP sandbox: %v", resp.Header)
			}

			resp = serveForTest(t, name, body, "bytes=4-9")
			got, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusPartialContent || string(got) != "456789" {
				t.Errorf("range: status %d body %q, want 206 456789", resp.StatusCode, got)
			}
			if resp.Header.Get("Content-Range") != "bytes 4-9/16" {
				t.Errorf("Content-Range = %q", resp.Header.Get("Content-Range"))
			}
		})
	}
}

// The name goes into a header: no line break, quote or path may escape it.
func TestServeFileContentDispositionFromHostileName(t *testing.T) {
	for _, name := range []string{
		"a\r\nX-Evil: 1.html",
		"x\".html\"; filename*=UTF-8''evil.exe",
		"../../etc/passwd.html",
		"tab\there.html",
		"név ő.html",
		strings.Repeat("a", 5000) + ".html",
	} {
		resp := serveForTest(t, name, []byte("<html>"), "")
		if resp.Header.Get("X-Evil") != "" {
			t.Fatalf("%q injected a header", name)
		}
		cd := resp.Header.Get("Content-Disposition")
		if strings.ContainsAny(cd, "\r\n") {
			t.Errorf("%q: Content-Disposition has a line break: %q", name, cd)
		}
		disp, params, err := mime.ParseMediaType(cd)
		if err != nil || disp != "attachment" {
			t.Errorf("%q: Content-Disposition %q does not parse as attachment (%v)", name, cd, err)
		}
		if strings.ContainsAny(params["filename"], "/\\\r\n") {
			t.Errorf("%q: filename %q keeps a path or control byte", name, params["filename"])
		}
	}
}
