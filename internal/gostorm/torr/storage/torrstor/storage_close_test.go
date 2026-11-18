package torrstor

import (
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

// The engine calls OpenTorrent under its client lock, and the goroutines Cache.Close joins
// take that lock. So CloseHash and Storage.Close must not hold Storage.mu while they join,
// or OpenTorrent of another torrent waits for a join that waits for the client lock.
// The test holds a channel in place of the client lock.
const stuckJoinDeadline = 10 * time.Second

func openParked(t *testing.T, s *Storage, hash metainfo.Hash) (entered, release chan struct{}) {
	t.Helper()
	info := &metainfo.Info{PieceLength: 1 << 20, Length: 8 << 20, Name: "x", Pieces: make([]byte, 20*8)}
	if _, err := s.OpenTorrent(info, hash); err != nil {
		t.Fatal(err)
	}
	entered, release = make(chan struct{}), make(chan struct{})
	c := s.GetCache(hash)
	c.goBG(func() {
		<-c.cleanStop // Close has begun
		close(entered)
		<-release // the client lock, held by someone waiting for Storage.mu
	})
	return entered, release
}

func checkOpenWhileJoining(t *testing.T, closeIt func(s *Storage, a metainfo.Hash)) {
	s := NewStorage(128 * liveMiB)
	a, b := metainfo.Hash{1}, metainfo.Hash{2}
	entered, release := openParked(t, s, a)
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()

	closing := make(chan struct{})
	go func() { closeIt(s, a); close(closing) }()
	<-entered

	opened := make(chan struct{})
	go func() {
		info := &metainfo.Info{PieceLength: 1 << 20, Length: 8 << 20, Name: "y", Pieces: make([]byte, 20*8)}
		_, _ = s.OpenTorrent(info, b)
		close(opened)
	}()
	select {
	case <-opened:
	case <-time.After(stuckJoinDeadline):
		t.Fatal("OpenTorrent of another torrent is blocked while a close joins its cleanup: Storage.mu is held across the join")
	}
	select {
	case <-closing:
		t.Fatal("the close returned before its cleanup finished")
	default:
	}
	released = true
	close(release)
	<-closing
}

func TestCloseHashDoesNotHoldStorageLockWhileJoining(t *testing.T) {
	checkOpenWhileJoining(t, func(s *Storage, a metainfo.Hash) { s.CloseHash(a) })
}

func TestStorageCloseDoesNotHoldStorageLockWhileJoining(t *testing.T) {
	checkOpenWhileJoining(t, func(s *Storage, _ metainfo.Hash) { _ = s.Close() })
}
