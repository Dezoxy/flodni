package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// The scope gate and the handlers must read one action from one body. The corpus
// is what a mismatch would hide in: other case, duplicates, escapes, trailing data.
var actionBodies = []string{
	`{"action":"list"}`,
	`{"action":"list","ACTION":"rem"}`,
	`{"ACTION":"rem","action":"list"}`,
	`{"Action":"drop"}`,
	`{"action":"list","action":"rem"}`,
	`{"action":"rem","action":"list"}`,
	`{"action":"rem"}`,
	`{"action":"rem"}`,
	`{"action":"list"} {"action":"rem"}`,
	`{"action":"list"}garbage`,
	`{"action":"add","link":"magnet:?xt=urn:btih:x","save_to_db":true}`,
	`{"hash":"x"}`,
	`{"action":null}`,
	`{"action":""}`,
	`{"ſction":"rem"}`,
	`{"aſction":"rem"}`,
	`{"action":"list","extra":{"action":"rem"}}`,
	`{"extra":{"action":"rem"},"action":"list"}`,
	`{"action":"get","hash":"` + strings.Repeat("a", 40) + `"}`,
}

func TestPeekActionReadsWhatTheHandlersBind(t *testing.T) {
	for _, body := range actionBodies {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("POST", "/torrents", strings.NewReader(body))
		var req torrReqJS
		bindErr := c.ShouldBindJSON(&req)
		peeked, peekErr := PeekAction([]byte(body))
		if (bindErr == nil) != (peekErr == nil) {
			t.Errorf("%q: bind err %v, peek err %v", body, bindErr, peekErr)
			continue
		}
		if bindErr == nil && peeked != req.Action {
			t.Errorf("%q: peek read %q, the handler binds %q", body, peeked, req.Action)
		}
		var sets setsReqJS
		c.Request = httptest.NewRequest("POST", "/settings", strings.NewReader(body))
		if c.ShouldBindJSON(&sets) == nil && peekErr == nil && sets.Action != peeked {
			t.Errorf("%q: settings binds %q, peek read %q", body, sets.Action, peeked)
		}
	}
}

// A handler refuses when the action the gate read differs from the one it bound,
// before it does anything.
func TestHandlersRefuseAnActionThePeekDidNotSee(t *testing.T) {
	for _, tc := range []struct {
		name   string
		handle gin.HandlerFunc
	}{{"torrents", torrents}, {"settings", settings}} {
		for _, body := range []string{`{"action":"rem","hash":"x"}`, `{"action":"set"}`, `{"action":"def"}`, `{"action":"get"}`, `{"action":"zzz"}`} {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest("POST", "/"+tc.name, strings.NewReader(body))
			c.Set(PeekedActionKey, "list") // what the gate authorized
			tc.handle(c)
			if rec.Code != http.StatusBadRequest || !c.IsAborted() {
				t.Errorf("%s %s: status %d aborted=%v, want a refused 400", tc.name, body, rec.Code, c.IsAborted())
			}
		}
	}
}

// Nothing peeked (open mode, or a caller holding admin): the check does nothing.
func TestHandlersWithNoPeekedActionAreUnchanged(t *testing.T) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest("POST", "/settings", strings.NewReader(`{"action":"nonsense"}`))
	settings(c)
	if rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Fatalf("status %d", rec.Code)
	}
	if c.IsAborted() && len(c.Errors) == 0 {
		t.Error("refused with no error: the mismatch check ran with nothing peeked")
	}
}
