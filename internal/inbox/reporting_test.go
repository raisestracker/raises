package inbox

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeBriefSender struct {
	sends int
	err   error
	body  string
}

func (f *fakeBriefSender) Send(_ context.Context, _, body, _ string) error {
	f.sends++
	f.body = body
	return f.err
}

func TestBriefMetricsRemainAccurateAfterNoticePruning(t *testing.T) {
	ctx := context.Background()
	location, err := time.LoadLocation("America/Phoenix")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 14, 10, 0, 0, 0, location)
	store, err := Open(filepath.Join(t.TempDir(), "raises.db"), func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.keepNotices = 1
	if _, err := store.UpsertGitHubUser(ctx, 6334, "demo", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertApp(ctx, "widget", "token", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateEvent(ctx, "token", EventInput{Message: "Deploy finished"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.Ingest(ctx, "token", Notice{Class: "RuntimeError", Backtrace: []string{"app/job.rb:1"}}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Hour)
	}
	var retained int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM notices`).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 1 {
		t.Fatalf("retained notices=%d", retained)
	}

	brief, err := store.BuildBrief(ctx, BriefDaily, time.Date(2026, 8, 15, 0, 0, 0, 0, location), location)
	if err != nil {
		t.Fatal(err)
	}
	if brief.Current.Occurrences != 3 || brief.Current.NewErrorGroups != 1 || brief.Current.AffectedProjects != 1 {
		t.Fatalf("metrics=%#v", brief.Current)
	}
	if brief.Current.NewProjects != 1 || brief.Totals.ActiveProjects != 1 || brief.Totals.Users != 1 {
		t.Fatalf("growth=%#v totals=%#v", brief.Current, brief.Totals)
	}
	_, body, _ := RenderBrief(brief)
	for _, expected := range []string{"Occurrences: 3", "New error groups: 1", "Informational notices: 1", "Webhook deliveries retrying: 0", "ntfy deliveries dead: 0", "widget - 3", "RuntimeError in app/job.rb:1 - 3"} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing %q in:\n%s", expected, body)
		}
	}
}

func TestFridayWeeklyReplacesDailyAndSaturdayCatchesWeekly(t *testing.T) {
	ctx := context.Background()
	location, _ := time.LoadLocation("America/Phoenix")
	now := time.Date(2026, 8, 14, 7, 0, 0, 0, location) // Friday.
	store, err := Open(filepath.Join(t.TempDir(), "raises.db"), func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conf := BriefConfig{Location: location, SendHour: 7}
	if err := store.EnqueueDueBriefs(ctx, conf); err != nil {
		t.Fatal(err)
	}
	if err := store.EnqueueDueBriefs(ctx, conf); err != nil {
		t.Fatal(err)
	}
	var daily, weekly int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM report_deliveries WHERE kind='daily'`).Scan(&daily); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM report_deliveries WHERE kind='weekly'`).Scan(&weekly); err != nil {
		t.Fatal(err)
	}
	if daily != 0 || weekly != 1 {
		t.Fatalf("Friday daily=%d weekly=%d", daily, weekly)
	}

	now = time.Date(2026, 8, 15, 7, 0, 0, 0, location) // Saturday.
	if err := store.EnqueueDueBriefs(ctx, conf); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM report_deliveries WHERE kind='daily'`).Scan(&daily); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM report_deliveries WHERE kind='weekly'`).Scan(&weekly); err != nil {
		t.Fatal(err)
	}
	if daily != 1 || weekly != 1 {
		t.Fatalf("Saturday daily=%d weekly=%d", daily, weekly)
	}
}

func TestBriefDeliveryIsDurableAndRetries(t *testing.T) {
	ctx := context.Background()
	location, _ := time.LoadLocation("America/Phoenix")
	now := time.Date(2026, 8, 15, 7, 0, 0, 0, location)
	store, err := Open(filepath.Join(t.TempDir(), "raises.db"), func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	conf := BriefConfig{Location: location, SendHour: 7}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO report_deliveries(kind, report_date, state, next_attempt_at_unix, created_at_unix)
		VALUES ('daily', '2026-08-15', 'pending', ?, ?)
	`, now.UTC().Unix(), now.UTC().Unix()); err != nil {
		t.Fatal(err)
	}
	sender := &fakeBriefSender{err: errors.New("SES unavailable")}
	if err := store.ProcessBriefDeliveryOnce(ctx, conf, sender); err == nil || !strings.Contains(err.Error(), "SES unavailable") {
		t.Fatalf("error=%v", err)
	}
	var state string
	var attempts int
	if err := store.db.QueryRowContext(ctx, `SELECT state, attempts FROM report_deliveries WHERE kind='daily'`).Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	if state != "pending" || attempts != 1 {
		t.Fatalf("state=%s attempts=%d", state, attempts)
	}

	now = now.Add(3 * time.Second)
	sender.err = nil
	if err := store.ProcessBriefDeliveryOnce(ctx, conf, sender); err != nil {
		t.Fatal(err)
	}
	if err := store.ProcessBriefDeliveryOnce(ctx, conf, sender); err != nil {
		t.Fatal(err)
	}
	if sender.sends != 2 {
		t.Fatalf("sends=%d", sender.sends)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT state FROM report_deliveries WHERE kind='daily'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "done" {
		t.Fatalf("state=%s", state)
	}
	delivered, err := store.BriefWasDelivered(ctx, BriefDaily, time.Date(2026, 8, 15, 0, 0, 0, 0, location))
	if err != nil || !delivered {
		t.Fatalf("delivered=%v error=%v", delivered, err)
	}
}

func TestRecordBriefDeliveredMakesCanaryIdempotent(t *testing.T) {
	ctx := context.Background()
	location, _ := time.LoadLocation("America/Phoenix")
	now := time.Date(2026, 8, 15, 6, 0, 0, 0, location)
	store, err := Open(filepath.Join(t.TempDir(), "raises.db"), func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	date := localMidnight(now, location)
	if err := store.RecordBriefDelivered(ctx, BriefDaily, date); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBriefDelivered(ctx, BriefDaily, date); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM report_deliveries`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("deliveries=%d", count)
	}
}
