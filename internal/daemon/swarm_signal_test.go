package daemon

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"flodni/internal/gostorm/torr"
	"flodni/internal/metadb"
)

func resetSwarmSnapshot(t *testing.T) {
	t.Helper()
	swarmSnapshot.Store(nil)
	t.Cleanup(func() { swarmSnapshot.Store(nil) })
}

// fakeSwarm is a fake engine: it lists only the torrents it has open, and counts calls,
// so a test can see nothing woke a closed one (the source has no wake path at all).
type fakeSwarm struct {
	open  []swarmObservation
	calls int
}

func (f *fakeSwarm) list() []swarmObservation { f.calls++; return f.open }

type healthSwarm struct {
	Hash  string       `json:"hash"`
	Swarm *swarmHealth `json:"swarm"`
}

func getHealthSwarm(t *testing.T, store readabilityLister) (map[string]*swarmHealth, string) {
	t.Helper()
	h := newTorrentsHealthHandler(store, nil, log.New(io.Discard, "", 0))
	rec := getHealth(h, http.MethodGet, "/api/torrents/health")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Torrents []healthSwarm `json:"torrents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	m := map[string]*swarmHealth{}
	for _, x := range out.Torrents {
		m[x.Hash] = x.Swarm
	}
	return m, rec.Body.String()
}

func TestSwarmAwakeReportsPeersLastPieceRate(t *testing.T) {
	resetSwarmSnapshot(t)
	last := time.Unix(1_700_000_000, 0)
	f := &fakeSwarm{open: []swarmObservation{{Hash: "aa", SwarmSignal: torr.SwarmSignal{
		Peers: 7, Seeders: 3, SeedersKnown: true, LastPieceAt: last, RateBps: 1_500_000.9,
	}}}}
	refreshSwarmSnapshot(f.list)

	got, _ := getHealthSwarm(t, &fakeLister{rows: []metadb.Readability{{Hash: "aa"}}})
	s := got["aa"]
	if s == nil || !s.Awake || s.Peers != 7 || s.RateBps != 1_500_000 {
		t.Fatalf("swarm = %+v", s)
	}
	if s.Seeders == nil || *s.Seeders != 3 {
		t.Fatalf("seeders = %v", s.Seeders)
	}
	if s.LastPieceAt == nil || *s.LastPieceAt != "2023-11-14T22:13:20Z" {
		t.Fatalf("last_piece_at = %v", s.LastPieceAt)
	}
}

func TestSwarmUnknownSeedersAndNoPieceAreNull(t *testing.T) {
	resetSwarmSnapshot(t)
	f := &fakeSwarm{open: []swarmObservation{{Hash: "aa", SwarmSignal: torr.SwarmSignal{Peers: 1}}}}
	refreshSwarmSnapshot(f.list)
	_, body := getHealthSwarm(t, &fakeLister{rows: []metadb.Readability{{Hash: "aa"}}})
	if !strings.Contains(body, `"seeders":null`) || !strings.Contains(body, `"last_piece_at":null`) {
		t.Fatalf("body = %s", body)
	}
}

func TestSwarmClosedIsNullAndNotWoken(t *testing.T) {
	resetSwarmSnapshot(t)
	// "aa" is open; "bb" has a record but the engine does not list it: closed.
	f := &fakeSwarm{open: []swarmObservation{{Hash: "aa", SwarmSignal: torr.SwarmSignal{Peers: 2}}}}
	refreshSwarmSnapshot(f.list)
	calls := f.calls

	got, body := getHealthSwarm(t, &fakeLister{rows: []metadb.Readability{{Hash: "aa"}, {Hash: "bb"}}})
	if got["aa"] == nil {
		t.Fatalf("aa should be awake: %s", body)
	}
	if got["bb"] != nil || !strings.Contains(body, `"hash":"bb"`) || !strings.Contains(body, `"swarm":null`) {
		t.Fatalf("bb should serve swarm null: %s", body)
	}
	if f.calls != calls {
		t.Fatalf("serving the handler asked the engine again (%d -> %d calls)", calls, f.calls)
	}
}

func TestSwarmNullBeforeFirstSnapshot(t *testing.T) {
	resetSwarmSnapshot(t)
	got, _ := getHealthSwarm(t, &fakeLister{rows: []metadb.Readability{{Hash: "aa"}}})
	if got["aa"] != nil {
		t.Fatalf("swarm = %+v, want null", got["aa"])
	}
}

func TestSwarmGauge(t *testing.T) {
	resetSwarmSnapshot(t)
	if strings.Contains(scrape(t).Body.String(), "flodni_swarm_awake_torrents") {
		t.Fatal("series present before the first snapshot")
	}
	f := &fakeSwarm{open: []swarmObservation{{Hash: "aa"}, {Hash: "bb"}}}
	refreshSwarmSnapshot(f.list)
	if body := scrape(t).Body.String(); !strings.Contains(body, "flodni_swarm_awake_torrents 2\n") {
		t.Fatalf("gauge missing in:\n%s", body)
	}
	f.open = nil
	refreshSwarmSnapshot(f.list)
	if body := scrape(t).Body.String(); !strings.Contains(body, "flodni_swarm_awake_torrents 0\n") {
		t.Fatalf("gauge not 0 after all closed:\n%s", body)
	}
}

// The field is experimental: the README must say so, as it does for files.
func TestSwarmFieldDocumentedExperimental(t *testing.T) {
	b, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	i := strings.Index(readme, "### Swarm signal (experimental)")
	if i < 0 {
		t.Fatal("README has no \"Swarm signal (experimental)\" section")
	}
	sec := readme[i:]
	for _, want := range []string{"`swarm`", "**experimental**", "flodni_swarm_awake_torrents", "`last_piece_at`", "`rate_bps`"} {
		if !strings.Contains(sec, want) {
			t.Errorf("section lacks %s", want)
		}
	}
}
