package daemon

import "testing"

func TestEOFReanchor(t *testing.T) {
	t.Parallel()
	const size, lead, chunk = 1000 * mb, 52 * mb, 4 * mb // the last chunk starts at 996 MiB
	cases := []struct {
		name      string
		playerOff int64
		want      int64
		ok        bool
	}{
		{"no primary", 0, 0, false},
		{"the player far from the end restarts the pump at its chunk", 108*mb + 100, 108 * mb, true},
		{"just short of the last chunk's reach", 996*mb - lead - 1, ((996*mb - lead - 1) / chunk) * chunk, true},
		{"the pump may read the last chunk from here: the end is where it belongs", 996*mb - lead, 0, false},
		{"the player at the end", size - mb, 0, false},
	}
	for _, c := range cases {
		got, ok := eofReanchor(c.playerOff, size, lead, chunk)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}

// pumpAtEnd and the pump's hard limit are one boundary: the pump reads the last chunk
// exactly when the player is at the end.
func TestPumpAtEndIsTheHardLimitOfTheLastChunk(t *testing.T) {
	t.Parallel()
	const size, lead, chunk = 1000 * mb, 52 * mb, 4 * mb
	lastStart := int64(996 * mb)
	for _, player := range []int64{0, 108 * mb, lastStart - lead - 1, lastStart - lead, lastStart, size - 1} {
		if got, want := pumpAtEnd(player, size, lead, chunk), chunkWithinLead(lastStart, player, lead); got != want {
			t.Errorf("player at %d: pumpAtEnd = %v, the last chunk within the lead = %v", player, got, want)
		}
	}
}
