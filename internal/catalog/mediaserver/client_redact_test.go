package mediaserver

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"flodni/internal/catalog"
)

const plexSecret = "PLEXSECRET0123456789"

// The refresh URL carries X-Plex-Token in its query, and net/http embeds the URL in
// the error it returns. The error leaves this package, so it must not carry the token.
func TestRefreshLibraryErrorHoldsNoToken(t *testing.T) {
	catalog.SetRetryDefaults(1, time.Millisecond)
	t.Cleanup(func() { catalog.SetRetryDefaults(0, 0) })

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close() // nothing listens there now

	c := New("http://"+addr, plexSecret)
	err = c.RefreshLibrary(context.Background(), 3)
	if err == nil {
		t.Fatal("refresh against an unreachable address succeeded")
	}
	if strings.Contains(err.Error(), plexSecret) || strings.Contains(err.Error(), "X-Plex-Token") {
		t.Errorf("error holds the token: %v", err)
	}
	if !strings.Contains(err.Error(), addr) {
		t.Errorf("error lost the host: %v", err)
	}
}
