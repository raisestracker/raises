package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
	"github.com/raisestracker/raises/internal/secretbox"
)

type captureOperationalReporter struct {
	key     string
	title   string
	details string
}

func (c *captureOperationalReporter) Report(_ context.Context, key, title, details string) error {
	c.key, c.title, c.details = key, title, details
	return nil
}

func TestPanicRecoveryReportsAndReturns500(t *testing.T) {
	reporter := &captureOperationalReporter{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := recoverPanics(logger, reporter, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("watcher broke")
	}))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/boom", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d", recorder.Code)
	}
	if reporter.key != "http-panic:GET:/boom" || reporter.title != "HTTP panic" || !strings.Contains(reporter.details, "watcher broke") {
		t.Fatalf("reporter=%#v", reporter)
	}
}

func TestCreateAndFetchError(t *testing.T) {
	handler := testHandler(t)
	body := `{
		"env":"production",
		"revision":"abc",
		"error":{"class":"NoMethodError","message":"boom","backtrace":["app/models/user.rb:9:in call"]}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/notices", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer app-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	list := httptest.NewRequest(http.MethodGet, "/v1/errors?app=widget&unacked=1", nil)
	list.Header.Set("Authorization", "Bearer agent-token")
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, list)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d body = %s", listRec.Code, listRec.Body.String())
	}
	var groups []inbox.Group
	if err := json.Unmarshal(listRec.Body.Bytes(), &groups); err != nil || len(groups) != 1 {
		t.Fatalf("groups = %s err=%v", listRec.Body.String(), err)
	}

	ack := httptest.NewRequest(http.MethodPost, "/v1/errors/1/ack", nil)
	ack.Header.Set("Authorization", "Bearer agent-token")
	ackRec := httptest.NewRecorder()
	handler.ServeHTTP(ackRec, ack)
	if ackRec.Code != http.StatusOK {
		t.Fatalf("ack status = %d", ackRec.Code)
	}
}

func TestSuppressAndUnsuppressErrorAPI(t *testing.T) {
	handler := testHandler(t)
	body := `{
		"env":"production",
		"revision":"abc",
		"error":{"class":"RuntimeError","message":"boom","backtrace":["app/models/user.rb:9:in call"]}
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/notices", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer app-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}

	suppress := httptest.NewRequest(http.MethodPost, "/v1/errors/1/suppress", nil)
	suppress.Header.Set("Authorization", "Bearer agent-token")
	suppressRec := httptest.NewRecorder()
	handler.ServeHTTP(suppressRec, suppress)
	if suppressRec.Code != http.StatusOK {
		t.Fatalf("suppress status = %d", suppressRec.Code)
	}
	var suppressed inbox.Group
	if err := json.Unmarshal(suppressRec.Body.Bytes(), &suppressed); err != nil || suppressed.SuppressedAt == nil {
		t.Fatalf("suppressed=%s err=%v", suppressRec.Body.String(), err)
	}

	unsuppress := httptest.NewRequest(http.MethodPost, "/v1/errors/1/unsuppress", nil)
	unsuppress.Header.Set("Authorization", "Bearer agent-token")
	unsuppressRec := httptest.NewRecorder()
	handler.ServeHTTP(unsuppressRec, unsuppress)
	if unsuppressRec.Code != http.StatusOK {
		t.Fatalf("unsuppress status = %d", unsuppressRec.Code)
	}
	var restored inbox.Group
	if err := json.Unmarshal(unsuppressRec.Body.Bytes(), &restored); err != nil || restored.SuppressedAt != nil {
		t.Fatalf("restored=%s err=%v", unsuppressRec.Body.String(), err)
	}
}

func TestHomepage(t *testing.T) {
	handler := testHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !bytes.Contains(rec.Body.Bytes(), []byte("raises")) {
		t.Fatalf("home = %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "G-16MS9N28TE") {
		t.Fatal("homepage is missing Google tag")
	}
	for _, want := range []string{"https://github.com/raisestracker/raises", "https://github.com/raisestracker/raises/blob/master/LICENSE", "AGPL-3.0"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("homepage is missing footer link %q", want)
		}
	}
	stylePath := regexp.MustCompile(`/assets/style-[a-f0-9]+\.css`).FindString(rec.Body.String())
	if stylePath == "" {
		t.Fatalf("homepage is missing a fingerprinted stylesheet: %s", rec.Body.String())
	}
	styleReq := httptest.NewRequest(http.MethodGet, stylePath, nil)
	styleRec := httptest.NewRecorder()
	handler.ServeHTTP(styleRec, styleReq)
	if styleRec.Code != http.StatusOK || !strings.Contains(styleRec.Header().Get("Cache-Control"), "immutable") {
		t.Fatalf("stylesheet = %d cache=%q", styleRec.Code, styleRec.Header().Get("Cache-Control"))
	}
}

func TestAgentDocumentation(t *testing.T) {
	handler := testHandler(t)

	for _, test := range []struct {
		path string
		want []string
	}{
		{path: "/llms.txt", want: []string{"https://raises.dev/migration.md", "Honeybadger", "Rollbar", "Sentry"}},
		{path: "/migration.md", want: []string{"Migrate a Rails app to Raises", "Rails.error.report", "Optional elevated verification", "controlled production canary", "RAISES_TOKEN"}},
	} {
		req := httptest.NewRequest(http.MethodGet, test.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status = %d body = %s", test.path, rec.Code, rec.Body.String())
		}
		for _, want := range test.want {
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("%s is missing %q", test.path, want)
			}
		}
	}
}

func TestRejectsBadAgentToken(t *testing.T) {
	handler := testHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/errors", nil)
	req.Header.Set("Authorization", "Bearer nope")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestBootstrapProjectAndIngestContract(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.UpsertGitHubUser(context.Background(), 99, "agent-user", "", "")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := store.CreateBootstrapToken(context.Background(), user.ID, "Codex", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{}).Handler()

	exchangeBody, _ := json.Marshal(map[string]string{"token": bootstrap})
	exchange := httptest.NewRequest(http.MethodPost, "/v1/bootstrap/exchange", bytes.NewReader(exchangeBody))
	exchangeRec := httptest.NewRecorder()
	handler.ServeHTTP(exchangeRec, exchange)
	if exchangeRec.Code != http.StatusCreated {
		t.Fatalf("exchange=%d %s", exchangeRec.Code, exchangeRec.Body.String())
	}
	var exchanged struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(exchangeRec.Body.Bytes(), &exchanged); err != nil || exchanged.Token == "" {
		t.Fatalf("exchange body=%s err=%v", exchangeRec.Body.String(), err)
	}

	projectReq := httptest.NewRequest(http.MethodPost, "/v1/projects", bytes.NewBufferString(`{"name":"Widget","slug":"widget"}`))
	projectReq.Header.Set("Authorization", "Bearer "+exchanged.Token)
	projectRec := httptest.NewRecorder()
	handler.ServeHTTP(projectRec, projectReq)
	if projectRec.Code != http.StatusCreated {
		t.Fatalf("project=%d %s", projectRec.Code, projectRec.Body.String())
	}
	var project inbox.Project
	_ = json.Unmarshal(projectRec.Body.Bytes(), &project)

	tokenReq := httptest.NewRequest(http.MethodPost, "/v1/projects/"+project.ID+"/ingestion-tokens", nil)
	tokenReq.Header.Set("Authorization", "Bearer "+exchanged.Token)
	tokenRec := httptest.NewRecorder()
	handler.ServeHTTP(tokenRec, tokenReq)
	if tokenRec.Code != http.StatusCreated {
		t.Fatalf("token=%d %s", tokenRec.Code, tokenRec.Body.String())
	}
	var tokenBody struct {
		Token string `json:"token"`
	}
	_ = json.Unmarshal(tokenRec.Body.Bytes(), &tokenBody)

	notice := httptest.NewRequest(http.MethodPost, "/v1/notices", bytes.NewBufferString(`{"error":{"class":"RuntimeError","message":"boom","backtrace":["app/jobs/widget.rb:3"]}}`))
	notice.Header.Set("Authorization", "Bearer "+tokenBody.Token)
	noticeRec := httptest.NewRecorder()
	handler.ServeHTTP(noticeRec, notice)
	if noticeRec.Code != http.StatusCreated {
		t.Fatalf("notice=%d %s", noticeRec.Code, noticeRec.Body.String())
	}

	list := httptest.NewRequest(http.MethodGet, "/v1/errors?app=widget&unacked=1", nil)
	list.Header.Set("Authorization", "Bearer "+exchanged.Token)
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, list)
	if listRec.Code != http.StatusOK || !bytes.Contains(listRec.Body.Bytes(), []byte(`"app":"widget"`)) {
		t.Fatalf("list=%d %s", listRec.Code, listRec.Body.String())
	}
}

func TestLoggedInSettingsIsAuthorizationConsole(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, _ := store.UpsertGitHubUser(context.Background(), 6334, "demo", "", "")
	err = store.UpsertInstallation(context.Background(), user.ID, 123, "demo", "User", "selected", "active", []inbox.GitHubRepository{{ID: 456, FullName: "example/widget"}})
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.CreateSession(context.Background(), user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{GitHubAppSlug: "raises"}).Handler()
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("settings=%d %s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"Bootstrap an agent", "Agent keys", "GitHub repository access", "example/widget", "Add or remove repositories", "https://github.com/settings/installations/123", "Issue delivery", "Webhook delivery", "Healthy", "https://github.com/raisestracker/raises", "https://github.com/raisestracker/raises/blob/master/LICENSE", "AGPL-3.0"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("missing %q", want)
		}
	}
	if strings.Contains(rec.Body.String(), "Create project") {
		t.Fatal("settings should not contain project CRUD")
	}
	if strings.Contains(rec.Body.String(), "G-16MS9N28TE") {
		t.Fatal("authenticated settings should not contain Google tag")
	}
}

func TestSettingsLinksRecentErrorsAndShowsSuppressedState(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, _ := store.UpsertGitHubUser(ctx, 6401, "demo", "", "")
	if err := store.UpsertApp(ctx, "widget", "token", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AssignLegacyProjects(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	result, err := store.Ingest(ctx, "token", inbox.Notice{Class: "RuntimeError", Message: "boom", Backtrace: []string{"app/jobs/x.rb:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Suppress(ctx, result.Group.ID); err != nil {
		t.Fatal(err)
	}
	sessionToken, session, err := store.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{}).Handler()

	settings := httptest.NewRequest(http.MethodGet, "/settings", nil)
	settings.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	settingsRec := httptest.NewRecorder()
	handler.ServeHTTP(settingsRec, settings)
	if settingsRec.Code != http.StatusOK {
		t.Fatalf("settings=%d %s", settingsRec.Code, settingsRec.Body.String())
	}
	for _, want := range []string{`href="/errors/1"`, "RuntimeError", "Suppressed", "Recent errors"} {
		if !strings.Contains(settingsRec.Body.String(), want) {
			t.Fatalf("settings missing %q", want)
		}
	}

	detail := httptest.NewRequest(http.MethodGet, "/errors/1", nil)
	detail.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	detailRec := httptest.NewRecorder()
	handler.ServeHTTP(detailRec, detail)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("detail=%d %s", detailRec.Code, detailRec.Body.String())
	}
	for _, want := range []string{"Suppressed", `action="/errors/1/unsuppress"`, "Restore notifications", "https://github.com/raisestracker/raises", "https://github.com/raisestracker/raises/blob/master/LICENSE", "AGPL-3.0"} {
		if !strings.Contains(detailRec.Body.String(), want) {
			t.Fatalf("detail missing %q", want)
		}
	}
	_ = session
}

func TestEventAndWebhookAgentAPI(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cipher, err := secretbox.New("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY")
	if err != nil {
		t.Fatal(err)
	}
	store.SetSecretCipher(cipher)
	user, _ := store.UpsertGitHubUser(ctx, 77, "owner", "", "")
	agentToken, _, err := store.CreateAgentKey(ctx, user.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	project, _ := store.CreateProject(ctx, user.ID, "Widget", "widget")
	projectToken, _, _ := store.CreateProjectToken(ctx, user.ID, project.ID)
	handler := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{}).Handler()

	create := httptest.NewRequest(http.MethodPost, "/v1/events", bytes.NewBufferString(`{"level":"info","message":"Deploy finished","source":"deploy"}`))
	create.Header.Set("Authorization", "Bearer "+projectToken)
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, create)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create=%d %s", createRec.Code, createRec.Body.String())
	}
	var event inbox.Event
	_ = json.Unmarshal(createRec.Body.Bytes(), &event)

	list := httptest.NewRequest(http.MethodGet, "/v1/events?project=widget&level=info", nil)
	list.Header.Set("Authorization", "Bearer "+agentToken)
	listRec := httptest.NewRecorder()
	handler.ServeHTTP(listRec, list)
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), event.ID) {
		t.Fatalf("list=%d %s", listRec.Code, listRec.Body.String())
	}

	endpoint := httptest.NewRequest(http.MethodPost, "/v1/webhook-endpoints", bytes.NewBufferString(`{"url":"https://example.com/raises","events":["notice.created"]}`))
	endpoint.Header.Set("Authorization", "Bearer "+agentToken)
	endpointRec := httptest.NewRecorder()
	handler.ServeHTTP(endpointRec, endpoint)
	if endpointRec.Code != http.StatusCreated || !strings.Contains(endpointRec.Body.String(), `"signing_secret":"whsec_`) {
		t.Fatalf("endpoint=%d %s", endpointRec.Code, endpointRec.Body.String())
	}

	bad := httptest.NewRequest(http.MethodPost, "/v1/webhook-endpoints", bytes.NewBufferString(`{"url":"http://127.0.0.1/hook"}`))
	bad.Header.Set("Authorization", "Bearer "+agentToken)
	badRec := httptest.NewRecorder()
	handler.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("bad endpoint=%d %s", badRec.Code, badRec.Body.String())
	}
}

type failingIssueFiler struct{}

func (failingIssueFiler) FindByMarker(context.Context, int64, string, string, time.Time) (int, string, bool, error) {
	return 0, "", false, nil
}
func (failingIssueFiler) Open(context.Context, int64, string, string, string) (int, string, error) {
	return 0, "", errors.New("github unavailable")
}
func (failingIssueFiler) Reopen(context.Context, int64, string, int) error {
	return errors.New("github unavailable")
}

func TestSettingsShowsDeadDeliveryAndOwnerCanRetry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), func() time.Time { return now }, failingIssueFiler{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, _ := store.UpsertGitHubUser(ctx, 6334, "demo", "", "")
	if err := store.UpsertApp(ctx, "widget", "token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	if err := store.AssignLegacyProjects(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(ctx, "token", inbox.Notice{Class: "RuntimeError", Backtrace: []string{"app/jobs/x.rb:1"}}); err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt < 20; attempt++ {
		now = now.Add(20 * time.Minute)
		if err := store.ProcessIssueJobsOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := store.IssueDeliveryHealthForUser(ctx, user.ID)
	if err != nil || len(jobs.Problems) != 1 {
		t.Fatalf("jobs=%#v err=%v", jobs, err)
	}
	sessionToken, session, err := store.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{GitHubAppSlug: "raises"}).Handler()
	request := httptest.NewRequest(http.MethodGet, "/settings", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	for _, want := range []string{"Attention", "needs attention", "github unavailable", ">Retry<"} {
		if !strings.Contains(recorder.Body.String(), want) {
			t.Fatalf("settings missing %q: %s", want, recorder.Body.String())
		}
	}
	body := strings.NewReader("csrf=" + session.CSRFToken)
	retry := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/settings/issue-jobs/%d/retry", jobs.Problems[0].ID), body)
	retry.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	retry.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	retryRecorder := httptest.NewRecorder()
	handler.ServeHTTP(retryRecorder, retry)
	if retryRecorder.Code != http.StatusSeeOther {
		t.Fatalf("retry=%d %s", retryRecorder.Code, retryRecorder.Body.String())
	}
	health, _ := store.IssueDeliveryHealthForUser(ctx, user.ID)
	if health.Dead != 0 || health.Retrying != 1 {
		t.Fatalf("health after retry=%#v", health)
	}
}

func testHandler(t *testing.T) http.Handler {
	t.Helper()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertApp(context.Background(), "widget", "app-token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	return New(store, "agent-token", slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20).Handler()
}
