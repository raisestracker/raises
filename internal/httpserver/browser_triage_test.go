package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func (f dashFixture) postForm(path string, form url.Values) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if f.cookie != nil {
		req.AddCookie(f.cookie)
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func (f dashFixture) csrf(t *testing.T) string {
	t.Helper()
	session, err := f.store.SessionByToken(context.Background(), f.cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	return session.CSRFToken
}

func TestBrowserAckThenResolveWithoutJS(t *testing.T) {
	f := newDashFixture(t)
	g := f.ingest(t, 1)[0]
	id := strconv.FormatInt(g.ID, 10)
	page := f.get("/errors/" + id).Body.String()
	for _, want := range []string{`action="/errors/` + id + `/ack"`, `action="/errors/` + id + `/resolve"`, ">Acknowledge<", ">Mark resolved<"} {
		if !strings.Contains(page, want) {
			t.Fatalf("open error page missing %q", want)
		}
	}
	csrf := f.csrf(t)

	ack := f.postForm("/errors/"+id+"/ack", url.Values{"csrf": {csrf}})
	if ack.Code != http.StatusSeeOther || ack.Header().Get("Location") != "/errors/"+id {
		t.Fatalf("ack=%d %q", ack.Code, ack.Header().Get("Location"))
	}
	got, err := f.store.GetForUser(context.Background(), 1, g.ID)
	if err != nil || got.AckedAt == nil || got.Status != "acknowledged" {
		t.Fatalf("after ack: %+v err=%v", got, err)
	}
	page = f.get("/errors/" + id).Body.String()
	if strings.Contains(page, `/ack"`) || !strings.Contains(page, `/resolve"`) {
		t.Fatal("acknowledged error must offer resolve and drop acknowledge")
	}

	res := f.postForm("/errors/"+id+"/resolve", url.Values{"csrf": {csrf}})
	if res.Code != http.StatusSeeOther {
		t.Fatalf("resolve=%d", res.Code)
	}
	got, _ = f.store.GetForUser(context.Background(), 1, g.ID)
	if got.ResolvedAt == nil || got.Status != "fixed" {
		t.Fatalf("after resolve: %+v", got)
	}
	page = f.get("/errors/" + id).Body.String()
	if strings.Contains(page, `/resolve"`) || strings.Contains(page, `/ack"`) {
		t.Fatal("fixed error must not offer ack/resolve")
	}
}

func TestBrowserTriageRejectsBadCSRFAndCrossTenant(t *testing.T) {
	f := newDashFixture(t)
	g := f.ingest(t, 1)[0]
	id := strconv.FormatInt(g.ID, 10)
	for _, action := range []string{"ack", "resolve"} {
		for _, form := range []url.Values{{"csrf": {"forged"}}, {}} {
			if rec := f.postForm("/errors/"+id+"/"+action, form); rec.Code != http.StatusForbidden {
				t.Fatalf("%s with csrf=%v status=%d", action, form, rec.Code)
			}
		}
	}
	if got, _ := f.store.GetForUser(context.Background(), 1, g.ID); got.AckedAt != nil || got.ResolvedAt != nil {
		t.Fatalf("forged CSRF mutated state: %+v", got)
	}

	ctx := context.Background()
	other, _ := f.store.UpsertGitHubUser(ctx, 9003, "mallory", "", "")
	token, session, err := f.store.CreateSession(ctx, other.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	mallory := f
	mallory.cookie = &http.Cookie{Name: sessionCookie, Value: token}
	for _, action := range []string{"ack", "resolve"} {
		if rec := mallory.postForm("/errors/"+id+"/"+action, url.Values{"csrf": {session.CSRFToken}}); rec.Code != http.StatusNotFound {
			t.Fatalf("cross-tenant %s=%d", action, rec.Code)
		}
		if rec := mallory.postForm("/errors/999999/"+action, url.Values{"csrf": {session.CSRFToken}}); rec.Code != http.StatusNotFound {
			t.Fatalf("missing %s=%d", action, rec.Code)
		}
	}
	if got, _ := f.store.GetForUser(ctx, 1, g.ID); got.AckedAt != nil || got.ResolvedAt != nil {
		t.Fatalf("cross-tenant mutated state: %+v", got)
	}

	f.cookie = nil
	if rec := f.postForm("/errors/"+id+"/ack", url.Values{"csrf": {"x"}}); rec.Code != http.StatusFound || rec.Header().Get("Location") != "/auth/github" {
		t.Fatalf("signed-out ack=%d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestBrowserTriageShowsOpenGitHubIssueNote(t *testing.T) {
	f := newDashFixture(t)
	g := f.ingest(t, 1)[0]
	if err := f.store.SetIssueForTest(context.Background(), g.ID, 42); err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(g.ID, 10)
	if page := f.get("/errors/" + id).Body.String(); !strings.Contains(page, "GitHub issue #42 is still open") {
		t.Fatal("GitHub-linked error must warn the issue stays open")
	}
	f.postForm("/errors/"+id+"/resolve", url.Values{"csrf": {f.csrf(t)}})
	if page := f.get("/errors/" + id).Body.String(); strings.Contains(page, "is still open") {
		t.Fatal("resolved error must drop the open-issue note")
	}
}
