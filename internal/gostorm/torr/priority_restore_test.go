package torr

import (
	"testing"

	sets "flodni/internal/gostorm/settings"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
)

// An engine rebuild re-adds a torrent as a new object, and the priority flag lived on the old
// one. A torrent re-added while a confirmed playback of it is open gets the flag back; any
// other does not.
func TestNewTorrentRestoresPriorityOfConfirmedPlayback(t *testing.T) {
	oldPath, oldRO := sets.Path, sets.ReadOnly
	sets.Path = t.TempDir()
	sets.InitSets(false, false)
	t.Cleanup(func() { sets.Path, sets.ReadOnly = oldPath, oldRO })
	cfg := torrent.NewDefaultClientConfig()
	cfg.NoDHT = true
	cfg.NoUpload = true
	cfg.ListenPort = 0
	cfg.DataDir = t.TempDir()
	cfg.DisableTrackers = true
	client, err := torrent.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	info := testInfoBytes(t, ptr(true))
	h := metainfo.HashBytes(info)
	prev := ConfirmedPlayback
	t.Cleanup(func() { ConfirmedPlayback = prev })

	for _, tc := range []struct {
		name string
		hook func(string) bool
		want bool
	}{
		{"confirmed playback", func(hash string) bool { return hash == h.HexString() }, true},
		{"another torrent is playing", func(string) bool { return false }, false},
		{"no hook", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ConfirmedPlayback = tc.hook
			bt := NewBTS()
			bt.client = client
			tor, err := NewTorrent(&torrent.TorrentSpec{InfoHash: h, InfoBytes: info}, bt)
			if err != nil {
				t.Fatal(err)
			}
			if got := tor.IsPriority.Load(); got != tc.want {
				t.Errorf("IsPriority after the re-add = %v, want %v", got, tc.want)
			}
		})
	}
}
