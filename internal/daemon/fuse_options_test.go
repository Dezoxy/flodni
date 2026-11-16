package daemon

import (
	"strings"
	"testing"
	"time"

	"flodni/internal/config"
)

// The mount options are built without a mount. With the default configuration they are
// what the mount block passed before the options existed.
func TestFuseOptionsDefaultsAreUnchanged(t *testing.T) {
	c := config.Config{AttrTimeoutSeconds: 1, EntryTimeoutSeconds: 1, ConcurrencyLimit: 25}
	o := fuseOptions(&c)
	if *o.AttrTimeout != time.Second || *o.EntryTimeout != time.Second || *o.NegativeTimeout != 0 {
		t.Fatalf("timeouts = %v/%v/%v", *o.AttrTimeout, *o.EntryTimeout, *o.NegativeTimeout)
	}
	m := o.MountOptions
	if m.DisableSplice {
		t.Fatal("splice off by default")
	}
	if m.MaxWrite != 4<<20 || m.MaxReadAhead != 4<<20 || !m.RememberInodes || !m.ExplicitDataCacheControl ||
		m.SyncRead || !m.AllowOther || m.FsName != "flodni" || m.MaxBackground != 25 {
		t.Fatalf("a fixed mount option changed: %+v", m)
	}
}

func TestFuseOptionsMapTheConfig(t *testing.T) {
	c := config.Config{AttrTimeoutSeconds: 2.5, EntryTimeoutSeconds: 5, NegativeTimeoutSeconds: 0.25,
		FuseDisableSplice: true, FuseMaxInflightMB: 64, UID: 7, GID: 9}
	o := fuseOptions(&c)
	if *o.AttrTimeout != 2500*time.Millisecond || *o.EntryTimeout != 5*time.Second || *o.NegativeTimeout != 250*time.Millisecond {
		t.Fatalf("timeouts = %v/%v/%v", *o.AttrTimeout, *o.EntryTimeout, *o.NegativeTimeout)
	}
	if !o.MountOptions.DisableSplice || o.MountOptions.MaxInflightRequestBytes != 64<<20 || o.UID != 7 || o.GID != 9 {
		t.Fatalf("not mapped: %+v", o.MountOptions)
	}
}

// Without kernel dentry invalidation a removed stub can stay visible for the entry TTL
// (ADR 5), so a TTL above the default says so at start. The default stays silent.
func TestFuseTTLWarning(t *testing.T) {
	if w := fuseTTLWarning(&config.Config{AttrTimeoutSeconds: 1, EntryTimeoutSeconds: 1}); w != "" {
		t.Fatalf("default warns: %q", w)
	}
	if w := fuseTTLWarning(&config.Config{AttrTimeoutSeconds: 0, EntryTimeoutSeconds: 0}); w != "" {
		t.Fatalf("shorter warns: %q", w)
	}
	for _, c := range []config.Config{{AttrTimeoutSeconds: 5, EntryTimeoutSeconds: 1}, {AttrTimeoutSeconds: 1, EntryTimeoutSeconds: 5}} {
		if w := fuseTTLWarning(&c); !strings.Contains(w, "invalidation") {
			t.Fatalf("no warning for %+v: %q", c, w)
		}
	}
}
