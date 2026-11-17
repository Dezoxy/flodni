package daemon

import (
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"flodni/internal/gostorm/settings"
)

// submitHandler returns the body of the engine form's submit handler.
func submitHandler(t *testing.T) string {
	t.Helper()
	html := string(settingsHTML)
	i := strings.Index(html, "document.getElementById('gostorm-form').onsubmit")
	if i < 0 {
		t.Fatal("the engine form's submit handler is missing")
	}
	j := strings.Index(html[i:], "loadEngine();")
	if j < 0 {
		t.Fatal("end of the submit handler not found")
	}
	return html[i : i+j]
}

// The panel's list of fields that restart the engine is the engine's own list.
func TestPanelClientKeysMatchTheEngine(t *testing.T) {
	m := regexp.MustCompile(`const CLIENT_KEYS = \[([^\]]*)\];`).FindStringSubmatch(string(settingsHTML))
	if m == nil {
		t.Fatal("CLIENT_KEYS not found in settings.html")
	}
	var got []string
	for _, q := range regexp.MustCompile(`"([A-Za-z0-9]+)"`).FindAllStringSubmatch(m[1], -1) {
		got = append(got, q[1])
	}
	sort.Strings(got)
	if want := settings.ClientFields(); !reflect.DeepEqual(got, want) {
		t.Errorf("panel CLIENT_KEYS = %v, engine ClientFields = %v", got, want)
	}
}

// A plain Apply sends no force. force is set in one place, after the user confirmed the
// restart dialog, and never in the literal payload.
func TestPanelSendsForceOnlyAfterTheUserConfirms(t *testing.T) {
	h := submitHandler(t)
	if got := len(regexp.MustCompile(`\bforce\b`).FindAllString(strings.ReplaceAll(h, "// only after the user confirmed", ""), -1)); got != 1 {
		t.Fatalf("force appears %d times in the submit handler, want exactly 1 (the confirmed assignment)", got)
	}
	if !strings.Contains(h, "const payload = { action: 'set', sets: sets };") {
		t.Error("the default payload must be {action, sets} with no force")
	}
	confirmAt := strings.Index(h, "confirm(")
	cancelAt := strings.Index(h, "if (!ok) return;")
	forceAt := strings.Index(h, "payload.force = true")
	sendAt := strings.Index(h, "api('/api/gostorm/settings'")
	if confirmAt < 0 || !(confirmAt < cancelAt && cancelAt < forceAt && forceAt < sendAt) {
		t.Errorf("order must be confirm, cancel check, force, send; got %d %d %d %d", confirmAt, cancelAt, forceAt, sendAt)
	}
	if !strings.Contains(h, "if (restartKeys.length) {") {
		t.Error("the confirmation is not gated on a CLIENT field change")
	}
	if !strings.Contains(h, "restarts the engine and drops all active torrents") {
		t.Error("the confirmation does not say that it restarts the engine and drops all active torrents")
	}
}

func TestPanelShowsTheRefusalMessage(t *testing.T) {
	h := submitHandler(t)
	if !strings.Contains(h, "res.status === 409") || !strings.Contains(h, ".error") {
		t.Error("a 409 from the engine is not shown to the user")
	}
}

func TestConfirmedPlayingSessionsCountsOnlyLiveConfirmedSessions(t *testing.T) {
	reg := func(path string, ps *PlaybackState, handle bool) {
		ps.Path = path
		app.playbackRegistry.Store(path, ps)
		var h *MkvHandle
		if handle {
			h = &MkvHandle{path: path}
			app.activeHandles.Store(h, true)
		}
		t.Cleanup(func() {
			app.playbackRegistry.Delete(path)
			if h != nil {
				app.activeHandles.Delete(h)
			}
		})
	}
	base := confirmedPlayingSessions()
	reg("/m/live.mkv", &PlaybackState{IsHealthy: true}, true)
	reg("/m/live2.mkv", &PlaybackState{IsHealthy: true}, true)
	reg("/m/stopped.mkv", &PlaybackState{IsHealthy: true, IsStopped: true}, true)
	reg("/m/nohandle.mkv", &PlaybackState{IsHealthy: true}, false)
	reg("/m/unconfirmed.mkv", &PlaybackState{}, true)
	if got := confirmedPlayingSessions() - base; got != 2 {
		t.Errorf("counted %d, want 2 (live confirmed only)", got)
	}
}
