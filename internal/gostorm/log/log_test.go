package log

import (
	"bytes"
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
