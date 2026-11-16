package daemon

import (
	"sync"
	"sync/atomic"

	"flodni/internal/apiauth"
)

// App holds the process-wide read-path state that the FUSE handle, the pump, the
// webhook handler and the cleanup manager share. One instance is created in Run()
// and published as app before any goroutine starts; tests install a fresh one with
// newTestApp so they stop sharing state.
type App struct {
	activeHandles      sync.Map // key: *MkvHandle, value: bool
	inFlightPrefetches sync.Map // key: "path:offset", value: bool
	// inFlightFetches dedups the sync FetchBlock fallback: concurrent misses in the same chunk
	// share one fetch instead of each running FetchBlock independently.
	inFlightFetches  sync.Map // key: "path:chunkAlignedOffset", value: *fetchFlight
	activePumps      sync.Map // Map[string]*NativePumpState: one pump per file path
	pumpTimers       sync.Map // key: path, value: *time.Timer
	priorityTimers   sync.Map // key: path, value: *time.Timer
	playbackRegistry sync.Map // path -> *PlaybackState

	// controlToken is the bearer token the control server enforces, fixed at start-up.
	// Empty means the port is open. Never read gc().APIToken to decide that. It is read
	// once in Run() and shared by the auth middleware and the confirm gate (apiTokenSet).
	controlToken string

	// auth is the token store decided at start-up (open, legacy_token or scoped). Both
	// servers enforce it, and the startup report carries its summary.
	auth *apiauth.Tokens

	// jwt verifies the proxy's assertion on the control port; nil when auth_jwt is off.
	jwt apiauth.JWTVerifier

	// startup collects the startup stages and emits the one [Startup] report.
	startup *startupReporter

	// Reader budget counters, since process start, for /metrics/prometheus.
	readaheadOvershootBytes          atomic.Int64
	attributedFillBytes              atomic.Int64
	unconfirmedRefusedReads          atomic.Int64
	unconfirmedHolds                 [2]atomic.Int64 // by rbHoldLifted, rbHoldRefused
	unconfirmedConfirmedAfterRefusal atomic.Int64    // sessions, once each
	playbackWebhookRules             [numWebhookRules]atomic.Int64
	unconfirmedReadBytes             atomic.Int64
	unconfirmedSessions              [2]atomic.Int64 // by rbUnconfirmed, rbConfirmedLate; resolved at close

	// Movie and episode play/resume webhooks, by whether they resolved to one file. The
	// gate arms on the matched count: refusal starts only once a webhook has matched a
	// file since the process started, so a media server that never sends one is never
	// cut off.
	playbackWebhooksMatched   atomic.Int64
	playbackWebhooksUnmatched atomic.Int64

	// linked is the set of stub names a library symlink points at (linked_stubs.go).
	linked linkedIndex
}

// authOn reports whether any credential is enforced: an api_token (controlToken) or a
// tokens file. It is the switch of the routes that fail closed without one (restart,
// confirm, api_token and plex writes), now "any auth enforced".
func (a *App) authOn() bool {
	return a.controlToken != "" || a.auth.Mode() == apiauth.ModeScoped
}

func newApp() *App {
	return &App{startup: &startupReporter{}, linked: linkedIndex{kick: make(chan struct{}, 1)}}
}

// app is set once in Run() before any goroutine starts and never reassigned there.
var app *App
