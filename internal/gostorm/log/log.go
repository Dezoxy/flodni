package log

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strings"

	"flodni/internal/redact"

	"github.com/gin-gonic/gin"
)

var (
	logPath    = ""
	webLogPath = ""
)

var webLog *log.Logger

var (
	logFile    *os.File
	webLogFile *os.File
)

func TLogln(v ...interface{}) {
	log.Println(v...)
}

func WebLogln(v ...interface{}) {
	if webLog != nil {
		webLog.Println(v...)
	}
}

func WebLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		if webLog == nil {
			c.Next()
			return
		}
		body := ""
		// save body if not form or file
		if !strings.HasPrefix(c.Request.Header.Get("Content-Type"), "multipart/form-data") {
			body, _ := io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
		} else {
			body = "body hidden, too large"
		}
		c.Next()

		statusCode := c.Writer.Status()
		clientIP := c.ClientIP()
		method := c.Request.Method
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery
		if raw != "" {
			path = path + "?" + raw
		}

		logStr := fmt.Sprintf("%3d | %12s | %-7s %#v %v",
			statusCode,
			clientIP,
			method,
			path,
			string(body),
		)
		// The query can hold ?token= and a body can hold a link: nothing leaves raw.
		WebLogln(redact.String(logStr))
	}
}

// Recovery answers 500 for a panicking handler and logs the panic value, a
// redacted method and path and the stack. gin.Recovery dumps the request line
// and every header (masking only Authorization), which would print a Cookie, the
// proxy JWT header and a /stream link, so this logs no header and no query.
func Recovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				TLogln(redact.String(fmt.Sprintf("[Recovery] panic: %v | %s %q\n%s",
					r, c.Request.Method, c.Request.URL.Path, debug.Stack())))
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}
