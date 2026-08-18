package inbox

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestBootstrapCreatesScopedAgentKeyOnce(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	user, err := store.UpsertGitHubUser(ctx, 6334, "demo", "Demo User", "")
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := store.CreateBootstrapToken(ctx, user.ID, "Codex", 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	token, key, err := store.ExchangeBootstrapToken(ctx, bootstrap, "")
	if err != nil {
		t.Fatal(err)
	}
	if key.Name != "Codex" || token == "" {
		t.Fatalf("key=%#v token=%q", key, token)
	}
	if got, err := store.AuthenticateAgent(ctx, token); err != nil || got != user.ID {
		t.Fatalf("user=%d err=%v", got, err)
	}
	if _, _, err := store.ExchangeBootstrapToken(ctx, bootstrap, "again"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("reuse err=%v", err)
	}
}

func TestProjectsTokensArchivingAndTenantIsolation(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	a, _ := store.UpsertGitHubUser(ctx, 1, "a", "", "")
	b, _ := store.UpsertGitHubUser(ctx, 2, "b", "", "")
	project, err := store.CreateProject(ctx, a.ID, "Alpha", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateProject(ctx, b.ID, "Another Alpha", "alpha"); err != nil {
		t.Fatalf("project slugs should be per-user: %v", err)
	}
	token, _, err := store.CreateProjectToken(ctx, a.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := store.Ingest(ctx, token, Notice{Class: "RuntimeError", Backtrace: []string{"app/jobs/a.rb:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetForUser(ctx, b.ID, result.Group.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant err=%v", err)
	}
	if _, err := store.SetProjectArchived(ctx, a.ID, project.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(ctx, token, Notice{Class: "RuntimeError"}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("archived ingest err=%v", err)
	}
	if _, err := store.SetProjectArchived(ctx, a.ID, project.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(ctx, token, Notice{Class: "RuntimeError"}); err != nil {
		t.Fatal(err)
	}
}

func TestArchivedProjectCannotExceedActiveLimitWhenRestored(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	user, _ := store.UpsertGitHubUser(ctx, 3, "limit", "", "")
	archived, err := store.CreateProject(ctx, user.ID, "Archived", "archived")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetProjectArchived(ctx, user.ID, archived.ID, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxActiveProjects; i++ {
		if _, err := store.CreateProject(ctx, user.ID, "Project", "project-"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SetProjectArchived(ctx, user.ID, archived.ID, false); err == nil {
		t.Fatal("expected restoring an archived project above the active limit to fail")
	}
	if _, _, err := store.CreateProjectToken(ctx, user.ID, archived.ID); err == nil {
		t.Fatal("expected token creation for archived project to fail")
	}
}

func TestLegacySchemaMigratesInPlace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := []string{
		`CREATE TABLE apps (name TEXT PRIMARY KEY, token_hash TEXT NOT NULL, github_repo TEXT NOT NULL DEFAULT '')`,
		`CREATE TABLE error_groups (id INTEGER PRIMARY KEY AUTOINCREMENT, app_name TEXT NOT NULL, env TEXT NOT NULL, fingerprint TEXT NOT NULL, class TEXT NOT NULL, location TEXT NOT NULL, message TEXT NOT NULL, count INTEGER NOT NULL, first_seen_unix INTEGER NOT NULL, last_seen_unix INTEGER NOT NULL, last_revision TEXT NOT NULL DEFAULT '', github_issue_number INTEGER NOT NULL DEFAULT 0, github_issue_url TEXT NOT NULL DEFAULT '', acked_at_unix INTEGER, sample_json TEXT NOT NULL, UNIQUE(app_name, env, fingerprint))`,
		`CREATE TABLE notices (id INTEGER PRIMARY KEY AUTOINCREMENT, group_id INTEGER NOT NULL, received_at_unix INTEGER NOT NULL, revision TEXT NOT NULL DEFAULT '', payload_json TEXT NOT NULL)`,
		`CREATE TABLE issue_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, group_id INTEGER NOT NULL, action TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at_unix INTEGER NOT NULL, last_error TEXT NOT NULL DEFAULT '', created_at_unix INTEGER NOT NULL, completed_at_unix INTEGER)`,
	}
	for _, statement := range legacy {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO apps(name,token_hash,github_repo) VALUES('widget',?,'example/widget')`, HashToken("token")); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	reopened, err := Open(path, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	app, err := reopened.AppByToken(context.Background(), "token")
	if err != nil || app.ID != "prj_widget" {
		t.Fatalf("app=%#v err=%v", app, err)
	}
	for _, column := range []string{"delivery_key", "lease_expires_at_unix", "failed_at_unix"} {
		if exists, err := reopened.columnExists(context.Background(), "issue_jobs", column); err != nil || !exists {
			t.Fatalf("issue_jobs.%s exists=%v err=%v", column, exists, err)
		}
	}
}
