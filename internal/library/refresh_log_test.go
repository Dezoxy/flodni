package library

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"flodni/internal/flog"
)

type lockedBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type failingMediaServer struct{ err error }

func (f failingMediaServer) RefreshLibrary(context.Context, int) error { return f.err }

// A refresh that fails is logged with the error, and a media server's error can
// carry the request URL: for Plex, the X-Plex-Token.
func TestRefreshFailureLogHoldsNoToken(t *testing.T) {
	const secret = "PLEXSECRET0123456789"
	buf := &lockedBuf{}
	m := New(Config{
		Logger:       flog.New(buf, "", false),
		RefreshDelay: time.Millisecond,
		MediaServer: failingMediaServer{errors.New(
			`Get "http://plex.example:32400/library/sections/3/refresh?X-Plex-Token=` + secret + `": connection refused`)},
	})
	m.scheduleRefresh(3)

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "library refresh failed") {
		if time.Now().After(deadline) {
			t.Fatalf("no failure line logged: %q", buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if out := buf.String(); strings.Contains(out, secret) {
		t.Errorf("log holds the token: %s", out)
	}
}
