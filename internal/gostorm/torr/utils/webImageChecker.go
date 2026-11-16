package utils

import (
	"net/http"
	"net/url"
	"strings"
	"time"

	"flodni/internal/gostorm/fetch"
	"flodni/internal/gostorm/log"
	"flodni/internal/redact"
)

// imageCheckTimeout bounds the whole poster check, HEAD and GET together.
var imageCheckTimeout = 10 * time.Second

func CheckImgUrl(link string) bool {
	if link == "" {
		return false
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	client := fetch.NewClient(imageCheckTimeout)
	// Try HEAD first to save bandwidth
	resp, err := client.Head(link)
	if err != nil || resp.StatusCode != http.StatusOK {
		if err == nil {
			resp.Body.Close()
		}
		// Fallback to GET if HEAD fails (some servers block HEAD)
		resp, err = client.Get(link)
		if err != nil {
			log.TLogln("Error check image:", redact.Err(err))
			return false
		}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false
	}

	ctype := resp.Header.Get("Content-Type")
	return strings.HasPrefix(ctype, "image/")
}
