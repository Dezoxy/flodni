package utils

import (
	"strings"
	"testing"
)

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

func TestLinkHashRefusesWhatPanicsTheParser(t *testing.T) {
	for _, l := range []string{
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"magnet:?xt=urn:btih:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		"magnet:?xt=urn:btmh:1220aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"torrs://" + strings.Repeat("A", maxTorrsToken+1),
	} {
		if _, ok := LinkHash(l); ok {
			t.Errorf("LinkHash(%.40q) ok", l)
		}
		if _, err := ParseLink(l); err == nil && !strings.HasPrefix(l, "torrs") {
			t.Errorf("ParseLink(%.40q) no error", l)
		}
	}
	if h, ok := StrictHash("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"); !ok || h != strings.Repeat("00", 20) {
		t.Errorf("StrictHash base32 = %q %v", h, ok)
	}
}
