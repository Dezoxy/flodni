package torr

import (
	"bytes"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// The type comes from a compiled-in table, not the host's mime table, and a playlist is
// never media: a player or browser would follow what it lists.
func TestServeFileContentTypeIsCompiledIn(t *testing.T) {
	// A host whose mime table calls playlists audio, as some distributions do.
	for ext, typ := range map[string]string{
		".m3u": "audio/x-mpegurl", ".m3u8": "application/vnd.apple.mpegurl", ".pls": "audio/x-scpls",
		".cue": "audio/x-cue", ".xyz": "video/x-host-only",
	} {
		if err := mime.AddExtensionType(ext, typ); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"a.m3u", "a.m3u8", "a.M3U", "a.pls", "a.cue", "a.xyz"} {
		resp := serveForTest(t, name, []byte("#EXTM3U\nhttp://x/\n"), "")
		if resp.Header.Get("Content-Type") != "application/octet-stream" {
			t.Errorf("%s: Content-Type = %q, want application/octet-stream", name, resp.Header.Get("Content-Type"))
		}
		if disp, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); disp != "attachment" {
			t.Errorf("%s: Content-Disposition = %q, want attachment", name, resp.Header.Get("Content-Disposition"))
		}
	}
	inline := map[string]string{
		"a.m4v": "video/mp4", "a.flv": "video/x-flv", "a.wmv": "video/x-ms-wmv", "a.3gp": "video/3gpp",
		"a.asf": "video/x-ms-asf", "a.M4V": "video/mp4", "a.mkv.part": "video/x-matroska",
		"a.m4b": "audio/mp4", "a.mka": "audio/x-matroska", "a.ac3": "audio/ac3",
		"a.dts": "audio/vnd.dts", "a.wma": "audio/x-ms-wma", "a.ts": "video/mpeg", "a.avi": "video/avi",
	}
	for name, want := range inline {
		resp := serveForTest(t, name, []byte("0123456789"), "")
		if got := resp.Header.Get("Content-Type"); got != want {
			t.Errorf("%s: Content-Type = %q, want %q", name, got, want)
		}
		if resp.Header.Get("Content-Disposition") != "" {
			t.Errorf("%s: media must not be an attachment", name)
		}
	}
}

// A file name chosen by the swarm must never make the daemon open a file of its own: an
// unknown extension is not sniffed from local disk. The local file below holds MP4 magic,
// so a content sniff would serve it inline as video.
func TestServeFileContentNeverReadsALocalFile(t *testing.T) {
	local := filepath.Join(t.TempDir(), "probe")
	if err := os.WriteFile(local, []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{local, "../" + filepath.Base(local), "probe"} {
		t.Chdir(filepath.Dir(local))
		resp := serveForTest(t, name, []byte("body"), "")
		if got := resp.Header.Get("Content-Type"); got != "application/octet-stream" {
			t.Errorf("%q: Content-Type = %q, want application/octet-stream", name, got)
		}
		if resp.Header.Get("Content-Disposition") == "" {
			t.Errorf("%q: not an attachment", name)
		}
	}
}

func TestAttachmentDispositionOfDotNames(t *testing.T) {
	for _, name := range []string{"..", ".", "a/..", "a/.", "", "/"} {
		if got := attachmentDisposition(name); got != "attachment" {
			t.Errorf("attachmentDisposition(%q) = %q, want a bare attachment", name, got)
		}
	}
}

// The safe headers must survive every ServeContent exit, not only the plain 200.
func TestServeFileContentHeadersOnEveryStatus(t *testing.T) {
	body := []byte("0123456789abcdef")
	for _, name := range []string{"a.html", "a.mkv"} {
		media := name == "a.mkv"
		for _, c := range []struct {
			label  string
			method string
			hdr    map[string]string
			etag   bool
			status int
		}{
			{"304", http.MethodGet, map[string]string{"If-None-Match": `"v1"`}, true, http.StatusNotModified},
			{"416", http.MethodGet, map[string]string{"Range": "bytes=100-200"}, false, http.StatusRequestedRangeNotSatisfiable},
			{"HEAD", http.MethodHead, nil, false, http.StatusOK},
			{"multipart 206", http.MethodGet, map[string]string{"Range": "bytes=0-1,4-5"}, false, http.StatusPartialContent},
		} {
			t.Run(name+"/"+c.label, func(t *testing.T) {
				req := httptest.NewRequest(c.method, "/stream/x", nil)
				for k, v := range c.hdr {
					req.Header.Set(k, v)
				}
				rec := httptest.NewRecorder()
				if c.etag {
					rec.Header().Set("ETag", `"v1"`)
				}
				serveFileContent(rec, req, name, time.Unix(0, 0), bytes.NewReader(body))
				resp := rec.Result()
				if resp.StatusCode != c.status {
					t.Fatalf("status = %d, want %d", resp.StatusCode, c.status)
				}
				if resp.Header.Get("X-Content-Type-Options") != "nosniff" || resp.Header.Get("Content-Security-Policy") != "sandbox" {
					t.Errorf("safe headers lost: %v", resp.Header)
				}
				if got := resp.Header.Get("Content-Disposition") != ""; got == media {
					t.Errorf("Content-Disposition present = %v, want %v", got, !media)
				}
				if c.label == "multipart 206" && !strings.HasPrefix(resp.Header.Get("Content-Type"), "multipart/byteranges") {
					t.Errorf("Content-Type = %q, want multipart/byteranges", resp.Header.Get("Content-Type"))
				}
			})
		}
	}
}
