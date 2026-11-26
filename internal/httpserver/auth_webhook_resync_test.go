package httpserver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/raisestracker/raises/internal/github"
	"github.com/raisestracker/raises/internal/inbox"
)

// fakeResyncApp splits repo visibility the way GitHub does: the user token
// sees only what the linker can read, while the app-token list covers the
// whole installation.
type fakeResyncApp struct {
	installation gh.Installation
	userRepos    []gh.Repository
	appRepos     []gh.Repository
}

func (f *fakeResyncApp) OAuthURL(_, _, _ string) string { return "" }

func (f *fakeResyncApp) ExchangeCode(_ context.Context, _, _, _ string) (string, error) {
	return "", errors.New("not implemented")
}

func (f *fakeResyncApp) CurrentUser(_ context.Context, _ string) (gh.User, error) {
	return gh.User{}, errors.New("not implemented")
}

func (f *fakeResyncApp) UserInstallations(_ context.Context, _ string) ([]gh.Installation, error) {
	return []gh.Installation{f.installation}, nil
}

func (f *fakeResyncApp) Installation(_ context.Context, _ int64) (gh.Installation, error) {
	return f.installation, nil
}

func (f *fakeResyncApp) Repositories(_ context.Context, _ int64) ([]gh.Repository, error) {
	return f.appRepos, nil
}

func (f *fakeResyncApp) UserInstallationRepositories(_ context.Context, _ string, _ int64) ([]gh.Repository, error) {
	return f.userRepos, nil
}

// TestWebhookResyncNeverGrantsUnlinkedRepos is the regression test for the
// privilege-escalation fix: linking with a user token that sees repo A, then
// receiving an installation_repositories webhook whose app-token list covers
// A and B, must leave B invisible and unbindable.
func TestWebhookResyncNeverGrantsUnlinkedRepos(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	user, err := store.UpsertGitHubUser(ctx, 4242001, "linker", "", "")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, user.ID, "Widget", "widget")
	if err != nil {
		t.Fatal(err)
	}

	const installationID = 4242
	var installation gh.Installation
	installation.ID = installationID
	installation.Account.Login = "org"
	installation.TargetType = "Organization"
	installation.RepositorySelection = "selected"

	repoA := gh.Repository{ID: 11, FullName: "org/a"}
	repoB := gh.Repository{ID: 22, FullName: "org/b"}
	fake := &fakeResyncApp{
		installation: installation,
		userRepos:    []gh.Repository{repoA},
		appRepos:     []gh.Repository{repoA, repoB},
	}
	srv := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{
		GitHubAppSlug:       "raises",
		GitHubWebhookSecret: "test-secret",
		GitHubApp:           fake,
	})
	handler := srv.Handler()

	// Link with the user token: only repo A is visible to the linker.
	linkReq := httptest.NewRequest(http.MethodGet, "/", nil)
	if err := srv.syncInstallation(linkReq, user.ID, installationID, "user-token"); err != nil {
		t.Fatalf("link: %v", err)
	}

	// Routine installation webhook whose app-token list covers A and B.
	body := `{"action":"added","installation":{"id":4242}}`
	mac := hmac.New(sha256.New, []byte("test-secret"))
	_, _ = mac.Write([]byte(body))
	hookReq := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(body))
	hookReq.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	hookReq.Header.Set("X-GitHub-Event", "installation_repositories")
	hookRec := httptest.NewRecorder()
	handler.ServeHTTP(hookRec, hookReq)
	if hookRec.Code != http.StatusAccepted {
		t.Fatalf("webhook status = %d body = %s", hookRec.Code, hookRec.Body.String())
	}

	repos, err := store.ListGitHubRepositories(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].ID != repoA.ID || repos[0].FullName != repoA.FullName {
		t.Fatalf("visible repos = %#v, want only org/a", repos)
	}

	if _, err := store.BindProjectRepository(ctx, user.ID, project.ID, repoB.ID); !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("binding unlinked repo B: err = %v, want ErrNotFound", err)
	}
	if _, err := store.BindProjectRepository(ctx, user.ID, project.ID, repoA.ID); err != nil {
		t.Fatalf("binding linked repo A: %v", err)
	}
}

// TestRefreshInstallationSemantics pins the rest of the refresh contract at
// the store level: renames propagate, repos missing from the resync are
// deactivated, and rows that are already inactive are never reactivated.
func TestRefreshInstallationSemantics(t *testing.T) {
	ctx := context.Background()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	user, err := store.UpsertGitHubUser(ctx, 4242002, "linker", "", "")
	if err != nil {
		t.Fatal(err)
	}
	const installationID = 4243
	if err := store.UpsertInstallation(ctx, user.ID, installationID, "org", "Organization", "selected", "active", []inbox.GitHubRepository{
		{ID: 11, InstallationID: installationID, FullName: "org/a"},
		{ID: 22, InstallationID: installationID, FullName: "org/b"},
	}); err != nil {
		t.Fatal(err)
	}

	// A is renamed, B is missing (removed), C is new to the app-token list.
	if err := store.RefreshInstallation(ctx, user.ID, installationID, "org", "Organization", "selected", "active", []inbox.GitHubRepository{
		{ID: 11, InstallationID: installationID, FullName: "org/a-renamed"},
		{ID: 33, InstallationID: installationID, FullName: "org/c"},
	}); err != nil {
		t.Fatal(err)
	}
	repos, err := store.ListGitHubRepositories(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].ID != 11 || repos[0].FullName != "org/a-renamed" {
		t.Fatalf("visible repos = %#v, want only renamed org/a", repos)
	}

	// A later resync listing the inactive B and C must not reactivate them:
	// only a user-token relink (UpsertInstallation) makes repos bindable.
	if err := store.RefreshInstallation(ctx, user.ID, installationID, "org", "Organization", "selected", "active", []inbox.GitHubRepository{
		{ID: 11, InstallationID: installationID, FullName: "org/a-renamed"},
		{ID: 22, InstallationID: installationID, FullName: "org/b"},
		{ID: 33, InstallationID: installationID, FullName: "org/c"},
	}); err != nil {
		t.Fatal(err)
	}
	repos, err = store.ListGitHubRepositories(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].ID != 11 {
		t.Fatalf("visible repos after relist = %#v, want only org/a-renamed", repos)
	}
}
