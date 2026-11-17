package utils

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"

	"flodni/internal/gostorm/fetch"
	"flodni/internal/gostorm/log"
	"flodni/internal/redact"
)

// imageCheckTimeout bounds the whole poster check: one deadline for the HEAD and the GET fallback.
var imageCheckTimeout = 10 * time.Second

func CheckImgUrl(link string) bool {
	if link == "" {
		return false
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	// One deadline spans the HEAD and the GET fallback: the check costs the limit once.
	ctx, cancel := context.WithTimeout(context.Background(), imageCheckTimeout)
	defer cancel()
	client := fetch.NewClient(0)
	resp, err := imageRequest(ctx, client, http.MethodHead, link)
	if err != nil || resp.StatusCode != http.StatusOK {
		if err == nil {
			resp.Body.Close()
		}
		// Fallback to GET if HEAD fails (some servers block HEAD)
		resp, err = imageRequest(ctx, client, http.MethodGet, link)
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

func imageRequest(ctx context.Context, client *http.Client, method, link string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, link, nil)
	if err != nil {
		return nil, err
	}
	return client.Do(req)
}
