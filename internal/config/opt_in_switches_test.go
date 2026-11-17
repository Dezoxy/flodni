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
	if c.UnconfirmedReadaheadClamp || c.AttributeFillsToReader || c.RedialDroppedPeer || c.EngineRefillV2 || c.EngineColdBoost {
		t.Fatalf("an opt-in switch is on by default: clamp=%v attribute=%v redial=%v refill_v2=%v cold_boost=%v",
			c.UnconfirmedReadaheadClamp, c.AttributeFillsToReader, c.RedialDroppedPeer, c.EngineRefillV2, c.EngineColdBoost)
	}
}

func TestOptInSwitchesSurviveThePanelRoundTrip(t *testing.T) {
	in := Config{UnconfirmedReadaheadClamp: true, AttributeFillsToReader: true, RedialDroppedPeer: true, EngineRefillV2: true, EngineColdBoost: true, WarmupDir: "/cache"}
	b, err := json.Marshal(in.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	b, _ = json.Marshal(out)
	for _, key := range []string{"unconfirmed_readahead_clamp", "attribute_fills_to_reader", "redial_dropped_peer", "engine_refill_v2", "engine_cold_boost"} {
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
	for _, key := range []string{"unconfirmed_readahead_clamp", "attribute_fills_to_reader", "redial_dropped_peer", "engine_refill_v2", "engine_cold_boost"} {
		if !strings.Contains(string(raw), `"`+key+`": false`) {
			t.Errorf(`config.json.example must carry "%s": false`, key)
		}
	}
	if c.UnconfirmedReadaheadClamp || c.AttributeFillsToReader || c.RedialDroppedPeer || c.EngineRefillV2 || c.EngineColdBoost {
		t.Error("config.json.example turns a switch on")
	}
}
