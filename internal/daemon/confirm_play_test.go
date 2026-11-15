package daemon

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"flodni/internal/apiauth"
	"flodni/internal/config"
	"flodni/internal/library"
	"flodni/internal/warmup"
)

// confirmPlayByHand (ADR 3, step 5): the played mark of a play webhook, written by hand.

func manualIndex(t *testing.T) *fileStateIndex {
	t.Helper()
	old := fileStates
	x := warmIndex(t)
	fileStates = x
	t.Cleanup(func() { fileStates = old })
	return x
}

func TestManualConfirmMarksPlayedAndClearsTheEvictionMark(t *testing.T) {
	x := manualIndex(t)
	k := warmup.FileKey{Hash: "aabbcc", FileIdx: 3}
	x.OnEvicted([]warmup.Eviction{{Key: k}})
	if x.IsPlayed(k) || !x.HasEvictedMark(k) {
		t.Fatal("setup: the file should be evicted and unplayed")
	}
	before := playMarks[playSourceManual].Load()
	starts := playbackStarts.Load()

	confirmPlayByHand("AABBCC", 3)

	if !x.IsPlayed(k) || x.HasEvictedMark(k) {
		t.Fatalf("played = %v, evicted mark = %v", x.IsPlayed(k), x.HasEvictedMark(k))
	}
	if got := playMarks[playSourceManual].Load() - before; got != 1 {
		t.Fatalf("manual play marks +%d, want 1", got)
	}
	if playbackStarts.Load() != starts {
		t.Fatal("a manual confirm counted a playback start, which is the webhook's")
	}
	// The windows are not the confirm's business: the cache state stays as recorded.
	if f := libraryFileFacts("aabbcc", 3); !f.Protected || f.Cache != cacheEvicted {
		t.Fatalf("facts = %+v", f)
	}
}

// A repeat is idempotent in state and renews the played time, as a second play does.
func TestManualConfirmRepeatRenewsThePlayedTime(t *testing.T) {
	x := manualIndex(t)
	k := warmup.FileKey{Hash: "aabbcc", FileIdx: 3}
	confirmPlayByHand("aabbcc", 3)
	first := libraryFileFacts("aabbcc", 3).PlayedAt
	x.now = func() time.Time { return wt0.Add(time.Hour) }
	confirmPlayByHand("aabbcc", 3)

	f := libraryFileFacts("aabbcc", 3)
	if !x.IsPlayed(k) || !f.PlayedAt.Equal(first.Add(time.Hour)) {
		t.Fatalf("played at %v then %v", first, f.PlayedAt)
	}
}

// A manual confirm records the play only: a session the reader budget holds stays
// unconfirmed and keeps being refused until a webhook confirms it.
func TestManualConfirmDoesNotLiftAReaderBudgetHold(t *testing.T) {
	path, key := playedEnv(t)
	ps, ok := app.playbackRegistry.Load(path)
	if !ok {
		t.Fatal("no session")
	}
	state := ps.(*PlaybackState)

	confirmPlayByHand(key.Hash, key.FileIdx)

	if state.GetStatus() {
		t.Fatal("a manual confirm confirmed the session")
	}
	if !fileStates.IsPlayed(key) {
		t.Fatal("the play was not recorded")
	}
	if confirmedSessionOn(key) {
		t.Fatal("the file reports a confirmed session")
	}
}

// The webhook and the manual confirm write the played mark through one function.
func TestWebhookPlayCountsAsAWebhookMark(t *testing.T) {
	_, key := playedEnv(t)
	web := playMarks[playSourceWebhook].Load()
	manual := playMarks[playSourceManual].Load()

	postWebhook(t, evPayload("media.play"))

	if !fileStates.IsPlayed(key) {
		t.Fatal("the webhook did not mark the file")
	}
	if playMarks[playSourceWebhook].Load()-web != 1 || playMarks[playSourceManual].Load() != manual {
		t.Fatal("the play was not counted under source webhook")
	}
}

// confirmServer is the real library handler behind the real bearer middleware, with
// the control token fixed at start-up the way main fixes it.
func confirmServer(t *testing.T, startToken string) (http.Handler, warmup.FileKey) {
	t.Helper()
	newTestApp(t).controlToken = startToken

	const hash = "0123456789abcdef0123456789abcdef01234567"
	tv := t.TempDir()
	stub := filepath.Join(tv, "Show", "Season.01", "Show.S01E01.mkv")
	if err := library.WriteStub(stub, "http://engine/stream?link="+hash+"&index=3&play", 10, hash, ""); err != nil {
		t.Fatal(err)
	}
	m := library.New(library.Config{TVDir: tv, Confirm: confirmPlayByHand, TokenSet: apiTokenSet})
	mux := http.NewServeMux()
	mux.HandleFunc("/api/library/confirm", library.NewHandler(m).Confirm)
	return apiauth.Middleware(app.controlToken, nil, mux), warmup.FileKey{Hash: hash, FileIdx: 3}
}

func postConfirm(h http.Handler, key warmup.FileKey, bearer string) int {
	body := `{"hash":"` + key.Hash + `","file_index":3}`
	req := httptest.NewRequest(http.MethodPost, "/api/library/confirm", strings.NewReader(body))
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

// The exploit this gate must not allow: start with no token (the middleware enforces
// nothing), then set one through the open POST /api/config. The live config now has a
// token and the middleware still has none, so the gate must still say no.
func TestConfirmStaysClosedWhenTheTokenIsSetAfterStart(t *testing.T) {
	h, key := confirmServer(t, "")
	x := manualIndex(t)
	withConfig(t, func(c *config.Config) { c.APIToken = "set-after-start" })

	if apiTokenSet() {
		t.Fatal("apiTokenSet follows the live config")
	}
	before := playMarks[playSourceManual].Load()
	if code := postConfirm(h, key, ""); code != http.StatusForbidden {
		t.Fatalf("no bearer: status %d, want 403", code)
	}
	if code := postConfirm(h, key, "set-after-start"); code != http.StatusForbidden {
		t.Fatalf("the late token: status %d, want 403", code)
	}
	if x.EverPlayed(key) || playMarks[playSourceManual].Load() != before {
		t.Fatal("a refused confirm recorded a play")
	}
}

// With a token at start the middleware enforces it and confirm works behind it.
func TestConfirmWithATokenSetAtStart(t *testing.T) {
	h, key := confirmServer(t, "secret")
	x := manualIndex(t)

	for _, bearer := range []string{"", "wrong"} {
		if code := postConfirm(h, key, bearer); code != http.StatusUnauthorized {
			t.Fatalf("bearer %q: status %d, want 401", bearer, code)
		}
	}
	if x.EverPlayed(key) {
		t.Fatal("an unauthorized confirm recorded a play")
	}
	if code := postConfirm(h, key, "secret"); code != http.StatusOK {
		t.Fatalf("with the bearer: status %d, want 200", code)
	}
	if !x.IsPlayed(key) {
		t.Fatal("the authorized confirm recorded nothing")
	}
}
