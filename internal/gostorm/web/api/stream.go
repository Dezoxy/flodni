package api

import (
	"flodni/internal/gostorm/log"
	"flodni/internal/gostorm/torrshash"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"flodni/internal/gostorm/torr"
	"flodni/internal/gostorm/torr/state"
	"flodni/internal/gostorm/web/api/utils"

	"github.com/anacrolix/torrent"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

// TokenEnforcedKey is the gin context key the engine's router sets when an API
// token is enforced. A request that reaches a handler then carried the token.
const TokenEnforcedKey = "api_token_enforced"

// Seams for the tests: the engine calls /stream may make.
var (
	peekTorrent = torr.PeekTorrent
	streamAdd   = torr.AddTorrent
	loadStored  = torr.LoadStored
	parseLink   = utils.ParseLink
)

// refuseGetAdd reports whether a GET or HEAD /stream on an open port (no token) must be
// refused, and answers 403. A page can send such a GET from an <img> tag with no Origin,
// so the open-port Origin gate cannot stop it. The request is allowed only when its link
// names a hash the engine already stores or holds, and it neither saves nor preloads.
// The hash comes from the link text alone: an http(s) or file: link has none until it
// is fetched, so it is refused before any fetch (SSRF, local file read).
func refuseGetAdd(c *gin.Context, link string, save, preload bool) bool {
	if c.GetBool(TokenEnforcedKey) {
		return false
	}
	reason := ""
	switch {
	case save || preload:
		reason = "save and preload by GET are disabled; use POST /torrents"
	default:
		hash, ok := utils.LinkHash(link)
		if !ok || peekTorrent(hash) == nil {
			reason = "adding by GET is disabled; use POST /torrents"
		}
	}
	if reason == "" {
		return false
	}
	c.String(http.StatusForbidden, reason)
	c.Abort()
	return true
}

func stream(c *gin.Context) {
	link := c.Query("link")
	indexStr := c.Query("index")
	_, preload := c.GetQuery("preload")
	_, stat := c.GetQuery("stat")
	_, save := c.GetQuery("save")
	_, play := c.GetQuery("play")
	title := c.Query("title")
	poster := c.Query("poster")
	category := c.Query("category")

	data := ""

	notAuth := c.GetBool("auth_required") && c.GetString(gin.AuthUserKey) == ""

	if notAuth && play {
		streamNoAuth(c)
		return
	}
	if notAuth {
		c.Header("WWW-Authenticate", "Basic realm=Authorization Required")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	if link == "" {
		c.AbortWithError(http.StatusBadRequest, errors.New("link should not be empty"))
		return
	}

	link, _ = url.QueryUnescape(link)
	title, _ = url.QueryUnescape(title)
	poster, _ = url.QueryUnescape(poster)
	category, _ = url.QueryUnescape(category)

	if refuseGetAdd(c, link, save, preload) {
		return
	}

	var spec *torrent.TorrentSpec
	var torrsHash *torrshash.TorrsHash
	var err error

	if strings.HasPrefix(link, "torrs://") || (len(link) > 45 && torrshash.IsBase62(link)) {
		spec, torrsHash, err = utils.ParseTorrsHash(link)
		if err != nil {
			log.TLogln("error parse torrshash:", err)
			c.AbortWithError(http.StatusBadRequest, err)
			return
		}
		if title == "" {
			title = torrsHash.Title()
		}
		if poster == "" {
			poster = torrsHash.Poster()
		}
		if category == "" {
			category = torrsHash.Category()
		}
	} else {
		spec, err = parseLink(link)
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
	}

	// V301: Use PeekTorrent for metadata checks to avoid keeping torrents in RAM during probes
	tor := peekTorrent(spec.InfoHash.HexString())
	if tor != nil {
		title = tor.Title
		poster = tor.Poster
		data = tor.Data
		category = tor.Category
	}
	// The explicit add: this link starts the torrent, a stored record or not.
	// A stored torrent wakes from its record only, with or without a token: AddTorrent
	// would merge the link's &tr= trackers into it (lifecycle.go addUnderLock), which
	// is never legitimate on a stored torrent, a private one above all. Only a hash the
	// engine does not hold is added from the link, and only with a token (refuseGetAdd).
	if tor == nil || tor.Stat == state.TorrentInDB {
		switch {
		case tor != nil:
			tor, err = loadStored(spec.InfoHash.HexString())
			if errors.Is(err, torr.ErrNotStored) {
				// No record, or one without metainfo: never added from the link
				// (lifecycle.go LoadStored refuses it for the same reason).
				c.AbortWithError(http.StatusNotFound, err)
				return
			}
		case !c.GetBool(TokenEnforcedKey):
			// Held when the gate looked and gone since: never add without a token.
			c.AbortWithError(http.StatusNotFound, errors.New("torrent is not held"))
			return
		default:
			tor, err = streamAdd(spec, title, poster, data, category)
		}
		if err != nil {
			if errors.Is(err, torr.ErrEngineNotReady) {
				c.AbortWithError(http.StatusServiceUnavailable, err)
				return
			}
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
	}

	if !tor.GotInfo() {
		c.AbortWithError(http.StatusInternalServerError, errors.New("torrent connection timeout"))
		return
	}

	if tor.Title == "" {
		tor.Title = tor.Name()
	}

	// save to db
	if save {
		torr.SaveTorrentToDB(tor)
		c.Status(200) // only set status, not return
	}

	// find file
	index := -1
	if len(tor.Files()) == 1 {
		index = 1
	} else {
		ind, err := strconv.Atoi(indexStr)
		if err == nil {
			index = ind
		}
	}
	if index == -1 && play { // if file index not set and play file exec
		c.AbortWithError(http.StatusBadRequest, errors.New("\"index\" is empty or wrong"))
		return
	}
	// preload torrent
	if preload {
		torr.Preload(tor, index)
	}
	// return stat if query
	if stat {
		c.JSON(200, tor.Status())
		return
	}

	// return play if query
	if play {
		streamOrFail(c, tor, index)
		return
	}
}

func streamNoAuth(c *gin.Context) {
	link := c.Query("link")
	indexStr := c.Query("index")
	_, preload := c.GetQuery("preload")
	_, play := c.GetQuery("play")
	title := c.Query("title")
	poster := c.Query("poster")
	category := c.Query("category")

	if link == "" {
		c.AbortWithError(http.StatusBadRequest, errors.New("link should not be empty"))
		return
	}

	link, _ = url.QueryUnescape(link)
	title, _ = url.QueryUnescape(title)
	poster, _ = url.QueryUnescape(poster)
	category, _ = url.QueryUnescape(category)

	var spec *torrent.TorrentSpec
	var torrsHash *torrshash.TorrsHash
	var err error

	if strings.HasPrefix(link, "torrs://") || (len(link) > 45 && torrshash.IsBase62(link)) {
		spec, torrsHash, err = utils.ParseTorrsHash(link)
		if err != nil {
			log.TLogln("error parse torrshash:", err)
			c.AbortWithError(http.StatusBadRequest, err)
			return
		}
		if title == "" {
			title = torrsHash.Title()
		}
		if poster == "" {
			poster = torrsHash.Poster()
		}
		if category == "" {
			category = torrsHash.Category()
		}
	} else {
		// An unauthenticated caller never makes the engine fetch: an http(s) or file: link
		// has no hash before the fetch, so it is refused as unauthorized.
		if _, ok := utils.LinkHash(link); !ok {
			c.Header("WWW-Authenticate", "Basic realm=Authorization Required")
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		spec, err = utils.ParseLink(link)
		if err != nil {
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
	}

	// V301: Use PeekTorrent for metadata checks
	tor := torr.PeekTorrent(spec.InfoHash.HexString())
	if tor == nil {
		c.Header("WWW-Authenticate", "Basic realm=Authorization Required")
		c.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	if tor.Stat == state.TorrentInDB {
		// A wake of a stored torrent: from its record only, never from the link.
		tor, err = torr.LoadStored(spec.InfoHash.HexString())
		if err != nil {
			if errors.Is(err, torr.ErrNotStored) {
				c.AbortWithError(http.StatusNotFound, err)
				return
			}
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}
	}

	if !tor.GotInfo() {
		c.AbortWithError(http.StatusInternalServerError, errors.New("torrent connection timeout"))
		return
	}

	// find file
	index := -1
	if len(tor.Files()) == 1 {
		index = 1
	} else {
		ind, err := strconv.Atoi(indexStr)
		if err == nil {
			index = ind
		}
	}
	if index == -1 && play { // if file index not set and play file exec
		c.AbortWithError(http.StatusBadRequest, errors.New("\"index\" is empty or wrong"))
		return
	}
	// preload torrent
	if preload {
		torr.Preload(tor, index)
	}

	// return play if query
	if play {
		streamOrFail(c, tor, index)
		return
	}
	c.Header("WWW-Authenticate", "Basic realm=Authorization Required")
	c.AbortWithStatus(http.StatusUnauthorized)
}

// streamOrFail serves a file of tor. Stream returns early, before writing anything,
// when the torrent has no info yet, the file id is unknown or no reader can be made;
// without a status the client would get an empty 200, so answer 500 instead. Once the
// response has started, the status is already sent and only the log remains.
func streamOrFail(c *gin.Context, tor *torr.Torrent, index int) {
	if err := tor.Stream(index, c.Request, c.Writer); err != nil {
		log.TLogln("Error stream:", err)
		if !c.Writer.Written() {
			c.String(http.StatusInternalServerError, "stream: %v", err)
		}
	}
}
