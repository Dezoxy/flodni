package library

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"regexp"
	"strings"
	"time"

	"flodni/internal/catalog"
)

// GoStormClient handles HTTP operations with the GoStorm engine.
type GoStormClient struct {
	baseURL    string
	token      string // bearer token for the engine's server; empty sends none
	httpClient *http.Client
}

// NewGoStormClient creates a client for GoStorm API operations. A non-empty
// token is sent as a bearer token on every request.
func NewGoStormClient(baseURL, token string) *GoStormClient {
	return &GoStormClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        10,
				MaxIdleConnsPerHost: 5,
				IdleConnTimeout:     30 * time.Second,
			},
		},
	}
}

// NewGoStormClientTransport is the client of Flodni's own calls to its engine in scoped
// mode: rt hands each request to the engine in-process, so no URL, socket or credential
// is involved (the base is a placeholder the transport ignores).
func NewGoStormClientTransport(rt http.RoundTripper) *GoStormClient {
	return &GoStormClient{
		baseURL:    "http://engine.internal",
		httpClient: &http.Client{Timeout: 30 * time.Second, Transport: rt},
	}
}

// AddTorrent adds a magnet URL to GoStorm via POST /torrents {"action":"add"}
// and returns its hash. It hides whether the engine
// echoed the hash back; callers that must not mistake an unacknowledged add for a
// statement about the swarm should use AddTorrentConfirmed.
func (c *GoStormClient) AddTorrent(ctx context.Context, magnet, title string) (string, error) {
	hash, _, err := c.AddTorrentConfirmed(ctx, magnet, title)
	return hash, err
}

// AddTorrentConfirmed reports confirmed=false when the engine answered 2xx without
// echoing a hash: the torrent may never have been taken, so its later silence says
// nothing about the release.
func (c *GoStormClient) AddTorrentConfirmed(ctx context.Context, magnet, title string) (string, bool, error) {
	m := regexp.MustCompile(`xt=urn:btih:([a-fA-F0-9]{32,40})`)
	match := m.FindStringSubmatch(magnet)
	if len(match) < 2 {
		return "", false, fmt.Errorf("cannot extract hash from magnet")
	}
	hash := strings.ToLower(match[1])

	body := map[string]interface{}{
		"action":     "add",
		"link":       magnet,
		"title":      title,
		"save_to_db": true,
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", false, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/torrents", bytes.NewReader(data))
	if err != nil {
		return "", false, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", false, err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", false, fmt.Errorf("gostorm error %d: %s", resp.StatusCode, string(respBody[:min(len(respBody), 120)]))
	}

	// Response contains the torrent object with hash
	var result struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.Hash != "" {
		return strings.ToLower(result.Hash), true, nil
	}

	// 2xx with nothing usable in the body: the hash is the one we sent, not one the
	// engine acknowledged.
	return hash, false, nil
}

// UploadTorrent hands GoStorm a release's .torrent (POST /torrent/upload, saved to its
// DB): the metadata is known at once and the file's own trackers are kept for every
// later wake. Returns the torrent's hash.
func (c *GoStormClient) UploadTorrent(ctx context.Context, data []byte, title string) (string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("save", "true")
	if title != "" {
		_ = w.WriteField("title", title)
	}
	part, err := w.CreateFormFile("file", "release.torrent")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/torrent/upload", &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	c.authorize(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 120))
		return "", fmt.Errorf("gostorm upload %d: %s", resp.StatusCode, msg)
	}
	var result struct {
		Hash string `json:"hash"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil || result.Hash == "" {
		return "", fmt.Errorf("gostorm upload: no hash in the answer")
	}
	return strings.ToLower(result.Hash), nil
}

// GetTorrentInfo polls GoStorm until file_stats appear.
func (c *GoStormClient) GetTorrentInfo(ctx context.Context, hash string, maxWait int) (*TorrentStats, error) {
	sleepSeq := []int{1, 2, 3, 3, 3, 5}
	deadline := time.Now().Add(time.Duration(maxWait) * time.Second)
	attempt := 0

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		info, err := c.getTorrent(ctx, hash)
		if err == nil && len(info.FileStats) > 0 {
			return info, nil
		}

		sleep := 3
		if attempt < len(sleepSeq) {
			sleep = sleepSeq[attempt]
		}
		time.Sleep(time.Duration(sleep) * time.Second)
		attempt++
	}

	return nil, fmt.Errorf("metadata timeout for %s… (waited %ds)", hash[:8], maxWait)
}

// RemoveTorrent removes a torrent from GoStorm.
func (c *GoStormClient) RemoveTorrent(ctx context.Context, hash string) error {
	body := map[string]string{"action": "rem", "hash": hash}
	return c.postTorrents(ctx, body)
}

// ReadFileHead requests the first n bytes of a file from /stream, the URL a stub
// carries, and reads them all. The client's 30 s timeout would cut the read short of
// the caller's deadline, so the request uses a copy without it; ctx bounds the call.
func (c *GoStormClient) ReadFileHead(ctx context.Context, hash string, fileID int, n int64) error {
	if n <= 0 {
		return nil
	}
	url := fmt.Sprintf("%s/stream?link=%s&index=%d&play", c.baseURL, hash, fileID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	c.authorize(req)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", n-1))
	hc := *c.httpClient
	hc.Timeout = 0
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("gostorm /stream: status %d", resp.StatusCode)
	}
	got, err := io.Copy(io.Discard, io.LimitReader(resp.Body, n))
	if err != nil {
		return fmt.Errorf("read after %d of %d bytes: %w", got, n, err)
	}
	if got < n {
		return fmt.Errorf("stream ended after %d of %d bytes", got, n)
	}
	return nil
}

// ListTorrents returns all active torrents.
func (c *GoStormClient) ListTorrents(ctx context.Context) ([]TorrentStats, error) {
	body := map[string]string{"action": "list"}
	data, err := c.doTorrents(ctx, body, true)
	if err != nil {
		return nil, err
	}

	var torrents []TorrentStats
	if err := json.Unmarshal(data, &torrents); err != nil {
		return nil, err
	}
	return torrents, nil
}

func (c *GoStormClient) getTorrent(ctx context.Context, hash string) (*TorrentStats, error) {
	body := map[string]string{"action": "get", "hash": hash}
	data, err := c.doTorrents(ctx, body, false)
	if err != nil {
		return nil, err
	}

	var info TorrentStats
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// doTorrents posts a JSON action body to GoStorm's /torrents endpoint. retry controls
// whether transient failures are retried (catalog.Do) - true for ListTorrents (a standalone
// read with no other retry mechanism around it), false for getTorrent (already wrapped in
// GetTorrentInfo's own outer polling/backoff loop - stacking catalog.Do's retry inside it
// would waste that loop's time budget for no benefit) and for mutations (add/rem), so a
// mutation that already succeeded server-side is never silently duplicated by a retry.
func (c *GoStormClient) doTorrents(ctx context.Context, body map[string]string, retry bool) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/torrents", bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	c.authorize(req)

	var resp *http.Response
	if retry {
		resp, err = catalog.Do(ctx, c.httpClient, req)
	} else {
		resp, err = c.httpClient.Do(req)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("gostorm /torrents: status %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

func (c *GoStormClient) postTorrents(ctx context.Context, body map[string]string) error {
	_, err := c.doTorrents(ctx, body, false)
	return err
}

func (c *GoStormClient) authorize(req *http.Request) {
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
}
