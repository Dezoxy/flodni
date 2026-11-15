package tuner

import (
	"strings"
	"testing"
)

// AnnounceTracked hands over "url: error" per failed tracker, and the url is the full
// announce URL: for a private tracker that is the user's passkey.
func TestSummarizeFailuresHidesAnnounceCredentials(t *testing.T) {
	const key = "abcdef0123456789abcdef0123456789"
	got := summarizeFailures([]string{
		"https://tracker.example/" + key + "/announce: announcing: connection refused",
		"udp://other.example:6969/announce?passkey=" + key + ": timeout",
	})
	if strings.Contains(got, key) || strings.Contains(got, "passkey") {
		t.Errorf("failures hold a credential: %q", got)
	}
	for _, keep := range []string{"tracker.example", "other.example:6969", "refused", "timeout"} {
		if !strings.Contains(got, keep) {
			t.Errorf("failures lost %q: %q", keep, got)
		}
	}
}
