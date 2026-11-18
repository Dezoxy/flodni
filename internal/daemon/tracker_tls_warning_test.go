package daemon

import (
	"strings"
	"testing"

	"flodni/internal/config"
)

// A start with tracker certificate verification off says so once, at WARN; the default is silent.
func TestTrackerTLSWarning(t *testing.T) {
	if w := trackerTLSWarning(&config.Config{}); w != "" {
		t.Fatalf("default warns: %q", w)
	}
	w := trackerTLSWarning(&config.Config{TrackerTLSInsecure: true})
	if want := "[Tracker] TLS certificate verification is OFF (tracker_tls_insecure)"; w != want {
		t.Fatalf("warning %q, want %q", w, want)
	}
	if strings.Contains(w, "\n") {
		t.Fatalf("warning is not one line: %q", w)
	}
}
