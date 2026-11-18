package daemon

import (
	"encoding/json"
	"testing"
)

// An open session lists the pump of its file: where the pump fetches (pump_offset), the
// player position it last saved (player_off), whether a primary handle is open on the path
// and the pump's reference count. The fields are absent while no pump runs.
func TestSessionViewShowsThePumpOfItsFile(t *testing.T) {
	const path = "/m/pump.mkv"
	s := &TTFFSession{path: path}

	var none sessionView
	s.fillPump(&none)
	raw, _ := json.Marshal(none)
	var bare map[string]any
	_ = json.Unmarshal(raw, &bare)
	for _, k := range []string{"pump_offset", "player_off", "primary", "refs"} {
		if _, ok := bare[k]; ok {
			t.Errorf("%s present with no pump", k)
		}
	}

	ps := &NativePumpState{path: path, refCount: 2, playerOff: 7 << 20}
	ps.pumpOff.Store(40 << 20)
	app.activePumps.Store(path, ps)
	primary := &MkvHandle{path: path}
	primary.isPrimaryHandle.Store(true)
	app.activeHandles.Store(primary, true)
	t.Cleanup(func() { app.activePumps.Delete(path); app.activeHandles.Delete(primary) })

	var v sessionView
	s.fillPump(&v)
	raw, _ = json.Marshal(v)
	var got struct {
		PumpOffset *int64 `json:"pump_offset"`
		PlayerOff  *int64 `json:"player_off"`
		Primary    *bool  `json:"primary"`
		Refs       *int32 `json:"refs"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.PumpOffset == nil || *got.PumpOffset != 40<<20 || got.PlayerOff == nil || *got.PlayerOff != 7<<20 ||
		got.Primary == nil || !*got.Primary || got.Refs == nil || *got.Refs != 2 {
		t.Errorf("view = %s", raw)
	}

	app.activeHandles.Delete(primary)
	var v2 sessionView
	s.fillPump(&v2)
	if v2.Primary == nil || *v2.Primary {
		t.Error("primary must be false when no primary handle is open on the path")
	}
}
