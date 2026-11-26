package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

type countingFiler struct {
	calls atomic.Int64
}

func (f *countingFiler) FindByMarker(context.Context, int64, string, string, time.Time) (int, string, bool, error) {
	f.calls.Add(1)
	return 0, "", false, nil
}
func (f *countingFiler) Open(context.Context, int64, string, string, string) (int, string, error) {
	f.calls.Add(1)
	return 1, "https://github.com/acme/app/issues/1", nil
}
func (f *countingFiler) Reopen(context.Context, int64, string, int) error {
	f.calls.Add(1)
	return nil
}

func TestResolveAndReopenStoreOnlyLifecycleAndIDOR(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	filer := &countingFiler{}
	store, err := inbox.Open(filepath.Join(t.TempDir(), "resolve-reopen.db"), func() time.Time { return now }, filer)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ownerA, _ := store.UpsertGitHubUser(ctx, 8201, "resolve-a", "", "")
	agentA, _, _ := store.CreateAgentKey(ctx, ownerA.ID, "agent-a")
	projA, _ := store.CreateProject(ctx, ownerA.ID, "App A", "app-a")
	tokA, _, _ := store.CreateProjectToken(ctx, ownerA.ID, projA.ID)

	ownerB, _ := store.UpsertGitHubUser(ctx, 8202, "resolve-b", "", "")
	agentB, _, _ := store.CreateAgentKey(ctx, ownerB.ID, "agent-b")

	res, err := store.Ingest(ctx, tokA, inbox.Notice{
		Env:       "production",
		Class:     "RuntimeError",
		Message:   "boom",
		Backtrace: []string{"app/models/job.rb:12"},
	})
	if err != nil {
		t.Fatal(err)
	}
	gid := res.Group.ID

	srv := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{
		BaseURL: "https://raises.example.com",
	})
	handler := srv.Handler()

	post := func(tok, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 1. Cross-tenant IDOR must return 404 and leave Tenant A's group open.
	if rec := post(agentB, fmt.Sprintf("/v1/errors/%d/resolve", gid)); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant resolve status = %d, want 404", rec.Code)
	}
	if rec := post(agentB, fmt.Sprintf("/v1/errors/%d/reopen", gid)); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant reopen status = %d, want 404", rec.Code)
	}
	gCheck, _ := store.GetForUser(ctx, ownerA.ID, gid)
	if gCheck.Status != inbox.StatusOpen {
		t.Fatalf("cross-tenant probe mutated status to %q", gCheck.Status)
	}

	// 2. Explicit resolve from open sets both resolved_at and acked_at -> fixed, 0 GitHub calls.
	recRes := post(agentA, fmt.Sprintf("/v1/errors/%d/resolve", gid))
	if recRes.Code != http.StatusOK {
		t.Fatalf("resolve status = %d body=%s", recRes.Code, recRes.Body.String())
	}
	var resolvedGroup inbox.Group
	if err := json.Unmarshal(recRes.Body.Bytes(), &resolvedGroup); err != nil {
		t.Fatal(err)
	}
	if resolvedGroup.Status != inbox.StatusFixed || resolvedGroup.ResolvedAt == nil || resolvedGroup.AckedAt == nil {
		t.Fatalf("expected fixed with resolved_at+acked_at, got %+v", resolvedGroup)
	}
	if filer.calls.Load() != 0 {
		t.Fatalf("resolve must make 0 GitHub API calls, got %d", filer.calls.Load())
	}

	// 3. Explicit reopen clears both resolved_at and acked_at -> open, 0 GitHub calls.
	recReopen := post(agentA, fmt.Sprintf("/v1/errors/%d/reopen", gid))
	if recReopen.Code != http.StatusOK {
		t.Fatalf("reopen status = %d body=%s", recReopen.Code, recReopen.Body.String())
	}
	var reopenedGroup inbox.Group
	if err := json.Unmarshal(recReopen.Body.Bytes(), &reopenedGroup); err != nil {
		t.Fatal(err)
	}
	if reopenedGroup.Status != inbox.StatusOpen || reopenedGroup.ResolvedAt != nil || reopenedGroup.AckedAt != nil {
		t.Fatalf("expected open with nil resolved_at+acked_at, got %+v", reopenedGroup)
	}
	if filer.calls.Load() != 0 {
		t.Fatalf("reopen must make 0 GitHub API calls, got %d", filer.calls.Load())
	}

	// 4. Resolve after ack preserves original acked_at timestamp (COALESCE).
	ackTime := now
	recAck := post(agentA, fmt.Sprintf("/v1/errors/%d/ack", gid))
	if recAck.Code != http.StatusOK {
		t.Fatalf("ack status = %d", recAck.Code)
	}
	now = now.Add(5 * time.Minute)
	recRes2 := post(agentA, fmt.Sprintf("/v1/errors/%d/resolve", gid))
	if recRes2.Code != http.StatusOK {
		t.Fatalf("resolve2 status = %d", recRes2.Code)
	}
	var resolved2 inbox.Group
	if err := json.Unmarshal(recRes2.Body.Bytes(), &resolved2); err != nil {
		t.Fatal(err)
	}
	if !resolved2.AckedAt.Equal(ackTime) {
		t.Fatalf("resolve overwrote acked_at: got %v want %v", resolved2.AckedAt, ackTime)
	}
	if !resolved2.ResolvedAt.Equal(now) {
		t.Fatalf("resolve resolved_at: got %v want %v", resolved2.ResolvedAt, now)
	}

	// 5. GitHub ReopenByIssue continues to transition fixed -> acknowledged (preserving acked_at).
	if err := store.UpsertInstallation(ctx, ownerA.ID, 501, "acme", "Organization", "selected", "active", []inbox.GitHubRepository{{ID: 601, InstallationID: 501, FullName: "acme/app-a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindProjectRepository(ctx, ownerA.ID, projA.ID, 601); err != nil {
		t.Fatal(err)
	}
	if err := store.SetIssueForTest(ctx, gid, 42); err != nil {
		t.Fatal(err)
	}
	changed, err := store.ReopenByIssue(ctx, 601, 501, 42)
	if err != nil || changed != 1 {
		t.Fatalf("ReopenByIssue changed=%d err=%v", changed, err)
	}
	afterGHReopen, _ := store.GetForUser(ctx, ownerA.ID, gid)
	if afterGHReopen.Status != inbox.StatusAcknowledged || afterGHReopen.AckedAt == nil || afterGHReopen.ResolvedAt != nil {
		t.Fatalf("GitHub ReopenByIssue must yield acknowledged, got %+v", afterGHReopen)
	}
}
