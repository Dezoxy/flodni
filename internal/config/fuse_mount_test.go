package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// The FUSE timeouts and splice are startup-only mount options. The defaults are the
// behaviour before they were options: 1 s attributes, 1 s entries, no negative cache,
// splice as go-fuse has it.
func TestFuseMountOptionDefaultsAreUnchanged(t *testing.T) {
	c := loadCfg(t)
	if c.AttrTimeoutSeconds != 1.0 || c.EntryTimeoutSeconds != 1.0 || c.NegativeTimeoutSeconds != 0.0 {
		t.Fatalf("timeouts = %v/%v/%v, want 1/1/0", c.AttrTimeoutSeconds, c.EntryTimeoutSeconds, c.NegativeTimeoutSeconds)
	}
	if c.FuseDisableSplice {
		t.Fatal("fuse_disable_splice is on by default")
	}
}

// An out-of-range timeout refuses the start (and a panel save, through checkLoadable);
// the bounds themselves are fine.
func TestFuseTimeoutsOutOfRangeRefuseTheLoad(t *testing.T) {
	for _, key := range []string{"attr_timeout_seconds", "entry_timeout_seconds", "negative_timeout_seconds"} {
		for _, v := range []string{"-0.5", "60.5", "1e9"} {
			if _, err := loadWithErr(t, `{"`+key+`": `+v+`}`); err == nil || !strings.Contains(err.Error(), key) {
				t.Errorf("%s=%s: err = %v, want a refusal naming the key", key, v, err)
			}
		}
		for _, v := range []string{"0", "0.5", "60"} {
			if _, err := loadWithErr(t, `{"`+key+`": `+v+`}`); err != nil {
				t.Errorf("%s=%s refused: %v", key, v, err)
			}
		}
	}
}

func TestFuseDisableSpliceRoundTripsAndExampleCarriesIt(t *testing.T) {
	if !loadWith(t, `{"fuse_disable_splice": true}`).FuseDisableSplice {
		t.Fatal("fuse_disable_splice true not read")
	}
	b, err := json.Marshal(Config{FuseDisableSplice: true}.Redacted())
	if err != nil || !strings.Contains(string(b), `"fuse_disable_splice":true`) {
		t.Fatalf("panel round trip loses the key: %s %v", b, err)
	}
	raw, err := os.ReadFile("../../config.json.example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"fuse_disable_splice": false`) {
		t.Fatal(`config.json.example must carry "fuse_disable_splice": false`)
	}
}
