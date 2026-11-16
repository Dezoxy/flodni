package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"flodni/internal/apiauth"
	"flodni/internal/gostorm/log"
	"flodni/internal/gostorm/web/api"

	"github.com/gin-gonic/gin"
)

// The second half of the engine's gate (ADR 0004, "Where each half runs"). The outer
// gate (apiauth.GoStorm) authenticates the bearer before routing and puts the
// identity in the request context. This middleware knows the route pattern
// (c.FullPath), so it makes the scope check, reads the body action on the routes
// that have one, and sets TokenEnforcedKey for a caller holding admin.

var (
	// Auth is the credential store of this process, set before Start. Nil: the
	// engine's own APIToken decides (open, or legacy).
	Auth *apiauth.Tokens

	// AuthObserver is told of every refusal of the router's half (and of the outer
	// gate's). Nil counts nothing.
	AuthObserver apiauth.Observer

	// onScopePass is a test seam: the middleware calls it for a request it lets
	// through, in place of the handlers. Nil in production.
	onScopePass func(*gin.Context)

	// peekReadDeadline bounds how long the body action peek may read. A server-wide
	// ReadTimeout would cut /stream and /torrent/upload, which are long.
	peekReadDeadline = 10 * time.Second
)

// peekCap is the most the action peek reads: read, not trusted from Content-Length.
const peekCap = 64 << 10

// authTokens is what the router and the outer gate enforce.
func authTokens() *apiauth.Tokens {
	if Auth != nil {
		return Auth
	}
	t, _ := apiauth.Load(apiauth.LoadOptions{APIToken: APIToken}) // api_token alone cannot fail
	return t
}

// scopeMiddleware is the router's half. The mode is fixed here, at construction: a
// request that reaches it with no identity on a route that is not open is refused,
// never read as "open mode". Open mode builds no middleware at all.
func scopeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		pattern := c.FullPath() // "" for a path no route matches: unmatched, so admin
		label := pattern
		if !apiauth.HasPattern(apiauth.Engine, pattern) {
			label = apiauth.RouteUnmatched
		}
		method := c.Request.Method
		need := apiauth.ScopeFor(apiauth.Engine, method, pattern, "")
		if need == apiauth.ScopeOpen {
			passed(c)
			return
		}
		id, ok := apiauth.IdentityFrom(c.Request.Context())
		if !ok {
			refuseRoute(c, http.StatusUnauthorized, label, need, apiauth.ReasonMissing, "unauthorized")
			return
		}
		// Nothing reads a body until the caller holds a scope that could apply, and
		// a caller holding admin needs nothing from it: every action is allowed.
		if apiauth.HasActions(apiauth.Engine, method, pattern) && !id.Scopes.Has(apiauth.ScopeAdmin) &&
			apiauth.CouldApply(id.Scopes, apiauth.Engine, method, pattern) {
			action, status, why := peekAction(c)
			if why != "" {
				refuseRoute(c, status, label, need, apiauth.ReasonBody, why)
				return
			}
			need = apiauth.ScopeFor(apiauth.Engine, method, pattern, action)
			c.Set(api.PeekedActionKey, action)
		}
		if !id.Scopes.Has(need) {
			refuseRoute(c, http.StatusForbidden, label, need, apiauth.ReasonScope, "forbidden")
			return
		}
		// Per request, and only for admin: an engine token gets the open-port rule on
		// /stream (no add by link), which is what makes "engine cannot add" true.
		if id.Scopes.Has(apiauth.ScopeAdmin) {
			c.Set(api.TokenEnforcedKey, true)
		}
		if passed(c) {
			return
		}
		c.Next()
		if !id.Internal && mutating(method, need) && c.Writer.Status() < http.StatusBadRequest {
			log.TLogln(fmt.Sprintf("[Auth] token %q %s %q", id.Name, method, label))
		}
	}
}

// mutating: a write the log names the token for, as on the control port.
func mutating(method string, need apiauth.Scope) bool {
	if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
		return false
	}
	return need == apiauth.ScopeAdmin || need == apiauth.ScopeUpload
}

// passed runs the test seam; it reports whether the request was taken over.
func passed(c *gin.Context) bool {
	if onScopePass == nil {
		return false
	}
	onScopePass(c)
	return true
}

func refuseRoute(c *gin.Context, status int, label string, need apiauth.Scope, reason, body string) {
	if AuthObserver != nil {
		AuthObserver(label, need, reason)
	}
	if Logger != nil {
		Logger.Info(fmt.Sprintf("[GoStorm] %d %s %q from %s: %s", status, c.Request.Method, c.Request.URL.Path, c.Request.RemoteAddr, reason),
			"name", "GoStorm", "method", c.Request.Method, "path", c.Request.URL.Path, "remote_addr", c.Request.RemoteAddr, "reason", reason)
	}
	if status == http.StatusUnauthorized {
		c.Header("WWW-Authenticate", "Bearer")
	}
	c.String(status, body)
	c.Abort()
}

// peekAction reads the action out of the body, with the same decoder and struct the
// handler binds (api.PeekAction), through a 64 KiB LimitedReader, and puts the bytes
// back for the handler. On a refusal it returns the status and a reason text.
func peekAction(c *gin.Context) (action string, status int, why string) {
	for _, v := range c.Request.Header.Values("Content-Encoding") {
		for _, enc := range strings.Split(v, ",") {
			if enc = strings.ToLower(strings.TrimSpace(enc)); enc != "" && enc != "identity" {
				return "", http.StatusBadRequest, "content encoding is not supported"
			}
		}
	}
	// The server sets no ReadTimeout (streams and uploads are long), so the peek sets
	// its own deadline for its read. A writer that cannot (a test recorder, the
	// in-process one) has no deadline to set. On a refusal the deadline stays: net/http
	// tries to discard the rest of an unread body before it answers, and a cleared
	// deadline would let a slow sender hold that for as long as it likes. On success
	// the body is at EOF and the deadline is cleared, so the handler is not cut.
	rc := http.NewResponseController(c.Writer)
	armed := false
	if err := rc.SetReadDeadline(time.Now().Add(peekReadDeadline)); err == nil {
		armed = true
	} else if !errors.Is(err, http.ErrNotSupported) {
		return "", http.StatusBadRequest, "body could not be read"
	}
	data, err := io.ReadAll(&io.LimitedReader{R: c.Request.Body, N: peekCap + 1})
	if err != nil {
		return "", http.StatusBadRequest, "body could not be read"
	}
	if len(data) > peekCap {
		return "", http.StatusRequestEntityTooLarge, "body too large"
	}
	action, err = api.PeekAction(data)
	if err != nil {
		return "", http.StatusBadRequest, "body is not a JSON request"
	}
	if armed {
		_ = rc.SetReadDeadline(time.Time{})
	}
	_ = c.Request.Body.Close()
	c.Request.Body = io.NopCloser(bytes.NewReader(data))
	c.Request.ContentLength = int64(len(data))
	return action, 0, ""
}

// ---- Flodni's own calls to its engine ----

// handlerBox lets the in-process transport hold the router once Start has built it.
type handlerBox struct{ h http.Handler }

var inproc atomic.Pointer[handlerBox]

// InProcess returns the transport Flodni's own engine calls use in scoped mode (the
// library client, the panel's engine-settings proxy). It hands each request to the
// engine's router in the same process, below the gate: no credential, no socket,
// no redirect off the machine, no gostorm_url. It is a value, not a listener, so a
// network client cannot reach it. Three rules: it overwrites any identity already in
// the request's context (the settings proxy passes the inbound r.Context(), which
// carries the caller's) with the in-process admin one; it ignores the URL host; and
// it never falls back to a real transport: an unset handler is an error, not a dial.
func InProcess() http.RoundTripper { return inprocTransport{} }

type inprocTransport struct{}

var errInProcessNotReady = errors.New("the engine is not up: no in-process handler")

func (inprocTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	box := inproc.Load()
	if box == nil || box.h == nil {
		if req.Body != nil {
			_ = req.Body.Close() // RoundTrip always closes the request body
		}
		return nil, errInProcessNotReady
	}
	ctx, cancel := context.WithCancel(req.Context())
	r := req.Clone(apiauth.WithIdentity(ctx, apiauth.InternalIdentity()))
	if r.Body == nil {
		r.Body = http.NoBody
	}
	r.RequestURI = r.URL.RequestURI()
	r.RemoteAddr = "127.0.0.1:0"
	r.Host = "engine.internal"

	rw := newPipeWriter()
	go func() {
		defer func() { _ = r.Body.Close() }()
		defer func() {
			if p := recover(); p != nil {
				rw.finish(fmt.Errorf("in-process handler panicked: %v", p))
			}
		}()
		box.h.ServeHTTP(rw, r)
		rw.finish(nil)
	}()
	select {
	case <-rw.headerSent:
	case <-req.Context().Done():
		cancel()
		return nil, req.Context().Err()
	}
	resp := rw.response(req)
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

// cancelBody ends the handler's request when the caller closes the response.
type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error {
	err := b.ReadCloser.Close() // a handler blocked writing gets ErrClosedPipe
	b.cancel()
	return err
}

// pipeWriter is an http.ResponseWriter whose body is read by the transport's caller
// while the handler writes it.
type pipeWriter struct {
	hdr        http.Header
	snapshot   http.Header // the headers as they were when the status was written
	status     int
	headerSent chan struct{}
	sent       bool
	pr         *io.PipeReader
	pw         *io.PipeWriter
}

func newPipeWriter() *pipeWriter {
	pr, pw := io.Pipe()
	return &pipeWriter{hdr: http.Header{}, headerSent: make(chan struct{}), pr: pr, pw: pw}
}

func (w *pipeWriter) Header() http.Header { return w.hdr }

func (w *pipeWriter) WriteHeader(code int) {
	if w.sent {
		return
	}
	w.sent, w.status, w.snapshot = true, code, w.hdr.Clone()
	close(w.headerSent)
}

func (w *pipeWriter) Write(b []byte) (int, error) {
	if !w.sent {
		w.WriteHeader(http.StatusOK)
	}
	return w.pw.Write(b)
}

func (w *pipeWriter) Flush() {}

// finish ends the body; with an error the reader sees it. A handler that wrote
// nothing still gets its status out.
func (w *pipeWriter) finish(err error) {
	if !w.sent {
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
		} else {
			w.WriteHeader(http.StatusOK)
		}
	}
	if err != nil {
		_ = w.pw.CloseWithError(err)
		return
	}
	_ = w.pw.Close()
}

func (w *pipeWriter) response(req *http.Request) *http.Response {
	return &http.Response{
		Status:        fmt.Sprintf("%d %s", w.status, http.StatusText(w.status)),
		StatusCode:    w.status,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        w.snapshot,
		Body:          w.pr,
		ContentLength: -1,
		Request:       req,
	}
}
