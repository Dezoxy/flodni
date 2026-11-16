package server

import (
	"testing"

	"flodni/internal/apiauth"
	"flodni/internal/gostorm/web"
)

// The engine starts only after the daemon handed it its credential decision: a start
// that skipped SetAuth would otherwise run on the engine's own zero state.
func TestStartNeedsSetAuth(t *testing.T) {
	prevA, prevC, prevS, prevT := web.Auth, web.AuthClosed, web.AuthConfigured, web.APIToken
	t.Cleanup(func() { web.Auth, web.AuthClosed, web.AuthConfigured, web.APIToken = prevA, prevC, prevS, prevT })
	web.Auth, web.AuthClosed, web.AuthConfigured, web.APIToken = nil, false, false, ""

	if authReady() == nil {
		t.Error("a zero state is ready")
	}
	SetAPIToken("legacy", nil)
	if authReady() == nil {
		t.Error("SetAPIToken alone is ready: it does not say what the daemon decided")
	}
	open, _ := apiauth.Load(apiauth.LoadOptions{})
	SetAuth(open, nil)
	if err := authReady(); err != nil {
		t.Errorf("after SetAuth: %v", err)
	}
	SetAuth(nil, nil) // a nil store is closed, loudly, not unconfigured
	if err := authReady(); err != nil {
		t.Errorf("after SetAuth(nil): %v", err)
	}
}
