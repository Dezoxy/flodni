package daemon

import (
	"bytes"
	"flodni/internal/apiauth"
	"flodni/internal/flog"
	"flodni/internal/redact"
	stdlog "log"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A valid ?token= on the two webhook routes passes the gate and the real
// handler; neither the control log nor the std log may hold it.
func TestWebhookTokenNeverReachesLogs(t *testing.T) {
	var ctl, std bytes.Buffer
	prev, prevOut := logger, stdlog.Writer()
	logger = flog.NewText(&ctl)
	stdlog.SetOutput(&std)
	t.Cleanup(func() { logger = prev; stdlog.SetOutput(prevOut) })

	const secret = "SECRETWEBHOOKTOKEN"
	mux := http.NewServeMux()
	mux.HandleFunc("/plex/webhook", handlePlexWebhook)
	mux.HandleFunc("/webhook", handlePlexWebhook)
	h := apiauth.Middleware(secret, logger, mux)
	for _, path := range []string{"/plex/webhook", "/webhook"} {
		form := url.Values{"payload": {`{"event":"media.pause"}`}}
		req := httptest.NewRequest(http.MethodPost, path+"?token="+secret, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
			t.Fatalf("%s: gate refused a valid token: %d", path, rec.Code)
		}
	}
	// A wrong token on the same routes is refused and logged without the query.
	req := httptest.NewRequest(http.MethodPost, "/webhook?token=WRONG"+secret, nil)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if ctl.Len() == 0 {
		t.Error("control log is empty")
	}
	for name, out := range map[string]string{"control": ctl.String(), "std": std.String()} {
		if strings.Contains(out, secret) {
			t.Errorf("%s log holds the token: %q", name, out)
		}
	}
}

// A panic in a control handler is printed by net/http (the mux has no
// recovery); it must hold no token or query.
func TestControlPanicLogsNoToken(t *testing.T) {
	std := &syncBuf{}
	prevOut := stdlog.Writer()
	stdlog.SetOutput(std)
	t.Cleanup(func() { stdlog.SetOutput(prevOut) })

	mux := http.NewServeMux()
	mux.HandleFunc("/api/boom", func(w http.ResponseWriter, r *http.Request) { panic("boom " + r.URL.String()) })
	srv := httptest.NewUnstartedServer(apiauth.Middleware("SECRETTOK", nil, mux))
	srv.Config.ErrorLog = redact.ErrorLog()
	srv.Start()
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/boom?token=SECRETQUERY", nil)
	req.Header.Set("Authorization", "Bearer SECRETTOK")
	req.Header.Set("Cookie", "s=SECRETCOOKIE")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
	}
	srv.Close()
	out := std.String()
	if !strings.Contains(out, "panic") {
		t.Errorf("panic not logged: %q", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Errorf("std log holds a secret: %q", out)
	}
}
