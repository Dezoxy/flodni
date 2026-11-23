package api

import (
	"errors"
	"net/http"

	"flodni/internal/gostorm/log"
	set "flodni/internal/gostorm/settings"
	"flodni/internal/gostorm/torr"
	"flodni/internal/gostorm/web/api/utils"

	"github.com/gin-gonic/gin"
)

// maxTorrentUploadBody caps the body of POST /torrent/upload: a real client may send a
// .torrent of up to about 16 MiB, and 32 MiB leaves room for that plus the multipart framing.
const maxTorrentUploadBody = 32 << 20

// torrentUpload godoc
//
//	@Summary		Add .torrent file
//	@Description	Only one file support.
//
//	@Tags			API
//
//	@Param			file	formData	file	true	"Torrent file to insert"
//	@Param			save	formData	string	false	"Save to DB"
//	@Param			title	formData	string	false	"Torrent title"
//	@Param			category	formData	string	false	"Torrent category"
//	@Param			poster	formData	string	false	"Torrent poster"
//	@Param			data	formData	string	false	"Torrent data"
//
//	@Accept			multipart/form-data
//
//	@Produce		json
//	@Success		200	{object}	state.TorrentStatus	"Torrent status"
//	@Router			/torrent/upload [post]
func torrentUpload(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxTorrentUploadBody)
	form, err := c.MultipartForm()
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			c.AbortWithError(http.StatusRequestEntityTooLarge, err)
			return
		}
		c.AbortWithError(http.StatusBadRequest, err)
		return
	}
	defer func() { _ = form.RemoveAll() }() // temp upload files; nothing to recover

	save := len(form.Value["save"]) > 0
	title := ""
	if len(form.Value["title"]) > 0 {
		title = form.Value["title"][0]
	}
	category := ""
	if len(form.Value["category"]) > 0 {
		category = form.Value["category"][0]
	}
	poster := ""
	if len(form.Value["poster"]) > 0 {
		poster = form.Value["poster"][0]
	}
	data := ""
	if len(form.Value["data"]) > 0 {
		data = form.Value["data"][0]
	}
	var tor *torr.Torrent
	var addErr error
	for name, file := range form.File {
		log.TLogln("add .torrent", name)

		torrFile, err := file[0].Open()
		if err != nil {
			log.TLogln("error upload torrent:", err)
			continue
		}
		defer func() { _ = torrFile.Close() }() // read-only

		spec, err := utils.ParseFile(torrFile)
		if err != nil {
			log.TLogln("error upload torrent:", err)
			continue
		}

		tor, err = addTorrentToEngine(spec, title, poster, data, category)
		if err != nil {
			log.TLogln("error upload torrent:", err)
			addErr = err
			continue
		}

		if tor.Data != "" && set.BTsets.EnableDebug {
			log.TLogln("torrent data:", tor.Data)
		}
		if tor.Category != "" && set.BTsets.EnableDebug {
			log.TLogln("torrent category:", tor.Category)
		}

		go func() {
			if !tor.GotInfo() {
				log.TLogln("error add torrent:", "torrent connection timeout")
				return
			}

			if tor.Title == "" {
				tor.Title = tor.Name()
			}

			if save {
				torr.SaveTorrentToDB(tor)
			}
		}()

		break
	}
	if tor == nil {
		if addErr != nil {
			c.AbortWithError(http.StatusInternalServerError, addErr)
			return
		}
		c.AbortWithError(http.StatusBadRequest, errors.New("no usable .torrent file in the upload"))
		return
	}
	c.JSON(200, tor.Status())
}
