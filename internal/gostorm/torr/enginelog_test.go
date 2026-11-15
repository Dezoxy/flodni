package torr

import (
	"bytes"
	"context"
	stdlog "log"
	"strings"
	"testing"

	alog "github.com/anacrolix/log"

	"flodni/internal/gostorm/settings"
)

// The engine formats these lines itself (tracker_scraper.go, torrent.go,
// wstracker.go in the vendored tree); the test writes the same shapes through the
// logger Flodni hands it.
func TestEngineLoggerRedactsTrackerURLs(t *testing.T) {
	const key = "abcdef0123456789abcdef0123456789"
	var buf bytes.Buffer
	l := engineLoggerTo(&buf).FilterLevel(alog.Debug)

	announce := "https://tracker.example/" + key + "/announce?passkey=" + key
	l.WithDefaultLevel(alog.Debug).Printf("announcing to %q: %#v", announce, struct{ Left int64 }{1})
	l.WithDefaultLevel(alog.Debug).Printf("announce to %q returned %#v: %v", announce, struct{}{}, `Get "`+announce+`": dial tcp: refused`)
	l.Levelf(alog.Warning, "error parsing tracker url: %v", `parse "https://u:`+key+`@tracker.example/x": invalid port`)
	l.WithText(func(m alog.Msg) string {
		return `tracker client for "wss://tracker.example/` + key + `": ` + m.Text()
	}).Levelf(alog.Warning, "error running tracker client for %q: %v", "wss://tracker.example/"+key, "closed")

	out := buf.String()
	if strings.Contains(out, key) || strings.Contains(out, "passkey") {
		t.Errorf("engine log still holds a credential:\n%s", out)
	}
	if n := strings.Count(out, "https://tracker.example/<redacted>"); n < 2 {
		t.Errorf("host should survive, got %d in:\n%s", n, out)
	}
	if !strings.Contains(out, "announcing to") || !strings.Contains(out, "refused") {
		t.Errorf("the lines should stay readable:\n%s", out)
	}
}

// configure is what the engine client is built from: its Logger must be Flodni's, or
// the engine falls back to the library default and writes tracker URLs unredacted.
func TestConfigureRoutesEngineLogs(t *testing.T) {
	prev, prevArgs, prevIP := settings.BTsets, settings.Args, settings.PubIPv4
	t.Cleanup(func() { settings.BTsets, settings.Args, settings.PubIPv4 = prev, prevArgs, prevIP })
	settings.PubIPv4 = "93.184.216.34" // configure would look the address up on the network otherwise
	settings.Args = &settings.ExecArgs{}
	settings.BTsets = &settings.BTSets{CacheSize: 1 << 20, ConnectionsLimit: 5, EnableDebug: true}

	bt := NewBTS()
	bt.configure(context.Background())
	if bt.config.Logger.IsZero() {
		t.Fatal("the engine client config has no Logger, so it logs through the library default")
	}
}

// "Client config:" prints every setting; BlockListURL is a URL a user may have put a
// key in.
func TestClientConfigLineHoldsNoBlocklistKey(t *testing.T) {
	prev, prevArgs, prevIP := settings.BTsets, settings.Args, settings.PubIPv4
	prevW, prevF := stdlog.Writer(), stdlog.Flags()
	var buf bytes.Buffer
	stdlog.SetOutput(&buf)
	t.Cleanup(func() {
		settings.BTsets, settings.Args, settings.PubIPv4 = prev, prevArgs, prevIP
		stdlog.SetOutput(prevW)
		stdlog.SetFlags(prevF)
	})
	settings.Args = &settings.ExecArgs{}
	settings.PubIPv4 = "93.184.216.34"
	settings.BTsets = &settings.BTSets{CacheSize: 1 << 20, ConnectionsLimit: 5,
		BlockListURL: "https://lists.example/u/BLOCKKEY0123/list.gz?apikey=BLOCKKEY0123"}

	NewBTS().configure(context.Background())
	out := buf.String()
	if !strings.Contains(out, "Client config:") {
		t.Fatalf("no config line: %q", out)
	}
	if strings.Contains(out, "BLOCKKEY0123") {
		t.Errorf("config line holds the blocklist key:\n%s", out)
	}
	if !strings.Contains(out, "lists.example") {
		t.Errorf("config line lost the blocklist host:\n%s", out)
	}
}
