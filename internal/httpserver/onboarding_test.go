package httpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

func newOnboardingFixture(t *testing.T) dashFixture {
	t.Helper()
	f := newDashFixture(t)
	ctx := context.Background()
	user, err := f.store.UpsertGitHubUser(ctx, 9100, "fresh", "", "")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := f.store.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.cookie = &http.Cookie{Name: sessionCookie, Value: token}
	return f
}

var ingestTokenPattern = regexp.MustCompile(`raises_ingest_[A-Za-z0-9_-]+`)

func TestDashboardSelfServeOnboardingRevealsTokenOnce(t *testing.T) {
	f := newOnboardingFixture(t)
	empty := f.get("/dashboard").Body.String()
	for _, want := range []string{`action="/dashboard/projects"`, `id="new-project" open`, "Create your first project", `name="name"`, `name="slug"`, `href="/errors"`} {
		if !strings.Contains(empty, want) {
			t.Fatalf("fresh dashboard missing %q", want)
		}
	}
	if ingestTokenPattern.MatchString(empty) {
		t.Fatal("token on dashboard before creation")
	}

	rec := f.postForm("/dashboard/projects", url.Values{"csrf": {f.csrf(t)}, "name": {"Solo App"}, "slug": {"solo-app"}})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Location") != "" {
		t.Fatalf("reveal headers: cache=%q location=%q", rec.Header().Get("Cache-Control"), rec.Header().Get("Location"))
	}
	body := rec.Body.String()
	token := ingestTokenPattern.FindString(body)
	if token == "" {
		t.Fatal("token not revealed")
	}
	for _, want := range []string{"Solo App is ready", "curl -sS -X POST", "/v1/notices", "bundle add raises", "go get github.com/raisestracker/raises-go", "fetch(", `data-copy-target="new-project-token"`, `href="/projects/solo-app"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("reveal missing %q", want)
		}
	}

	// The token authenticates ingestion immediately.
	notice := httptest.NewRequest(http.MethodPost, "/v1/notices", strings.NewReader(`{"env":"production","error":{"class":"RaisesTestError","message":"hi","backtrace":["app/hello.rb:1"]}}`))
	notice.Header.Set("Authorization", "Bearer "+token)
	notice.Header.Set("Content-Type", "application/json")
	nrec := httptest.NewRecorder()
	f.handler.ServeHTTP(nrec, notice)
	if nrec.Code != http.StatusCreated {
		t.Fatalf("ingest with revealed token=%d %s", nrec.Code, nrec.Body.String())
	}

	// Never again: reload shows the project but not the secret.
	after := f.get("/dashboard").Body.String()
	if strings.Contains(after, token) || ingestTokenPattern.MatchString(after) {
		t.Fatal("token leaked after reload")
	}
	if !strings.Contains(after, `href="/projects/solo-app"`) || strings.Contains(after, `id="new-project" open`) {
		t.Fatal("after creation the project is listed and the form folds away")
	}
}

func TestDashboardOnboardingRejectsBadInputCSRFAndQuota(t *testing.T) {
	f := newOnboardingFixture(t)
	csrf := f.csrf(t)
	if rec := f.postForm("/dashboard/projects", url.Values{"csrf": {"forged"}, "name": {"X"}}); rec.Code != http.StatusForbidden {
		t.Fatalf("forged csrf=%d", rec.Code)
	}
	bad := f.postForm("/dashboard/projects", url.Values{"csrf": {csrf}, "name": {"<b>Shop</b>"}, "slug": {"Not A Slug"}})
	if bad.Code != http.StatusBadRequest || !strings.Contains(bad.Body.String(), "Slugs use lowercase") || !strings.Contains(bad.Body.String(), "&lt;b&gt;Shop&lt;/b&gt;") {
		t.Fatalf("bad input=%d", bad.Code)
	}
	if rec := f.postForm("/dashboard/projects", url.Values{"csrf": {csrf}, "name": {"Dup Base"}}); rec.Code != http.StatusCreated {
		t.Fatalf("first create=%d", rec.Code)
	}
	if dup := f.postForm("/dashboard/projects", url.Values{"csrf": {csrf}, "name": {"Another"}, "slug": {"dup-base"}}); dup.Code != http.StatusConflict || !strings.Contains(dup.Body.String(), "already have a project with that slug") {
		t.Fatalf("duplicate=%d", dup.Code)
	}
	for i := 1; i < inbox.MaxActiveProjects; i++ {
		if rec := f.postForm("/dashboard/projects", url.Values{"csrf": {csrf}, "name": {"Project " + string(rune('A'+i))}}); rec.Code != http.StatusCreated {
			t.Fatalf("create %d=%d", i, rec.Code)
		}
	}
	over := f.postForm("/dashboard/projects", url.Values{"csrf": {csrf}, "name": {"One too many"}})
	if over.Code != http.StatusUnprocessableEntity || !strings.Contains(over.Body.String(), "Project limit reached") || ingestTokenPattern.MatchString(over.Body.String()) {
		t.Fatalf("over quota=%d", over.Code)
	}
}

func TestErrorsNavLinkMarksActivePage(t *testing.T) {
	f := newDashFixture(t)
	if body := f.get("/errors").Body.String(); !strings.Contains(body, `href="/errors" aria-current="page">Errors`) {
		t.Fatal("/errors must mark the Errors nav link current")
	}
	if body := f.get("/dashboard").Body.String(); !strings.Contains(body, `href="/errors">Errors`) {
		t.Fatal("dashboard nav must link Errors")
	}
	// The existing JSON API stays untouched.
	req := httptest.NewRequest(http.MethodGet, "/v1/projects", nil)
	req.Header.Set("Authorization", "Bearer "+f.agent)
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	var projects []inbox.Project
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &projects) != nil {
		t.Fatalf("/v1/projects=%d", rec.Code)
	}
}
