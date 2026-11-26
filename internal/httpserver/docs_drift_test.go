package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raisestracker/raises/internal/inbox"
)

func TestSkillDocParity(t *testing.T) {
	webSkill, err := os.ReadFile(filepath.Join("..", "..", "web", "skill.md"))
	if err != nil {
		t.Fatalf("read web/skill.md: %v", err)
	}
	repoSkill, err := os.ReadFile(filepath.Join("..", "..", "skill", "SKILL.md"))
	if err != nil {
		t.Fatalf("read skill/SKILL.md: %v", err)
	}
	if !bytes.Equal(webSkill, repoSkill) {
		t.Fatalf("skill/SKILL.md (%d bytes) drifted from web/skill.md (%d bytes)", len(repoSkill), len(webSkill))
	}
	wantCompact := "?app=<project-slug>&status=open&page=1&per_page=25&compact=1"
	if !strings.Contains(string(webSkill), wantCompact) {
		t.Fatalf("web/skill.md missing compact Step 1 query %q", wantCompact)
	}
	llms, err := os.ReadFile(filepath.Join("..", "..", "web", "llms.txt"))
	if err != nil {
		t.Fatalf("read web/llms.txt: %v", err)
	}
	if !strings.Contains(string(llms), wantCompact) {
		t.Fatalf("web/llms.txt missing compact list query %q", wantCompact)
	}
}

func TestCompactListProjectionAndGroupURL(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "compact.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	owner, _ := store.UpsertGitHubUser(ctx, 9901, "compact-owner", "", "")
	agentTok, _, _ := store.CreateAgentKey(ctx, owner.ID, "agent")
	proj, _ := store.CreateProject(ctx, owner.ID, "CompactApp", "compact-app")
	ingestTok, _, _ := store.CreateProjectToken(ctx, owner.ID, proj.ID)

	res, err := store.Ingest(ctx, ingestTok, inbox.Notice{
		Env:       "production",
		Class:     "RuntimeError",
		Message:   "compact test",
		Backtrace: []string{"app/models/compact.rb:12:in `run'"},
		Context:   map[string]any{"blob": strings.Repeat("x", 2048)},
	})
	if err != nil {
		t.Fatal(err)
	}

	srv := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{
		BaseURL: "https://raises.example.com",
	})
	handler := srv.Handler()

	// 1. Default unacked=1 preserves flat array with sample and includes additive url.
	reqDefault := httptest.NewRequest(http.MethodGet, "/v1/errors?unacked=1", nil)
	reqDefault.Header.Set("Authorization", "Bearer "+agentTok)
	recDefault := httptest.NewRecorder()
	handler.ServeHTTP(recDefault, reqDefault)
	if recDefault.Code != http.StatusOK {
		t.Fatalf("default status = %d", recDefault.Code)
	}
	var rawDefault []map[string]any
	if err := json.Unmarshal(recDefault.Body.Bytes(), &rawDefault); err != nil || len(rawDefault) != 1 {
		t.Fatalf("default decode err=%v len=%d", err, len(rawDefault))
	}
	if _, ok := rawDefault[0]["sample"].(map[string]any); !ok {
		t.Fatalf("default response missing sample: %s", recDefault.Body.String())
	}
	wantURL := "https://raises.example.com/errors/1"
	if got := rawDefault[0]["url"]; got != wantURL {
		t.Fatalf("url = %v, want %q", got, wantURL)
	}

	// 2. ?unacked=1&compact=1 omits sample completely and stays flat array.
	reqCompact := httptest.NewRequest(http.MethodGet, "/v1/errors?unacked=1&compact=1", nil)
	reqCompact.Header.Set("Authorization", "Bearer "+agentTok)
	recCompact := httptest.NewRecorder()
	handler.ServeHTTP(recCompact, reqCompact)
	if recCompact.Code != http.StatusOK {
		t.Fatalf("compact status = %d", recCompact.Code)
	}
	var rawCompact []map[string]any
	if err := json.Unmarshal(recCompact.Body.Bytes(), &rawCompact); err != nil || len(rawCompact) != 1 {
		t.Fatalf("compact decode err=%v len=%d", err, len(rawCompact))
	}
	if _, hasSample := rawCompact[0]["sample"]; hasSample {
		t.Fatalf("compact response must omit sample: %s", recCompact.Body.String())
	}
	if len(recCompact.Body.Bytes()) >= len(recDefault.Body.Bytes())/2 {
		t.Fatalf("compact payload not significantly smaller: compact=%d default=%d", len(recCompact.Body.Bytes()), len(recDefault.Body.Bytes()))
	}

	// 3. Paginated ?status=open&page=1&per_page=25&compact=1 omits sample and includes url.
	reqPage := httptest.NewRequest(http.MethodGet, "/v1/errors?status=open&page=1&per_page=25&compact=1", nil)
	reqPage.Header.Set("Authorization", "Bearer "+agentTok)
	recPage := httptest.NewRecorder()
	handler.ServeHTTP(recPage, reqPage)
	if recPage.Code != http.StatusOK {
		t.Fatalf("page compact status = %d", recPage.Code)
	}
	var rawPage struct {
		Errors []map[string]any `json:"errors"`
	}
	if err := json.Unmarshal(recPage.Body.Bytes(), &rawPage); err != nil || len(rawPage.Errors) != 1 {
		t.Fatalf("page compact decode err=%v len=%d", err, len(rawPage.Errors))
	}
	if _, hasSample := rawPage.Errors[0]["sample"]; hasSample {
		t.Fatalf("page compact response must omit sample: %s", recPage.Body.String())
	}
	if rawPage.Errors[0]["url"] != wantURL {
		t.Fatalf("page compact url = %v, want %q", rawPage.Errors[0]["url"], wantURL)
	}

	// 4. Empty BaseURL omits url field via omitempty.
	srvNoBase := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{})
	reqSingle := httptest.NewRequest(http.MethodGet, "/v1/errors/1", nil)
	reqSingle.Header.Set("Authorization", "Bearer "+agentTok)
	recSingle := httptest.NewRecorder()
	srvNoBase.Handler().ServeHTTP(recSingle, reqSingle)
	var single map[string]any
	if err := json.Unmarshal(recSingle.Body.Bytes(), &single); err != nil {
		t.Fatal(err)
	}
	if _, hasURL := single["url"]; hasURL {
		t.Fatalf("expected url omitted when BaseURL is empty, got %v (group id %d)", single["url"], res.Group.ID)
	}
}
