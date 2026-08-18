package inbox

import (
	"context"
	"errors"
	"testing"
)

func TestSuppressRetainsEvidenceAndSkipsSideEffects(t *testing.T) {
	store, filer := testStore(t)
	ctx := context.Background()
	store.SetSecretCipher(testCipher{})
	user, _ := store.UpsertGitHubUser(ctx, 20, "owner", "", "")
	if err := store.UpsertApp(ctx, "widget", "app-token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	if err := store.AssignLegacyProjects(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	_, _, _ = store.CreateWebhookEndpoint(ctx, user.ID, "https://example.com/raises", []string{"error.created", "error.regressed"})
	store.ConfigureOperatorNtfy(user.ID, true)

	n := Notice{
		Env:       "production",
		Revision:  "aaa",
		Class:     "RuntimeError",
		Message:   "boom",
		Backtrace: []string{"app/jobs/work_job.rb:4:in `perform`"},
	}
	first, err := store.Ingest(ctx, "app-token", n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ack(ctx, first.Group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Suppress(ctx, first.Group.ID); err != nil {
		t.Fatal(err)
	}
	var outboundBefore int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_events WHERE group_id=? AND event_type IN ('error.created','error.regressed')`, first.Group.ID).Scan(&outboundBefore); err != nil {
		t.Fatal(err)
	}

	n.Revision = "bbb"
	n.Message = "boom again"
	result, err := store.Ingest(ctx, "app-token", n)
	if err != nil {
		t.Fatal(err)
	}
	if result.Group.Count != 2 {
		t.Fatalf("count=%d", result.Group.Count)
	}
	if result.Group.AckedAt == nil {
		t.Fatal("suppressed recurrence should preserve acknowledgement")
	}
	if result.Regressed {
		t.Fatal("suppressed recurrence should not regress")
	}
	if filer.reopened != 0 {
		t.Fatalf("reopened=%d", filer.reopened)
	}

	var notices, outboundAfter, jobs int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices WHERE group_id=?`, first.Group.ID).Scan(&notices); err != nil || notices != 2 {
		t.Fatalf("notices=%d err=%v", notices, err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_events WHERE group_id=? AND event_type IN ('error.created','error.regressed')`, first.Group.ID).Scan(&outboundAfter); err != nil || outboundAfter != outboundBefore {
		t.Fatalf("outbound before=%d after=%d err=%v", outboundBefore, outboundAfter, err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_jobs WHERE group_id=?`, first.Group.ID).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("jobs=%d err=%v", jobs, err)
	}

	unacked, err := store.ListForUser(ctx, user.ID, "widget", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(unacked) != 0 {
		t.Fatalf("suppressed group should be excluded from unacked listings: %#v", unacked)
	}
}

func TestUnsuppressRestoresFutureBehaviorOnly(t *testing.T) {
	store, filer := testStore(t)
	ctx := context.Background()
	store.SetSecretCipher(testCipher{})
	user, _ := store.UpsertGitHubUser(ctx, 21, "owner", "", "")
	if err := store.UpsertApp(ctx, "widget", "app-token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	if err := store.AssignLegacyProjects(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	_, _, _ = store.CreateWebhookEndpoint(ctx, user.ID, "https://example.com/raises", []string{"error.regressed"})

	n := Notice{
		Env:       "production",
		Revision:  "aaa",
		Class:     "RuntimeError",
		Message:   "boom",
		Backtrace: []string{"app/jobs/work_job.rb:4:in `perform`"},
	}
	first, err := store.Ingest(ctx, "app-token", n)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ack(ctx, first.Group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Suppress(ctx, first.Group.ID); err != nil {
		t.Fatal(err)
	}
	n.Revision = "bbb"
	if _, err := store.Ingest(ctx, "app-token", n); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Unsuppress(ctx, first.Group.ID); err != nil {
		t.Fatal(err)
	}

	var outbound int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_events WHERE group_id=? AND event_type='error.regressed'`, first.Group.ID).Scan(&outbound); err != nil || outbound != 0 {
		t.Fatalf("retroactive outbound=%d err=%v", outbound, err)
	}

	n.Revision = "ccc"
	result, err := store.Ingest(ctx, "app-token", n)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Regressed || filer.reopened != 1 {
		t.Fatalf("regressed=%v reopened=%d", result.Regressed, filer.reopened)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_events WHERE group_id=? AND event_type='error.regressed'`, first.Group.ID).Scan(&outbound); err != nil || outbound != 1 {
		t.Fatalf("future outbound=%d err=%v", outbound, err)
	}
}

func TestSuppressOwnershipChecks(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	owner, _ := store.UpsertGitHubUser(ctx, 22, "owner", "", "")
	other, _ := store.UpsertGitHubUser(ctx, 23, "other", "", "")
	if err := store.UpsertApp(ctx, "widget", "app-token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	if err := store.AssignLegacyProjects(ctx, owner.ID); err != nil {
		t.Fatal(err)
	}
	result, err := store.Ingest(ctx, "app-token", Notice{Class: "RuntimeError", Backtrace: []string{"app/jobs/x.rb:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.SuppressForUser(ctx, other.ID, result.Group.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account suppress=%v", err)
	}
	if _, err := store.SuppressForUser(ctx, owner.ID, result.Group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UnsuppressForUser(ctx, other.ID, result.Group.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account unsuppress=%v", err)
	}
}

func TestSuppressedNewGroupStillOpensIssueOnce(t *testing.T) {
	store, filer := testStore(t)
	ctx := context.Background()
	if err := store.UpsertApp(ctx, "widget", "app-token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	result, err := store.Ingest(ctx, "app-token", Notice{Class: "RuntimeError", Backtrace: []string{"app/jobs/x.rb:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Suppress(ctx, result.Group.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(ctx, "app-token", Notice{Class: "RuntimeError", Backtrace: []string{"app/jobs/x.rb:1"}}); err != nil {
		t.Fatal(err)
	}
	if filer.opened != 1 || filer.reopened != 0 {
		t.Fatalf("opened=%d reopened=%d", filer.opened, filer.reopened)
	}
}

func TestSuppressedAtIsReturnedInGroup(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	if err := store.UpsertApp(ctx, "widget", "app-token", ""); err != nil {
		t.Fatal(err)
	}
	result, err := store.Ingest(ctx, "app-token", Notice{Class: "RuntimeError", Backtrace: []string{"app/jobs/x.rb:1"}})
	if err != nil {
		t.Fatal(err)
	}
	group, err := store.Suppress(ctx, result.Group.ID)
	if err != nil || group.SuppressedAt == nil {
		t.Fatalf("group=%#v err=%v", group, err)
	}
	if !group.SuppressedAt.Equal(store.now()) {
		t.Fatalf("suppressed_at=%v now=%v", group.SuppressedAt, store.now())
	}
}
