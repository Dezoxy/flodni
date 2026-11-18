package api

import (
	stderrors "errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"

	sets "flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr"
)

// Action: get, set, def
type setsReqJS struct {
	requestI
	Sets *sets.BTSets `json:"sets,omitempty"`
	// Force lets a change that restarts the engine go ahead while a session is playing.
	Force bool `json:"force,omitempty"`
}

// settings godoc
//
//	@Summary		Get / Set server settings
//	@Description	Allow to get or set server settings.
//
//	@Tags			API
//
//	@Param			request	body	setsReqJS	true	"Settings request. Available params for action: get, set, def"
//
//	@Accept			json
//	@Produce		json
//	@Success		200	{object}	sets.BTSets	"Settings JSON or nothing. Depends on what action has been asked."
//	@Router			/settings [post]
func settings(c *gin.Context) {
	var req setsReqJS
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}

	if refuseActionMismatch(c, req.Action) {
		return
	}
	if req.Action == "get" {
		c.JSON(200, sets.BTsets)
		return
	} else if req.Action == "set" {
		if req.Sets == nil {
			c.AbortWithError(http.StatusBadRequest, errors.New("sets is empty"))
			return
		}
		if refuseRestart(c, torr.SetSettings(req.Sets, req.Force)) {
			return
		}
		c.Status(200)
		return
	} else if req.Action == "def" {
		if refuseRestart(c, torr.SetDefSettings(req.Force)) {
			return
		}
		c.Status(200)
		return
	}
	c.AbortWithError(http.StatusBadRequest, errors.New("action is empty"))
}

// refuseRestart answers 409 when the engine refused a restart under playback, and reports
// whether it did. The body names the CLIENT fields and the playing session count.
func refuseRestart(c *gin.Context, err error) bool {
	var refused *torr.RestartRefused
	if !stderrors.As(err, &refused) {
		return false
	}
	c.JSON(http.StatusConflict, gin.H{
		"error":    refused.Error(),
		"fields":   refused.Fields,
		"sessions": refused.Sessions,
	})
	return true
}
