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
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

type securityFixture struct {
	store     *inbox.Store
	srv       *Server
	handler   http.Handler
	userA     inbox.User
	userB     inbox.User
	keyA      string
	keyB      string
	tokenA    string
	tokenB    string
	projA     inbox.Project
	projB     inbox.Project
	cookieA   *http.Cookie
	cookieB   *http.Cookie
	csrfA     string
	csrfB     string
	groupA    inbox.Group
	groupB    inbox.Group
	bodyLimit int64
}

func newSecurityFixture(t *testing.T) securityFixture {
	t.Helper()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "sec.db"), nil, nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()

	userA, err := store.UpsertGitHubUser(ctx, 8101, "sec-alice", "Alice", "")
	if err != nil {
		t.Fatal(err)
	}
	userB, err := store.UpsertGitHubUser(ctx, 8102, "sec-bob", "Bob", "")
	if err != nil {
		t.Fatal(err)
	}
	keyA, _, err := store.CreateAgentKey(ctx, userA.ID, "alice-agent")
	if err != nil {
		t.Fatal(err)
	}
	keyB, _, err := store.CreateAgentKey(ctx, userB.ID, "bob-agent")
	if err != nil {
		t.Fatal(err)
	}
	projA, tokenA, _, err := store.CreateProjectWithToken(ctx, userA.ID, "Alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	projB, tokenB, _, err := store.CreateProjectWithToken(ctx, userB.ID, "Beta", "beta")
	if err != nil {
		t.Fatal(err)
	}
	sessTokA, sessA, err := store.CreateSession(ctx, userA.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sessTokB, sessB, err := store.CreateSession(ctx, userB.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	resA, err := store.Ingest(ctx, tokenA, inbox.Notice{
		Env:       "production",
		Revision:  "rev-a1",
		Class:     "CheckoutError",
		Message:   "100% failed_order\\path",
		Backtrace: []string{"app/controllers/checkout_controller.rb:42:in `create'"},
		Request:   map[string]any{"url": "https://alpha.example.com/checkout"},
		Context:   map[string]any{"order_id": "ord_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resB, err := store.Ingest(ctx, tokenB, inbox.Notice{
		Env:       "production",
		Revision:  "rev-b1",
		Class:     "SecretTenantBError",
		Message:   "bob private secret",
		Backtrace: []string{"app/models/secret.rb:9:in `leak'"},
	})
	if err != nil {
		t.Fatal(err)
	}

	const limit = 64 << 10 // 64 KiB for fast bounded tests
	srv := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), limit, Config{
		BaseURL: "https://raises.example.com",
	})
	return securityFixture{
		store:     store,
		srv:       srv,
		handler:   srv.Handler(),
		userA:     userA,
		userB:     userB,
		keyA:      keyA,
		keyB:      keyB,
		tokenA:    tokenA,
		tokenB:    tokenB,
		projA:     projA,
		projB:     projB,
		cookieA:   &http.Cookie{Name: sessionCookie, Value: sessTokA},
		cookieB:   &http.Cookie{Name: sessionCookie, Value: sessTokB},
		csrfA:     sessA.CSRFToken,
		csrfB:     sessB.CSRFToken,
		groupA:    resA.Group,
		groupB:    resB.Group,
		bodyLimit: limit,
	}
}

func (f securityFixture) do(method, target string, body io.Reader, headers map[string]string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, body)
	req.RemoteAddr = "203.0.113.10:12345"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestHardeningCrossTenantIDORMatrix(t *testing.T) {
	f := newSecurityFixture(t)
	bobID := strconv.FormatInt(f.groupB.ID, 10)
	aliceAuth := map[string]string{"Authorization": "Bearer " + f.keyA}

	// API endpoints: tenant A accessing tenant B's group must get 404 (never 200/403 leaking existence).
	apiCases := []struct {
		method  string
		path    string
		body    string
		headers map[string]string
	}{
		{http.MethodGet, "/v1/errors/" + bobID, "", aliceAuth},
		{http.MethodGet, "/v1/errors/" + bobID, "", map[string]string{"Authorization": "Bearer " + f.keyA, "Accept": "text/markdown"}},
		{http.MethodGet, "/v1/errors/" + bobID + "/context", "", aliceAuth},
		{http.MethodGet, "/v1/errors/" + bobID + "/notices", "", aliceAuth},
		{http.MethodPost, "/v1/errors/" + bobID + "/ack", "", aliceAuth},
		{http.MethodPost, "/v1/errors/" + bobID + "/resolve", "", aliceAuth},
		{http.MethodPost, "/v1/errors/" + bobID + "/reopen", "", aliceAuth},
		{http.MethodPost, "/v1/errors/" + bobID + "/claim", `{"agent":"attacker"}`, aliceAuth},
		{http.MethodPost, "/v1/errors/" + bobID + "/release", `{"agent":"attacker"}`, aliceAuth},
		{http.MethodPost, "/v1/errors/" + bobID + "/suppress", "", aliceAuth},
		{http.MethodPost, "/v1/errors/" + bobID + "/unsuppress", "", aliceAuth},
	}
	for _, tc := range apiCases {
		rec := f.do(tc.method, tc.path, strings.NewReader(tc.body), tc.headers, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s cross-tenant = %d, want 404 (body=%s)", tc.method, tc.path, rec.Code, rec.Body.String())
		}
		// Unauthenticated must be 401.
		unauth := f.do(tc.method, tc.path, strings.NewReader(tc.body), nil, nil)
		if unauth.Code != http.StatusUnauthorized {
			t.Errorf("%s %s unauth = %d, want 401", tc.method, tc.path, unauth.Code)
		}
	}

	// Search/list must never leak tenant B's group.
	rec := f.do(http.MethodGet, "/v1/errors?compact=1&q=SecretTenantB&env=production&sort=count&exclude_claimed=1", nil, aliceAuth, nil)
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "SecretTenantBError") {
		t.Fatalf("search leaked cross-tenant data: %d %s", rec.Code, rec.Body.String())
	}

	// Browser endpoints: tenant A attempting ack/resolve/suppress/detail on tenant B's group -> 404.
	browserGet := f.do(http.MethodGet, "/errors/"+bobID, nil, nil, f.cookieA)
	if browserGet.Code != http.StatusNotFound {
		t.Errorf("GET /errors/%s cross-tenant = %d, want 404", bobID, browserGet.Code)
	}
	for _, action := range []string{"ack", "resolve", "suppress", "unsuppress"} {
		form := url.Values{"csrf": {f.csrfA}}.Encode()
		headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
		rec := f.do(http.MethodPost, "/errors/"+bobID+"/"+action, strings.NewReader(form), headers, f.cookieA)
		if rec.Code != http.StatusNotFound {
			t.Errorf("POST /errors/%s/%s cross-tenant = %d, want 404", bobID, action, rec.Code)
		}
		// Bad CSRF must be 403 before touching store.
		badForm := url.Values{"csrf": {"wrong-csrf"}}.Encode()
		badRec := f.do(http.MethodPost, "/errors/"+bobID+"/"+action, strings.NewReader(badForm), headers, f.cookieA)
		if badRec.Code != http.StatusForbidden {
			t.Errorf("POST /errors/%s/%s bad CSRF = %d, want 403", bobID, action, badRec.Code)
		}
	}

	// Browser POST /dashboard/projects CSRF & unauth checks.
	headers := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	if rec := f.do(http.MethodPost, "/dashboard/projects", strings.NewReader(url.Values{"name": {"Hack"}, "csrf": {"bad"}}.Encode()), headers, f.cookieA); rec.Code != http.StatusForbidden {
		t.Errorf("POST /dashboard/projects bad CSRF = %d, want 403", rec.Code)
	}
	if rec := f.do(http.MethodPost, "/dashboard/projects", strings.NewReader(url.Values{"name": {"Hack"}}.Encode()), headers, nil); rec.Code != http.StatusFound {
		t.Errorf("POST /dashboard/projects unauth = %d, want 302", rec.Code)
	}

	// Sentry envelope path spoofing: Alice's token with Bob's project ID in URL must land ONLY in Alice's project.
	envBody := `{"event_id":"spoof123"}` + "\n" + `{"type":"event"}` + "\n" + `{"event_id":"spoof123","exception":{"values":[{"type":"SpoofedError","value":"test"}]}}`
	rec = f.do(http.MethodPost, "/api/"+f.projB.ID+"/envelope/", strings.NewReader(envBody), map[string]string{
		"X-Sentry-Auth": sentryAuthHeader(f.tokenA),
	}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("sentry envelope spoofed path = %d %s", rec.Code, rec.Body.String())
	}
	bobGroups, err := f.store.ListForUser(context.Background(), f.userB.ID, "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range bobGroups {
		if g.Class == "SpoofedError" {
			t.Fatalf("cross-tenant sentry envelope leaked into Bob's project!")
		}
	}
}

func TestHardeningTypeConfusionAndDeepJSON(t *testing.T) {
	f := newSecurityFixture(t)
	aliceID := strconv.FormatInt(f.groupA.ID, 10)
	aliceAuth := map[string]string{"Authorization": "Bearer " + f.keyA, "Content-Type": "application/json"}
	ingestAuth := map[string]string{"Authorization": "Bearer " + f.tokenA, "Content-Type": "application/json"}

	// POST /v1/bootstrap/exchange type confusion
	for _, body := range []string{
		`{"token": 12345}`,
		`{"token": ["a"]}`,
		`{"token": {"nested": true}}`,
		`[]`,
		`{"token": "x", "unknown_field": 1}`,
		`not json`,
	} {
		rec := f.do(http.MethodPost, "/v1/bootstrap/exchange", strings.NewReader(body), map[string]string{"Content-Type": "application/json"}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bootstrap/exchange body=%s -> %d, want 400", body, rec.Code)
		}
	}
	// null token returns 401 (unauthorized, never 500)
	if rec := f.do(http.MethodPost, "/v1/bootstrap/exchange", strings.NewReader(`{"token": null}`), map[string]string{"Content-Type": "application/json"}, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("bootstrap/exchange null token -> %d, want 401", rec.Code)
	}

	// POST /v1/notices type confusion & invalid custom fingerprints
	for _, body := range []string{
		`null`,
		`[]`,
		`123`,
		`"string"`,
		`{"error": "not-an-object"}`,
		`{"error": {"class": 123}}`,
		`{"error": {"class": "E", "backtrace": "not-an-array"}}`,
		`{"error": {"class": "E"}, "fingerprint": "not-an-array"}`,
		`{"error": {"class": "E"}, "fingerprint": [123]}`,
		`{"error": {"class": "E"}, "fingerprint": []}`,
		`{"error": {"class": "E"}, "fingerprint": ["1","2","3","4","5","6","7","8","9","10","11"]}`,
		`{"error": {"class": "E"}, "fingerprint": ["` + strings.Repeat("a", 257) + `"]}`,
		`{"error": {"class": "E"}, "fingerprint": ["   "]}`,
	} {
		rec := f.do(http.MethodPost, "/v1/notices", strings.NewReader(body), ingestAuth, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("POST /v1/notices type confusion %q -> %d, want 400 (body=%s)", body[:min(len(body), 60)], rec.Code, rec.Body.String())
		}
	}

	// Deeply nested JSON (2000 levels) must return 400 (Go encoding/json max nesting depth = 10000, bodyLimit caps at 64 KiB) without panic.
	deep := strings.Repeat(`{"a":`, 2000) + `1` + strings.Repeat(`}`, 2000)
	rec := f.do(http.MethodPost, "/v1/notices", strings.NewReader(`{"error":{"class":"Deep","message":"x"},"context":`+deep+`}`), ingestAuth, nil)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK && rec.Code != http.StatusBadRequest {
		t.Errorf("deep JSON notice returned unexpected status %d", rec.Code)
	}

	// POST /v1/errors/{id}/claim type confusion & integer overflow on ttl_seconds
	for _, body := range []string{
		`[]`,
		`{"agent": 123}`,
		`{"agent": ["a"]}`,
		`{"ttl_seconds": "60"}`,
		`{"ttl_seconds": -10}`,
		`{"agent": "` + strings.Repeat("a", 81) + `"}`,
		`{"note": "` + strings.Repeat("n", 257) + `"}`,
	} {
		rec := f.do(http.MethodPost, "/v1/errors/"+aliceID+"/claim", strings.NewReader(body), aliceAuth, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("claim body=%q -> %d, want 400 (resp=%s)", body[:min(len(body), 50)], rec.Code, rec.Body.String())
		}
	}

	// Huge ttl_seconds must clamp cleanly to MaxClaimTTL (2h) instead of overflowing int64 time.Duration!
	rec = f.do(http.MethodPost, "/v1/errors/"+aliceID+"/claim", strings.NewReader(`{"agent":"overflow-bot","ttl_seconds":9223372036854775}`), aliceAuth, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim huge ttl_seconds = %d %s, want 200 clamped to MaxClaimTTL", rec.Code, rec.Body.String())
	}
	var claimed inbox.Group
	if err := json.Unmarshal(rec.Body.Bytes(), &claimed); err != nil {
		t.Fatal(err)
	}
	if claimed.Claim == nil {
		t.Fatal("expected active claim")
	}
	dur := claimed.Claim.ExpiresAt.Sub(claimed.Claim.ClaimedAt)
	if dur > inbox.MaxClaimTTL+time.Second || dur < inbox.MaxClaimTTL-time.Second {
		t.Fatalf("huge ttl_seconds produced duration %v, want clamped %v", dur, inbox.MaxClaimTTL)
	}

	// POST /v1/errors/{id}/release type confusion
	for _, body := range []string{`[]`, `{"agent": 42}`} {
		rec := f.do(http.MethodPost, "/v1/errors/"+aliceID+"/release", strings.NewReader(body), aliceAuth, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("release body=%s -> %d, want 400", body, rec.Code)
		}
	}
}

func TestHardeningMalformedPayloadsGzipBombLIKEAndXSS(t *testing.T) {
	f := newSecurityFixture(t)
	aliceAuth := map[string]string{"Authorization": "Bearer " + f.keyA}

	// 1. SQL LIKE wildcard escaping (% _ \) & SQL injection payloads in q/env/sort
	rec := f.do(http.MethodGet, "/v1/errors?compact=1&q="+url.QueryEscape(`100% failed_order\path`), nil, aliceAuth, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("LIKE literal search = %d %s", rec.Code, rec.Body.String())
	}
	var page compactErrorPage
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Groups[0].ID != f.groupA.ID {
		t.Fatalf("LIKE literal search returned total=%d, want 1", page.Total)
	}
	// Plain "%" alone must NOT act as wildcard matching everything if no literal "%" exists in other groups.
	// Searching for "failedXorder" must NOT match "failed_order" (proving "_" is escaped).
	rec = f.do(http.MethodGet, "/v1/errors?compact=1&q="+url.QueryEscape("failed_order"), nil, aliceAuth, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "CheckoutError") {
		t.Fatalf("underscore literal failed: %d %s", rec.Code, rec.Body.String())
	}
	// Null bytes & invalid UTF-8 in query params must not 500.
	for _, rawQ := range []string{"%00null", "%ff%fe", url.QueryEscape("'; DROP TABLE error_groups; --")} {
		r := f.do(http.MethodGet, "/v1/errors?compact=1&q="+rawQ+"&env="+rawQ, nil, aliceAuth, nil)
		if r.Code != http.StatusOK {
			t.Errorf("malformed q=%s -> %d, want 200", rawQ, r.Code)
		}
	}
	// Invalid sort must return 400 (SQL injection attempt blocked by allowlist).
	if r := f.do(http.MethodGet, "/v1/errors?sort="+url.QueryEscape("count; DROP TABLE apps"), nil, aliceAuth, nil); r.Code != http.StatusBadRequest {
		t.Errorf("SQLi sort -> %d, want 400", r.Code)
	}

	// 2. Sentry envelope adversarial matrix:
	// 2a. ?sentry_key= query param rejected 401 even if header is also present.
	if r := f.do(http.MethodPost, "/api/1/envelope/?sentry_key="+f.tokenA, strings.NewReader("{}\n"), map[string]string{"X-Sentry-Auth": sentryAuthHeader(f.tokenA)}, nil); r.Code != http.StatusUnauthorized {
		t.Errorf("?sentry_key= query param -> %d, want 401", r.Code)
	}
	// 2b. Gzip bomb expanding past bodyLimit (64 KiB) -> 413.
	bomb := gzipBytes(t, bytes.Repeat([]byte("A"), int(f.bodyLimit)+4096))
	if int64(len(bomb)) >= f.bodyLimit {
		t.Fatalf("expected compressed gzip bomb to be smaller than bodyLimit")
	}
	if r := f.do(http.MethodPost, "/api/1/envelope/", bytes.NewReader(bomb), map[string]string{
		"X-Sentry-Auth":    sentryAuthHeader(f.tokenA),
		"Content-Encoding": "gzip",
	}, nil); r.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("gzip bomb -> %d, want 413 (body=%s)", r.Code, r.Body.String())
	}
	// 2c. Uncompressed body exceeding bodyLimit -> 413 on both /v1/notices and /api/1/envelope/.
	oversized := bytes.Repeat([]byte(" "), int(f.bodyLimit)+1024)
	if r := f.do(http.MethodPost, "/v1/notices", bytes.NewReader(oversized), map[string]string{"Authorization": "Bearer " + f.tokenA}, nil); r.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized /v1/notices -> %d, want 413", r.Code)
	}
	if r := f.do(http.MethodPost, "/api/1/envelope/", bytes.NewReader(oversized), map[string]string{"X-Sentry-Auth": sentryAuthHeader(f.tokenA)}, nil); r.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized /api/1/envelope/ -> %d, want 413", r.Code)
	}
	// 2d. >10 Sentry items rejected 400.
	var sb strings.Builder
	sb.WriteString(`{"event_id":"many"}` + "\n")
	for i := 0; i < 11; i++ {
		sb.WriteString(`{"type":"event"}` + "\n")
		sb.WriteString(fmt.Sprintf(`{"event_id":"e%d","exception":{"values":[{"type":"TooMany","value":"%d"}]}}`+"\n", i, i))
	}
	if r := f.do(http.MethodPost, "/api/1/envelope/", strings.NewReader(sb.String()), map[string]string{"X-Sentry-Auth": sentryAuthHeader(f.tokenA)}, nil); r.Code != http.StatusBadRequest {
		t.Errorf(">10 sentry items -> %d, want 400", r.Code)
	}
	// 2e. Integer overflow length header in Sentry envelope must return 400 (NEVER panic!).
	intOverflowEnv := `{"event_id":"ovf"}` + "\n" + `{"type":"event","length":9223372036854775807}` + "\n" + `{}`
	if r := f.do(http.MethodPost, "/api/1/envelope/", strings.NewReader(intOverflowEnv), map[string]string{"X-Sentry-Auth": sentryAuthHeader(f.tokenA)}, nil); r.Code != http.StatusBadRequest {
		t.Errorf("sentry int64 length overflow -> %d, want 400", r.Code)
	}

	// 3. XSS / script / textarea-breakout / backtick fence-breakout / 4-byte UTF-8 Markdown budget!
	xssPayload := map[string]any{
		"env":      "production",
		"revision": "rev-xss",
		"error": map[string]any{
			"class":   `</textarea><script>alert("xss-class")</script>`,
			"message": "``````\n</textarea><img src=x onerror=alert(1)>\n" + strings.Repeat("🔥", 600),
			"backtrace": []string{
				`app/controllers/xss.rb:1:in ` + "`</textarea><script>alert(2)</script>`",
			},
		},
		"request": map[string]any{"url": `https://x.test/</pre><script>alert(3)</script>`},
		"context": map[string]any{"note": `</textarea><svg onload=alert(4)>`},
	}
	xssRaw, _ := json.Marshal(xssPayload)
	rec = f.do(http.MethodPost, "/v1/notices", bytes.NewReader(xssRaw), map[string]string{
		"Authorization": "Bearer " + f.tokenA,
		"Content-Type":  "application/json",
	}, nil)
	if rec.Code != http.StatusCreated {
		t.Fatalf("ingest xss notice = %d %s", rec.Code, rec.Body.String())
	}
	var xssRes inbox.IngestResult
	_ = json.Unmarshal(rec.Body.Bytes(), &xssRes)
	xssID := strconv.FormatInt(xssRes.Group.ID, 10)

	// Browser inspector must escape ALL raw <script>, </textarea>, <svg, <img tags!
	htmlRec := f.do(http.MethodGet, "/errors/"+xssID, nil, nil, f.cookieA)
	if htmlRec.Code != http.StatusOK {
		t.Fatalf("GET /errors/%s = %d", xssID, htmlRec.Code)
	}
	htmlBody := htmlRec.Body.String()
	for _, forbidden := range []string{"<script>alert", "<img src=x", "<svg onload=", "</textarea><script>", "</pre><script>"} {
		if strings.Contains(htmlBody, forbidden) {
			t.Fatalf("HTML inspector contained unescaped XSS payload %q", forbidden)
		}
	}

	// Accept: text/markdown must enforce <= 4800 bytes EVEN with 600 4-byte UTF-8 runes AND 6 backticks!
	mdRec := f.do(http.MethodGet, "/v1/errors/"+xssID, nil, map[string]string{
		"Authorization": "Bearer " + f.keyA,
		"Accept":        "text/markdown",
	}, nil)
	if mdRec.Code != http.StatusOK {
		t.Fatalf("GET /v1/errors/%s markdown = %d", xssID, mdRec.Code)
	}
	if mdRec.Body.Len() > maxMarkdownBytes {
		t.Fatalf("markdown packet exceeded %d bytes with 4-byte UTF-8 input: got %d bytes", maxMarkdownBytes, mdRec.Body.Len())
	}
	// Verify dynamic fence > 6 backticks was used around the 6-backtick message.
	if !strings.Contains(mdRec.Body.String(), "```````") {
		t.Fatalf("expected >=7-backtick fence around 6-backtick message; got:\n%s", mdRec.Body.String())
	}
}

func TestHardeningIdempotencyMatrix(t *testing.T) {
	f := newSecurityFixture(t)
	aliceID := strconv.FormatInt(f.groupA.ID, 10)
	aliceAuth := map[string]string{"Authorization": "Bearer " + f.keyA, "Content-Type": "application/json"}

	// 1. Repeated ack -> idempotent 200 acknowledged
	for i := 0; i < 2; i++ {
		rec := f.do(http.MethodPost, "/v1/errors/"+aliceID+"/ack", nil, aliceAuth, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("ack #%d = %d", i, rec.Code)
		}
		var g inbox.Group
		_ = json.Unmarshal(rec.Body.Bytes(), &g)
		if g.Status != inbox.StatusAcknowledged {
			t.Fatalf("ack #%d status = %q", i, g.Status)
		}
	}

	// 2. Repeated resolve -> idempotent 200 fixed
	for i := 0; i < 2; i++ {
		rec := f.do(http.MethodPost, "/v1/errors/"+aliceID+"/resolve", nil, aliceAuth, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("resolve #%d = %d", i, rec.Code)
		}
		var g inbox.Group
		_ = json.Unmarshal(rec.Body.Bytes(), &g)
		if g.Status != inbox.StatusFixed {
			t.Fatalf("resolve #%d status = %q", i, g.Status)
		}
	}

	// 3. Repeated reopen -> idempotent 200 open
	for i := 0; i < 2; i++ {
		rec := f.do(http.MethodPost, "/v1/errors/"+aliceID+"/reopen", nil, aliceAuth, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("reopen #%d = %d", i, rec.Code)
		}
		var g inbox.Group
		_ = json.Unmarshal(rec.Body.Bytes(), &g)
		if g.Status != inbox.StatusOpen {
			t.Fatalf("reopen #%d status = %q", i, g.Status)
		}
	}

	// 4. Same-agent claim renewal succeeds (200), other-agent claim conflicts (409), repeated release is idempotent (200)
	if r := f.do(http.MethodPost, "/v1/errors/"+aliceID+"/claim", strings.NewReader(`{"agent":"agent-1","ttl_seconds":600}`), aliceAuth, nil); r.Code != http.StatusOK {
		t.Fatalf("claim 1 = %d", r.Code)
	}
	if r := f.do(http.MethodPost, "/v1/errors/"+aliceID+"/claim", strings.NewReader(`{"agent":"agent-1","ttl_seconds":900,"note":"renewed"}`), aliceAuth, nil); r.Code != http.StatusOK {
		t.Fatalf("same-agent claim renewal = %d", r.Code)
	}
	if r := f.do(http.MethodPost, "/v1/errors/"+aliceID+"/claim", strings.NewReader(`{"agent":"agent-2"}`), aliceAuth, nil); r.Code != http.StatusConflict {
		t.Fatalf("other-agent claim = %d, want 409", r.Code)
	}
	// Other agent release must conflict (409).
	if r := f.do(http.MethodPost, "/v1/errors/"+aliceID+"/release", strings.NewReader(`{"agent":"agent-2"}`), aliceAuth, nil); r.Code != http.StatusConflict {
		t.Fatalf("other-agent release = %d, want 409", r.Code)
	}
	// Holder release twice -> both 200 (idempotent!).
	for i := 0; i < 2; i++ {
		if r := f.do(http.MethodPost, "/v1/errors/"+aliceID+"/release", strings.NewReader(`{"agent":"agent-1"}`), aliceAuth, nil); r.Code != http.StatusOK {
			t.Fatalf("holder release #%d = %d, want 200", i, r.Code)
		}
	}

	// 5. Duplicate POST /v1/notices retry merges cleanly into same group with 201 then 200.
	noticeBody := `{"env":"staging","revision":"r1","error":{"class":"DupError","message":"same","backtrace":["app/jobs/sync.rb:12:in 'perform'"]}}`
	ingestHdr := map[string]string{"Authorization": "Bearer " + f.tokenA, "Content-Type": "application/json"}
	r1 := f.do(http.MethodPost, "/v1/notices", strings.NewReader(noticeBody), ingestHdr, nil)
	r2 := f.do(http.MethodPost, "/v1/notices", strings.NewReader(noticeBody), ingestHdr, nil)
	if r1.Code != http.StatusCreated || r2.Code != http.StatusOK {
		t.Fatalf("duplicate notice codes = %d, %d; want 201, 200", r1.Code, r2.Code)
	}
	var res1, res2 inbox.IngestResult
	_ = json.Unmarshal(r1.Body.Bytes(), &res1)
	_ = json.Unmarshal(r2.Body.Bytes(), &res2)
	if res1.Group.ID != res2.Group.ID || res2.Group.Count != 2 || res2.Created || res2.Regressed {
		t.Fatalf("duplicate retry mismatch: res1=%+v res2=%+v", res1, res2)
	}
}
