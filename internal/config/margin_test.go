package config

import "testing"

func TestUnconfirmedReadMargin(t *testing.T) {
	if got := loadCfg().UnconfirmedReadMarginMB; got != 64 {
		t.Fatalf("default = %d MB, want 64", got)
	}
	tests := []struct {
		name   string
		in     int64
		wantMB int64
	}{
		{"default value", 64, 64},
		{"custom", 128, 128},
		{"zero is kept", 0, 0},
		{"negative falls back", -1, 64},
		{"absurd falls back", 1 << 30, 64},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := Config{UnconfirmedReadMarginMB: tt.in}
			c.finalize()
			if c.UnconfirmedReadMarginMB != tt.wantMB {
				t.Errorf("MB = %d, want %d", c.UnconfirmedReadMarginMB, tt.wantMB)
			}
			if c.UnconfirmedReadMargin != tt.wantMB*1024*1024 {
				t.Errorf("bytes = %d, want %d", c.UnconfirmedReadMargin, tt.wantMB*1024*1024)
			}
		})
	}
}
