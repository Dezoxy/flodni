package warmup

import (
	"os"
	"sync/atomic"
)

// allocatedBytes is the size the quota counts for a file: the bytes it holds on disk, so
// a sparse file counts what it allocated and not its length. Code that needs a file's
// logical length (windows, coverage) keeps using Size(). A var so a test that fakes a
// large cache with sparse files can count their apparent size instead.
var allocatedBytes = func(fi os.FileInfo) int64 { return diskBytes(fi) }

// credit records the allocated size now seen for path and adds the change since the last
// credit to totalSize. The ledger holds what each file has contributed, so a removal
// gives back exactly that, even where the filesystem reports blocks late. tailMu held.
func (d *DiskWarmupCache) credit(path string, now int64) {
	if d.ledger == nil {
		d.ledger = map[string]int64{}
	}
	atomic.AddInt64(&d.totalSize, now-d.ledger[path])
	d.ledger[path] = now
}

// debit takes a file's contribution back out of totalSize: exactly what the ledger holds
// for it, nothing for a file the ledger has not seen. tailMu held.
func (d *DiskWarmupCache) debit(path string) {
	atomic.AddInt64(&d.totalSize, -d.ledger[path])
	delete(d.ledger, path)
}
