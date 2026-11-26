package httpserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

func setupSentryTestServer(t *testing.T, bodyLimit int64) (*Server, *inbox.Store, int64, string, int64, string) {
	t.Helper()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "sentry.db"), nil, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	userA, err := store.UpsertGitHubUser(ctx, 2001, "tenant-a", "Tenant A", "")
	if err != nil {
		t.Fatal(err)
	}
	_, tokenA, _, err := store.CreateProjectWithToken(ctx, userA.ID, "Alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	userB, err := store.UpsertGitHubUser(ctx, 2002, "tenant-b", "Tenant B", "")
	if err != nil {
		t.Fatal(err)
	}
	_, tokenB, _, err := store.CreateProjectWithToken(ctx, userB.ID, "Beta", "beta")
	if err != nil {
		t.Fatal(err)
	}
	srv := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), bodyLimit, Config{
		BaseURL: "https://raises.example.com",
	})
	return srv, store, userA.ID, tokenA, userB.ID, tokenB
}

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sentryAuthHeader(token string) string {
	return fmt.Sprintf("Sentry sentry_version=7, sentry_client=sentry.go/0.28.1, sentry_key=%s", token)
}

func TestSentryEnvelopeEndToEndFourSDKsAndNumericDSN(t *testing.T) {
	srv, store, userA, tokenA, _, _ := setupSentryTestServer(t, 1<<20)
	ctx := context.Background()

	// Verify SENTRY_DSN generated for onboarding parses cleanly as a numeric-project DSN.
	dsnSnippet := sentryDSNSnippet("https://raises.example.com", tokenA)
	dsnLine := ""
	for _, line := range strings.Split(dsnSnippet, "\n") {
		if strings.HasPrefix(line, "SENTRY_DSN=") {
			dsnLine = strings.TrimPrefix(line, "SENTRY_DSN=")
		}
	}
	parsedDSN, err := url.Parse(dsnLine)
	if err != nil || parsedDSN.User.Username() != tokenA || parsedDSN.Path != "/1" {
		t.Fatalf("unexpected SENTRY_DSN %q (err=%v)", dsnLine, err)
	}
	envelopeURL := "/api/" + strings.TrimPrefix(parsedDSN.Path, "/") + "/envelope/"

	fixtures := []struct {
		name      string
		gzipped   bool
		payload   string
		wantClass string
		wantLoc   string
	}{
		{
			name:    "node_gzip",
			gzipped: true,
			payload: "{\"event_id\":\"n1\",\"sdk\":{\"name\":\"sentry.javascript.node\",\"version\":\"8.20.0\"}}\n" +
				"{\"type\":\"event\"}\n" +
				"{\"event_id\":\"n1\",\"platform\":\"node\",\"environment\":\"production\",\"release\":\"web@1.0\",\"exception\":{\"values\":[{\"type\":\"TypeError\",\"value\":\"undefined user\",\"stacktrace\":{\"frames\":[{\"abs_path\":\"/srv/web/src/checkout.ts\",\"function\":\"createSession\",\"lineno\":54},{\"abs_path\":\"/srv/web/node_modules/express/index.js\",\"function\":\"handle\",\"lineno\":10}]}}]}}\n",
			wantClass: "TypeError", wantLoc: "/srv/web/src/checkout.ts:54",
		},
		{
			name:    "python_plain",
			gzipped: false,
			payload: "{\"event_id\":\"p1\",\"sdk\":{\"name\":\"sentry.python\",\"version\":\"2.12.0\"}}\n" +
				"{\"type\":\"event\"}\n" +
				"{\"event_id\":\"p1\",\"platform\":\"python\",\"environment\":\"production\",\"release\":\"py@1.0\",\"exception\":{\"values\":[{\"type\":\"KeyError\",\"value\":\"'order_id'\",\"stacktrace\":{\"frames\":[{\"abs_path\":\"/srv/api/services/payments.py\",\"function\":\"charge\",\"lineno\":42},{\"abs_path\":\"/usr/lib/python3.11/json/__init__.py\",\"function\":\"loads\",\"lineno\":346}]}}]}}\n",
			wantClass: "KeyError", wantLoc: "/srv/api/services/payments.py:42",
		},
		{
			name:    "go_gzip",
			gzipped: true,
			payload: "{\"event_id\":\"g1\",\"sdk\":{\"name\":\"sentry.go\",\"version\":\"0.28.1\"}}\n" +
				"{\"type\":\"event\"}\n" +
				"{\"event_id\":\"g1\",\"platform\":\"go\",\"environment\":\"production\",\"release\":\"go@1.0\",\"exception\":{\"values\":[{\"type\":\"*errors.errorString\",\"value\":\"nil card\",\"stacktrace\":{\"frames\":[{\"filename\":\"internal/billing/charge.go\",\"function\":\"Charge\",\"lineno\":88},{\"filename\":\"net/http/server.go\",\"function\":\"ServeHTTP\",\"lineno\":2220}]}}]}}\n",
			wantClass: "*errors.errorString", wantLoc: "internal/billing/charge.go:88",
		},
		{
			name:    "cocoa_plain",
			gzipped: false,
			payload: "{\"event_id\":\"c1\",\"sdk\":{\"name\":\"sentry.cocoa\",\"version\":\"8.33.0\"}}\n" +
				"{\"type\":\"event\"}\n" +
				"{\"event_id\":\"c1\",\"platform\":\"cocoa\",\"environment\":\"production\",\"release\":\"ios@2.0\",\"exception\":{\"values\":[{\"type\":\"FatalError\",\"value\":\"nil cart\",\"stacktrace\":{\"frames\":[{\"filename\":\"CartViewModel.swift\",\"function\":\"CartViewModel.checkout()\",\"lineno\":88},{\"package\":\"libswiftCore.dylib\",\"function\":\"_assertionFailure\"}]}}]}}\n",
			wantClass: "FatalError", wantLoc: "CartViewModel.swift:88",
		},
	}

	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(tc.payload)
			var body io.Reader = bytes.NewReader(raw)
			if tc.gzipped {
				body = bytes.NewReader(gzipBytes(t, raw))
			}
			req := httptest.NewRequest(http.MethodPost, envelopeURL, body)
			req.Header.Set("X-Sentry-Auth", sentryAuthHeader(tokenA))
			if tc.gzipped {
				req.Header.Set("Content-Encoding", "gzip")
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			var resp map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp["id"] == "" {
				t.Fatalf("unexpected response %q", rec.Body.String())
			}
		})
	}

	groups, err := store.ListForUser(ctx, userA, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 4 {
		t.Fatalf("expected 4 ingested groups, got %d", len(groups))
	}
	byClass := map[string]inbox.Group{}
	for _, g := range groups {
		byClass[g.Class] = g
	}
	for _, tc := range fixtures {
		g, ok := byClass[tc.wantClass]
		if !ok {
			t.Fatalf("missing group for %s", tc.wantClass)
		}
		if g.Location != tc.wantLoc {
			t.Fatalf("%s location=%q, want %q", tc.name, g.Location, tc.wantLoc)
		}
	}
}

func TestSentryEnvelopeSecurityGuards(t *testing.T) {
	// Use 4 KiB bodyLimit so gzip-bomb test is instant and deterministic.
	const limit = 4096
	srv, store, userA, tokenA, userB, tokenB := setupSentryTestServer(t, limit)
	ctx := context.Background()

	simpleEnv := "{\"event_id\":\"e1\"}\n{\"type\":\"event\"}\n{\"event_id\":\"e1\",\"message\":\"hi\"}\n"

	// 1. Reject ?sentry_key= query parameter with 401 (both without and WITH valid X-Sentry-Auth).
	for _, withHeader := range []bool{false, true} {
		req := httptest.NewRequest(http.MethodPost, "/api/1/envelope/?sentry_key="+tokenA, strings.NewReader(simpleEnv))
		if withHeader {
			req.Header.Set("X-Sentry-Auth", sentryAuthHeader(tokenA))
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("query sentry_key (withHeader=%v) status=%d, want 401", withHeader, rec.Code)
		}
	}

	// 2. Bounded gzip decompression: small compressed payload expanding > bodyLimit returns 413.
	bombPlain := bytes.Repeat([]byte("A"), limit*16) // 64 KiB -> compresses to < 300 bytes!
	bombGzip := gzipBytes(t, bombPlain)
	if len(bombGzip) >= limit {
		t.Fatalf("test setup error: compressed bomb %d >= limit %d", len(bombGzip), limit)
	}
	reqBomb := httptest.NewRequest(http.MethodPost, "/api/1/envelope/", bytes.NewReader(bombGzip))
	reqBomb.Header.Set("X-Sentry-Auth", sentryAuthHeader(tokenA))
	reqBomb.Header.Set("Content-Encoding", "gzip")
	recBomb := httptest.NewRecorder()
	srv.Handler().ServeHTTP(recBomb, reqBomb)
	if recBomb.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("gzip bomb status=%d body=%s, want 413", recBomb.Code, recBomb.Body.String())
	}

	// 3. Cross-tenant {project} placeholder cannot write to another tenant's project.
	projA, err := store.ProjectBySlug(ctx, userA, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, targetPath := range []string{"/api/" + projA.ID + "/envelope/", "/api/alpha/envelope/", "/api/999999/envelope/"} {
		req := httptest.NewRequest(http.MethodPost, targetPath, strings.NewReader(simpleEnv))
		req.Header.Set("X-Sentry-Auth", sentryAuthHeader(tokenB))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("tenant B post to %s status=%d body=%s", targetPath, rec.Code, rec.Body.String())
		}
	}
	groupsA, _ := store.ListForUser(ctx, userA, "", false)
	groupsB, _ := store.ListForUser(ctx, userB, "", false)
	if len(groupsA) != 0 {
		t.Fatalf("IDOR! tenant A received %d groups from tenant B", len(groupsA))
	}
	if len(groupsB) != 1 || groupsB[0].Count != 3 {
		t.Fatalf("expected tenant B to own all 3 notices, got %+v", groupsB)
	}

	// 4. Multi-item envelope charges postAuthIngestPerTokenLimit PER ITEM -> flood hits 429!
	var tenItemEnv strings.Builder
	tenItemEnv.WriteString("{\"event_id\":\"batch\"}\n")
	for i := 0; i < 10; i++ {
		tenItemEnv.WriteString("{\"type\":\"event\"}\n")
		fmt.Fprintf(&tenItemEnv, "{\"event_id\":\"b%d\",\"message\":\"flood\"}\n", i)
	}
	batchPayload := tenItemEnv.String()
	got429 := false
	sent := 0
	for i := 0; i < (postAuthIngestPerTokenLimit/10)+5; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/1/envelope/", strings.NewReader(batchPayload))
		req.Header.Set("X-Sentry-Auth", sentryAuthHeader(tokenA))
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		sent++
		if rec.Code == http.StatusTooManyRequests {
			got429 = true
			break
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("iter %d unexpected status=%d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	if !got429 {
		t.Fatalf("multi-item flood did not hit 429 after %d x 10-item envelopes", sent)
	}
	if sent > (postAuthIngestPerTokenLimit/10)+1 {
		t.Fatalf("rate limit triggered too late at envelope %d (expected <= %d)", sent, (postAuthIngestPerTokenLimit/10)+1)
	}

	// 5. >10 items in single envelope returns 400.
	var elevenItemEnv strings.Builder
	elevenItemEnv.WriteString("{\"event_id\":\"too-many\"}\n")
	for i := 0; i < 11; i++ {
		elevenItemEnv.WriteString("{\"type\":\"event\"}\n{\"event_id\":\"x\",\"message\":\"m\"}\n")
	}
	req11 := httptest.NewRequest(http.MethodPost, "/api/1/envelope/", strings.NewReader(elevenItemEnv.String()))
	req11.Header.Set("X-Sentry-Auth", sentryAuthHeader(tokenB))
	rec11 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec11, req11)
	if rec11.Code != http.StatusBadRequest {
		t.Fatalf("11-item envelope status=%d, want 400", rec11.Code)
	}

	// 6. Dashboard onboarding reveal includes SENTRY_DSN snippet.
	sessTok, sess, err := store.CreateSession(ctx, userA, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"csrf": {sess.CSRFToken}, "name": {"Sentry Demo"}, "slug": {"sentry-demo"}}
	dashReq := httptest.NewRequest(http.MethodPost, "/dashboard/projects", strings.NewReader(form.Encode()))
	dashReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	dashReq.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessTok})
	dashRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(dashRec, dashReq)
	if dashRec.Code != http.StatusCreated {
		t.Fatalf("dashboard create status=%d", dashRec.Code)
	}
	if !strings.Contains(dashRec.Body.String(), "SENTRY_DSN=https://raises_ingest_") || !strings.Contains(dashRec.Body.String(), "@raises.example.com/1") {
		t.Fatalf("dashboard reveal missing SENTRY_DSN snippet")
	}
}
