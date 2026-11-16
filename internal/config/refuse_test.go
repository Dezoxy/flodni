package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// refuse_unconfirmed_reads is off unless the owner turns it on, and the control panel's
// save (decode what it got, encode it back) keeps it.

func TestRefuseUnconfirmedReadsDefaultsToOff(t *testing.T) {
	if loadCfg(t).RefuseUnconfirmedReads {
		t.Fatal("refuse_unconfirmed_reads is on by default: it refuses reads")
	}
}

func TestRefuseUnconfirmedReadsSurvivesThePanelRoundTrip(t *testing.T) {
	in := Config{RefuseUnconfirmedReads: true, WarmupDir: "/cache"}
	b, err := json.Marshal(in.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"refuse_unconfirmed_reads":true`) {
		t.Fatalf("GET /api/config would not carry the key: %s", b)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !out.RefuseUnconfirmedReads {
		t.Fatal("refuse_unconfirmed_reads lost in the round trip")
	}
	b, _ = json.Marshal(out)
	if !strings.Contains(string(b), `"refuse_unconfirmed_reads":true`) {
		t.Fatalf("the saved file would lose the key: %s", b)
	}
}

func TestConfigExampleCarriesRefuseUnconfirmedReadsOff(t *testing.T) {
	raw, err := os.ReadFile("../../config.json.example")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"refuse_unconfirmed_reads": false`) || c.RefuseUnconfirmedReads {
		t.Fatal(`config.json.example must carry "refuse_unconfirmed_reads": false`)
	}
}
