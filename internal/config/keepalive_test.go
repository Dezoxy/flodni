package config

import (
	"encoding/json"
	"os"
	"testing"
)

// playback_keepalive_minutes is the keep-alive's one off switch: 15 by default, 0 turns
// it off, a negative or absurd value falls back to 15.
func TestPlaybackKeepaliveMinutes(t *testing.T) {
	if got := loadCfg(t).PlaybackKeepaliveMinutes; got != 15 {
		t.Fatalf("default = %d, want 15", got)
	}
	for body, want := range map[string]int{
		`{"playback_keepalive_minutes": 0}`:     0,
		`{"playback_keepalive_minutes": 30}`:    30,
		`{"playback_keepalive_minutes": 1440}`:  1440,
		`{"playback_keepalive_minutes": -1}`:    15,
		`{"playback_keepalive_minutes": 99999}`: 15,
		`{"played_protection_days": 5}`:         15, // key absent: default
	} {
		if got := loadWith(t, body).PlaybackKeepaliveMinutes; got != want {
			t.Errorf("%s -> %d, want %d", body, got, want)
		}
	}
}

// playback_keepalive_max_torrents is the cap: 4 by default, at least 1. 0 or a negative
// value is replaced by 4, so 0 never means off or unlimited here.
func TestPlaybackKeepaliveMaxTorrents(t *testing.T) {
	if got := loadCfg(t).PlaybackKeepaliveMaxTorrents; got != 4 {
		t.Fatalf("default = %d, want 4", got)
	}
	for body, want := range map[string]int{
		`{"playback_keepalive_max_torrents": 1}`:  1,
		`{"playback_keepalive_max_torrents": 9}`:  9,
		`{"playback_keepalive_max_torrents": 0}`:  4,
		`{"playback_keepalive_max_torrents": -2}`: 4,
		`{"played_protection_days": 5}`:           4,
	} {
		if got := loadWith(t, body).PlaybackKeepaliveMaxTorrents; got != want {
			t.Errorf("%s -> %d, want %d", body, got, want)
		}
	}
}

func TestPlaybackKeepaliveSurvivesThePanelRoundTripAndTheExample(t *testing.T) {
	b, err := json.Marshal(Config{PlaybackKeepaliveMinutes: 20, PlaybackKeepaliveMaxTorrents: 2}.Redacted())
	if err != nil {
		t.Fatal(err)
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil || out.PlaybackKeepaliveMinutes != 20 || out.PlaybackKeepaliveMaxTorrents != 2 {
		t.Fatalf("lost in the round trip: %+v, %v", out, err)
	}
	raw, err := os.ReadFile("../../config.json.example")
	if err != nil {
		t.Fatal(err)
	}
	var ex Config
	if err := json.Unmarshal(raw, &ex); err != nil {
		t.Fatal(err)
	}
	if ex.PlaybackKeepaliveMinutes != 15 || ex.PlaybackKeepaliveMaxTorrents != 4 {
		t.Fatalf("config.json.example must carry 15 and 4, has %d and %d",
			ex.PlaybackKeepaliveMinutes, ex.PlaybackKeepaliveMaxTorrents)
	}
}
