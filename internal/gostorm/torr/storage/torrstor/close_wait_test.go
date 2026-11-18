package torrstor

import (
	"sync/atomic"
	"testing"
)

// Close returns only after the cache's background goroutines are done. They need muReaders,
// which Close holds, so they can only finish once it unlocks: Close has to join them after.
// A goroutine that is released by Close and then takes muReaders is the cleanup's shape.
func TestCloseWaitsForItsBackgroundGoroutines(t *testing.T) {
	const rounds = 200
	for i := 0; i < rounds; i++ {
		c := NewCache(1<<20, nil)
		var finished atomic.Bool
		c.goBG(func() {
			<-c.cleanStop // released inside Close, while it holds muReaders
			c.muReaders.RLock()
			c.muReaders.RUnlock() //nolint:staticcheck // the wait is the point
			finished.Store(true)
		})
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if !finished.Load() {
			t.Fatalf("round %d: Close returned while a background goroutine was still running", i)
		}
	}
}

// A second Close joins too, and a goroutine asked for after Close never starts.
func TestCloseTwiceJoinsAndLateBackgroundWorkIsDropped(t *testing.T) {
	c := NewCache(1<<20, nil)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	var ran atomic.Bool
	c.goBG(func() { ran.Store(true) })
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c.bg.Wait()
	if ran.Load() {
		t.Error("a goroutine started on a closed cache")
	}
}
