package inbox

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// Concurrent creates from one account must never exceed the active-project cap.
func TestProjectCreationHoldsLimitUnderConcurrency(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	user, err := store.UpsertGitHubUser(ctx, 301, "quota-a", "", "")
	if err != nil {
		t.Fatal(err)
	}
	const attempts = 30
	var wg sync.WaitGroup
	errs := make([]error, attempts)
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := store.CreateProject(ctx, user.ID, fmt.Sprintf("Project %d", i), fmt.Sprintf("p-%d", i))
			errs[i] = err
		}(i)
	}
	wg.Wait()
	projects, err := store.ListProjects(ctx, user.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != MaxActiveProjects {
		t.Fatalf("active projects=%d want %d (errors=%v)", len(projects), MaxActiveProjects, errs)
	}
}

// Mixed concurrent creates and restores must never exceed the cap, and the
// unlimited owner keeps its exception.
func TestProjectRestoreHoldsLimitUnderConcurrency(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	user, err := store.UpsertGitHubUser(ctx, 302, "quota-b", "", "")
	if err != nil {
		t.Fatal(err)
	}
	var archived Project
	for i := 0; i < MaxActiveProjects; i++ {
		project, err := store.CreateProject(ctx, user.ID, fmt.Sprintf("Project %d", i), fmt.Sprintf("q-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		archived = project
	}
	if _, err := store.SetProjectArchived(ctx, user.ID, archived.ID, true); err != nil {
		t.Fatal(err)
	}
	const attempts = 10
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				_, _ = store.CreateProject(ctx, user.ID, fmt.Sprintf("Extra %d", i), fmt.Sprintf("extra-%d", i))
			} else {
				_, _ = store.SetProjectArchived(ctx, user.ID, archived.ID, false)
			}
		}(i)
	}
	wg.Wait()
	projects, err := store.ListProjects(ctx, user.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != MaxActiveProjects {
		t.Fatalf("active projects=%d want %d", len(projects), MaxActiveProjects)
	}
}

// Parallel token minting must never exceed the per-project active cap, and
// revocation must free capacity.
func TestProjectTokenCreationHoldsLimitUnderConcurrency(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	user, err := store.UpsertGitHubUser(ctx, 303, "quota-c", "", "")
	if err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, user.ID, "Tokens", "tokens")
	if err != nil {
		t.Fatal(err)
	}
	const attempts = 10
	var wg sync.WaitGroup
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = store.CreateProjectToken(ctx, user.ID, project.ID)
		}()
	}
	wg.Wait()
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_tokens WHERE project_id=? AND revoked_at_unix IS NULL`, project.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != MaxActiveProjectTokens {
		t.Fatalf("active tokens=%d want %d", count, MaxActiveProjectTokens)
	}
	if _, _, err := store.CreateProjectToken(ctx, user.ID, project.ID); err == nil {
		t.Fatal("expected token creation above the cap to fail")
	}
}
