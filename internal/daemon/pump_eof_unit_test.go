package daemon

import "testing"

func TestEOFReanchor(t *testing.T) {
	t.Parallel()
	const size, lead, chunk = 1000 * mb, 52 * mb, 4 * mb
	cases := []struct {
		name      string
		playerOff int64
		want      int64
		ok        bool
	}{
		{"no primary", 0, 0, false},
		{"the player far from the end restarts the pump at its chunk", 108*mb + 100, 108 * mb, true},
		{"the player within one lead of the end: the end is where the pump belongs", size - lead + 1, 0, false},
		{"the player at the end", size - mb, 0, false},
		{"just outside one lead", size - lead - 1, ((size - lead - 1) / chunk) * chunk, true},
	}
	for _, c := range cases {
		got, ok := eofReanchor(c.playerOff, size, lead, chunk)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: got (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.ok)
		}
	}
}
