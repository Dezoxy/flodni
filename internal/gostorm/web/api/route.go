package api

import (
	"bytes"
	"encoding/json"
	"net/http"

	"flodni/internal/gostorm/web/auth"

	"github.com/gin-gonic/gin"
)

// requestI is the action every body-action request type embeds (torrReqJS,
// setsReqJS, cacheReqJS). The scope gate decodes into this same struct (PeekAction),
// so it and the handler read the same key, with the same case folding and the
// same last-duplicate-wins, from the same bytes.
type requestI struct {
	Action string `json:"action,omitempty"`
}

// PeekAction reads the action the way the handlers bind it: encoding/json's
// Decoder into requestI, the first JSON value only (trailing data is ignored, as
// gin's own decode ignores it).
func PeekAction(body []byte) (string, error) {
	var req requestI
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&req); err != nil {
		return "", err
	}
	return req.Action, nil
}

// PeekedActionKey is the gin context key that holds the action the scope gate read
// from the body before authorizing the request. The handlers that bind an action
// refuse a request whose bound action differs (an engine to admin escalation if the
// gate and the handler ever read two different ones). Absent: nothing was peeked
// (open mode, or a caller who holds admin) and the check does nothing.
const PeekedActionKey = "peeked_action"

// refuseActionMismatch answers 400 when the gate peeked an action and the handler
// bound another. It reports whether it refused.
func refuseActionMismatch(c *gin.Context, bound string) bool {
	if peeked, ok := c.Get(PeekedActionKey); ok && peeked != bound {
		c.AbortWithStatus(http.StatusBadRequest)
		return true
	}
	return false
}

func SetupRoute(route gin.IRouter) {
	authorized := route.Group("/", auth.CheckAuth())

	// POST only: a GET from a web page carries no Origin header, so the open-port
	// Origin gate could not tell it from a script.
	authorized.POST("/shutdown", shutdown)
	authorized.POST("/shutdown/*reason", shutdown)

	authorized.POST("/settings", settings)

	authorized.POST("/torrents", torrents)

	authorized.POST("/torrent/upload", torrentUpload)

	authorized.POST("/cache", cache)

	route.HEAD("/stream", stream)
	route.GET("/stream", stream)

	route.HEAD("/stream/*fname", stream)
	route.GET("/stream/*fname", stream)

	route.HEAD("/play/:hash/:id", play)
	route.GET("/play/:hash/:id", play)

	authorized.GET("/download/:size", download)

	// Add storage settings endpoints
	authorized.GET("/storage/settings", GetStorageSettings)
	authorized.POST("/storage/settings", UpdateStorageSettings)
}
