package torrstor

import (
	"sync/atomic"
	"testing"

	"flodni/internal/gostorm/settings"
)

// A sequential reader whose readahead is R must never see a piece inside [pos-behind, pos+R]
// offered for eviction, while complete pieces outside it still are. Pieces between the old
// percentage split and the full readahead used to be fetched with deadlines and then evicted.
func TestReadaheadWindowIsNeverEvictable(t *testing.T) {
	oldSets := settings.BTsets
	// Registered first, so it runs last: Close starts a background rebuild that reads settings.
	t.Cleanup(func() { settings.BTsets = oldSets })
	settings.BTsets = &settings.BTSets{ReaderReadAHead: 75}

	const (
		pieces   = 100
		pieceLen = int64(1 << 20) // the head and tail 8 MiB of a file are always kept
		capPcs   = 40             // protected = 85% of it = 34 pieces
		readPos  = 40             // reader sits at the start of piece 40
		raPieces = 32             // forward 32, behind 2; the 75% split would stop at 25.5 pieces
	)
	_, cache := newPriorityTestCacheSized(t, pieces, pieceLen)
	cache.capacity = capPcs * pieceLen
	for id, p := range cache.pieces {
		p.Size = pieceLen
		p.Complete.Store(true)
		atomic.StoreInt64(&p.Accessed, int64(id))
	}

	file := cache.torrent.Files()[0]
	r := cache.NewReader(file)
	t.Cleanup(func() {
		r.Close()
		_ = cache.Close()
		cache.bg.Wait()
	})
	r.mu.Lock()
	r.offset = readPos * pieceLen
	r.mu.Unlock()
	r.SetReadahead(raPieces * pieceLen)

	evictable := map[int]bool{}
	for _, p := range cache.getRemPieces() {
		evictable[p.Id] = true
	}
	behind := int(capPcs*pieceLen*protectedSafetyPct/100/pieceLen) - raPieces
	for id := 0; id < pieces; id++ {
		inWindow := id >= readPos-behind && id <= readPos+raPieces
		if inWindow && evictable[id] {
			t.Errorf("piece %d is inside the readahead window [%d, %d] but evictable", id, readPos-behind, readPos+raPieces)
		}
		// isIdInFileBE keeps the first and last 8 MiB of the file regardless of any reader.
		keptEnds := id < 8 || id > pieces-8
		if !inWindow && !keptEnds && !evictable[id] {
			t.Errorf("piece %d is outside the window [%d, %d] but protected", id, readPos-behind, readPos+raPieces)
		}
	}
}

func TestProtectedWindow(t *testing.T) {
	const mib = int64(1 << 20)
	cases := []struct {
		name                    string
		cur, protected, prc, ra int64
		wantBegin, wantEnd      int64
	}{
		{"small pieces, readahead inside the window", 100 * mib, 100 * mib, 75, 60 * mib, 60 * mib, 160 * mib},
		{"readahead above the old split", 100 * mib, 100 * mib, 75, 90 * mib, 90 * mib, 190 * mib},
		{"large pieces, readahead fills the window", 500 * mib, 160 * mib, 95, 160 * mib, 500 * mib, 660 * mib},
		{"readahead larger than capacity is capped", 100 * mib, 50 * mib, 95, 400 * mib, 100 * mib, 150 * mib},
		{"no readahead yet uses the percentage", 100 * mib, 100 * mib, 75, 0, 75 * mib, 175 * mib},
		{"negative readahead behaves like none", 100 * mib, 100 * mib, 75, -1, 75 * mib, 175 * mib},
		{"file start is not clamped here", 10 * mib, 100 * mib, 75, 60 * mib, -30 * mib, 70 * mib},
	}
	for _, c := range cases {
		b, e := protectedWindow(c.cur, c.protected, c.prc, c.ra)
		if b != c.wantBegin || e != c.wantEnd {
			t.Errorf("%s: protectedWindow = [%d, %d], want [%d, %d]", c.name, b, e, c.wantBegin, c.wantEnd)
		}
		if e-b != c.protected && c.ra >= 0 {
			t.Errorf("%s: window width %d, want the protected share %d", c.name, e-b, c.protected)
		}
	}
}

// A pure sequential read with readahead R re-fetches nothing: each step the engine delivers the
// pieces up to pos+R in order, the reader advances, and every piece in the window stays complete.
func TestSequentialReadRefetchesNothing(t *testing.T) {
	oldSets := settings.BTsets
	// Registered first, so it runs last: Close starts a background rebuild that reads settings.
	t.Cleanup(func() { settings.BTsets = oldSets })
	settings.BTsets = &settings.BTSets{ReaderReadAHead: 75}

	const (
		pieces   = 100
		pieceLen = int64(1 << 20)
		capPcs   = 40
		raPieces = 32 // forward 32, behind 2 of the 34-piece protected share
		start    = 20
	)
	_, cache := newPriorityTestCacheSized(t, pieces, pieceLen)
	cache.capacity = capPcs * pieceLen
	r := cache.NewReader(cache.torrent.Files()[0])
	t.Cleanup(func() {
		r.Close()
		_ = cache.Close()
		cache.bg.Wait()
	})
	r.SetReadahead(raPieces * pieceLen)

	refetched0 := RefetchedBytes()
	stamp := int64(0)
	for pos := start; pos < start+40; pos++ {
		r.mu.Lock()
		r.offset = int64(pos) * pieceLen
		r.mu.Unlock()
		for id := pos; id <= pos+raPieces && id < pieces; id++ {
			p := cache.pieces[id]
			if p.Complete.Load() {
				continue
			}
			if _, err := p.WriteAt(make([]byte, pieceLen), 0); err != nil {
				t.Fatal(err)
			}
			stamp++
			atomic.StoreInt64(&p.Accessed, stamp)
			if err := p.MarkComplete(); err != nil {
				t.Fatal(err)
			}
		}
		cache.evictNow()
		for id := pos; id <= pos+raPieces && id < pieces; id++ {
			if !cache.pieces[id].Complete.Load() {
				t.Fatalf("pos %d: piece %d of the readahead window was evicted", pos, id)
			}
		}
	}
	if got := RefetchedBytes() - refetched0; got != 0 {
		t.Errorf("a sequential read refetched %d bytes, want 0", got)
	}
	if got := cache.strandedPartialBytes(); got != 0 {
		t.Errorf("stranded partial bytes = %d, want 0", got)
	}
}
