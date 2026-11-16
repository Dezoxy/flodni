package config

import "testing"

// The tracker scrape settings: 24 hours, batches of 100 and a 1000 ms pace by
// default; 0 hours turns the schedule off; a value out of range is replaced by
// the default with a warning.
func TestTrackerScrapeSettings(t *testing.T) {
	c := loadCfg(t)
	if c.TrackerScrapeHours != 24 || c.TrackerScrapeBatch != 100 || c.TrackerScrapePaceMs != 1000 {
		t.Fatalf("defaults = %d %d %d", c.TrackerScrapeHours, c.TrackerScrapeBatch, c.TrackerScrapePaceMs)
	}
	for body, want := range map[string][3]int{
		`{"tracker_scrape_hours": 0}`:     {0, 100, 1000},
		`{"tracker_scrape_hours": 6}`:     {6, 100, 1000},
		`{"tracker_scrape_hours": -1}`:    {24, 100, 1000},
		`{"tracker_scrape_hours": 99999}`: {24, 100, 1000},
		`{"tracker_scrape_batch": 50}`:    {24, 50, 1000},
		`{"tracker_scrape_batch": 0}`:     {24, 100, 1000},
		`{"tracker_scrape_batch": 5000}`:  {24, 100, 1000},
		`{"tracker_scrape_pace_ms": 250}`: {24, 100, 250},
		`{"tracker_scrape_pace_ms": 5}`:   {24, 100, 1000},
		`{"played_protection_days": 5}`:   {24, 100, 1000},
	} {
		c := loadWith(t, body)
		if got := [3]int{c.TrackerScrapeHours, c.TrackerScrapeBatch, c.TrackerScrapePaceMs}; got != want {
			t.Errorf("%s -> %v, want %v", body, got, want)
		}
	}
	// A number the int cannot hold is a type error: the file is refused.
	if _, err := loadWithErr(t, `{"tracker_scrape_pace_ms": 9e9}`); err == nil {
		t.Error("9e9 loaded without an error")
	}
}
