package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// warm_catchup_per_hour is the pace of catch-up (Readiness step 3): files started per
// hour, 20 by default, 0 off, and a negative value falls back to the default.

func loadWith(t *testing.T, body string) Config {
	t.Helper()
	c, err := loadWithErr(t, body)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return c
}

// loadWithErr is loadWith for the tests that expect the file to be refused.
func loadWithErr(t *testing.T, body string) (Config, error) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MKV_PROXY_CONFIG_PATH", p)
	return LoadConfig()
}

func TestWarmCatchupPerHourDefaultsToTwenty(t *testing.T) {
	if got := loadCfg(t).WarmCatchupPerHour; got != 20 {
		t.Fatalf("warm_catchup_per_hour = %d, want 20", got)
	}
}

func TestWarmCatchupPerHourZeroStaysOffAndNegativeFallsBack(t *testing.T) {
	if got := loadWith(t, `{"warm_catchup_per_hour": 0}`).WarmCatchupPerHour; got != 0 {
		t.Fatalf("an explicit 0 became %d: 0 turns catch-up off", got)
	}
	if got := loadWith(t, `{"warm_catchup_per_hour": 7}`).WarmCatchupPerHour; got != 7 {
		t.Fatalf("7 became %d", got)
	}
	if got := loadWith(t, `{"warm_catchup_per_hour": -3}`).WarmCatchupPerHour; got != 20 {
		t.Fatalf("a negative value became %d, want the default 20", got)
	}
}

func TestWarmCatchupPerHourSurvivesThePanelRoundTrip(t *testing.T) {
	in := Config{WarmCatchupPerHour: 5}
	b, err := json.Marshal(in.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"warm_catchup_per_hour":5`) {
		t.Fatalf("GET /api/config would not carry the key: %s", b)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(out)
	if out.WarmCatchupPerHour != 5 || !strings.Contains(string(b), `"warm_catchup_per_hour":5`) {
		t.Fatalf("lost in the round trip: %d, %s", out.WarmCatchupPerHour, b)
	}
}

func TestConfigExampleCarriesTheCatchupPace(t *testing.T) {
	raw, err := os.ReadFile("../../config.json.example")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"warm_catchup_per_hour": 20`) || c.WarmCatchupPerHour != 20 {
		t.Fatal(`config.json.example must carry "warm_catchup_per_hour": 20`)
	}
}

// warm_catchup_scope (roadmap step 8): "all" by default, "retries" the opt-in, and an
// unknown value falls back to "all".

func TestWarmCatchupScopeDefaultsToAll(t *testing.T) {
	if got := loadCfg(t).WarmCatchupScope; got != "all" {
		t.Fatalf("warm_catchup_scope = %q, want all", got)
	}
	if got := loadWith(t, `{"warm_on_add": true}`).WarmCatchupScope; got != "all" {
		t.Fatalf("an absent key became %q", got)
	}
}

func TestWarmCatchupScopeAcceptsRetriesAndRefusesTheRest(t *testing.T) {
	if got := loadWith(t, `{"warm_catchup_scope": "retries"}`).WarmCatchupScope; got != "retries" {
		t.Fatalf("retries became %q", got)
	}
	if got := loadWith(t, `{"warm_catchup_scope": "all"}`).WarmCatchupScope; got != "all" {
		t.Fatalf("all became %q", got)
	}
	for _, bad := range []string{"Retries", "everything", "", " retries"} {
		if got := loadWith(t, `{"warm_catchup_scope": "`+bad+`"}`).WarmCatchupScope; got != "all" {
			t.Fatalf("%q became %q, want all", bad, got)
		}
	}
}

func TestWarmCatchupScopeSurvivesThePanelRoundTrip(t *testing.T) {
	in := Config{WarmCatchupScope: "retries"}
	b, err := json.Marshal(in.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"warm_catchup_scope":"retries"`) {
		t.Fatalf("GET /api/config would not carry the key: %s", b)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(out)
	if out.WarmCatchupScope != "retries" || !strings.Contains(string(b), `"warm_catchup_scope":"retries"`) {
		t.Fatalf("lost in the round trip: %q, %s", out.WarmCatchupScope, b)
	}
}

func TestConfigExampleCarriesTheCatchupScope(t *testing.T) {
	raw, err := os.ReadFile("../../config.json.example")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"warm_catchup_scope": "all"`) || c.WarmCatchupScope != "all" {
		t.Fatal(`config.json.example must carry "warm_catchup_scope": "all"`)
	}
}

// warmup_eviction_order (roadmap step 22): "written" by default, "read" the opt-in, and an
// unknown value falls back to "written".

func TestWarmupEvictionOrderDefaultsToWritten(t *testing.T) {
	if got := loadCfg(t).WarmupEvictionOrder; got != "written" {
		t.Fatalf("warmup_eviction_order = %q, want written", got)
	}
	if got := loadWith(t, `{"disk_warmup_quota_gb": 5}`).WarmupEvictionOrder; got != "written" {
		t.Fatalf("an absent key became %q", got)
	}
}

func TestWarmupEvictionOrderAcceptsReadAndRefusesTheRest(t *testing.T) {
	if got := loadWith(t, `{"warmup_eviction_order": "read"}`).WarmupEvictionOrder; got != "read" {
		t.Fatalf("read became %q", got)
	}
	for _, bad := range []string{"Read", "lru", "", " read"} {
		if got := loadWith(t, `{"warmup_eviction_order": "`+bad+`"}`).WarmupEvictionOrder; got != "written" {
			t.Fatalf("%q became %q, want written", bad, got)
		}
	}
}

// warm_next_episode (roadmap step 23) and its same-torrent switch are off by default.

func TestWarmNextEpisodeIsOffByDefault(t *testing.T) {
	c := loadCfg(t)
	if c.WarmNextEpisode || c.WarmNextEpisodeSameTorrent {
		t.Fatalf("warm_next_episode = %v, same_torrent = %v, want both false", c.WarmNextEpisode, c.WarmNextEpisodeSameTorrent)
	}
	c = loadWith(t, `{"warm_next_episode": true, "warm_next_episode_same_torrent": true}`)
	if !c.WarmNextEpisode || !c.WarmNextEpisodeSameTorrent {
		t.Fatalf("explicit true became %v / %v", c.WarmNextEpisode, c.WarmNextEpisodeSameTorrent)
	}
}
