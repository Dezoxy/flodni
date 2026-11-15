package torr

import (
	"io"

	alog "github.com/anacrolix/log"

	"flodni/internal/redact"
)

// engineLogger is the logger the vendored engine writes through. The engine logs
// tracker URLs, whole, at the default level (a failed tracker parse, a failed
// websocket announce) and at debug level, which EnableDebug turns on (every
// announce). A private tracker's passkey sits in those URLs, and the log is shared
// when asking for help. So the engine's own handler stays as it is, with its format
// and destination, and every record it writes is redacted on the way out. This is
// the one place the engine's lines are cleaned; the vendored files stay untouched.
func engineLogger() alog.Logger { return engineLoggerTo(alog.DefaultHandler.W) }

func engineLoggerTo(w io.Writer) alog.Logger {
	l := alog.Default
	l.Handlers = []alog.Handler{alog.StreamHandler{
		W:   redactWriter{w},
		Fmt: alog.DefaultHandler.Fmt,
	}}
	return l
}

// redactWriter redacts each write: the stream handler writes one record per call.
type redactWriter struct{ w io.Writer }

func (r redactWriter) Write(p []byte) (int, error) {
	if _, err := r.w.Write([]byte(redact.String(string(p)))); err != nil {
		return 0, err
	}
	return len(p), nil
}
