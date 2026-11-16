package log

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestWebLoggerHidesQueryToken(t *testing.T) {
	var buf bytes.Buffer
	prev := webLog
	webLog = log.New(&buf, "", 0)
	t.Cleanup(func() { webLog = prev })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(WebLogger())
	r.GET("/stream", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/stream?link=abc&token=SECRET&index=1", nil)
	r.ServeHTTP(httptest.NewRecorder(), req)

	out := buf.String()
	if !strings.Contains(out, "/stream") {
		t.Fatalf("request not logged: %q", out)
	}
	if strings.Contains(out, "SECRET") {
		t.Errorf("token survived in the web log: %q", out)
	}
	if !strings.Contains(out, "token=<redacted>") {
		t.Errorf("want token=<redacted> in %q", out)
	}
}

func TestRecoveryRedactsPanicValue(t *testing.T) {
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Recovery())
	r.GET("/p", func(c *gin.Context) { panic(fmt.Errorf("bad %s", c.Request.URL)) })
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/p?token=SECRET", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
	if !strings.Contains(buf.String(), "bad") || strings.Contains(buf.String(), "SECRET") {
		t.Errorf("panic value not logged redacted: %q", buf.String())
	}
}

func TestRecoveryRepanicsAbortHandler(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(Recovery())
	r.GET("/p", func(c *gin.Context) { panic(http.ErrAbortHandler) })
	defer func() {
		if got := recover(); got != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler", got)
		}
	}()
	r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/p", nil))
}
