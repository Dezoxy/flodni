package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// playback_webhook_warn_days is the window of the "no play webhook" warning: 7 by
// default, 0 turns the warning off, a negative or absurd value falls back to 7.

func TestPlaybackWebhookWarnDaysDefaultsToSeven(t *testing.T) {
	if got := loadCfg(t).PlaybackWebhookWarnDays; got != 7 {
		t.Fatalf("playback_webhook_warn_days = %d, want 7", got)
	}
}

func TestPlaybackWebhookWarnDaysZeroStaysOffAndBadValuesFallBack(t *testing.T) {
	for body, want := range map[string]int{
		`{"playback_webhook_warn_days": 0}`:      0,
		`{"playback_webhook_warn_days": 3}`:      3,
		`{"playback_webhook_warn_days": -1}`:     7,
		`{"playback_webhook_warn_days": 999999}`: 7,
		`{"playback_webhook_warn_days": 36500}`:  36500,
		`{"played_protection_days": 5}`:          7, // key absent: default
	} {
		if got := loadWith(t, body).PlaybackWebhookWarnDays; got != want {
			t.Errorf("%s -> %d, want %d", body, got, want)
		}
	}
}

func TestPlaybackWebhookWarnDaysSurvivesThePanelRoundTrip(t *testing.T) {
	b, err := json.Marshal(Config{PlaybackWebhookWarnDays: 5}.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"playback_webhook_warn_days":5`) {
		t.Fatalf("GET /api/config would not carry the key: %s", b)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil || out.PlaybackWebhookWarnDays != 5 {
		t.Fatalf("lost in the round trip: %d, %v", out.PlaybackWebhookWarnDays, err)
	}
}

func TestConfigExampleCarriesPlaybackWebhookWarnDays(t *testing.T) {
	raw, err := os.ReadFile("../../config.json.example")
	if err != nil {
		t.Fatal(err)
	}
	var c Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"playback_webhook_warn_days": 7`) || c.PlaybackWebhookWarnDays != 7 {
		t.Fatal(`config.json.example must carry "playback_webhook_warn_days": 7`)
	}
}
