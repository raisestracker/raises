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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

func TestConcurrentClaimExclusivityTTLAndAutoClear(t *testing.T) {
	ctx := context.Background()
	var nowUnix atomic.Int64
	nowUnix.Store(time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC).Unix())
	clock := func() time.Time { return time.Unix(nowUnix.Load(), 0).UTC() }

	store, err := inbox.Open(filepath.Join(t.TempDir(), "claim-race.db"), clock, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ownerA, _ := store.UpsertGitHubUser(ctx, 8301, "claim-a", "", "")
	agentA, _, _ := store.CreateAgentKey(ctx, ownerA.ID, "agent-a")
	projA, _ := store.CreateProject(ctx, ownerA.ID, "Factory", "factory")
	tokA, _, _ := store.CreateProjectToken(ctx, ownerA.ID, projA.ID)

	ownerB, _ := store.UpsertGitHubUser(ctx, 8302, "claim-b", "", "")
	agentB, _, _ := store.CreateAgentKey(ctx, ownerB.ID, "agent-b")

	res, err := store.Ingest(ctx, tokA, inbox.Notice{
		Env:       "production",
		Class:     "RuntimeError",
		Message:   "boom",
		Backtrace: []string{"app/jobs/worker.rb:42"},
	})
	if err != nil {
		t.Fatal(err)
	}
	gid := res.Group.ID

	srv := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{})
	handler := srv.Handler()

	// 1. Unclaimed error omits "claim" key in JSON and status stays "open".
	reqGet := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/errors/%d", gid), nil)
	reqGet.Header.Set("Authorization", "Bearer "+agentA)
	recGet := httptest.NewRecorder()
	handler.ServeHTTP(recGet, reqGet)
	var rawMap map[string]any
	if err := json.Unmarshal(recGet.Body.Bytes(), &rawMap); err != nil {
		t.Fatal(err)
	}
	if _, hasClaim := rawMap["claim"]; hasClaim {
		t.Fatalf("unclaimed group must omit claim field: %s", recGet.Body.String())
	}
	if rawMap["status"] != inbox.StatusOpen {
		t.Fatalf("status = %v, want open", rawMap["status"])
	}

	// 2. Cross-tenant IDOR on claim & release (even with empty body) returns 404.
	for _, path := range []string{fmt.Sprintf("/v1/errors/%d/claim", gid), fmt.Sprintf("/v1/errors/%d/release", gid)} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+agentB)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("IDOR %s: status=%d want 404", path, rec.Code)
		}
	}

	// 3. 10-way concurrent claim under -race -> strictly 1 x 200 and 9 x 409 with holder metadata.
	const workers = 10
	start := make(chan struct{})
	var wg sync.WaitGroup
	var okCount, conflictCount atomic.Int32
	var winnerAgent atomic.Value
	var conflictHolders sync.Map

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			name := fmt.Sprintf("worker-%d", idx)
			body := fmt.Sprintf(`{"agent":%q,"ttl_seconds":900,"note":"triage %d"}`, name, idx)
			<-start
			req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/errors/%d/claim", gid), bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer "+agentA)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			switch rec.Code {
			case http.StatusOK:
				okCount.Add(1)
				var g inbox.Group
				if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
					t.Errorf("decode 200: %v", err)
					return
				}
				if g.Status != inbox.StatusOpen {
					t.Errorf("claim mutated status to %q", g.Status)
				}
				if g.Claim == nil || g.Claim.Agent != name {
					t.Errorf("unexpected claim: %+v", g.Claim)
				}
				winnerAgent.Store(name)
			case http.StatusConflict:
				conflictCount.Add(1)
				var payload struct {
					Error string       `json:"error"`
					Claim *inbox.Claim `json:"claim"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
					t.Errorf("decode 409: %v", err)
					return
				}
				if payload.Claim == nil || payload.Claim.Agent == "" {
					t.Errorf("409 missing holder metadata: %s", rec.Body.String())
					return
				}
				conflictHolders.Store(idx, payload.Claim.Agent)
			default:
				t.Errorf("worker %d unexpected status %d: %s", idx, rec.Code, rec.Body.String())
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if okCount.Load() != 1 || conflictCount.Load() != workers-1 {
		t.Fatalf("concurrent claim results: 200=%d 409=%d (want 1 and %d)", okCount.Load(), conflictCount.Load(), workers-1)
	}
	winner := winnerAgent.Load().(string)
	conflictHolders.Range(func(_, v any) bool {
		if v.(string) != winner {
			t.Fatalf("409 reported holder %q, want %q", v, winner)
		}
		return true
	})

	// 4. ?exclude_claimed=1 hides actively claimed errors on both paginated and unacked lists.
	for _, url := range []string{
		"/v1/errors?status=open&exclude_claimed=1&compact=1",
		"/v1/errors?unacked=1&exclude_claimed=1&compact=1",
	} {
		req := httptest.NewRequest(http.MethodGet, url, nil)
		req.Header.Set("Authorization", "Bearer "+agentA)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d", url, rec.Code)
		}
		if bytes.Contains(rec.Body.Bytes(), []byte(`"app":"factory"`)) {
			t.Fatalf("%s failed to exclude claimed error: %s", url, rec.Body.String())
		}
	}

	// 5. Post-TTL expiration: claim disappears from JSON, exclude_claimed=1 shows it, new agent reclaims (200) and TTL > 2h caps at 2h.
	nowUnix.Add(int64((15 * time.Minute / time.Second) + 1))
	reqExpired := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/errors/%d", gid), nil)
	reqExpired.Header.Set("Authorization", "Bearer "+agentA)
	recExpired := httptest.NewRecorder()
	handler.ServeHTTP(recExpired, reqExpired)
	var expiredMap map[string]any
	_ = json.Unmarshal(recExpired.Body.Bytes(), &expiredMap)
	if _, hasClaim := expiredMap["claim"]; hasClaim {
		t.Fatalf("expired claim must be omitted from JSON: %s", recExpired.Body.String())
	}

	reqReclaim := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/errors/%d/claim", gid), bytes.NewBufferString(`{"agent":"fresh-agent","ttl_seconds":99999}`))
	reqReclaim.Header.Set("Authorization", "Bearer "+agentA)
	recReclaim := httptest.NewRecorder()
	handler.ServeHTTP(recReclaim, reqReclaim)
	if recReclaim.Code != http.StatusOK {
		t.Fatalf("post-TTL reclaim status=%d body=%s", recReclaim.Code, recReclaim.Body.String())
	}
	var reclaimed inbox.Group
	_ = json.Unmarshal(recReclaim.Body.Bytes(), &reclaimed)
	if reclaimed.Claim == nil || reclaimed.Claim.Agent != "fresh-agent" {
		t.Fatalf("reclaim failed: %+v", reclaimed.Claim)
	}
	if dur := reclaimed.Claim.ExpiresAt.Sub(reclaimed.Claim.ClaimedAt); dur != inbox.MaxClaimTTL {
		t.Fatalf("ttl cap = %v, want %v", dur, inbox.MaxClaimTTL)
	}

	// 6. Release by wrong agent -> 409; release by holder -> 200 clears claim.
	reqBadRel := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/errors/%d/release", gid), bytes.NewBufferString(`{"agent":"other-agent"}`))
	reqBadRel.Header.Set("Authorization", "Bearer "+agentA)
	recBadRel := httptest.NewRecorder()
	handler.ServeHTTP(recBadRel, reqBadRel)
	if recBadRel.Code != http.StatusConflict {
		t.Fatalf("wrong-agent release status=%d want 409", recBadRel.Code)
	}
	reqRel := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/errors/%d/release", gid), bytes.NewBufferString(`{"agent":"fresh-agent"}`))
	reqRel.Header.Set("Authorization", "Bearer "+agentA)
	recRel := httptest.NewRecorder()
	handler.ServeHTTP(recRel, reqRel)
	if recRel.Code != http.StatusOK {
		t.Fatalf("holder release status=%d", recRel.Code)
	}

	// 7. Auto-clear on ack, resolve, suppress, and ResolveByIssue.
	assertClearedAfter := func(label string, fn func()) {
		t.Helper()
		if _, err := store.ClaimForUser(ctx, ownerA.ID, gid, "holder", "working", 15*time.Minute); err != nil {
			t.Fatalf("%s pre-claim: %v", label, err)
		}
		fn()
		g, err := store.GetForUser(ctx, ownerA.ID, gid)
		if err != nil {
			t.Fatalf("%s get: %v", label, err)
		}
		if g.Claim != nil {
			t.Fatalf("%s did not clear claim: %+v", label, g.Claim)
		}
	}
	assertClearedAfter("ack", func() {
		_, err := store.AckForUser(ctx, ownerA.ID, gid)
		if err != nil {
			t.Fatal(err)
		}
	})
	assertClearedAfter("resolve", func() {
		_, err := store.ResolveForUser(ctx, ownerA.ID, gid)
		if err != nil {
			t.Fatal(err)
		}
	})
	assertClearedAfter("suppress", func() {
		_, err := store.SuppressForUser(ctx, ownerA.ID, gid)
		if err != nil {
			t.Fatal(err)
		}
	})
	_, _ = store.UnsuppressForUser(ctx, ownerA.ID, gid)
	_, _ = store.ReopenForUser(ctx, ownerA.ID, gid)
	if err := store.UpsertInstallation(ctx, ownerA.ID, 701, "acme", "Organization", "selected", "active", []inbox.GitHubRepository{{ID: 801, InstallationID: 701, FullName: "acme/factory"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindProjectRepository(ctx, ownerA.ID, projA.ID, 801); err != nil {
		t.Fatal(err)
	}
	if err := store.SetIssueForTest(ctx, gid, 77); err != nil {
		t.Fatal(err)
	}
	assertClearedAfter("ResolveByIssue", func() {
		n, err := store.ResolveByIssue(ctx, 801, 701, 77)
		if err != nil || n != 1 {
			t.Fatalf("ResolveByIssue n=%d err=%v", n, err)
		}
	})
}
