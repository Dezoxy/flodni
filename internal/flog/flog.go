// Package flog is the daemon's logging layer on log/slog.
//
// The default text output keeps the shape the daemon has always printed:
// an optional "[Name] " prefix, an optional "2006/01/02 15:04:05 " time, the
// message, and no level token. The message text of every line is therefore
// what operators already grep. Structured fields ride along as slog
// attributes: the JSON format (log_format "json") prints them as keys, and
// the text format appends only the ones the message does not already show.
//
// Level and format are process-wide, set once from the configuration with
// Configure, so every logger (including package-level ones created before the
// configuration was read) follows them.
package flog

import (
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// Level is the process-wide threshold. The default is Info.
var Level slog.LevelVar

var jsonMode atomic.Bool

// Configure applies the log_format and log_level configuration values. An
// unknown format is returned as an error and leaves the format at text; an
// unknown level means Info, as log_level has always defaulted.
func Configure(format, level string) error {
	SetLevel(level)
	return SetFormat(format)
}

// SetFormat selects "text" (also ""), or "json".
func SetFormat(format string) error {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "", "text":
		jsonMode.Store(false)
	case "json":
		jsonMode.Store(true)
	default:
		jsonMode.Store(false)
		return fmt.Errorf("unknown log_format %q (want \"text\" or \"json\")", format)
	}
	return nil
}

// Format reports the active format, "text" or "json".
func Format() string {
	if jsonMode.Load() {
		return "json"
	}
	return "text"
}

// SetLevel maps a log_level value to the threshold. Only DEBUG changes
// anything: it shows Debug lines. Every other value, WARN and ERROR included,
// behaves like INFO, so a log_level never hides a contract line.
func SetLevel(name string) {
	Level.Set(ParseLevel(name))
}

// ParseLevel returns the handler's minimum level: Debug for DEBUG, otherwise
// Info (WARN and ERROR are reserved and behave like INFO).
func ParseLevel(name string) slog.Level {
	if strings.EqualFold(strings.TrimSpace(name), "DEBUG") {
		return slog.LevelDebug
	}
	return slog.LevelInfo
}

var (
	levelHead = regexp.MustCompile(`^\s*\[DEBUG\]`)
	errorWord = regexp.MustCompile(`\bERROR\b|\[PANIC\]`)
	warnWord  = regexp.MustCompile(`\bWARNING\b|\bWarning\b`)
)

// classifyWindow is how much of a message's start Classify looks at, so a
// word inside a title further along never changes the level.
const classifyWindow = 60

// Classify maps the prefix convention the daemon has always used to a level:
// a leading [DEBUG] is Debug, ERROR or [PANIC] near the start is Error,
// WARNING or Warning near the start is Warn, everything else is Info. It is
// applied to a format string or to a rendered message.
func Classify(msg string) slog.Level {
	head := msg
	if len(head) > classifyWindow {
		head = head[:classifyWindow]
	}
	switch {
	case levelHead.MatchString(head):
		return slog.LevelDebug
	case errorWord.MatchString(head):
		return slog.LevelError
	case warnWord.MatchString(head):
		return slog.LevelWarn
	}
	return slog.LevelInfo
}

// Printf logs a message whose format is not a literal at the call site, so
// its level is taken from the rendered text.
func Printf(l *slog.Logger, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	l.Log(context.Background(), Classify(msg), msg)
}

// Logf returns Printf bound to l, for callbacks typed func(string, ...any).
func Logf(l *slog.Logger) func(format string, args ...any) {
	return func(format string, args ...any) { Printf(l, format, args...) }
}

// New returns a logger that writes to w. name, when set, prints as a
// "[name] " prefix in text and as a "logger" key in JSON; withTime prints the
// "2006/01/02 15:04:05 " stamp the standard logger used (JSON always carries
// "time").
func New(w io.Writer, name string, withTime bool) *slog.Logger {
	h := &handler{name: name, withTime: withTime, w: w, mu: &sync.Mutex{}}
	h.json = slog.NewJSONHandler(&lockedWriter{w: w, mu: h.mu}, &slog.HandlerOptions{Level: slog.Level(-100)})
	if name != "" {
		h.json = h.json.WithAttrs([]slog.Attr{slog.String("logger", name)})
	}
	return slog.New(h)
}

// NewText returns a logger with no prefix and no time, for tests that read the
// message back.
func NewText(w io.Writer) *slog.Logger { return New(w, "", false) }

// Std is the logger for code with no logger of its own. It writes where the
// standard library logger writes, so log.SetOutput redirects it, and prints
// the time like the standard logger did.
var Std = New(stdWriter{}, "", true)

type stdWriter struct{}

func (stdWriter) Write(p []byte) (int, error) { return log.Writer().Write(p) }

type lockedWriter struct {
	w  io.Writer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

type handler struct {
	name     string
	withTime bool
	w        io.Writer
	mu       *sync.Mutex
	json     slog.Handler
	attrs    []slog.Attr
	group    string
}

func (h *handler) Enabled(_ context.Context, l slog.Level) bool { return l >= Level.Level() }

func (h *handler) WithAttrs(as []slog.Attr) slog.Handler {
	c := *h
	c.json = h.json.WithAttrs(as)
	c.attrs = append(append([]slog.Attr(nil), h.attrs...), as...)
	return &c
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	c := *h
	c.json = h.json.WithGroup(name)
	c.group = h.group + name + "."
	return &c
}

func (h *handler) Handle(ctx context.Context, r slog.Record) error {
	if jsonMode.Load() {
		return h.json.Handle(ctx, r)
	}
	var b strings.Builder
	if h.name != "" {
		b.WriteString("[" + h.name + "] ")
	}
	if h.withTime {
		t := r.Time
		if t.IsZero() {
			t = time.Now()
		}
		b.WriteString(t.Format("2006/01/02 15:04:05 "))
	}
	b.WriteString(r.Message)
	add := func(a slog.Attr) {
		v := valueText(a.Value.Resolve())
		if a.Key == "" || (v != "" && strings.Contains(r.Message, v)) {
			return
		}
		b.WriteString(" " + h.group + a.Key + "=")
		if v == "" || strings.ContainsFunc(v, func(r rune) bool { return r == ' ' || r == '"' || r == '=' || !unicode.IsPrint(r) }) {
			v = strconv.Quote(v)
		}
		b.WriteString(v)
	}
	for _, a := range h.attrs {
		add(a)
	}
	r.Attrs(func(a slog.Attr) bool { add(a); return true })
	if !strings.HasSuffix(b.String(), "\n") {
		b.WriteByte('\n')
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := io.WriteString(h.w, b.String())
	return err
}

// valueText renders a value the way the %v and %s verbs would, so a []byte
// shows as its text and the message-already-shows-it check compares like
// with like.
func valueText(v slog.Value) string {
	if b, ok := v.Any().([]byte); ok && v.Kind() == slog.KindAny {
		return string(b)
	}
	return v.String()
}
