package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// warm_on_add is off unless the owner turns it on, and the control panel's save (decode
// what it got, encode it back) keeps it.

func TestWarmOnAddDefaultsToOff(t *testing.T) {
	if loadCfg(t).WarmOnAdd {
		t.Fatal("warm_on_add is on by default: it spends bandwidth on every add")
	}
}

func TestWarmOnAddSurvivesThePanelRoundTrip(t *testing.T) {
	in := Config{WarmOnAdd: true, WarmupDir: "/cache"}
	b, err := json.Marshal(in.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"warm_on_add":true`) {
		t.Fatalf("GET /api/config would not carry the key: %s", b)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.WarmOnAdd {
		t.Fatal("warm_on_add lost in the round trip")
	}
	b, _ = json.Marshal(out)
	if !strings.Contains(string(b), `"warm_on_add":true`) {
		t.Fatalf("the saved file would lose the key: %s", b)
	}
}

func TestConfigExampleCarriesWarmOnAddOff(t *testing.T) {
	raw, err := os.ReadFile("../../config.json.example")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"warm_on_add": false`) || c.WarmOnAdd {
		t.Fatal(`config.json.example must carry "warm_on_add": false`)
	}
}
