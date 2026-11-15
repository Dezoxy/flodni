package utils

import "testing"

func TestLinkHashNeverFetches(t *testing.T) {
	const h = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, l := range []string{"magnet:?xt=urn:btih:" + h + "&tr=http://x.invalid/a", h} {
		if got, ok := LinkHash(l); !ok || got != h {
			t.Errorf("LinkHash(%q) = %q, %v", l, got, ok)
		}
	}
	for _, l := range []string{"http://x.invalid/a.torrent", "https://x.invalid/a", "file:///etc/passwd", "ftp://x/a", "zz", ""} {
		if _, ok := LinkHash(l); ok {
			t.Errorf("LinkHash(%q) ok, want no hash", l)
		}
	}
}
