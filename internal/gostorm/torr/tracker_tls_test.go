package torr

import (
	"context"
	"testing"

	"flodni/internal/gostorm/settings"
)

// tracker_tls_insecure reaches the engine client's configuration when the client is built, and
// is off unless the daemon sets it.
func TestConfigureThreadsTrackerTLSInsecure(t *testing.T) {
	prev, prevArgs, prevIP, prevFlag := settings.BTsets, settings.Args, settings.PubIPv4, TrackerTLSInsecure
	t.Cleanup(func() {
		settings.BTsets, settings.Args, settings.PubIPv4, TrackerTLSInsecure = prev, prevArgs, prevIP, prevFlag
	})
	settings.PubIPv4 = "93.184.216.34"
	settings.Args = &settings.ExecArgs{}
	settings.BTsets = &settings.BTSets{CacheSize: 1 << 20, ConnectionsLimit: 5}

	TrackerTLSInsecure = false
	bt := NewBTS()
	bt.configure(context.Background())
	if bt.config.TrackerInsecureSkipVerify {
		t.Fatal("tracker certificates are not verified by default")
	}

	TrackerTLSInsecure = true
	bt = NewBTS()
	bt.configure(context.Background())
	if !bt.config.TrackerInsecureSkipVerify {
		t.Fatal("tracker_tls_insecure did not reach the engine client config")
	}
}
