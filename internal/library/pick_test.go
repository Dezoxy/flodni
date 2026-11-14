package library

import (
	"io"
	"log"
	"testing"
)

// Dezoxy fork: a single-episode release whose Sample/ clip is listed first must file
// the episode, not the clip; both name the episode.
func TestPickFileForEpisodeSkipsTheSample(t *testing.T) {
	m := New(Config{Logger: log.New(io.Discard, "", 0)})
	files := []FileStat{
		{ID: 1, Path: "South.Park.S29E01.1080p.WEB.h264-GRACE/Sample/south.park.s29e01.1080p.web.h264-grace-sample.mkv", Length: 33856278},
		{ID: 2, Path: "South.Park.S29E01.1080p.WEB.h264-GRACE/south.park.s29e01.1080p.web.h264-grace.mkv", Length: 612345678},
		{ID: 3, Path: "South.Park.S29E01.1080p.WEB.h264-GRACE/south.park.s29e01.1080p.web.h264-grace.nfo", Length: 4000},
	}
	got, err := m.pickFileForEpisode(AddRequest{Season: 29, Episode: 1}, files)
	if err != nil || got.ID != 2 {
		t.Fatalf("picked %+v (err %v), want the episode file 2", got, err)
	}

	// A file naming another episode never wins, however large.
	files = append(files, FileStat{ID: 4, Path: "x/South.Park.S29E02.mkv", Length: 9e9})
	if got, _ := m.pickFileForEpisode(AddRequest{Season: 29, Episode: 1}, files); got.ID != 2 {
		t.Fatalf("picked %d, want 2", got.ID)
	}

	// Nothing names the episode: the largest video file, as before.
	plain := []FileStat{
		{ID: 1, Path: "Show/a.mkv", Length: 10},
		{ID: 2, Path: "Show/b.mkv", Length: 20},
	}
	if got, _ := m.pickFileForEpisode(AddRequest{Season: 1, Episode: 3}, plain); got.ID != 2 {
		t.Fatalf("fallback picked %d, want the largest, 2", got.ID)
	}

	// An explicit file index still wins.
	if got, _ := m.pickFileForEpisode(AddRequest{Season: 29, Episode: 1, FileIndex: 1}, files); got.ID != 1 {
		t.Fatalf("explicit index picked %d, want 1", got.ID)
	}
}
