package inbox

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeFiler struct {
	opened    int
	reopened  int
	nextNum   int
	fail      bool
	issues    map[string]int
	ambiguous bool
}

func (f *fakeFiler) FindByMarker(_ context.Context, _ int64, _ string, marker string, _ time.Time) (int, string, bool, error) {
	if number := f.issues[marker]; number > 0 {
		return number, "https://github.com/example/widget/issues/" + fmt.Sprint(number), true, nil
	}
	return 0, "", false, nil
}

func (f *fakeFiler) Open(_ context.Context, _ int64, _ string, _ string, body string) (int, string, error) {
	f.opened++
	if f.fail && !f.ambiguous {
		return 0, "", errors.New("github unavailable")
	}
	f.nextNum++
	if f.issues == nil {
		f.issues = map[string]int{}
	}
	if start := strings.Index(body, "<!-- raises-delivery:"); start >= 0 {
		if end := strings.Index(body[start:], " -->"); end >= 0 {
			f.issues[body[start:start+end+4]] = f.nextNum
		}
	}
	if f.ambiguous {
		f.ambiguous = false
		return 0, "", errors.New("github response lost")
	}
	return f.nextNum, "https://github.com/example/widget/issues/1", nil
}

func (f *fakeFiler) Reopen(context.Context, int64, string, int) error {
	f.reopened++
	if f.fail {
		return errors.New("github unavailable")
	}
	return nil
}

func TestIssueFailureIsQueuedWithoutRejectingNotice(t *testing.T) {
	ctx := context.Background()
	filer := &fakeFiler{fail: true}
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	store, err := Open(filepath.Join(t.TempDir(), "raises.db"), func() time.Time { return now }, filer)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertApp(ctx, "widget", "token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	result, err := store.Ingest(ctx, "token", Notice{Class: "RuntimeError", Message: "boom", Backtrace: []string{"app/jobs/x.rb:1"}})
	if err != nil {
		t.Fatalf("ingest should survive github failure: %v", err)
	}
	if result.Group.GitHubIssueNumber != 0 {
		t.Fatalf("unexpected issue=%d", result.Group.GitHubIssueNumber)
	}
	filer.fail = false
	now = now.Add(3 * time.Second)
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	group, err := store.Get(ctx, result.Group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if group.GitHubIssueNumber != 1 {
		t.Fatalf("issue=%d", group.GitHubIssueNumber)
	}
}

func TestAmbiguousIssueCreateIsReconciledWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	filer := &fakeFiler{ambiguous: true}
	store, err := Open(filepath.Join(t.TempDir(), "raises.db"), func() time.Time { return now }, filer)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertApp(ctx, "widget", "token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	result, err := store.Ingest(ctx, "token", Notice{Class: "RuntimeError", Message: "boom", Backtrace: []string{"app/jobs/x.rb:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Group.GitHubIssueNumber != 0 || filer.opened != 1 {
		t.Fatalf("first issue=%d opened=%d", result.Group.GitHubIssueNumber, filer.opened)
	}
	now = now.Add(3 * time.Second)
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	group, err := store.Get(ctx, result.Group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if group.GitHubIssueNumber != 1 || filer.opened != 1 {
		t.Fatalf("reconciled issue=%d opened=%d", group.GitHubIssueNumber, filer.opened)
	}
}

func TestIssueJobEmitsGitHubLifecycleWithoutDuplicatingIssue(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	filer := &fakeFiler{}
	store, err := Open(filepath.Join(t.TempDir(), "raises.db"), func() time.Time { return now }, filer)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertApp(ctx, "widget", "token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(ctx, "token", Notice{Class: "RuntimeError", Backtrace: []string{"app/jobs/x.rb:1"}}); err != nil {
		t.Fatal(err)
	}
	if filer.opened != 1 {
		t.Fatalf("opened=%d", filer.opened)
	}
	var lifecycle int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_events WHERE event_type='github_issue.opened'`).Scan(&lifecycle); err != nil {
		t.Fatal(err)
	}
	if lifecycle != 1 {
		t.Fatalf("lifecycle=%d", lifecycle)
	}
	now = now.Add(time.Hour)
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if filer.opened != 1 {
		t.Fatalf("opened=%d", filer.opened)
	}
}

func TestIssueJobLeaseRecoveryAndDeadLetterRetry(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	filer := &fakeFiler{fail: true}
	store, err := Open(filepath.Join(t.TempDir(), "raises.db"), func() time.Time { return now }, filer)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	deadDeliveries := 0
	store.OnIssueJobDead(func(delivery IssueDelivery) {
		deadDeliveries++
		if delivery.State != "dead" || delivery.Attempts != maxIssueJobAttempts {
			t.Errorf("delivery=%#v", delivery)
		}
	})
	user, _ := store.UpsertGitHubUser(ctx, 6334, "demo", "", "")
	if err := store.UpsertApp(ctx, "widget", "token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	if err := store.AssignLegacyProjects(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	result, err := store.Ingest(ctx, "token", Notice{Class: "RuntimeError", Backtrace: []string{"app/jobs/x.rb:1"}})
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 1; attempt < maxIssueJobAttempts; attempt++ {
		now = now.Add(20 * time.Minute)
		if err := store.ProcessIssueJobsOnce(ctx); err != nil {
			t.Fatal(err)
		}
	}
	health, err := store.IssueDeliveryHealthForUser(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if health.Dead != 1 || health.Retrying != 0 || len(health.Problems) != 1 {
		t.Fatalf("health=%#v", health)
	}
	if deadDeliveries != 1 {
		t.Fatalf("dead deliveries=%d", deadDeliveries)
	}
	jobID := health.Problems[0].ID
	other, _ := store.UpsertGitHubUser(ctx, 99, "other", "", "")
	if err := store.RetryIssueJobForUser(ctx, other.ID, jobID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account retry=%v", err)
	}
	if err := store.RetryIssueJobForUser(ctx, user.ID, jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE issue_jobs SET state='working',lease_expires_at_unix=? WHERE id=?`, now.Add(time.Minute).Unix(), jobID); err != nil {
		t.Fatal(err)
	}
	filer.fail = false
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	group, _ := store.Get(ctx, result.Group.ID)
	if group.GitHubIssueNumber != 0 {
		t.Fatal("active working lease was stolen")
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE issue_jobs SET lease_expires_at_unix=? WHERE id=?`, now.Add(-time.Second).Unix(), jobID); err != nil {
		t.Fatal(err)
	}
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	group, _ = store.Get(ctx, result.Group.ID)
	if group.GitHubIssueNumber == 0 {
		t.Fatal("expired working lease was not recovered")
	}
}

func TestIngestGroupsAndOpensIssueOnce(t *testing.T) {
	store, filer := testStore(t)
	ctx := context.Background()
	if err := store.UpsertApp(ctx, "widget", "app-token", "example/widget"); err != nil {
		t.Fatal(err)
	}

	n := Notice{
		Env:       "production",
		Revision:  "aaa",
		Class:     "NoMethodError",
		Message:   "undefined method foo for nil",
		Backtrace: []string{"app/models/user.rb:12:in `foo`"},
	}
	first, err := store.Ingest(ctx, "app-token", n)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || filer.opened != 1 {
		t.Fatalf("first ingest created=%v opened=%d", first.Created, filer.opened)
	}

	n.Message = "undefined method foo for nil (id 99)"
	second, err := store.Ingest(ctx, "app-token", n)
	if err != nil {
		t.Fatal(err)
	}
	if second.Created || second.Group.Count != 2 || filer.opened != 1 {
		t.Fatalf("second ingest = %#v opened=%d", second, filer.opened)
	}
}

func TestRegressionReopensAfterAck(t *testing.T) {
	store, filer := testStore(t)
	ctx := context.Background()
	if err := store.UpsertApp(ctx, "widget", "app-token", "example/widget"); err != nil {
		t.Fatal(err)
	}
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
	n.Revision = "bbb"
	result, err := store.Ingest(ctx, "app-token", n)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Regressed || filer.reopened != 1 {
		t.Fatalf("regressed=%v reopened=%d", result.Regressed, filer.reopened)
	}
	if result.Group.AckedAt != nil {
		t.Fatal("ack should clear on regression")
	}
}

func TestSameRevisionAfterAckDoesNotReopen(t *testing.T) {
	store, filer := testStore(t)
	ctx := context.Background()
	if err := store.UpsertApp(ctx, "widget", "app-token", "example/widget"); err != nil {
		t.Fatal(err)
	}
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
	result, err := store.Ingest(ctx, "app-token", n)
	if err != nil {
		t.Fatal(err)
	}
	if result.Regressed || filer.reopened != 0 {
		t.Fatalf("should not reopen same revision: %#v reopened=%d", result, filer.reopened)
	}
}

func TestBadToken(t *testing.T) {
	store, _ := testStore(t)
	_, err := store.Ingest(context.Background(), "nope", Notice{Class: "E"})
	if err != ErrUnauthorized {
		t.Fatalf("err = %v", err)
	}
}

func testStore(t *testing.T) (*Store, *fakeFiler) {
	t.Helper()
	filer := &fakeFiler{}
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	store, err := Open(filepath.Join(t.TempDir(), "raises.db"), func() time.Time { return now }, filer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, filer
}
