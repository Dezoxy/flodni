package daemon

import (
	"bytes"
	"strings"
	"testing"

	"flodni/internal/flog"
)

// A sender that fills a template (Tautulli's JSON webhook in Plex's shape) quotes every
// value. A quoted or empty year must not drop the webhook: the play still matches by
// the file it names.
func TestWebhookWithQuotedValuesFromATemplateSender(t *testing.T) {
	for _, year := range []string{`"2026"`, `""`, `2026`} {
		t.Run(year, func(t *testing.T) {
			_, key := playedEnv(t)
			postWebhook(t, `{"event":"media.play","Metadata":{"title":"Pilot","grandparentTitle":"Some Show",`+
				`"year":`+year+`,"librarySectionType":"show","parentIndex":"1","index":"2",`+
				`"Guid":[{"id":"imdb://tt0000001"}],`+
				`"Media":[{"Part":[{"file":"/data/Some.Show.S01E02.1080p.mkv"}]}]}}`)
			if !fileStates.IsPlayed(key) {
				t.Fatalf("year %s: the play was not matched", year)
			}
		})
	}
}

// A payload that does not parse used to return without a word: a misconfigured sender
// then looks like no sender. It is logged.
func TestWebhookThatDoesNotParseIsLogged(t *testing.T) {
	newTestApp(t)
	var buf bytes.Buffer
	prev := logger
	logger = flog.NewText(&buf)
	t.Cleanup(func() { logger = prev })
	postWebhook(t, `{"event":"media.play","Metadata":{"librarySectionType":["not","a","string"]}}`)
	if !strings.Contains(buf.String(), "[PLEX] Webhook payload does not parse") {
		t.Fatalf("no log line for an unparsable payload: %q", buf.String())
	}
}
