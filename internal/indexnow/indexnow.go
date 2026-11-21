package indexnow

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/raisestracker/raises/web"
)

const (
	// Key is public by design: search engines fetch it from KeyLocation to
	// confirm ownership of Host. The matching file lives at web/<Key>.txt.
	Key                 = "3aff9d89fda08c367b45d279593f7ce1"
	Host                = "raises.dev"
	KeyPath             = "/" + Key + ".txt"
	KeyLocation         = "https://" + Host + KeyPath
	DefaultEndpoint     = "https://api.indexnow.org/indexnow"
	BatchSize           = 10000
	DefaultInterval     = 6 * time.Hour
	DefaultInitialDelay = time.Minute
	userAgent           = "RaisesIndexNow/1.0"
)

// Enabled reports whether automatic IndexNow submission should run.
// Production (https://raises.dev) enables it by default; dev, test and CI
// never ping IndexNow unless explicitly opted in via INDEXNOW_ENABLED=1/true.
func Enabled(baseURL, envFlag string) bool {
	flag := strings.TrimSpace(strings.ToLower(envFlag))
	switch flag {
	case "1", "true":
		return true
	case "0", "false":
		return false
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(parsed.Scheme, "https") && strings.EqualFold(parsed.Hostname(), Host)
}

type Client struct {
	Endpoint    string
	Host        string
	Key         string
	KeyLocation string
	HTTPClient  *http.Client
}

func NewClient() *Client {
	return &Client{
		Endpoint:    DefaultEndpoint,
		Host:        Host,
		Key:         Key,
		KeyLocation: KeyLocation,
		HTTPClient:  &http.Client{Timeout: 15 * time.Second},
	}
}

type submitPayload struct {
	Host        string   `json:"host"`
	Key         string   `json:"key"`
	KeyLocation string   `json:"keyLocation"`
	URLList     []string `json:"urlList"`
}

// Submit sends the given URLs to IndexNow in batches, ignoring any URL not on
// this client's Host. Returns the deduplicated, sorted list of URLs sent.
func (c *Client) Submit(ctx context.Context, urls []string) ([]string, error) {
	host := c.Host
	if host == "" {
		host = Host
	}
	key := c.Key
	if key == "" {
		key = Key
	}
	keyLocation := c.KeyLocation
	if keyLocation == "" {
		keyLocation = KeyLocation
	}
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}

	seen := make(map[string]struct{}, len(urls))
	filtered := make([]string, 0, len(urls))
	for _, raw := range urls {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || !ownURL(trimmed, host) {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		filtered = append(filtered, trimmed)
	}
	if len(filtered) == 0 {
		return nil, nil
	}
	sort.Strings(filtered)

	for start := 0; start < len(filtered); start += BatchSize {
		end := min(start+BatchSize, len(filtered))
		if err := postBatch(ctx, httpClient, endpoint, submitPayload{
			Host:        host,
			Key:         key,
			KeyLocation: keyLocation,
			URLList:     filtered[start:end],
		}); err != nil {
			return nil, err
		}
	}
	return filtered, nil
}

func ownURL(raw, host string) bool {
	parsed, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "https" || parsed.Scheme == "http") && strings.EqualFold(parsed.Hostname(), host)
}

func postBatch(ctx context.Context, httpClient *http.Client, endpoint string, payload submitPayload) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal indexnow payload: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build indexnow request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("User-Agent", userAgent)

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post indexnow: %w", err)
	}
	defer resp.Body.Close()

	// 200 = accepted, 202 = accepted while the key is still being verified.
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusAccepted {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil
	}
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	return fmt.Errorf("indexnow returned %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
}

type sitemapDocument struct {
	URLs []struct {
		Location string `xml:"loc"`
		LastMod  string `xml:"lastmod"`
	} `xml:"url"`
}

// SitemapEntries reads sitemap.xml from fsys and returns each own-host URL
// mapped to its change fingerprint. Explicit <lastmod> values win; when absent,
// the fingerprint hashes the embedded asset(s) served for that path so content
// updates are still detected across deploys.
func SitemapEntries(fsys fs.FS) (map[string]string, error) {
	if fsys == nil {
		fsys = web.FS
	}
	raw, err := fs.ReadFile(fsys, "sitemap.xml")
	if err != nil {
		return nil, fmt.Errorf("read sitemap.xml: %w", err)
	}
	var doc sitemapDocument
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse sitemap.xml: %w", err)
	}
	entries := make(map[string]string, len(doc.URLs))
	for _, item := range doc.URLs {
		loc := strings.TrimSpace(item.Location)
		if !ownURL(loc, Host) {
			continue
		}
		if lastMod := strings.TrimSpace(item.LastMod); lastMod != "" {
			entries[loc] = "lastmod:" + lastMod
			continue
		}
		entries[loc] = "sha256:" + assetFingerprint(fsys, loc)
	}
	return entries, nil
}

func assetFingerprint(fsys fs.FS, rawURL string) string {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	var assets []string
	switch parsed.Path {
	case "", "/":
		assets = []string{"index.html", "index.md"}
	default:
		assets = []string{strings.TrimPrefix(parsed.Path, "/")}
	}
	h := sha256.New()
	matched := false
	for _, name := range assets {
		data, err := fs.ReadFile(fsys, name)
		if err != nil {
			continue
		}
		matched = true
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{0})
	}
	if !matched {
		_, _ = h.Write([]byte(rawURL))
	}
	return hex.EncodeToString(h.Sum(nil))
}

type StateStore interface {
	IndexNowSubmitted(ctx context.Context) (map[string]string, error)
	RecordIndexNowSubmitted(ctx context.Context, entries map[string]string) error
}

type Worker struct {
	Client       *Client
	Store        StateStore
	FS           fs.FS
	Interval     time.Duration
	InitialDelay time.Duration
	Logger       *slog.Logger
}

// SyncOnce diffs current sitemap entries against the last submitted state and
// submits only new or changed URLs.
func (w *Worker) SyncOnce(ctx context.Context) ([]string, error) {
	client := w.Client
	if client == nil {
		client = NewClient()
	}
	current, err := SitemapEntries(w.FS)
	if err != nil {
		return nil, err
	}
	var submitted map[string]string
	if w.Store != nil {
		submitted, err = w.Store.IndexNowSubmitted(ctx)
		if err != nil {
			return nil, fmt.Errorf("load indexnow state: %w", err)
		}
	}
	var changed []string
	for loc, fp := range current {
		if submitted[loc] != fp {
			changed = append(changed, loc)
		}
	}
	if len(changed) == 0 {
		return nil, nil
	}
	sort.Strings(changed)
	sent, err := client.Submit(ctx, changed)
	if err != nil {
		return nil, err
	}
	if w.Store != nil {
		if err := w.Store.RecordIndexNowSubmitted(ctx, current); err != nil {
			return sent, fmt.Errorf("record indexnow state: %w", err)
		}
	}
	if w.Logger != nil {
		w.Logger.Info("submitted IndexNow URLs", "count", len(sent), "urls", sent)
	}
	return sent, nil
}

// Run checks for new or changed sitemap URLs after InitialDelay (so a fresh
// deploy has cut over traffic before search engines fetch the key file) and
// then every Interval.
func (w *Worker) Run(ctx context.Context, onError func(error)) {
	interval := w.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	delay := w.InitialDelay
	if delay < 0 {
		delay = 0
	}
	if delay == 0 && w.InitialDelay == 0 {
		delay = DefaultInitialDelay
	}
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
	runOnce := func() {
		reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if _, err := w.SyncOnce(reqCtx); err != nil && ctx.Err() == nil && onError != nil {
			onError(err)
		}
	}
	runOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runOnce()
		}
	}
}
