package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raisestracker/raises/internal/inbox"
)

func TestCustomFingerprintHTTPValidationAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "custom-fp.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ownerA, _ := store.UpsertGitHubUser(ctx, 8101, "fp-owner-a", "", "")
	projA, _ := store.CreateProject(ctx, ownerA.ID, "Shared Slug", "shared-slug")
	tokA, _, _ := store.CreateProjectToken(ctx, ownerA.ID, projA.ID)

	ownerB, _ := store.UpsertGitHubUser(ctx, 8102, "fp-owner-b", "", "")
	projB, _ := store.CreateProject(ctx, ownerB.ID, "Shared Slug", "shared-slug")
	tokB, _, _ := store.CreateProjectToken(ctx, ownerB.ID, projB.ID)

	srv := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{})
	handler := srv.Handler()

	postNotice := func(tok string, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/v1/notices", bytes.NewBufferString(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// 1. Consolidate 4 distinct files under one custom fingerprint for Tenant A.
	var firstGroupID int64
	files := []string{"app/models/a.rb:10", "app/models/b.rb:20", "app/jobs/c.rb:30", "app/services/d.rb:40"}
	for i, f := range files {
		payload := map[string]any{
			"env":         "production",
			"revision":    "r1",
			"fingerprint": []string{"db-pool-exhausted"},
			"error": map[string]any{
				"class":     "DBPoolTimeout",
				"message":   "pool exhausted",
				"backtrace": []string{f},
			},
		}
		raw, _ := json.Marshal(payload)
		rec := postNotice(tokA, string(raw))
		wantCode := http.StatusOK
		if i == 0 {
			wantCode = http.StatusCreated
		}
		if rec.Code != wantCode {
			t.Fatalf("notice %d: status=%d want=%d body=%s", i, rec.Code, wantCode, rec.Body.String())
		}
		var res inbox.IngestResult
		if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			firstGroupID = res.Group.ID
		} else if res.Group.ID != firstGroupID {
			t.Fatalf("notice %d created separate group %d vs %d", i, res.Group.ID, firstGroupID)
		}
		if res.Group.Count != i+1 {
			t.Fatalf("notice %d count=%d want=%d", i, res.Group.Count, i+1)
		}
	}

	// 2. Tenant B with identical slug, env, and custom fingerprint gets an isolated group.
	recB := postNotice(tokB, `{"env":"production","fingerprint":["db-pool-exhausted"],"error":{"class":"DBPoolTimeout","message":"other tenant","backtrace":["app/models/a.rb:10"]}}`)
	if recB.Code != http.StatusCreated {
		t.Fatalf("tenant B status=%d want=201 body=%s", recB.Code, recB.Body.String())
	}
	var resB inbox.IngestResult
	if err := json.Unmarshal(recB.Body.Bytes(), &resB); err != nil {
		t.Fatal(err)
	}
	if resB.Group.ID == firstGroupID || resB.Group.ProjectID != projB.ID {
		t.Fatalf("cross-tenant collision: A=%d B=%d projB=%s gotProj=%s", firstGroupID, resB.Group.ID, projB.ID, resB.Group.ProjectID)
	}

	// 3. Same tenant, different env gets an isolated group.
	recStaging := postNotice(tokA, `{"env":"staging","fingerprint":["db-pool-exhausted"],"error":{"class":"DBPoolTimeout","message":"staging","backtrace":["app/models/a.rb:10"]}}`)
	if recStaging.Code != http.StatusCreated {
		t.Fatalf("staging status=%d want=201", recStaging.Code)
	}

	// 4. Malformed / oversized fingerprint arrays return 400.
	badBodies := []string{
		`{"env":"production","fingerprint":"not-an-array","error":{"class":"Err","message":"x"}}`,
		`{"env":"production","fingerprint":[],"error":{"class":"Err","message":"x"}}`,
		`{"env":"production","fingerprint":[""],"error":{"class":"Err","message":"x"}}`,
		`{"env":"production","fingerprint":["   "],"error":{"class":"Err","message":"x"}}`,
		`{"env":"production","fingerprint":["1","2","3","4","5","6","7","8","9","10","11"],"error":{"class":"Err","message":"x"}}`,
		`{"env":"production","fingerprint":["` + strings.Repeat("z", 257) + `"],"error":{"class":"Err","message":"x"}}`,
	}
	for i, body := range badBodies {
		rec := postNotice(tokA, body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("bad body %d: status=%d want=400 body=%s", i, rec.Code, rec.Body.String())
		}
	}
}
