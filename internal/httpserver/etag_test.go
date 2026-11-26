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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

func setupETagServer(t *testing.T) (*Server, *inbox.Store, string, string) {
	t.Helper()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "etag.db"), nil, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	ctx := context.Background()
	user, err := store.UpsertGitHubUser(ctx, 1001, "etag-owner", "ETag Owner", "etag@example.com")
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	_, ingestToken, _, err := store.CreateProjectWithToken(ctx, user.ID, "Demo App", "demo-app")
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	agentToken, _, err := store.CreateAgentKey(ctx, user.ID, "etag-agent")
	if err != nil {
		t.Fatalf("create agent key: %v", err)
	}
	srv := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{
		Revision: "deadbeef1234567890",
		BaseURL:  "https://raises.example.com",
	})
	return srv, store, agentToken, ingestToken
}

func TestETagStaticDocsAndVersionUntouched(t *testing.T) {
	srv, _, _, _ := setupETagServer(t)

	for _, path := range []string{"/llms.txt", "/skill.md", "/migration.md"} {
		t.Run(path, func(t *testing.T) {
			rec1 := httptest.NewRecorder()
			req1 := httptest.NewRequest(http.MethodGet, path, nil)
			srv.Handler().ServeHTTP(rec1, req1)
			if rec1.Code != http.StatusOK {
				t.Fatalf("%s cold status = %d, want 200", path, rec1.Code)
			}
			etag := rec1.Header().Get("ETag")
			if !strings.HasPrefix(etag, `W/"`) || !strings.HasSuffix(etag, `"`) {
				t.Fatalf("%s ETag = %q, want weak ETag W/\"...\"", path, etag)
			}
			if rec1.Body.Len() == 0 {
				t.Fatalf("%s cold body empty", path)
			}

			// Matching weak ETag -> 304 Not Modified with 0 body bytes.
			rec2 := httptest.NewRecorder()
			req2 := httptest.NewRequest(http.MethodGet, path, nil)
			req2.Header.Set("If-None-Match", etag)
			srv.Handler().ServeHTTP(rec2, req2)
			if rec2.Code != http.StatusNotModified {
				t.Fatalf("%s 304 status = %d, want 304", path, rec2.Code)
			}
			if rec2.Body.Len() != 0 {
				t.Fatalf("%s 304 body = %d bytes, want 0", path, rec2.Body.Len())
			}
			if rec2.Header().Get("ETag") != etag {
				t.Fatalf("%s 304 ETag = %q, want %q", path, rec2.Header().Get("ETag"), etag)
			}

			// Comma-separated list with strong form matches weakly per RFC 9110.
			strong := strings.TrimPrefix(etag, "W/")
			rec3 := httptest.NewRecorder()
			req3 := httptest.NewRequest(http.MethodGet, path, nil)
			req3.Header.Set("If-None-Match", `"other", `+strong)
			srv.Handler().ServeHTTP(rec3, req3)
			if rec3.Code != http.StatusNotModified {
				t.Fatalf("%s multi-etag status = %d, want 304", path, rec3.Code)
			}

			// Mismatched ETag -> 200 OK.
			rec4 := httptest.NewRecorder()
			req4 := httptest.NewRequest(http.MethodGet, path, nil)
			req4.Header.Set("If-None-Match", `W/"00000000000000000000000000000000"`)
			srv.Handler().ServeHTTP(rec4, req4)
			if rec4.Code != http.StatusOK || rec4.Body.Len() != rec1.Body.Len() {
				t.Fatalf("%s mismatch status=%d len=%d, want 200 len=%d", path, rec4.Code, rec4.Body.Len(), rec1.Body.Len())
			}
		})
	}

	// /version must NEVER emit ETag or return 304.
	recV := httptest.NewRecorder()
	reqV := httptest.NewRequest(http.MethodGet, "/version", nil)
	reqV.Header.Set("If-None-Match", "*")
	srv.Handler().ServeHTTP(recV, reqV)
	if recV.Code != http.StatusOK {
		t.Fatalf("/version status = %d, want 200", recV.Code)
	}
	if etag := recV.Header().Get("ETag"); etag != "" {
		t.Fatalf("/version emitted ETag %q, want empty", etag)
	}
	if cc := recV.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("/version Cache-Control = %q, want no-store", cc)
	}
	if want := `{"revision":"deadbeef1234567890"}`; recV.Body.String() != want {
		t.Fatalf("/version body = %q, want %q", recV.Body.String(), want)
	}
}

func TestETagErrorsAndProjectsFreshAfterMutationsAndConcurrent(t *testing.T) {
	srv, store, agentToken, ingestToken := setupETagServer(t)
	ctx := context.Background()

	getWithETag := func(path, inm string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+agentToken)
		if inm != "" {
			req.Header.Set("If-None-Match", inm)
		}
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	// Seed an initial error.
	res, err := store.Ingest(ctx, ingestToken, inbox.Notice{
		App: "demo-app", Env: "production", Class: "RuntimeError", Message: "boom 1",
		Backtrace: []string{"app/models/order.rb:12:in `save'"},
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}

	paths := []string{
		"/v1/errors?unacked=1",
		"/v1/errors?unacked=1&compact=1",
		"/v1/errors?status=open&page=1&per_page=25&compact=1",
		"/v1/errors?status=open&page=1&per_page=25",
	}

	// Verify 200 -> 304 -> mutation -> 200 (0 stale 304s) across all /v1/errors variants.
	etags := map[string]string{}
	for _, p := range paths {
		rec := getWithETag(p, "")
		if rec.Code != http.StatusOK {
			t.Fatalf("%s cold status=%d", p, rec.Code)
		}
		etag := rec.Header().Get("ETag")
		if !strings.HasPrefix(etag, `W/"`) {
			t.Fatalf("%s ETag=%q", p, etag)
		}
		etags[p] = etag

		rec304 := getWithETag(p, etag)
		if rec304.Code != http.StatusNotModified || rec304.Body.Len() != 0 {
			t.Fatalf("%s unchanged got status=%d len=%d, want 304 len=0", p, rec304.Code, rec304.Body.Len())
		}
	}

	// Mutate via ingest (count increases) -> all previous ETags must now return 200 with a fresh ETag.
	if _, err := store.Ingest(ctx, ingestToken, inbox.Notice{
		App: "demo-app", Env: "production", Class: "RuntimeError", Message: "boom 2",
		Backtrace: []string{"app/models/order.rb:12:in `save'"},
	}); err != nil {
		t.Fatalf("second ingest: %v", err)
	}
	for _, p := range paths {
		rec := getWithETag(p, etags[p])
		if rec.Code != http.StatusOK {
			t.Fatalf("%s stale 304 after ingest mutation!", p)
		}
		newETag := rec.Header().Get("ETag")
		if newETag == etags[p] {
			t.Fatalf("%s ETag unchanged after mutation", p)
		}
		etags[p] = newETag
	}

	// Mutate via ack -> stale ETag must return 200.
	ackReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v1/errors/%d/ack", res.Group.ID), nil)
	ackReq.Header.Set("Authorization", "Bearer "+agentToken)
	ackRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(ackRec, ackReq)
	if ackRec.Code != http.StatusOK {
		t.Fatalf("ack status=%d", ackRec.Code)
	}
	for _, p := range paths {
		rec := getWithETag(p, etags[p])
		if rec.Code != http.StatusOK {
			t.Fatalf("%s stale 304 after ack!", p)
		}
		etags[p] = rec.Header().Get("ETag")
	}

	// /v1/projects 200 -> 304 -> create project -> 200.
	p1 := getWithETag("/v1/projects", "")
	if p1.Code != http.StatusOK || !strings.HasPrefix(p1.Header().Get("ETag"), `W/"`) {
		t.Fatalf("/v1/projects cold status=%d etag=%q", p1.Code, p1.Header().Get("ETag"))
	}
	p2 := getWithETag("/v1/projects", p1.Header().Get("ETag"))
	if p2.Code != http.StatusNotModified || p2.Body.Len() != 0 {
		t.Fatalf("/v1/projects 304 status=%d len=%d", p2.Code, p2.Body.Len())
	}
	createBody := bytes.NewBufferString(`{"name":"Second App","slug":"second-app"}`)
	cReq := httptest.NewRequest(http.MethodPost, "/v1/projects", createBody)
	cReq.Header.Set("Authorization", "Bearer "+agentToken)
	cRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(cRec, cReq)
	if cRec.Code != http.StatusCreated {
		t.Fatalf("create project status=%d body=%s", cRec.Code, cRec.Body.String())
	}
	p3 := getWithETag("/v1/projects", p1.Header().Get("ETag"))
	if p3.Code != http.StatusOK {
		t.Fatalf("/v1/projects stale 304 after create!")
	}

	// Concurrent post-mutation consistency check: sequential mutations interleaved
	// with concurrent readers verifying every post-mutation read with the pre-mutation
	// ETag returns 200 (0 stale 304s).
	var wg sync.WaitGroup
	errCh := make(chan error, 64)
	for i := 0; i < 20; i++ {
		before := getWithETag("/v1/errors?status=all&page=1&per_page=25&compact=1", "")
		if before.Code != http.StatusOK {
			t.Fatalf("iter %d pre-read status=%d", i, before.Code)
		}
		preETag := before.Header().Get("ETag")

		if _, err := store.Ingest(ctx, ingestToken, inbox.Notice{
			App: "demo-app", Env: "production",
			Class:     fmt.Sprintf("ConcurrentErr%d", i),
			Message:   fmt.Sprintf("msg %d", i),
			Backtrace: []string{fmt.Sprintf("app/jobs/worker_%d.rb:%d:in `run'", i, i+1)},
		}); err != nil {
			t.Fatalf("iter %d ingest: %v", i, err)
		}

		// Fan out 4 concurrent readers using the pre-mutation ETag.
		for rIdx := 0; rIdx < 4; rIdx++ {
			wg.Add(1)
			go func(iter int) {
				defer wg.Done()
				after := getWithETag("/v1/errors?status=all&page=1&per_page=25&compact=1", preETag)
				if after.Code != http.StatusOK {
					errCh <- fmt.Errorf("iter %d: got stale status %d after committed mutation", iter, after.Code)
					return
				}
				if after.Header().Get("ETag") == preETag {
					errCh <- fmt.Errorf("iter %d: ETag did not change after committed mutation", iter)
				}
			}(i)
		}
		wg.Wait()
	}
	close(errCh)
	for e := range errCh {
		t.Fatal(e)
	}
}

func TestETagColdOverheadUnderBudget(t *testing.T) {
	// Measure pure cold-200 overhead of writeJSONWithETag vs writeJSON on a
	// realistic 25-row compact page (~8-10 KB) and 90-row uncompacted list (~100 KB).
	groups := make([]compactGroup, 25)
	now := time.Now().UTC()
	for i := range groups {
		groups[i] = compactGroup{
			ID:       int64(i + 1),
			App:      "demo-app",
			Env:      "production",
			Class:    "ActiveRecord::StatementInvalid",
			Location: fmt.Sprintf("app/models/order_%d.rb:42", i),
			Message:  "PG::QueryCanceled: ERROR: canceling statement due to statement timeout",
			Count:    100 + i,
			LastSeen: now,
			URL:      fmt.Sprintf("https://raises.example.com/errors/%d", i+1),
			Status:   "open",
		}
	}
	page := compactErrorPage{Groups: groups, Page: 1, PageSize: 25, Total: 90, Pages: 4}
	const iters = 500

	// Warm up
	for i := 0; i < 20; i++ {
		writeJSON(httptest.NewRecorder(), http.StatusOK, page)
		writeJSONWithETag(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/errors", nil), http.StatusOK, page)
	}

	t0 := time.Now()
	for i := 0; i < iters; i++ {
		rec := httptest.NewRecorder()
		writeJSON(rec, http.StatusOK, page)
	}
	basePerReq := time.Since(t0) / iters

	t1 := time.Now()
	for i := 0; i < iters; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/errors", nil)
		writeJSONWithETag(rec, req, http.StatusOK, page)
	}
	etagPerReq := time.Since(t1) / iters

	overhead := etagPerReq - basePerReq
	t.Logf("writeJSON=%v writeJSONWithETag=%v overhead=%v", basePerReq, etagPerReq, overhead)
	if overhead > 300*time.Microsecond {
		t.Fatalf("cold 200 ETag overhead %v exceeds 0.3ms budget", overhead)
	}

	// Verify byte-exact JSON equivalence with writeJSON.
	recA := httptest.NewRecorder()
	writeJSON(recA, http.StatusOK, page)
	recB := httptest.NewRecorder()
	writeJSONWithETag(recB, httptest.NewRequest(http.MethodGet, "/v1/errors", nil), http.StatusOK, page)
	if !bytes.Equal(recA.Body.Bytes(), recB.Body.Bytes()) {
		t.Fatalf("writeJSONWithETag body differs from writeJSON:\nA=%q\nB=%q", recA.Body.String(), recB.Body.String())
	}
	var parsed compactErrorPage
	if err := json.Unmarshal(recB.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}
