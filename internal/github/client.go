package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Client struct {
	token      string
	httpClient *http.Client
	baseURL    string
}

func (c *Client) FindByMarker(ctx context.Context, _ int64, repo, marker string, since time.Time) (int, string, bool, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return 0, "", false, err
	}
	for page := 1; ; page++ {
		query := url.Values{
			"state":     {"all"},
			"sort":      {"created"},
			"direction": {"desc"},
			"since":     {since.UTC().Format(time.RFC3339)},
			"per_page":  {"100"},
			"page":      {fmt.Sprintf("%d", page)},
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/repos/"+owner+"/"+name+"/issues?"+query.Encode(), nil)
		if err != nil {
			return 0, "", false, err
		}
		c.headers(req)
		res, err := c.httpClient.Do(req)
		if err != nil {
			return 0, "", false, err
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 16<<20))
		_ = res.Body.Close()
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return 0, "", false, fmt.Errorf("github list issues: %s: %s", res.Status, raw)
		}
		var issues []struct {
			Number  int    `json:"number"`
			HTMLURL string `json:"html_url"`
			Body    string `json:"body"`
		}
		if err := json.Unmarshal(raw, &issues); err != nil {
			return 0, "", false, err
		}
		for _, issue := range issues {
			if strings.Contains(issue.Body, marker) {
				return issue.Number, issue.HTMLURL, true, nil
			}
		}
		if len(issues) < 100 {
			return 0, "", false, nil
		}
	}
}

func New(token string) *Client {
	return &Client{
		token:      token,
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    "https://api.github.com",
	}
}

func (c *Client) Open(ctx context.Context, _ int64, repo, title, body string) (int, string, error) {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return 0, "", err
	}
	payload, _ := json.Marshal(map[string]any{
		"title": title,
		"body":  body,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/repos/"+owner+"/"+name+"/issues", bytes.NewReader(payload))
	if err != nil {
		return 0, "", err
	}
	c.headers(req)
	res, err := c.httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return 0, "", fmt.Errorf("github create issue: %s: %s", res.Status, raw)
	}
	var parsed struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return 0, "", err
	}
	return parsed.Number, parsed.HTMLURL, nil
}

func (c *Client) Reopen(ctx context.Context, _ int64, repo string, number int) error {
	owner, name, err := splitRepo(repo)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(map[string]any{"state": "open"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, fmt.Sprintf("%s/repos/%s/%s/issues/%d", c.baseURL, owner, name, number), bytes.NewReader(payload))
	if err != nil {
		return err
	}
	c.headers(req)
	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("github reopen issue: %s: %s", res.Status, raw)
	}
	return nil
}

func (c *Client) headers(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

func splitRepo(repo string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(repo), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid github repo %q", repo)
	}
	return parts[0], parts[1], nil
}
