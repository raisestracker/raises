package inbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestCreateProjectWithTokenIsAtomic(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	user, err := store.UpsertGitHubUser(ctx, 401, "onboard", "", "")
	if err != nil {
		t.Fatal(err)
	}
	project, token, tok, err := store.CreateProjectWithToken(ctx, user.ID, "Solo App", "")
	if err != nil {
		t.Fatal(err)
	}
	if project.Slug != "solo-app" || project.Name != "Solo App" || !strings.HasPrefix(token, "raises_ingest_"+tok.Prefix+"_") {
		t.Fatalf("project=%+v token=%q tok=%+v", project, token, tok)
	}
	app, err := store.AppByToken(ctx, token)
	if err != nil || app.OwnerUserID != user.ID {
		t.Fatalf("token must authenticate ingestion for the new project: app=%+v err=%v", app, err)
	}
	res, err := store.IngestForApp(ctx, app, Notice{Class: "E", Message: "m", Backtrace: []string{"app/a.rb:1"}})
	if err != nil || res.Group.ProjectID != project.ID {
		t.Fatalf("ingest=%+v err=%v", res.Group, err)
	}

	countRows := func(q string, args ...any) int {
		var n int
		if err := store.db.QueryRowContext(ctx, q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	// Duplicate slug: the app insert fails, nothing is left behind.
	if _, _, _, err := store.CreateProjectWithToken(ctx, user.ID, "Again", "solo-app"); err == nil {
		t.Fatal("duplicate slug must fail")
	}
	// Token insert failure must roll back the project insert (no orphan).
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_token BEFORE INSERT ON project_tokens BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.CreateProjectWithToken(ctx, user.ID, "Orphan", "orphan"); err == nil {
		t.Fatal("token failure must fail the whole create")
	}
	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_token`); err != nil {
		t.Fatal(err)
	}
	if n := countRows(`SELECT COUNT(*) FROM apps WHERE owner_user_id=?`, user.ID); n != 1 {
		t.Fatalf("orphan projects: apps=%d want 1", n)
	}
	if n := countRows(`SELECT COUNT(*) FROM project_tokens pt JOIN apps a ON a.project_id = pt.project_id WHERE a.owner_user_id=?`, user.ID); n != 1 {
		t.Fatalf("tokens=%d want 1", n)
	}
	for _, bad := range [][2]string{{"", "x"}, {"Name", "Bad Slug"}, {strings.Repeat("n", 101), ""}, {"!!!", ""}} {
		if _, _, _, err := store.CreateProjectWithToken(ctx, user.ID, bad[0], bad[1]); err == nil || !strings.Contains(err.Error(), "valid name and slug") {
			t.Fatalf("CreateProjectWithToken(%q,%q) err=%v", bad[0], bad[1], err)
		}
	}
}

func TestCreateProjectWithTokenHoldsLimitUnderConcurrency(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	user, err := store.UpsertGitHubUser(ctx, 402, "onboard-race", "", "")
	if err != nil {
		t.Fatal(err)
	}
	const attempts = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok, limited := 0, 0
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _, err := store.CreateProjectWithToken(ctx, user.ID, fmt.Sprintf("Race %d", i), "")
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else if strings.Contains(err.Error(), "limit reached") {
				limited++
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if ok != MaxActiveProjects || limited != attempts-MaxActiveProjects {
		t.Fatalf("ok=%d limited=%d", ok, limited)
	}
	var apps, tokens int
	_ = store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM apps WHERE owner_user_id=?`, user.ID).Scan(&apps)
	_ = store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_tokens pt JOIN apps a ON a.project_id = pt.project_id WHERE a.owner_user_id=?`, user.ID).Scan(&tokens)
	if apps != MaxActiveProjects || tokens != MaxActiveProjects {
		t.Fatalf("apps=%d tokens=%d", apps, tokens)
	}
}

func TestSlugFromName(t *testing.T) {
	for in, want := range map[string]string{
		"Solo App 101":     "solo-app-101",
		"  --Hello__World": "hello-world",
		"Ünïcode Shop!":    "n-code-shop",
		"!!!":              "",
	} {
		if got := SlugFromName(in); got != want {
			t.Errorf("SlugFromName(%q)=%q want %q", in, got, want)
		}
	}
	if got := SlugFromName(strings.Repeat("ab ", 60)); len(got) > maxProjectSlugLength || strings.HasSuffix(got, "-") || !slugPattern.MatchString(got) {
		t.Fatalf("long slug %q", got)
	}
}
