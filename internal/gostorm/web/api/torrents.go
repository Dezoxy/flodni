package api

import (
	"flodni/internal/gostorm/torrshash"
	"net/http"
	"strings"

	"flodni/internal/gostorm/log"
	"flodni/internal/gostorm/torr"
	"flodni/internal/gostorm/torr/state"
	"flodni/internal/gostorm/web/api/utils"
	"flodni/internal/redact"

	"github.com/anacrolix/torrent"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// Action: add, get, set, rem, list, drop
type torrReqJS struct {
	requestI
	Link     string `json:"link,omitempty"`
	Hash     string `json:"hash,omitempty"`
	Title    string `json:"title,omitempty"`
	Category string `json:"category,omitempty"`
	Poster   string `json:"poster,omitempty"`
	Data     string `json:"data,omitempty"`
	SaveToDB bool   `json:"save_to_db,omitempty"`
}

// torrents godoc
//
//	@Summary		Handle torrents informations
//	@Description	Allow to list, add, remove, get, set, drop torrents on server. The action depends of what has been asked.
//
//	@Tags			API
//
//	@Param			request	body	torrReqJS	true	"Torrent request. Available params for action: add, get, set, rem, list, drop. link required for add, hash required for get, set, rem, drop."
//
//	@Accept			json
//	@Produce		json
//	@Success		200
//	@Router			/torrents [post]
func torrents(c *gin.Context) {
	var req torrReqJS
	err := c.ShouldBindJSON(&req)
	if err != nil {
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}
	if refuseActionMismatch(c, req.Action) {
		return
	}
	c.Status(http.StatusBadRequest)
	switch req.Action {
	case "add":
		{
			addTorrent(req, c)
		}
	case "get":
		{
			getTorrent(req, c)
		}
	case "set":
		{
			setTorrent(req, c)
		}
	case "rem":
		{
			remTorrent(req, c)
		}
	case "list":
		{
			listTorrents(c)
		}
	case "active":
		{
			listActiveTorrents(c)
		}
	case "drop":
		{
			dropTorrent(req, c)
		}
	}
}

// The engine calls the add handler makes, as variables so a test can stand in for them.
var (
	addTorrentToEngine = torr.AddTorrent
	saveAdded          = torr.SaveAdded
)

func addTorrent(req torrReqJS, c *gin.Context) {
	if req.Link == "" {
		c.AbortWithError(http.StatusBadRequest, errors.New("link is empty"))
		return
	}

	log.TLogln("add torrent", redact.String(req.Link))
	req.Link = strings.ReplaceAll(req.Link, "&amp;", "&")

	var torrSpec *torrent.TorrentSpec
	var torrsHash *torrshash.TorrsHash
	var err error

	if strings.HasPrefix(req.Link, "torrs://") {
		torrSpec, torrsHash, err = utils.ParseTorrsHash(req.Link)
		if err != nil {
			log.TLogln("error parse torrshash:", redact.Err(err))
			c.AbortWithError(http.StatusBadRequest, err)
			return
		}
		if req.Title == "" {
			req.Title = torrsHash.Title()
		}
		if req.Poster == "" {
			req.Poster = torrsHash.Poster()
		}
		if req.Category == "" {
			req.Category = torrsHash.Category()
		}
	} else {
		torrSpec, err = utils.ParseLink(req.Link)
		if err != nil {
			log.TLogln("error parse link:", redact.Err(err))
			c.AbortWithError(http.StatusBadRequest, err)
			return
		}
	}

	tor, err := addTorrentToEngine(torrSpec, req.Title, req.Poster, req.Data, req.Category)
	if err != nil {
		log.TLogln("error add torrent:", redact.Err(err))
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}

	if req.SaveToDB {
		// The answer waits for the record: a caller that asked for it saved must not be
		// told 200 before it exists, nor never hear that it could not be.
		if err := saveAdded(tor, torrSpec.DisplayName); err != nil {
			log.TLogln("error add torrent:", redact.Err(err))
			status := http.StatusInternalServerError
			if errors.Is(err, torr.ErrNoMetadata) {
				status = http.StatusGatewayTimeout
			}
			c.AbortWithStatusJSON(status, gin.H{"error": err.Error()})
			return
		}
	} else {
		go func() {
			if !tor.GotInfo() {
				log.TLogln("error add torrent:", "timeout connection get torrent info")
				return
			}
			torr.SetAddedTitle(tor, torrSpec.DisplayName)
		}()
	}

	c.JSON(200, tor.Status())
}

func getTorrent(req torrReqJS, c *gin.Context) {
	if req.Hash == "" {
		c.AbortWithError(http.StatusBadRequest, errors.New("hash is empty"))
		return
	}
	// V301: Use PeekTorrent instead of GetTorrent to avoid resetting expiration timer on metadata requests
	tor := torr.PeekTorrent(req.Hash)

	if tor != nil {
		st := tor.Status()
		c.JSON(200, st)
	} else {
		c.Status(http.StatusNotFound)
	}
}

func setTorrent(req torrReqJS, c *gin.Context) {
	if req.Hash == "" {
		c.AbortWithError(http.StatusBadRequest, errors.New("hash is empty"))
		return
	}
	torr.SetTorrent(req.Hash, req.Title, req.Poster, req.Category, req.Data)
	c.Status(200)
}

func remTorrent(req torrReqJS, c *gin.Context) {
	if req.Hash == "" {
		c.AbortWithError(http.StatusBadRequest, errors.New("hash is empty"))
		return
	}
	torr.RemTorrent(req.Hash)

	c.Status(200)
}

func listTorrents(c *gin.Context) {
	list := torr.ListTorrent()
	if len(list) == 0 {
		c.JSON(200, []*state.TorrentStatus{})
		return
	}
	var stats []*state.TorrentStatus
	for _, tr := range list {
		stats = append(stats, tr.Status())
	}
	c.JSON(200, stats)
}

func listActiveTorrents(c *gin.Context) {
	list := torr.ListActiveTorrent()
	var stats []*state.TorrentStatus
	for _, tr := range list {
		if tr.IsStreaming() {
			stats = append(stats, tr.Status())
		}
	}
	if stats == nil {
		stats = []*state.TorrentStatus{}
	}
	c.JSON(200, stats)
}

func dropTorrent(req torrReqJS, c *gin.Context) {
	if req.Hash == "" {
		c.AbortWithError(http.StatusBadRequest, errors.New("hash is empty"))
		return
	}
	torr.DropTorrent(req.Hash)
	c.Status(200)
}
