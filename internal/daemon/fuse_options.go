package daemon

import (
	"fmt"
	"time"

	"flodni/internal/config"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// fuseOptions is what the mount is created with. The timeouts and splice come from the
// configuration (startup-only, ADR 5); the rest is fixed.
func fuseOptions(c *config.Config) *fs.Options {
	attrTimeout := time.Duration(c.AttrTimeoutSeconds * float64(time.Second))
	entryTimeout := time.Duration(c.EntryTimeoutSeconds * float64(time.Second))
	negativeTimeout := time.Duration(c.NegativeTimeoutSeconds * float64(time.Second))

	// go-fuse checks out ~MaxWrite per in-flight request; 0 leaves that unbounded.
	// Past the cap it stops reading /dev/fuse, so this trades RAM for latency.
	maxInflight := int(c.FuseMaxInflightMB * 1024 * 1024)

	return &fs.Options{
		AttrTimeout: &attrTimeout, EntryTimeout: &entryTimeout,
		NegativeTimeout: &negativeTimeout,
		MountOptions: fuse.MountOptions{
			MaxInflightRequestBytes: maxInflight,
			AllowOther:              true,
			MaxBackground:           c.ConcurrencyLimit,
			DisableSplice:           c.FuseDisableSplice,
			// MaxWrite:                 1024 * 1024,
			MaxWrite: 4 * 1024 * 1024, // Samba Turbo: 4MB write buffer
			// MaxReadAhead:             1024 * 1024,
			MaxReadAhead:             4 * 1024 * 1024, // Samba Turbo: 4MB read-ahead
			RememberInodes:           true,            // ENABLED: safe with explicit cache control
			ExplicitDataCacheControl: true,            // attribute-driven invalidation only, not open-time
			SyncRead:                 false,           // ENABLED ASYNC READS for 4K performance
			// NFS Export: Stable filesystem identification
			FsName: "flodni",
		},
		UID: c.UID, // Default file ownership: pi user (1000)
		GID: c.GID, // Default file ownership: pi group (1000)
	}
}

// fuseTTLWarning is the start-up line for an attribute or entry TTL above the default
// 1 s, "" otherwise. Flodni does not invalidate the kernel's dentries when a stub is
// removed or replaced, so a longer TTL keeps the old name visible that long (ADR 5).
func fuseTTLWarning(c *config.Config) string {
	if c.AttrTimeoutSeconds <= 1 && c.EntryTimeoutSeconds <= 1 {
		return ""
	}
	return fmt.Sprintf("[Config] WARNING: FUSE attribute/entry timeouts %.1f/%.1f s are above the default 1/1: "+
		"Flodni has no kernel dentry invalidation, so a removed or replaced stub can stay visible for that long (ADR 5)",
		c.AttrTimeoutSeconds, c.EntryTimeoutSeconds)
}
