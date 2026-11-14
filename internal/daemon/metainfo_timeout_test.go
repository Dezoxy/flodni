package daemon

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"testing"
	"time"

	"flodni/internal/gostorm/native"
)

// setClock replaces the window clock for the test and returns a mover.
func setClock(t *testing.T, start time.Time) func(time.Duration) {
	t.Helper()
	prev := readabilityNow
	t.Cleanup(func() { readabilityNow = prev })
	now := start
	readabilityNow = func() time.Time { return now }
	return func(d time.Duration) { now = now.Add(d) }
}

func timeoutOutcome(hash string) native.Outcome {
	return native.Outcome{Hash: hash, Kind: native.OutcomeMetainfoTimeout}
}

func TestMetainfoTimeoutIsRecordedOncePerOccasion(t *testing.T) {
	db := readabilityEnv(t, 0, true)
	t0 := time.Unix(1_700_000_000, 0)
	advance := setClock(t, t0)
	before := deadSwarmVerdictCounts()[[2]string{failReasonMetainfoTimeout, failSourceWake}]

	// A player retrying every minute for five minutes is one occasion.
	for i := 0; i < 6; i++ {
		native.ReachabilityOutcome(timeoutOutcome(rHash))
		advance(time.Minute)
	}
	rows := records(t, db)
	if len(rows) != 1 {
		t.Fatalf("records = %+v, want one", rows)
	}
	r := rows[0]
	if r.FailCount != 1 || r.FirstFail != t0.Unix() || r.LastFail != t0.Unix() ||
		r.LastReason != failReasonMetainfoTimeout || r.LastSource != failSourceWake || r.LastSwarmRead != 0 {
		t.Fatalf("record = %+v, want one metainfo_timeout from the wake at t0", r)
	}
	if got := deadSwarmVerdictCounts()[[2]string{failReasonMetainfoTimeout, failSourceWake}] - before; got != 1 {
		t.Fatalf("metric rose by %d, want 1", got)
	}

	// Past the window it is a new occasion; another torrent is independent.
	advance(metainfoTimeoutWindow)
	native.ReachabilityOutcome(timeoutOutcome(rHash))
	native.ReachabilityOutcome(timeoutOutcome("ffff"))
	for _, r := range records(t, db) {
		if r.FailCount != map[string]int64{rHash: 2, "ffff": 1}[r.Hash] {
			t.Fatalf("%s fail_count = %d", r.Hash, r.FailCount)
		}
	}
}

// A metainfo that arrives ends the streak, as for any failure.
func TestMetainfoTimeoutStreakEndsWithAMetainfo(t *testing.T) {
	db := readabilityEnv(t, 0, true)
	setClock(t, time.Unix(1_700_000_000, 0))
	native.ReachabilityOutcome(timeoutOutcome(rHash))
	native.ReachabilityOutcome(native.Outcome{Hash: rHash, Kind: native.OutcomeMetainfo})
	if rows := records(t, db); len(rows) != 1 || rows[0].FailCount != 0 || rows[0].LastReason != failReasonMetainfoTimeout {
		t.Fatalf("rows = %+v", rows)
	}
}

// not_stored and the session verdicts are unaffected by the new kind: a refusal
// still leaves fail_count alone, and a timeout never touches the other reasons' counts.
func TestMetainfoTimeoutLeavesNotStoredAndVerdictsAlone(t *testing.T) {
	db := readabilityEnv(t, 0, true)
	setClock(t, time.Unix(1_700_000_000, 0))
	verdicts := deadSwarmVerdictCounts()

	native.ReachabilityOutcome(native.Outcome{Hash: rHash, Kind: native.OutcomeNotStored, Source: failSourceMount})
	rows := records(t, db)
	if len(rows) != 1 || rows[0].FailCount != 0 || rows[0].FirstFail != 0 || rows[0].LastReason != "not_stored" {
		t.Fatalf("after not_stored: %+v", rows)
	}
	native.ReachabilityOutcome(timeoutOutcome("ffff"))
	for _, r := range records(t, db) {
		switch r.Hash {
		case rHash:
			if r.FailCount != 0 || r.LastReason != "not_stored" {
				t.Errorf("not_stored record changed: %+v", r)
			}
		case "ffff":
			if r.FailCount != 1 || r.LastReason != failReasonMetainfoTimeout {
				t.Errorf("timeout record: %+v", r)
			}
		}
	}
	for k, v := range deadSwarmVerdictCounts() {
		if k[0] != failReasonMetainfoTimeout && v != verdicts[k] {
			t.Errorf("verdict %v rose to %d", k, v)
		}
	}
}

func TestMetainfoTimeoutShowsInTorrentsHealth(t *testing.T) {
	db := readabilityEnv(t, 0, true)
	setClock(t, time.Unix(1_700_000_000, 0))
	native.ReachabilityOutcome(timeoutOutcome(rHash))

	h := newTorrentsHealthHandler(db, nil, log.New(io.Discard, "", 0))
	rec := getHealth(h, http.MethodGet, "/api/torrents/health?failing=1")
	var body struct {
		Torrents []struct {
			Hash   string `json:"hash"`
			Fails  int    `json:"consecutive_failures"`
			Reason string `json:"last_failure_reason"`
			Source string `json:"last_failure_source"`
		} `json:"torrents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if rec.Code != 200 || len(body.Torrents) != 1 || body.Torrents[0].Hash != rHash ||
		body.Torrents[0].Fails != 1 || body.Torrents[0].Reason != "metainfo_timeout" || body.Torrents[0].Source != "wake" {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
}
