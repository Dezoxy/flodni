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

// A plain Apply sends no force. force is assigned in exactly two places, each right after
// a confirm() the user answered yes to, and never in the literal payload.
func TestPanelSendsForceOnlyAfterTheUserConfirms(t *testing.T) {
	h := submitHandler(t)
	body := strings.ReplaceAll(h, "// only after the user confirmed", "")
	if got := len(regexp.MustCompile(`\bforce\b`).FindAllString(body, -1)); got != 2 {
		t.Fatalf("force appears %d times in the submit handler, want 2 (the two confirmed assignments)", got)
	}
	if !strings.Contains(h, "const payload = { action: 'set', sets: sets };") {
		t.Error("the default payload must be {action, sets} with no force")
	}
	// each assignment is guarded: the first by a cancelled confirm returning, the second by
	// being inside an if on the confirm's answer, and neither follows a send
	first := strings.Index(h, "payload.force = true")
	second := strings.Index(h[first+1:], "payload.force = true") + first + 1
	if first < 0 || second <= first {
		t.Fatal("expected two force assignments")
	}
	if c, r := strings.Index(h, "const ok = confirm("), strings.Index(h, "if (!ok) return;"); !(0 < c && c < r && r < first) {
		t.Errorf("first force must follow confirm and the cancel return (%d %d %d)", c, r, first)
	}
	again, lastSend, firstSend := strings.Index(h, "if (again) {"), strings.LastIndex(h, "await send()"), strings.Index(h, "await send()")
	if !strings.Contains(h, "const again = confirm(") || again < 0 || !(first < firstSend && firstSend < again && again < second && second < lastSend) {
		t.Errorf("the retry after a 409 must be gated on a second confirm (first=%d send=%d if=%d second=%d retry=%d)", first, firstSend, again, second, lastSend)
	}
	if got := strings.Count(h, "await send()"); got != 2 {
		t.Errorf("send() is awaited %d times, want the first send and the confirmed retry", got)
	}
	if !strings.Contains(h, "if (restartKeys.length) {") {
		t.Error("the confirmation is not gated on a CLIENT field change")
	}
	if !strings.Contains(h, "restarts the engine and drops all active torrents") {
		t.Error("the confirmation does not say that it restarts the engine and drops all active torrents")
	}
}

func TestPanelShowsTheRefusalMessageAndOffersTheRestart(t *testing.T) {
	h := submitHandler(t)
	if !strings.Contains(h, "res.status === 409") || !strings.Contains(h, ".error") {
		t.Error("a 409 from the engine is not shown to the user")
	}
	if !strings.Contains(h, "Restart anyway?") {
		t.Error("the 409 does not offer an explicit restart")
	}
}

// A blank or non-numeric number field keeps the stored value: it is no change, so it can
// not raise the restart confirmation or a force.
func TestPanelBlankNumberFieldKeepsTheStoredValue(t *testing.T) {
	h := submitHandler(t)
	for _, f := range []string{"connectionsLimit", "peersListenPort", "torrentDisconnectTimeout", "retrackersMode", "readerReadAHead"} {
		if strings.Contains(h, "parseInt(formData.get('"+f+"'))") {
			t.Errorf("%s is read with a bare parseInt: a blank field becomes NaN", f)
		}
		if !strings.Contains(h, "intOr(formData.get('"+f+"'),") {
			t.Errorf("%s does not go through intOr()", f)
		}
	}
	html := string(settingsHTML)
	if !strings.Contains(html, "function intOr(raw, stored)") || !strings.Contains(html, "Number.isFinite(n) ? n : stored") {
		t.Error("intOr() does not fall back to the stored value")
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
