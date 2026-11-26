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
	if result.Group.GitHubIssueNumber != 0 || filer.opened != 0 {
		t.Fatalf("ingest should enqueue asynchronously without calling filer: issue=%d opened=%d", result.Group.GitHubIssueNumber, filer.opened)
	}
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if filer.opened != 1 {
		t.Fatalf("expected failed first attempt, opened=%d", filer.opened)
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
	if result.Group.GitHubIssueNumber != 0 || filer.opened != 0 {
		t.Fatalf("ingest should be async: issue=%d opened=%d", result.Group.GitHubIssueNumber, filer.opened)
	}
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if filer.opened != 1 {
		t.Fatalf("first attempt opened=%d", filer.opened)
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
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
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
	for attempt := 0; attempt < maxIssueJobAttempts; attempt++ {
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
	if !first.Created || filer.opened != 0 {
		t.Fatalf("first ingest created=%v opened=%d (expected async)", first.Created, filer.opened)
	}
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if filer.opened != 1 {
		t.Fatalf("after worker pass opened=%d", filer.opened)
	}

	n.Message = "undefined method foo for nil (id 99)"
	second, err := store.Ingest(ctx, "app-token", n)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
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
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
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
	if !result.Regressed || filer.reopened != 0 {
		t.Fatalf("regressed=%v reopened=%d (expected async)", result.Regressed, filer.reopened)
	}
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if filer.reopened != 1 {
		t.Fatalf("after worker pass reopened=%d", filer.reopened)
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
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ack(ctx, first.Group.ID); err != nil {
		t.Fatal(err)
	}
	result, err := store.Ingest(ctx, "app-token", n)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ProcessIssueJobsOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if result.Regressed || filer.reopened != 0 {
		t.Fatalf("should not reopen same revision: %#v reopened=%d", result, filer.reopened)
	}
}

type slowFiler struct {
	delay time.Duration
	inner fakeFiler
}

func (f *slowFiler) FindByMarker(ctx context.Context, installationID int64, repo, marker string, since time.Time) (int, string, bool, error) {
	time.Sleep(f.delay)
	return f.inner.FindByMarker(ctx, installationID, repo, marker, since)
}

func (f *slowFiler) Open(ctx context.Context, installationID int64, repo, title, body string) (int, string, error) {
	time.Sleep(f.delay)
	return f.inner.Open(ctx, installationID, repo, title, body)
}

func (f *slowFiler) Reopen(ctx context.Context, installationID int64, repo string, number int) error {
	time.Sleep(f.delay)
	return f.inner.Reopen(ctx, installationID, repo, number)
}

func TestIngestDecoupledFromSlowFiler(t *testing.T) {
	ctx := context.Background()
	filer := &slowFiler{delay: 150 * time.Millisecond}
	store, err := Open(filepath.Join(t.TempDir(), "async.db"), nil, filer)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.UpsertApp(ctx, "widget", "tok", "example/widget"); err != nil {
		t.Fatal(err)
	}

	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.RunIssueWorker(workerCtx, nil)
	}()
	defer func() {
		cancel()
		<-done
	}()

	start := time.Now()
	res, err := store.Ingest(ctx, "tok", Notice{
		Class:     "TimeoutError",
		Message:   "slow gh test",
		Backtrace: []string{"app/models/slow.rb:1"},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed >= 100*time.Millisecond {
		t.Fatalf("Ingest blocked on slow filer: %s", elapsed)
	}
	if res.Group.GitHubIssueNumber != 0 {
		t.Fatalf("expected async issue filing, got issue=%d", res.Group.GitHubIssueNumber)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		g, err := store.Get(ctx, res.Group.ID)
		if err == nil && g.GitHubIssueNumber == 1 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("async issue worker did not deliver GitHub issue in time")
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

func TestSQLiteDSNPragmasOnFileAndMemory(t *testing.T) {
	ctx := context.Background()

	// File-backed store: verify all DSN pragmas apply and survive connection recycling.
	fileStore, err := Open(filepath.Join(t.TempDir(), "pragmas.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer fileStore.Close()
	// Force connection recycling so a brand-new connection is opened from the pool.
	fileStore.db.SetMaxIdleConns(0)
	fileStore.db.SetMaxIdleConns(1)

	var journalMode string
	var busyTimeout, synchronous, foreignKeys, journalSizeLimit int64
	if err := fileStore.db.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if err := fileStore.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := fileStore.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if err := fileStore.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := fileStore.db.QueryRowContext(ctx, `PRAGMA journal_size_limit`).Scan(&journalSizeLimit); err != nil {
		t.Fatal(err)
	}
	if strings.ToLower(journalMode) != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}
	if synchronous != 1 {
		t.Fatalf("synchronous = %d, want 1 (NORMAL)", synchronous)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}
	if journalSizeLimit != 67108864 {
		t.Fatalf("journal_size_limit = %d, want 67108864", journalSizeLimit)
	}

	// :memory: safe fallback: must initialize cleanly and enforce busy_timeout, synchronous, foreign_keys.
	memStore, err := Open(":memory:", nil, nil)
	if err != nil {
		t.Fatalf("open :memory:: %v", err)
	}
	defer memStore.Close()
	if err := memStore.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatal(err)
	}
	if err := memStore.db.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatal(err)
	}
	if err := memStore.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if busyTimeout != 5000 || synchronous != 1 || foreignKeys != 1 {
		t.Fatalf(":memory: pragmas busy=%d sync=%d fk=%d", busyTimeout, synchronous, foreignKeys)
	}
}
