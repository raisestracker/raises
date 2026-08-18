package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewAppAcceptsBase64EncodedPrivateKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	encoded := base64.StdEncoding.EncodeToString(privatePEM)

	if _, err := NewApp(123, "client", "secret", encoded); err != nil {
		t.Fatal(err)
	}
}

func TestFindByMarkerPaginatesRepositoryIssues(t *testing.T) {
	marker := "<!-- raises-delivery:ij_test -->"
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.Header.Get("Authorization") != "Bearer token" || r.URL.Query().Get("state") != "all" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("request headers=%v query=%v", r.Header, r.URL.Query())
		}
		if r.URL.Query().Get("page") == "1" {
			issues := make([]map[string]any, 100)
			for i := range issues {
				issues[i] = map[string]any{"number": i + 1, "body": "another issue"}
			}
			_ = json.NewEncoder(w).Encode(issues)
			return
		}
		_, _ = io.WriteString(w, `[{"number":101,"html_url":"https://github.test/acme/widget/issues/101","body":"`+marker+`"}]`)
	}))
	defer server.Close()
	client := New("token")
	client.baseURL = server.URL
	client.httpClient = server.Client()
	number, issueURL, found, err := client.FindByMarker(context.Background(), 0, "acme/widget", marker, time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC))
	if err != nil || !found || number != 101 || issueURL == "" || pages != 2 {
		t.Fatalf("number=%d url=%q found=%v pages=%d err=%v", number, issueURL, found, pages, err)
	}
}

func TestNewAppAcceptsWhitespaceFlattenedPrivateKey(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	flattened := strings.Join(strings.Fields(string(privatePEM)), " ")

	if _, err := NewApp(123, "client", "secret", flattened); err != nil {
		t.Fatal(err)
	}
}

func TestAppClientFilesIssueWithInstallationToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	var sawInstallation, sawIssue bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/42/access_tokens":
			sawInstallation = true
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer eyJ") {
				t.Errorf("missing app jwt")
			}
			_, _ = io.WriteString(w, `{"token":"installation-token","expires_at":"2099-01-01T00:00:00Z"}`)
		case "/repos/acme/widget/issues":
			sawIssue = true
			if r.Header.Get("Authorization") != "Bearer installation-token" {
				t.Errorf("authorization=%q", r.Header.Get("Authorization"))
			}
			var payload map[string]any
			_ = json.NewDecoder(r.Body).Decode(&payload)
			if payload["title"] != "Boom" {
				t.Errorf("payload=%v", payload)
			}
			_, _ = io.WriteString(w, `{"number":7,"html_url":"https://github.test/acme/widget/issues/7"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := NewApp(123, "client", "secret", string(privatePEM))
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	client.httpClient = server.Client()
	number, url, err := client.Open(context.Background(), 42, "acme/widget", "Boom", "details")
	if err != nil {
		t.Fatal(err)
	}
	if number != 7 || url == "" || !sawInstallation || !sawIssue {
		t.Fatalf("number=%d url=%q installation=%v issue=%v", number, url, sawInstallation, sawIssue)
	}
}

func TestPKCEChallengeIsStable(t *testing.T) {
	if got := PKCEChallenge("verifier"); got != "iMnq5o6zALKXGivsnlom_0F5_WYda32GHkxlV7mq7hQ" {
		t.Fatalf("challenge=%q", got)
	}
}
