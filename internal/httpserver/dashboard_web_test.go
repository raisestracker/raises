package httpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

type dashFixture struct {
	handler http.Handler
	store   *inbox.Store
	cookie  *http.Cookie
	agent   string
}

func newDashFixture(t *testing.T) dashFixture {
	t.Helper()
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	user, _ := store.UpsertGitHubUser(ctx, 9001, "demo", "", "")
	if err := store.UpsertApp(ctx, "widget", "token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	if err := store.AssignLegacyProjects(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	sessionToken, _, err := store.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	agent, _, err := store.CreateAgentKey(ctx, user.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{GitHubWebhookSecret: "whsec"}).Handler()
	return dashFixture{handler: handler, store: store, cookie: &http.Cookie{Name: sessionCookie, Value: sessionToken}, agent: agent}
}

func (f dashFixture) get(path string, headers ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if f.cookie != nil {
		req.AddCookie(f.cookie)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f dashFixture) ingest(t *testing.T, n int) []inbox.Group {
	t.Helper()
	var groups []inbox.Group
	for i := 0; i < n; i++ {
		res, err := f.store.Ingest(context.Background(), "token", inbox.Notice{Class: fmt.Sprintf("Err%02d", i), Message: "boom", Backtrace: []string{fmt.Sprintf("app/x%d.rb:1", i)}})
		if err != nil {
			t.Fatal(err)
		}
		groups = append(groups, res.Group)
	}
	return groups
}

func TestRootRedirectsSignedInToDashboard(t *testing.T) {
	f := newDashFixture(t)
	rec := f.get("/")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/dashboard" {
		t.Fatalf("signed-in /=%d %q", rec.Code, rec.Header().Get("Location"))
	}
	md := f.get("/", "Accept", "text/markdown")
	if md.Code != http.StatusOK || !strings.HasPrefix(md.Header().Get("Content-Type"), "text/markdown") {
		t.Fatalf("markdown / = %d %q", md.Code, md.Header().Get("Content-Type"))
	}
	f.cookie = nil
	out := f.get("/")
	if out.Code != http.StatusOK || !strings.Contains(out.Body.String(), "<html") {
		t.Fatalf("signed-out /=%d", out.Code)
	}
	for _, path := range []string{"/dashboard", "/settings", "/projects/widget", "/errors"} {
		if rec := f.get(path); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/github" {
			t.Fatalf("signed-out %s=%d %q", path, rec.Code, rec.Header().Get("Location"))
		}
	}
}

func TestDashboardShowsKPIsAndProjects(t *testing.T) {
	f := newDashFixture(t)
	f.ingest(t, 3)
	rec := f.get("/dashboard")
	body := rec.Body.String()
	for _, want := range []string{"open errors", "new today", "events / 24h", "fixed / 7d", "<polyline", `href="/projects/widget"`, `aria-current="page">Dashboard`, "3 open", "<details"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}
	stats, err := f.store.DashboardForUser(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Open != 3 || stats.NewToday != 3 || stats.Events24h != 3 || stats.Fixed7d != 0 {
		t.Fatalf("stats=%+v", stats)
	}
	if stats.OpenSpark[inbox.SparkDays-1] != 3 {
		t.Fatalf("open spark=%v", stats.OpenSpark)
	}
}

func TestDashboardExcludesArchivedProjects(t *testing.T) {
	f := newDashFixture(t)
	f.ingest(t, 2)
	ctx := context.Background()
	project, err := f.store.ProjectBySlug(ctx, 1, "widget")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.SetProjectArchived(ctx, 1, project.ID, true); err != nil {
		t.Fatal(err)
	}
	stats, err := f.store.DashboardForUser(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Open != 0 || stats.Events24h != 0 || stats.EventsSpark[inbox.SparkDays-1] != 0 {
		t.Fatalf("archived project leaked into stats: %+v", stats)
	}
}

func TestProjectPageStatusFilterAndPagination(t *testing.T) {
	f := newDashFixture(t)
	groups := f.ingest(t, 30)
	ctx := context.Background()
	if _, err := f.store.Ack(ctx, groups[0].ID); err != nil {
		t.Fatal(err)
	}
	if n, err := f.store.ResolveByIssue(ctx, "example/widget", 0); err != nil || n != 0 {
		t.Fatalf("resolve zero issue n=%d err=%v", n, err)
	}

	page1 := f.get("/projects/widget").Body.String()
	if !strings.Contains(page1, "Page 1 of 2") || !strings.Contains(page1, `href="/projects/widget?page=2"`) || strings.Count(page1, `href="/errors/`) != 25 {
		t.Fatalf("page1 wrong: rows=%d", strings.Count(page1, `href="/errors/`))
	}
	page2 := f.get("/projects/widget?page=2").Body.String()
	if strings.Count(page2, `href="/errors/`) != 4 || !strings.Contains(page2, "Page 2 of 2") { // 29 open, 25 + 4
		t.Fatalf("page2 rows=%d", strings.Count(page2, `href="/errors/`))
	}
	ack := f.get("/projects/widget?status=acknowledged").Body.String()
	if strings.Count(ack, `href="/errors/`) != 1 || !strings.Contains(ack, "Acknowledged") {
		t.Fatalf("ack filter rows=%d", strings.Count(ack, `href="/errors/`))
	}
	fixed := f.get("/projects/widget?status=fixed").Body.String()
	if strings.Contains(fixed, `href="/errors/`) || !strings.Contains(fixed, "Nothing marked fixed in widget yet") {
		t.Fatal("fixed filter should show the empty state")
	}
	all := f.get("/projects/widget?status=all&page=2").Body.String()
	if strings.Count(all, `href="/errors/`) != 5 {
		t.Fatalf("all page2 rows=%d", strings.Count(all, `href="/errors/`))
	}
	if rec := f.get("/projects/nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown project=%d", rec.Code)
	}
	if rec := f.get("/projects/widget?page=999"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Page 2 of 2") {
		t.Fatalf("out of range page=%d", rec.Code)
	}
}

func signedIssueEvent(t *testing.T, f dashFixture, action string, number int) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"action": action, "issue": map[string]any{"number": number}, "repository": map[string]any{"full_name": "example/widget"}})
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body))
	req.Header.Set("X-GitHub-Event", "issues")
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec.Code
}

func TestIssueClosedWebhookMarksErrorFixed(t *testing.T) {
	f := newDashFixture(t)
	ctx := context.Background()
	groups := f.ingest(t, 2)
	if _, err := f.store.Ingest(ctx, "token", inbox.Notice{Class: "Err00", Message: "boom", Backtrace: []string{"app/x0.rb:1"}}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetIssueForTest(ctx, groups[0].ID, 7); err != nil {
		t.Fatal(err)
	}
	if code := signedIssueEvent(t, f, "closed", 7); code != http.StatusAccepted {
		t.Fatalf("webhook=%d", code)
	}
	got, _ := f.store.Get(ctx, groups[0].ID)
	if got.Status != inbox.StatusFixed || got.ResolvedAt == nil || got.AckedAt == nil {
		t.Fatalf("after close: %+v", got)
	}
	other, _ := f.store.Get(ctx, groups[1].ID)
	if other.Status != inbox.StatusOpen {
		t.Fatalf("unrelated group changed: %s", other.Status)
	}
	if fixed := f.get("/projects/widget?status=fixed").Body.String(); strings.Count(fixed, `href="/errors/`) != 1 {
		t.Fatal("fixed filter should list the closed issue's error")
	}
	if code := signedIssueEvent(t, f, "reopened", 7); code != http.StatusAccepted {
		t.Fatalf("reopen webhook=%d", code)
	}
	got, _ = f.store.Get(ctx, groups[0].ID)
	if got.ResolvedAt != nil || got.Status != inbox.StatusAcknowledged {
		t.Fatalf("after reopen: %+v", got)
	}
}

func TestAPIErrorsStatusAndPaging(t *testing.T) {
	f := newDashFixture(t)
	groups := f.ingest(t, 27)
	if _, err := f.store.Ack(context.Background(), groups[0].ID); err != nil {
		t.Fatal(err)
	}
	call := func(query string) (int, inbox.ErrorPage) {
		req := httptest.NewRequest(http.MethodGet, "/v1/errors?"+query, nil)
		req.Header.Set("Authorization", "Bearer "+f.agent)
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		var page inbox.ErrorPage
		_ = json.Unmarshal(rec.Body.Bytes(), &page)
		return rec.Code, page
	}
	code, page := call("status=open")
	if code != 200 || page.Total != 26 || len(page.Groups) != 25 || page.Pages != 2 || page.Groups[0].Status != "open" {
		t.Fatalf("open: %d %+v", code, page)
	}
	if _, p2 := call("status=open&page=2"); len(p2.Groups) != 1 {
		t.Fatalf("open page 2: %d", len(p2.Groups))
	}
	if _, ack := call("status=acknowledged"); ack.Total != 1 {
		t.Fatalf("ack total %d", ack.Total)
	}
	if code, _ := call("status=bogus"); code != http.StatusBadRequest {
		t.Fatalf("bogus status=%d", code)
	}
	// The legacy shape is unchanged without the new parameters.
	req := httptest.NewRequest(http.MethodGet, "/v1/errors?unacked=1", nil)
	req.Header.Set("Authorization", "Bearer "+f.agent)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	var legacy []inbox.Group
	if err := json.Unmarshal(rec.Body.Bytes(), &legacy); err != nil || len(legacy) != 26 {
		t.Fatalf("legacy list err=%v n=%d", err, len(legacy))
	}
}
