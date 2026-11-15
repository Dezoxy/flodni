package daemon

import (
	"strings"
	"testing"

	"flodni/internal/promtext"
)

func TestUnlinkedStubsGaugeIsAbsentWithoutASet(t *testing.T) {
	libraryUnlinked.Store(nil)
	t.Cleanup(func() { libraryUnlinked.Store(nil) })
	render := func() string {
		var w promtext.Writer
		collectUnlinkedProm(&w)
		if len(w.Bytes()) > 0 {
			if err := promtext.Lint(w.Bytes()); err != nil {
				t.Fatal(err)
			}
		}
		return string(w.Bytes())
	}
	if got := render(); got != "" {
		t.Fatalf("gauge before the manager exists: %q", got)
	}
	n, ok := 0, false
	libraryUnlinked.Store(&libraryUnlinkedFn{func() (int, bool) { return n, ok }})
	if got := render(); got != "" {
		t.Fatalf("gauge with no linked set: %q", got)
	}
	n, ok = 0, true
	if got := render(); !strings.Contains(got, "flodni_library_unlinked_stubs 0\n") {
		t.Fatalf("gauge with a set and no unlinked stub: %q", got)
	}
	n = 3
	if got := render(); !strings.Contains(got, "flodni_library_unlinked_stubs 3\n") {
		t.Fatalf("gauge = %q, want 3", got)
	}
}
