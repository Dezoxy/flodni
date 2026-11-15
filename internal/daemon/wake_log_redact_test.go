package daemon

import (
	"bytes"
	"errors"
	"flodni/internal/flog"
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// A wake failure is logged with the engine's error and the stored magnet. Both can
// hold a private tracker's announce URL, passkey included.
func TestWakeLogsHoldNoTrackerCredentials(t *testing.T) {
	const key = "abcdef0123456789abcdef0123456789"
	announce := "https://tracker.example/" + key + "/announce?passkey=" + key
	magnet := "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567&tr=" + url.QueryEscape(announce)
	engineErr := fmt.Errorf("parse link error: %w", errors.New(`Get "https://u:`+key+`@tracker.example/`+key+`/announce": refused`))

	prev := logger
	t.Cleanup(func() {
		logger = prev
		_ = flog.Configure("text", "INFO")
	})
	for _, format := range []string{"text", "json"} {
		var buf bytes.Buffer
		logger = flog.New(&buf, "GoProxy", false)
		if err := flog.Configure(format, "INFO"); err != nil {
			t.Fatal(err)
		}
		logActivationFailed("activation", "/lib/a.mkv", engineErr)
		logWakePanic(magnet, "bad "+announce, []byte("stack"))
		out := buf.String()
		for _, leak := range []string{key, "passkey", "announce"} {
			if strings.Contains(out, leak) {
				t.Errorf("%s log holds %q:\n%s", format, leak, out)
			}
		}
		if !strings.Contains(out, "tracker.example") {
			t.Errorf("%s: the host should stay in the log:\n%s", format, out)
		}
	}
}
