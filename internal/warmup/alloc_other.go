//go:build !linux && !darwin

package warmup

import "os"

// diskBytes falls back to the apparent size where the platform has no block count.
func diskBytes(fi os.FileInfo) int64 { return fi.Size() }
