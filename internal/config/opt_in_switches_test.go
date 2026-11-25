package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The reader budget follow-ups and the dropped-peer redial change behaviour, so each is
// opt-in: off by default, off in config.json.example, and kept by the control panel's
// save (decode what it got, encode it back).

func TestOptInSwitchesDefaultToOff(t *testing.T) {
	c := loadCfg(t)
	if c.UnconfirmedReadaheadClamp || c.AttributeFillsToReader || c.RedialDroppedPeer || c.EngineRefillV2 || c.EngineColdBoost || c.TrackerTLSInsecure || c.ScrapeOnFailure {
		t.Fatalf("an opt-in switch is on by default: clamp=%v attribute=%v redial=%v refill_v2=%v cold_boost=%v tls_insecure=%v",
			c.UnconfirmedReadaheadClamp, c.AttributeFillsToReader, c.RedialDroppedPeer, c.EngineRefillV2, c.EngineColdBoost, c.TrackerTLSInsecure)
	}
}

func TestOptInSwitchesSurviveThePanelRoundTrip(t *testing.T) {
	in := Config{UnconfirmedReadaheadClamp: true, AttributeFillsToReader: true, RedialDroppedPeer: true, EngineRefillV2: true, EngineColdBoost: true, TrackerTLSInsecure: true, ScrapeOnFailure: true, WarmupDir: "/cache"}
	b, err := json.Marshal(in.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(out)
	for _, key := range []string{"unconfirmed_readahead_clamp", "attribute_fills_to_reader", "redial_dropped_peer", "engine_refill_v2", "engine_cold_boost", "tracker_tls_insecure", "scrape_on_failure"} {
		if !strings.Contains(string(b), `"`+key+`":true`) {
			t.Errorf("%s lost in the round trip: %s", key, b)
		}
	}
}

func TestConfigExampleCarriesTheOptInSwitchesOff(t *testing.T) {
	raw, err := os.ReadFile("../../config.json.example")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"unconfirmed_readahead_clamp", "attribute_fills_to_reader", "redial_dropped_peer", "engine_refill_v2", "engine_cold_boost", "tracker_tls_insecure", "scrape_on_failure"} {
		if !strings.Contains(string(raw), `"`+key+`": false`) {
			t.Errorf(`config.json.example must carry "%s": false`, key)
		}
	}
	if c.UnconfirmedReadaheadClamp || c.AttributeFillsToReader || c.RedialDroppedPeer || c.EngineRefillV2 || c.EngineColdBoost || c.TrackerTLSInsecure || c.ScrapeOnFailure {
		t.Error("config.json.example turns a switch on")
	}
}

// tracker_tls_insecure parses from config.json and is read from the key's own name.
func TestTrackerTLSInsecureParses(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"tracker_tls_insecure": true}`), &c); err != nil {
		t.Fatal(err)
	}
	if !c.TrackerTLSInsecure {
		t.Fatal("tracker_tls_insecure did not parse")
	}
}

// OptInSwitches is the one list of opt-in switches: its keys are the JSON names.
func TestOptInSwitchesListsEverySwitchUnderItsJSONName(t *testing.T) {
	all := &Config{UnconfirmedReadaheadClamp: true, AttributeFillsToReader: true, RedialDroppedPeer: true, EngineRefillV2: true, EngineColdBoost: true, TrackerTLSInsecure: true, ScrapeOnFailure: true}
	got := all.OptInSwitches()
	if len(got) != 7 {
		t.Fatalf("OptInSwitches has %d entries, want 7: %v", len(got), got)
	}
	b, err := json.Marshal(*all)
	if err != nil {
		t.Fatal(err)
	}
	for key, on := range got {
		if !on {
			t.Errorf("%s is false with every switch on", key)
		}
		if !strings.Contains(string(b), `"`+key+`":true`) {
			t.Errorf("%s is not a JSON tag of Config: %s", key, b)
		}
	}
	def := loadCfg(t)
	for key, on := range def.OptInSwitches() {
		if on {
			t.Errorf("%s is on in the default config", key)
		}
	}
}
