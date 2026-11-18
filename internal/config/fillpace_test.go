package config

import "testing"

func TestFillPaceDefaultsAndLimits(t *testing.T) {
	c := loadCfg(t)
	if !c.FillPaceEnabled || c.FillPaceMbit != 500 || c.FillPaceMarginMB != 128 {
		t.Fatalf("defaults = %v %d %d, want true 500 128", c.FillPaceEnabled, c.FillPaceMbit, c.FillPaceMarginMB)
	}
	tests := []struct {
		name         string
		mbit, margin int64
		wantMbit     int
		wantMargin   int64
	}{
		{"in range", 100, 64, 100, 64},
		{"zero margin is kept", 100, 0, 100, 0},
		{"zero rate falls back", 0, 64, 500, 64},
		{"negative rate falls back", -5, 64, 500, 64},
		{"absurd rate falls back", 1 << 40, 64, 500, 64},
		{"negative margin falls back", 100, -1, 100, 128},
		{"absurd margin falls back", 100, 1 << 40, 100, 128},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{FillPaceMbit: int(tt.mbit), FillPaceMarginMB: tt.margin}
			c.finalize()
			if c.FillPaceMbit != tt.wantMbit || c.FillPaceMarginMB != tt.wantMargin {
				t.Errorf("got %d Mbit, %d MB; want %d, %d", c.FillPaceMbit, c.FillPaceMarginMB, tt.wantMbit, tt.wantMargin)
			}
		})
	}
}
