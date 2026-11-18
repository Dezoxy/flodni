package daemon

import "flodni/internal/config"

// trackerTLSWarning is the start-up line when tracker_tls_insecure turns certificate
// verification off for https trackers, "" while it is on (the default).
func trackerTLSWarning(c *config.Config) string {
	if !c.TrackerTLSInsecure {
		return ""
	}
	return "[Tracker] TLS certificate verification is OFF (tracker_tls_insecure)"
}
