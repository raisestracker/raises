package inbox

import (
	"context"
	"testing"
)

// Retirement must reach legacy ingestion hashes whatever owner they carry —
// the default owner 0, an assigned owner, or a rewritten hash from a later
// boot — while modern project tokens keep working.
func TestClearAllLegacyTokensRetiresEveryOwnerShape(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()

	if err := store.UpsertApp(ctx, "unowned", "unowned-token", ""); err != nil {
		t.Fatal(err)
	}
	owner, err := store.UpsertGitHubUser(ctx, 401, "legacy-owner", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertApp(ctx, "owned", "owned-token", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.AssignLegacyProjects(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}
	project, err := store.CreateProject(ctx, owner.ID, "Modern", "modern")
	if err != nil {
		t.Fatal(err)
	}
	modernToken, _, err := store.CreateProjectToken(ctx, owner.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := store.ClearAllLegacyTokens(ctx); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"unowned-token", "owned-token"} {
		if _, err := store.AppByToken(ctx, token); err != ErrUnauthorized {
			t.Fatalf("retired token accepted: %v", err)
		}
	}
	if _, err := store.AppByToken(ctx, modernToken); err != nil {
		t.Fatalf("modern project token broke: %v", err)
	}

	// A later boot rewrites the static hashes from the environment; clearing
	// again (the flagged startup order: upsert, then clear) keeps them retired.
	if err := store.UpsertApp(ctx, "unowned", "unowned-token", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppByToken(ctx, "unowned-token"); err != nil {
		t.Fatal("rewritten hash should be live before clearing")
	}
	if err := store.ClearAllLegacyTokens(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AppByToken(ctx, "unowned-token"); err != ErrUnauthorized {
		t.Fatalf("rewritten retired token accepted: %v", err)
	}
}
