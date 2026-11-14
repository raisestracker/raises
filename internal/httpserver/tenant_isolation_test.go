package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/raisestracker/raises/internal/inbox"
)

// Two unrelated owners share the project slug "web". Ingestion and reads
// through the real HTTP handlers must stay per project identity: the second
// tenant creates its own group and can never read the first tenant's group
// or notices.
func TestCrossTenantIngestIsolationHTTP(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	userA, err := store.UpsertGitHubUser(ctx, 201, "owner-a", "", "")
	if err != nil {
		t.Fatal(err)
	}
	userB, err := store.UpsertGitHubUser(ctx, 202, "owner-b", "", "")
	if err != nil {
		t.Fatal(err)
	}
	agentA, _, err := store.CreateAgentKey(ctx, userA.ID, "agent a")
	if err != nil {
		t.Fatal(err)
	}
	agentB, _, err := store.CreateAgentKey(ctx, userB.ID, "agent b")
	if err != nil {
		t.Fatal(err)
	}
	projectA, err := store.CreateProject(ctx, userA.ID, "Web A", "web")
	if err != nil {
		t.Fatal(err)
	}
	projectB, err := store.CreateProject(ctx, userB.ID, "Web B", "web")
	if err != nil {
		t.Fatal(err)
	}
	tokenA, _, err := store.CreateProjectToken(ctx, userA.ID, projectA.ID)
	if err != nil {
		t.Fatal(err)
	}
	tokenB, _, err := store.CreateProjectToken(ctx, userB.ID, projectB.ID)
	if err != nil {
		t.Fatal(err)
	}

	handler := New(store, "", slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20).Handler()
	notice := `{"env":"production","revision":"aaa","error":{"class":"RuntimeError","message":"boom","backtrace":["app/jobs/work_job.rb:4:in ` + "`perform`" + `"]},"context":{"api_key":"DUMMY-VALUE"}}`

	postNotice := func(token string) (int, inbox.IngestResult) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/notices", bytes.NewBufferString(notice))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		var result inbox.IngestResult
		if rec.Code == http.StatusCreated || rec.Code == http.StatusOK {
			if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
				t.Fatalf("decode ingest result: %v body=%s", err, rec.Body.String())
			}
		}
		return rec.Code, result
	}

	code, first := postNotice(tokenA)
	if code != http.StatusCreated || !first.Created {
		t.Fatalf("tenant A ingest: code=%d created=%v", code, first.Created)
	}
	code, second := postNotice(tokenB)
	if code != http.StatusCreated || !second.Created || second.Group.ID == first.Group.ID {
		t.Fatalf("tenant B must create its own group: code=%d result=%+v", code, second.Group)
	}
	if second.Group.ProjectID != projectB.ID || first.Group.ProjectID != projectA.ID {
		t.Fatalf("A=%q (want %q) B=%q (want %q)", first.Group.ProjectID, projectA.ID, second.Group.ProjectID, projectB.ID)
	}

	list := func(agent string) []inbox.Group {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/v1/errors?app=web&unacked=1", nil)
		req.Header.Set("Authorization", "Bearer "+agent)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("list code=%d body=%s", rec.Code, rec.Body.String())
		}
		var groups []inbox.Group
		if err := json.Unmarshal(rec.Body.Bytes(), &groups); err != nil {
			t.Fatal(err)
		}
		return groups
	}
	groupsA, groupsB := list(agentA), list(agentB)
	if len(groupsA) != 1 || groupsA[0].ID != first.Group.ID {
		t.Fatalf("tenant A sees %v", groupsA)
	}
	if len(groupsB) != 1 || groupsB[0].ID != second.Group.ID {
		t.Fatalf("tenant B sees %v", groupsB)
	}

	// Tenant A must not read tenant B's notices.
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/errors/%d/notices", second.Group.ID), nil)
	req.Header.Set("Authorization", "Bearer "+agentA)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant notices code=%d body=%s", rec.Code, rec.Body.String())
	}
}
