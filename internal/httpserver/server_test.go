package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
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

func TestAccountKeyAgentKeyAPI(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, err := store.UpsertGitHubUser(context.Background(), 4242, "owner", "", "")
	if err != nil {
		t.Fatal(err)
	}
	accountToken, accountKey, err := store.CreateAccountKey(context.Background(), user.ID, "CI host")
	if err != nil {
		t.Fatal(err)
	}
	ordinaryToken, _, err := store.CreateAgentKey(context.Background(), user.ID, "ordinary")
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{}).Handler()
	call := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	mint := call(http.MethodPost, "/v1/agent-keys", accountToken, `{"name":"SuperReception"}`)
	if mint.Code != http.StatusCreated {
		t.Fatalf("mint=%d %s", mint.Code, mint.Body.String())
	}
	var minted struct {
		Token string         `json:"token"`
		Key   inbox.AgentKey `json:"key"`
	}
	if err := json.Unmarshal(mint.Body.Bytes(), &minted); err != nil || minted.Token == "" || minted.Key.Name != "SuperReception" {
		t.Fatalf("minted=%s err=%v", mint.Body.String(), err)
	}
	if minted.Key.MintedByKeyID == nil || *minted.Key.MintedByKeyID != accountKey.ID {
		t.Fatalf("minted key is missing its audit trail: %s", mint.Body.String())
	}

	list := call(http.MethodGet, "/v1/agent-keys", accountToken, "")
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), minted.Key.ID) {
		t.Fatalf("list=%d %s", list.Code, list.Body.String())
	}

	if rec := call(http.MethodPost, "/v1/agent-keys", ordinaryToken, `{"name":"nope"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("ordinary key mint=%d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodGet, "/v1/agent-keys", ordinaryToken, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("ordinary key list=%d %s", rec.Code, rec.Body.String())
	}
	if rec := call(http.MethodGet, "/v1/agent-keys", "nope", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad token list=%d %s", rec.Code, rec.Body.String())
	}

	revoke := call(http.MethodDelete, "/v1/agent-keys/"+minted.Key.ID, accountToken, "")
	if revoke.Code != http.StatusNoContent {
		t.Fatalf("revoke=%d %s", revoke.Code, revoke.Body.String())
	}
	if rec := call(http.MethodDelete, "/v1/agent-keys/"+minted.Key.ID, accountToken, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("double revoke=%d %s", rec.Code, rec.Body.String())
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
	token, session, err := store.CreateSession(context.Background(), user.ID, time.Hour)
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
	for _, want := range []string{"Bootstrap an agent", "Agent keys", "GitHub repository access", "example/widget", "Add or remove repositories", "https://github.com/settings/installations/123", "Issue delivery", "Webhook delivery", "Healthy", `href="/dashboard"`, `aria-current="page">Settings`, `id="agent-key-form"`, `id="agent-account-key"`, `id="copy-agent-key"`, `id="bootstrap-form"`, `id="bootstrap-result"`, `id="bootstrap-prompt"`, `id="copy-bootstrap-prompt"`, `id="copy-bootstrap-prompt-status"`, `id="bootstrap-form-status"`, `Accept: "application/json"`, "https://github.com/raisestracker/raises", "https://github.com/raisestracker/raises/blob/master/LICENSE", "AGPL-3.0"} {
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

	create := httptest.NewRequest(http.MethodPost, "/settings/agent-keys", strings.NewReader("csrf="+session.CSRFToken+"&name=Codex"))
	create.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	create.Header.Set("Accept", "application/json")
	create.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, create)
	if createRec.Code != http.StatusCreated || !strings.Contains(createRec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("create key=%d content-type=%q body=%s", createRec.Code, createRec.Header().Get("Content-Type"), createRec.Body.String())
	}
	var created struct {
		Token string         `json:"token"`
		Key   inbox.AgentKey `json:"key"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil || created.Token == "" || created.Key.Name != "Codex" {
		t.Fatalf("created=%#v err=%v body=%s", created, err, createRec.Body.String())
	}
	if created.Key.CanMint {
		t.Fatalf("ordinary browser key must not mint: %#v", created.Key)
	}

	var accountBody bytes.Buffer
	accountWriter := multipart.NewWriter(&accountBody)
	if err := accountWriter.WriteField("csrf", session.CSRFToken); err != nil {
		t.Fatal(err)
	}
	if err := accountWriter.WriteField("name", "CI host"); err != nil {
		t.Fatal(err)
	}
	if err := accountWriter.WriteField("account_key", "1"); err != nil {
		t.Fatal(err)
	}
	if err := accountWriter.Close(); err != nil {
		t.Fatal(err)
	}
	account := httptest.NewRequest(http.MethodPost, "/settings/agent-keys", &accountBody)
	account.Header.Set("Content-Type", accountWriter.FormDataContentType())
	account.Header.Set("Accept", "application/json")
	account.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	accountRec := httptest.NewRecorder()
	handler.ServeHTTP(accountRec, account)
	if accountRec.Code != http.StatusCreated {
		t.Fatalf("create account key=%d body=%s", accountRec.Code, accountRec.Body.String())
	}
	var accountCreated struct {
		Token string         `json:"token"`
		Key   inbox.AgentKey `json:"key"`
	}
	if err := json.Unmarshal(accountRec.Body.Bytes(), &accountCreated); err != nil || accountCreated.Token == "" || !accountCreated.Key.CanMint || accountCreated.Key.Name != "CI host" {
		t.Fatalf("accountCreated=%#v err=%v body=%s", accountCreated, err, accountRec.Body.String())
	}

	// The settings page submits via fetch(FormData), i.e. multipart: the
	// server must accept that encoding for both bootstrap and key creation.
	var bootstrapBody bytes.Buffer
	bootstrapWriter := multipart.NewWriter(&bootstrapBody)
	if err := bootstrapWriter.WriteField("csrf", session.CSRFToken); err != nil {
		t.Fatal(err)
	}
	if err := bootstrapWriter.Close(); err != nil {
		t.Fatal(err)
	}
	bootstrapCreate := httptest.NewRequest(http.MethodPost, "/settings/bootstrap", &bootstrapBody)
	bootstrapCreate.Header.Set("Content-Type", bootstrapWriter.FormDataContentType())
	bootstrapCreate.Header.Set("Accept", "application/json")
	bootstrapCreate.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	bootstrapCreateRec := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapCreateRec, bootstrapCreate)
	if bootstrapCreateRec.Code != http.StatusCreated || !strings.Contains(bootstrapCreateRec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("create bootstrap=%d content-type=%q body=%s", bootstrapCreateRec.Code, bootstrapCreateRec.Header().Get("Content-Type"), bootstrapCreateRec.Body.String())
	}
	var bootstrapCreated struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(bootstrapCreateRec.Body.Bytes(), &bootstrapCreated); err != nil || !strings.Contains(bootstrapCreated.Prompt, "bootstrap token") {
		t.Fatalf("bootstrapCreated=%#v err=%v body=%s", bootstrapCreated, err, bootstrapCreateRec.Body.String())
	}

	bootstrapFallback := httptest.NewRequest(http.MethodPost, "/settings/bootstrap", strings.NewReader("csrf="+session.CSRFToken))
	bootstrapFallback.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	bootstrapFallback.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	bootstrapFallbackRec := httptest.NewRecorder()
	handler.ServeHTTP(bootstrapFallbackRec, bootstrapFallback)
	if bootstrapFallbackRec.Code != http.StatusOK {
		t.Fatalf("bootstrap fallback=%d body=%s", bootstrapFallbackRec.Code, bootstrapFallbackRec.Body.String())
	}
	for _, want := range []string{`id="bootstrap-result"`, "Copy this prompt now", "Set up Raises for me", `id="copy-bootstrap-prompt"`} {
		if !strings.Contains(bootstrapFallbackRec.Body.String(), want) {
			t.Fatalf("bootstrap fallback missing %q", want)
		}
	}
}

func TestSettingsAgentKeyNameHintAndPlainName(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, _ := store.UpsertGitHubUser(context.Background(), 6339, "hint", "", "")
	token, session, err := store.CreateSession(context.Background(), user.ID, time.Hour)
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
	body := rec.Body.String()
	if !strings.Contains(body, `id="agent-name-hint"`) {
		t.Fatal("settings is missing the agent name format hint")
	}
	if strings.Contains(body, `id="agent-name" name="name" placeholder="e.g. Codex on my Mac"`) {
		t.Fatal("settings still shows the stale placeholder without parentheses example")
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, `id="agent-name"`) && strings.Contains(line, "pattern=") {
			t.Fatalf("agent name input must not carry a restrictive pattern: %s", strings.TrimSpace(line))
		}
	}

	create := httptest.NewRequest(http.MethodPost, "/settings/agent-keys", strings.NewReader("csrf="+session.CSRFToken+"&name=Superreception"))
	create.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	create.Header.Set("Accept", "application/json")
	create.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	createRec := httptest.NewRecorder()
	handler.ServeHTTP(createRec, create)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("plain name create=%d body=%s", createRec.Code, createRec.Body.String())
	}

	bad := httptest.NewRequest(http.MethodPost, "/settings/agent-keys", strings.NewReader("csrf="+session.CSRFToken+"&name=Bad%3Cname%3E"))
	bad.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	bad.Header.Set("Accept", "application/json")
	bad.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
	badRec := httptest.NewRecorder()
	handler.ServeHTTP(badRec, bad)
	if badRec.Code != http.StatusBadRequest || !strings.Contains(badRec.Body.String(), "agent name is not valid") {
		t.Fatalf("invalid name create=%d body=%s", badRec.Code, badRec.Body.String())
	}
}

func TestCreateBrowserAgentKeyJSONFailuresStayJSON(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, _ := store.UpsertGitHubUser(context.Background(), 6340, "jsonfail", "", "")
	token, session, err := store.CreateSession(context.Background(), user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{GitHubAppSlug: "raises"}).Handler()
	post := func(body, cookie string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/settings/agent-keys", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")
		if cookie != "" {
			req.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	// No session (expired or missing cookie): the fetch client parses the body
	// as JSON, so a redirect to an HTML login page surfaces as a JSON parse
	// error in the browser. It must be a JSON 401 instead.
	anon := post("csrf=x&name=Ghost&account_key=1", "")
	if anon.Code != http.StatusUnauthorized || !strings.Contains(anon.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("anonymous create=%d content-type=%q body=%q", anon.Code, anon.Header().Get("Content-Type"), anon.Body.String())
	}
	var anonErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(anon.Body.Bytes(), &anonErr); err != nil || anonErr.Error == "" {
		t.Fatalf("anonymous body is not a JSON error: %q err=%v", anon.Body.String(), err)
	}

	// Wrong CSRF token with a valid session cookie: must be a JSON 403, not
	// plain text, for the same reason.
	forged := post("csrf=wrong&name=Ghost&account_key=1", token)
	if forged.Code != http.StatusForbidden || !strings.Contains(forged.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("forged csrf create=%d content-type=%q body=%q", forged.Code, forged.Header().Get("Content-Type"), forged.Body.String())
	}
	var forgedErr struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(forged.Body.Bytes(), &forgedErr); err != nil || forgedErr.Error == "" {
		t.Fatalf("forged csrf body is not a JSON error: %q err=%v", forged.Body.String(), err)
	}

	// Sanity: the real session + CSRF still creates an account key.
	account := post("csrf="+session.CSRFToken+"&name=CI+host&account_key=1", token)
	if account.Code != http.StatusCreated {
		t.Fatalf("account key create=%d body=%s", account.Code, account.Body.String())
	}
}

func TestCreateBrowserAgentKeyMultipartFormData(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	user, _ := store.UpsertGitHubUser(context.Background(), 6341, "multipart", "", "")
	token, session, err := store.CreateSession(context.Background(), user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	handler := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{GitHubAppSlug: "raises"}).Handler()
	// The settings page submits with fetch + FormData, which is multipart.
	// ParseForm alone leaves multipart fields unparsed, which used to fail the
	// CSRF check and surface in Safari as a JSON parse error.
	multipartPost := func(fields map[string]string) *httptest.ResponseRecorder {
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		for k, v := range fields {
			if err := w.WriteField(k, v); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/settings/agent-keys", &buf)
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.Header.Set("Accept", "application/json")
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: token})
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	plain := multipartPost(map[string]string{"csrf": session.CSRFToken, "name": "Superreception"})
	if plain.Code != http.StatusCreated {
		t.Fatalf("multipart plain name=%d content-type=%q body=%s", plain.Code, plain.Header().Get("Content-Type"), plain.Body.String())
	}
	var created struct {
		Token string         `json:"token"`
		Key   inbox.AgentKey `json:"key"`
	}
	if err := json.Unmarshal(plain.Body.Bytes(), &created); err != nil || created.Token == "" || created.Key.Name != "Superreception" {
		t.Fatalf("multipart created=%#v err=%v", created, err)
	}

	account := multipartPost(map[string]string{"csrf": session.CSRFToken, "name": "raises-account-key", "account_key": "1"})
	if account.Code != http.StatusCreated {
		t.Fatalf("multipart account key=%d body=%s", account.Code, account.Body.String())
	}
	var accountCreated struct {
		Token string         `json:"token"`
		Key   inbox.AgentKey `json:"key"`
	}
	if err := json.Unmarshal(account.Body.Bytes(), &accountCreated); err != nil || !accountCreated.Key.CanMint {
		t.Fatalf("multipart accountCreated=%#v err=%v", accountCreated, err)
	}
}

func TestDashboardLinksRecentErrorsAndShowsSuppressedState(t *testing.T) {
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
	if strings.Contains(settingsRec.Body.String(), "Recent errors") {
		t.Fatal("settings must not list recent errors; they live on the dashboard")
	}
	dash := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	dash.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	dashRec := httptest.NewRecorder()
	handler.ServeHTTP(dashRec, dash)
	if dashRec.Code != http.StatusOK {
		t.Fatalf("dashboard=%d %s", dashRec.Code, dashRec.Body.String())
	}
	for _, want := range []string{`href="/errors/1"`, "RuntimeError", "Suppressed", "Recent errors"} {
		if !strings.Contains(dashRec.Body.String(), want) {
			t.Fatalf("dashboard missing %q", want)
		}
	}

	detail := httptest.NewRequest(http.MethodGet, "/errors/1", nil)
	detail.AddCookie(&http.Cookie{Name: sessionCookie, Value: sessionToken})
	detailRec := httptest.NewRecorder()
	handler.ServeHTTP(detailRec, detail)
	if detailRec.Code != http.StatusOK {
		t.Fatalf("detail=%d %s", detailRec.Code, detailRec.Body.String())
	}
	for _, want := range []string{"Suppressed", `action="/errors/1/unsuppress"`, "Restore notifications", `href="/dashboard"`, "https://github.com/raisestracker/raises", "https://github.com/raisestracker/raises/blob/master/LICENSE", "AGPL-3.0"} {
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

	endpoint := httptest.NewRequest(http.MethodPost, "/v1/webhook-endpoints", bytes.NewBufferString(`{"url":"https://example.com/raises","events":["notice.created"],"authorization_header":"Authorization","authorization_value":"Bearer destination-secret"}`))
	endpoint.Header.Set("Authorization", "Bearer "+agentToken)
	endpointRec := httptest.NewRecorder()
	handler.ServeHTTP(endpointRec, endpoint)
	if endpointRec.Code != http.StatusCreated || !strings.Contains(endpointRec.Body.String(), `"signing_secret":"whsec_`) || !strings.Contains(endpointRec.Body.String(), `"authorization_header":"Authorization"`) || strings.Contains(endpointRec.Body.String(), "destination-secret") {
		t.Fatalf("endpoint=%d %s", endpointRec.Code, endpointRec.Body.String())
	}

	incompleteAuthorization := httptest.NewRequest(http.MethodPost, "/v1/webhook-endpoints", bytes.NewBufferString(`{"url":"https://example.com/raises","authorization_header":"Authorization"}`))
	incompleteAuthorization.Header.Set("Authorization", "Bearer "+agentToken)
	incompleteAuthorizationRec := httptest.NewRecorder()
	handler.ServeHTTP(incompleteAuthorizationRec, incompleteAuthorization)
	if incompleteAuthorizationRec.Code != http.StatusBadRequest {
		t.Fatalf("incomplete authorization=%d %s", incompleteAuthorizationRec.Code, incompleteAuthorizationRec.Body.String())
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

func TestPublicDiscovery(t *testing.T) {
	handler := testHandler(t)
	for _, path := range []string{"/", "/robots.txt", "/sitemap.xml"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		switch path {
		case "/":
			for _, link := range []string{`</sitemap.xml>; rel="sitemap"`, `</llms.txt>; rel="describedby"`} {
				if !strings.Contains(rec.Header().Get("Link"), link) {
					t.Errorf("missing link %s", link)
				}
			}
			if rec.Header().Get("Vary") != "Accept" {
				t.Error("missing Vary: Accept")
			}
		case "/robots.txt":
			for _, want := range []string{"User-agent: *\nAllow: /", "Content-Signal: search=yes, ai-input=yes, ai-train=no", "Sitemap: https://raises.dev/sitemap.xml"} {
				if !strings.Contains(rec.Body.String(), want) {
					t.Errorf("missing robots directive %s", want)
				}
			}
		case "/sitemap.xml":
			var sitemap struct {
				XMLName xml.Name
				URLs    []struct {
					Location string `xml:"loc"`
				} `xml:"url"`
			}
			if err := xml.Unmarshal(rec.Body.Bytes(), &sitemap); err != nil {
				t.Fatal(err)
			}
			if sitemap.XMLName.Space != "http://www.sitemaps.org/schemas/sitemap/0.9" || len(sitemap.URLs) != 4 {
				t.Fatalf("invalid sitemap: %+v", sitemap)
			}
			for _, entry := range sitemap.URLs {
				if !strings.HasPrefix(entry.Location, "https://raises.dev/") {
					t.Fatalf("unexpected URL %s", entry.Location)
				}
				page := httptest.NewRecorder()
				handler.ServeHTTP(page, httptest.NewRequest(http.MethodGet, entry.Location, nil))
				if page.Code != http.StatusOK {
					t.Errorf("sitemap URL %s: %d", entry.Location, page.Code)
				}
			}
		}
	}
}

func TestHomepageMarkdownNegotiation(t *testing.T) {
	handler := testHandler(t)
	for _, test := range []struct {
		accept   string
		markdown bool
	}{
		{"", false}, {"*/*", false}, {"text/html", false},
		{"text/markdown", true}, {"text/markdown; charset=utf-8", true},
		{"text/html;q=0.5, text/markdown", true},
		{"text/html, text/markdown;q=0.5", false},
		{"text/markdown;q=0", false}, {"text/markdown;q=invalid", false},
		{"text/markdown;q=2", false},
		{"text/markdown, */*;q=0.8", true}, {"text/markdown;q=0.5, */*", false},
		{"text/markdown;q=0.5, text/*", false}, {"text/markdown;q=0.5, text/html;q=0.1, */*", true},
	} {
		t.Run(test.accept, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.Header.Set("Accept", test.accept)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			markdown := strings.HasPrefix(rec.Header().Get("Content-Type"), "text/markdown")
			if rec.Code != http.StatusOK || markdown != test.markdown {
				t.Fatalf("status=%d type=%s", rec.Code, rec.Header().Get("Content-Type"))
			}
			if rec.Header().Get("Vary") != "Accept" {
				t.Error("missing Vary: Accept")
			}
			if test.markdown && (!strings.HasPrefix(rec.Body.String(), "# Raises") || strings.Contains(rec.Body.String(), "<html")) {
				t.Error("invalid markdown body")
			}
		})
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/unknown", nil)
	req.Header.Set("Accept", "text/markdown")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown path status=%d", rec.Code)
	}
}
