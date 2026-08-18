package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type AppClient struct {
	appID        int64
	clientID     string
	clientSecret string
	privateKey   *rsa.PrivateKey
	httpClient   *http.Client
	baseURL      string
	mu           sync.Mutex
	tokens       map[int64]installationToken
}

type installationToken struct {
	Token     string
	ExpiresAt time.Time
}

type User struct {
	ID        int64  `json:"id"`
	Login     string `json:"login"`
	Name      string `json:"name"`
	AvatarURL string `json:"avatar_url"`
}
type Installation struct {
	ID      int64 `json:"id"`
	Account struct {
		Login string `json:"login"`
	} `json:"account"`
	TargetType          string     `json:"target_type"`
	RepositorySelection string     `json:"repository_selection"`
	SuspendedAt         *time.Time `json:"suspended_at"`
}
type Repository struct {
	ID       int64  `json:"id"`
	FullName string `json:"full_name"`
}

func NewApp(appID int64, clientID, clientSecret, privateKeyPEM string) (*AppClient, error) {
	block := decodePrivateKeyBlock(privateKeyPEM)
	if block == nil {
		return nil, fmt.Errorf("decode github app private key")
	}
	var key *rsa.PrivateKey
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err == nil {
		key, _ = parsed.(*rsa.PrivateKey)
	}
	if key == nil {
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
	}
	if err != nil || key == nil {
		return nil, fmt.Errorf("parse github app private key: %w", err)
	}
	return &AppClient{appID: appID, clientID: clientID, clientSecret: clientSecret, privateKey: key, httpClient: &http.Client{Timeout: 10 * time.Second}, baseURL: "https://api.github.com", tokens: map[int64]installationToken{}}, nil
}

func decodePrivateKeyBlock(value string) *pem.Block {
	raw := strings.TrimSpace(value)
	if block, _ := pem.Decode([]byte(raw)); block != nil {
		return block
	}
	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		if block, _ := pem.Decode(decoded); block != nil {
			return block
		}
	}
	for _, keyType := range []string{"RSA PRIVATE KEY", "PRIVATE KEY"} {
		header := "-----BEGIN " + keyType + "-----"
		footer := "-----END " + keyType + "-----"
		start := strings.Index(raw, header)
		end := strings.Index(raw, footer)
		if start < 0 || end <= start {
			continue
		}
		payload := strings.Join(strings.Fields(raw[start+len(header):end]), "")
		decoded, err := base64.StdEncoding.DecodeString(payload)
		if err == nil {
			return &pem.Block{Type: keyType, Bytes: decoded}
		}
	}
	return nil
}

func (c *AppClient) OAuthURL(redirectURI, state, challenge string) string {
	v := url.Values{"client_id": {c.clientID}, "redirect_uri": {redirectURI}, "state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	return "https://github.com/login/oauth/authorize?" + v.Encode()
}

func (c *AppClient) ExchangeCode(ctx context.Context, code, redirectURI, verifier string) (string, error) {
	payload, _ := json.Marshal(map[string]string{"client_id": c.clientID, "client_secret": c.clientSecret, "code": code, "redirect_uri": redirectURI, "code_verifier": verifier})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", bytes.NewReader(payload))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	res, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("github oauth: %s", res.Status)
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}
	if parsed.AccessToken == "" {
		return "", fmt.Errorf("github oauth: %s", parsed.Error)
	}
	return parsed.AccessToken, nil
}

func (c *AppClient) CurrentUser(ctx context.Context, token string) (User, error) {
	var user User
	err := c.userRequest(ctx, token, "/user", &user)
	return user, err
}
func (c *AppClient) UserInstallations(ctx context.Context, token string) ([]Installation, error) {
	var out struct {
		Installations []Installation `json:"installations"`
	}
	err := c.userRequest(ctx, token, "/user/installations?per_page=100", &out)
	return out.Installations, err
}

func (c *AppClient) userRequest(ctx context.Context, token, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	c.apiHeaders(req, token)
	res, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("github api: %s: %s", res.Status, raw)
	}
	return json.Unmarshal(raw, out)
}

func (c *AppClient) Installation(ctx context.Context, id int64) (Installation, error) {
	var out Installation
	token, err := c.jwt()
	if err != nil {
		return out, err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/app/installations/%d", c.baseURL, id), nil)
	c.apiHeaders(req, token)
	res, err := c.httpClient.Do(req)
	if err != nil {
		return out, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return out, fmt.Errorf("github installation: %s: %s", res.Status, raw)
	}
	err = json.Unmarshal(raw, &out)
	return out, err
}

func (c *AppClient) Repositories(ctx context.Context, installationID int64) ([]Repository, error) {
	token, err := c.installationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	var out struct {
		Repositories []Repository `json:"repositories"`
	}
	err = c.userRequest(ctx, token, "/installation/repositories?per_page=100", &out)
	return out.Repositories, err
}

func (c *AppClient) Open(ctx context.Context, installationID int64, repo, title, body string) (int, string, error) {
	token, err := c.installationToken(ctx, installationID)
	if err != nil {
		return 0, "", err
	}
	legacy := New(token)
	legacy.baseURL = c.baseURL
	legacy.httpClient = c.httpClient
	return legacy.Open(ctx, installationID, repo, title, body)
}
func (c *AppClient) FindByMarker(ctx context.Context, installationID int64, repo, marker string, since time.Time) (int, string, bool, error) {
	token, err := c.installationToken(ctx, installationID)
	if err != nil {
		return 0, "", false, err
	}
	legacy := New(token)
	legacy.baseURL = c.baseURL
	legacy.httpClient = c.httpClient
	return legacy.FindByMarker(ctx, installationID, repo, marker, since)
}
func (c *AppClient) Reopen(ctx context.Context, installationID int64, repo string, number int) error {
	token, err := c.installationToken(ctx, installationID)
	if err != nil {
		return err
	}
	legacy := New(token)
	legacy.baseURL = c.baseURL
	legacy.httpClient = c.httpClient
	return legacy.Reopen(ctx, installationID, repo, number)
}

func (c *AppClient) installationToken(ctx context.Context, id int64) (string, error) {
	c.mu.Lock()
	cached := c.tokens[id]
	c.mu.Unlock()
	if cached.Token != "" && time.Until(cached.ExpiresAt) > 5*time.Minute {
		return cached.Token, nil
	}
	jwt, err := c.jwt()
	if err != nil {
		return "", err
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/app/installations/%d/access_tokens", c.baseURL, id), bytes.NewReader([]byte(`{}`)))
	c.apiHeaders(req, jwt)
	res, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return "", fmt.Errorf("github installation token: %s: %s", res.Status, raw)
	}
	var parsed struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.tokens[id] = installationToken{parsed.Token, parsed.ExpiresAt}
	c.mu.Unlock()
	return parsed.Token, nil
}

func (c *AppClient) jwt() (string, error) {
	now := time.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payloadRaw, _ := json.Marshal(map[string]any{"iat": now.Add(-60 * time.Second).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": strconv.FormatInt(c.appID, 10)})
	payload := base64.RawURLEncoding.EncodeToString(payloadRaw)
	unsigned := header + "." + payload
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func (c *AppClient) apiHeaders(req *http.Request, token string) {
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
}

func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
func RandomOAuthValue() (string, error) {
	raw := make([]byte, 32)
	_, err := rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw), err
}
func ConstantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var result byte
	for i := range a {
		result |= a[i] ^ b[i]
	}
	return result == 0
}
func ParsePrivateKeyEnv(value string) string { return strings.ReplaceAll(value, `\n`, "\n") }
