package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	BriefDaily  = "daily"
	BriefWeekly = "weekly"

	briefJobLease       = 2 * time.Minute
	maxBriefJobAttempts = 20
)

type BriefConfig struct {
	Location   *time.Location
	SendHour   int
	SendMinute int
}

type BriefSender interface {
	Send(ctx context.Context, subject, body, messageID string) error
}

type BriefMetrics struct {
	NewUsers         int
	NewProjects      int
	Occurrences      int
	NewErrorGroups   int
	AffectedProjects int
	Notices          int
}

type BriefTotals struct {
	Users             int
	ActiveProjects    int
	Unacknowledged    int
	IssueJobsRetrying int
	IssueJobsDead     int
	EmailReportsDead  int
	WebhooksRetrying  int
	WebhooksDead      int
	NtfyDead          int
}

type BriefRank struct {
	Name        string
	Occurrences int
}

type Brief struct {
	Kind            string
	ReportDate      time.Time
	PeriodStart     time.Time
	ComparisonStart time.Time
	PeriodEnd       time.Time
	Current         BriefMetrics
	Previous        BriefMetrics
	Totals          BriefTotals
	TopProjects     []BriefRank
	TopErrors       []BriefRank
	CoverageStart   time.Time
}

type BriefDelivery struct {
	ID         int64
	Kind       string
	ReportDate string
	Attempts   int
}

func (s *Store) initializeReporting(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS error_occurrence_buckets (
			hour_start_unix INTEGER NOT NULL,
			project_id TEXT NOT NULL,
			group_id INTEGER NOT NULL,
			occurrences INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY(hour_start_unix, project_id, group_id),
			FOREIGN KEY(group_id) REFERENCES error_groups(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_occurrence_buckets_project ON error_occurrence_buckets(project_id, hour_start_unix)`,
		`CREATE TABLE IF NOT EXISTS service_metadata (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS report_deliveries (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			kind TEXT NOT NULL,
			report_date TEXT NOT NULL,
			state TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at_unix INTEGER NOT NULL,
			lease_expires_at_unix INTEGER,
			last_error TEXT NOT NULL DEFAULT '',
			created_at_unix INTEGER NOT NULL,
			completed_at_unix INTEGER,
			failed_at_unix INTEGER,
			UNIQUE(kind, report_date)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_report_deliveries_pending ON report_deliveries(state, next_attempt_at_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_users_created_at ON users(created_at_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_apps_created_at ON apps(created_at_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_error_groups_first_seen ON error_groups(first_seen_unix)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize reporting: %w", err)
		}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO service_metadata(key, value)
		VALUES ('occurrence_metrics_started_at_unix', ?)
	`, strconv.FormatInt(s.now().UTC().Unix(), 10))
	return err
}

func (s *Store) BuildBrief(ctx context.Context, kind string, reportDate time.Time, location *time.Location) (Brief, error) {
	if location == nil {
		return Brief{}, fmt.Errorf("report timezone is required")
	}
	if kind != BriefDaily && kind != BriefWeekly {
		return Brief{}, fmt.Errorf("unknown brief kind %q", kind)
	}
	reportDate = localMidnight(reportDate.In(location), location)
	days := 1
	if kind == BriefWeekly {
		days = 7
	}
	end := reportDate
	start := end.AddDate(0, 0, -days)
	priorStart := start.AddDate(0, 0, -days)

	current, err := s.briefMetrics(ctx, start, end)
	if err != nil {
		return Brief{}, err
	}
	previous, err := s.briefMetrics(ctx, priorStart, start)
	if err != nil {
		return Brief{}, err
	}
	totals, err := s.briefTotals(ctx)
	if err != nil {
		return Brief{}, err
	}
	topProjects, err := s.topProjects(ctx, start, end, 3)
	if err != nil {
		return Brief{}, err
	}
	topErrors, err := s.topErrors(ctx, start, end, 3)
	if err != nil {
		return Brief{}, err
	}
	coverageStart, err := s.metricsCoverageStart(ctx)
	if err != nil {
		return Brief{}, err
	}
	return Brief{
		Kind: kind, ReportDate: reportDate, PeriodStart: start, PeriodEnd: end,
		ComparisonStart: priorStart,
		Current:         current, Previous: previous, Totals: totals,
		TopProjects: topProjects, TopErrors: topErrors, CoverageStart: coverageStart,
	}, nil
}

func (s *Store) briefMetrics(ctx context.Context, start, end time.Time) (BriefMetrics, error) {
	var metrics BriefMetrics
	startUnix, endUnix := start.UTC().Unix(), end.UTC().Unix()
	queries := []struct {
		dest  *int
		query string
		args  []any
	}{
		{&metrics.NewUsers, `SELECT COUNT(*) FROM users WHERE created_at_unix >= ? AND created_at_unix < ?`, []any{startUnix, endUnix}},
		{&metrics.NewProjects, `SELECT COUNT(*) FROM apps WHERE created_at_unix >= ? AND created_at_unix < ?`, []any{startUnix, endUnix}},
		{&metrics.Occurrences, `SELECT COALESCE(SUM(occurrences), 0) FROM error_occurrence_buckets WHERE hour_start_unix >= ? AND hour_start_unix < ?`, []any{startUnix, endUnix}},
		{&metrics.NewErrorGroups, `SELECT COUNT(*) FROM error_groups WHERE first_seen_unix >= ? AND first_seen_unix < ?`, []any{startUnix, endUnix}},
		{&metrics.AffectedProjects, `SELECT COUNT(DISTINCT project_id) FROM error_occurrence_buckets WHERE hour_start_unix >= ? AND hour_start_unix < ?`, []any{startUnix, endUnix}},
		{&metrics.Notices, `SELECT COUNT(*) FROM events WHERE created_at_unix >= ? AND created_at_unix < ?`, []any{startUnix, endUnix}},
	}
	for _, item := range queries {
		if err := s.db.QueryRowContext(ctx, item.query, item.args...).Scan(item.dest); err != nil {
			return BriefMetrics{}, err
		}
	}
	return metrics, nil
}

func (s *Store) briefTotals(ctx context.Context) (BriefTotals, error) {
	var totals BriefTotals
	queries := []struct {
		dest  *int
		query string
	}{
		{&totals.Users, `SELECT COUNT(*) FROM users`},
		{&totals.ActiveProjects, `SELECT COUNT(*) FROM apps WHERE archived_at_unix IS NULL`},
		{&totals.Unacknowledged, `SELECT COUNT(*) FROM error_groups WHERE acked_at_unix IS NULL AND suppressed_at_unix IS NULL`},
		{&totals.IssueJobsRetrying, `SELECT COUNT(*) FROM issue_jobs WHERE state IN ('pending','working')`},
		{&totals.IssueJobsDead, `SELECT COUNT(*) FROM issue_jobs WHERE state = 'dead'`},
		{&totals.EmailReportsDead, `SELECT COUNT(*) FROM report_deliveries WHERE state = 'dead'`},
		{&totals.WebhooksRetrying, `SELECT COUNT(*) FROM outbound_deliveries WHERE destination_kind='webhook' AND state IN ('pending','working')`},
		{&totals.WebhooksDead, `SELECT COUNT(*) FROM outbound_deliveries WHERE destination_kind='webhook' AND state='dead'`},
		{&totals.NtfyDead, `SELECT COUNT(*) FROM outbound_deliveries WHERE destination_kind='ntfy' AND state='dead'`},
	}
	for _, item := range queries {
		if err := s.db.QueryRowContext(ctx, item.query).Scan(item.dest); err != nil {
			return BriefTotals{}, err
		}
	}
	return totals, nil
}

func (s *Store) topProjects(ctx context.Context, start, end time.Time, limit int) ([]BriefRank, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(a.display_name, ''), a.name), SUM(b.occurrences)
		FROM error_occurrence_buckets b JOIN apps a ON a.project_id = b.project_id
		WHERE b.hour_start_unix >= ? AND b.hour_start_unix < ?
		GROUP BY b.project_id
		ORDER BY SUM(b.occurrences) DESC, a.name
		LIMIT ?
	`, start.UTC().Unix(), end.UTC().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBriefRanks(rows)
}

func (s *Store) topErrors(ctx context.Context, start, end time.Time, limit int) ([]BriefRank, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.class || ' in ' || g.location, SUM(b.occurrences)
		FROM error_occurrence_buckets b JOIN error_groups g ON g.id = b.group_id
		WHERE b.hour_start_unix >= ? AND b.hour_start_unix < ?
		GROUP BY b.group_id
		ORDER BY SUM(b.occurrences) DESC, g.class, g.location
		LIMIT ?
	`, start.UTC().Unix(), end.UTC().Unix(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanBriefRanks(rows)
}

func scanBriefRanks(rows *sql.Rows) ([]BriefRank, error) {
	var ranks []BriefRank
	for rows.Next() {
		var rank BriefRank
		if err := rows.Scan(&rank.Name, &rank.Occurrences); err != nil {
			return nil, err
		}
		ranks = append(ranks, rank)
	}
	return ranks, rows.Err()
}

func (s *Store) metricsCoverageStart(ctx context.Context) (time.Time, error) {
	var value string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM service_metadata WHERE key = 'occurrence_metrics_started_at_unix'`).Scan(&value); err != nil {
		return time.Time{}, err
	}
	unix, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(unix, 0).UTC(), nil
}

func RenderBrief(brief Brief) (subject, body, messageID string) {
	title := strings.ToUpper(brief.Kind[:1]) + brief.Kind[1:]
	subject = fmt.Sprintf("[Raises] %s brief - %s", title, brief.ReportDate.Format("2006-01-02"))
	messageID = fmt.Sprintf("raises-%s-%s@raises.dev", brief.Kind, brief.ReportDate.Format("2006-01-02"))

	var out strings.Builder
	fmt.Fprintf(&out, "Raises %s brief\n", brief.Kind)
	fmt.Fprintf(&out, "%s through %s\n\n", brief.PeriodStart.Format("Jan 2, 2006"), brief.PeriodEnd.AddDate(0, 0, -1).Format("Jan 2, 2006"))
	out.WriteString("GROWTH\n")
	fmt.Fprintf(&out, "New users: %s (%d total)\n", metricTrend(brief.Current.NewUsers, brief.Previous.NewUsers), brief.Totals.Users)
	fmt.Fprintf(&out, "New projects/apps: %s (%d active)\n\n", metricTrend(brief.Current.NewProjects, brief.Previous.NewProjects), brief.Totals.ActiveProjects)
	out.WriteString("ERRORS\n")
	fmt.Fprintf(&out, "Occurrences: %s\n", metricTrend(brief.Current.Occurrences, brief.Previous.Occurrences))
	fmt.Fprintf(&out, "New error groups: %s\n", metricTrend(brief.Current.NewErrorGroups, brief.Previous.NewErrorGroups))
	fmt.Fprintf(&out, "Projects affected: %s\n", metricTrend(brief.Current.AffectedProjects, brief.Previous.AffectedProjects))
	fmt.Fprintf(&out, "Unacknowledged now: %d\n\n", brief.Totals.Unacknowledged)
	out.WriteString("NOTICES\n")
	fmt.Fprintf(&out, "Informational notices: %s\n\n", metricTrend(brief.Current.Notices, brief.Previous.Notices))
	out.WriteString("DELIVERY HEALTH\n")
	fmt.Fprintf(&out, "GitHub issue jobs retrying: %d\n", brief.Totals.IssueJobsRetrying)
	fmt.Fprintf(&out, "GitHub issue jobs dead: %d\n", brief.Totals.IssueJobsDead)
	fmt.Fprintf(&out, "Email reports dead: %d\n", brief.Totals.EmailReportsDead)
	fmt.Fprintf(&out, "Webhook deliveries retrying: %d\n", brief.Totals.WebhooksRetrying)
	fmt.Fprintf(&out, "Webhook deliveries dead: %d\n", brief.Totals.WebhooksDead)
	fmt.Fprintf(&out, "ntfy deliveries dead: %d\n", brief.Totals.NtfyDead)
	writeRanks(&out, "TOP PROJECTS", brief.TopProjects)
	writeRanks(&out, "TOP ERRORS", brief.TopErrors)

	if brief.CoverageStart.After(brief.ComparisonStart.UTC()) {
		fmt.Fprintf(&out, "\nNote: occurrence trend coverage begins %s; this comparison is partial.\n", brief.CoverageStart.Format(time.RFC3339))
	}
	return subject, strings.TrimSpace(out.String()) + "\n", messageID
}

func metricTrend(current, previous int) string {
	delta := current - previous
	if previous == 0 {
		if current == 0 {
			return "0 (no change)"
		}
		return fmt.Sprintf("%d (new; previous period was 0)", current)
	}
	percent := float64(delta) / float64(previous) * 100
	return fmt.Sprintf("%d (%+d, %+.0f%%)", current, delta, percent)
}

func writeRanks(out *strings.Builder, heading string, ranks []BriefRank) {
	if len(ranks) == 0 {
		return
	}
	out.WriteString("\n" + heading + "\n")
	for i, rank := range ranks {
		fmt.Fprintf(out, "%d. %s - %d\n", i+1, rank.Name, rank.Occurrences)
	}
}

func (s *Store) EnqueueDueBriefs(ctx context.Context, conf BriefConfig) error {
	if conf.Location == nil {
		return fmt.Errorf("report timezone is required")
	}
	now := s.now().In(conf.Location)
	today := localMidnight(now, conf.Location)
	dueAt := today.Add(time.Duration(conf.SendHour)*time.Hour + time.Duration(conf.SendMinute)*time.Minute)

	var due []struct {
		kind string
		date time.Time
	}
	if !now.Before(dueAt) && today.Weekday() != time.Friday {
		due = append(due, struct {
			kind string
			date time.Time
		}{BriefDaily, today})
	}
	friday := today
	for friday.Weekday() != time.Friday {
		friday = friday.AddDate(0, 0, -1)
	}
	fridayDue := friday.Add(time.Duration(conf.SendHour)*time.Hour + time.Duration(conf.SendMinute)*time.Minute)
	if !now.Before(fridayDue) && now.Sub(fridayDue) <= 7*24*time.Hour {
		due = append(due, struct {
			kind string
			date time.Time
		}{BriefWeekly, friday})
	}
	sort.Slice(due, func(i, j int) bool { return due[i].date.Before(due[j].date) })
	for _, item := range due {
		nowUnix := s.now().UTC().Unix()
		if _, err := s.db.ExecContext(ctx, `
			INSERT OR IGNORE INTO report_deliveries(kind, report_date, state, next_attempt_at_unix, created_at_unix)
			VALUES (?, ?, 'pending', ?, ?)
		`, item.kind, item.date.Format("2006-01-02"), nowUnix, nowUnix); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) BriefWasDelivered(ctx context.Context, kind string, reportDate time.Time) (bool, error) {
	var delivered int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM report_deliveries
		WHERE kind = ? AND report_date = ? AND state = 'done'
	`, kind, reportDate.Format("2006-01-02")).Scan(&delivered)
	return delivered > 0, err
}

func (s *Store) RecordBriefDelivered(ctx context.Context, kind string, reportDate time.Time) error {
	now := s.now().UTC().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO report_deliveries(
			kind, report_date, state, next_attempt_at_unix, created_at_unix, completed_at_unix
		) VALUES (?, ?, 'done', ?, ?, ?)
		ON CONFLICT(kind, report_date) DO UPDATE SET
			state = 'done', completed_at_unix = excluded.completed_at_unix,
			failed_at_unix = NULL, lease_expires_at_unix = NULL, last_error = ''
	`, kind, reportDate.Format("2006-01-02"), now, now, now)
	return err
}

func (s *Store) ProcessBriefDeliveryOnce(ctx context.Context, conf BriefConfig, sender BriefSender) error {
	if sender == nil {
		return nil
	}
	now := s.now().UTC()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE report_deliveries SET state = 'pending', next_attempt_at_unix = ?, lease_expires_at_unix = NULL
		WHERE state = 'working' AND lease_expires_at_unix <= ?
	`, now.Unix(), now.Unix()); err != nil {
		return err
	}
	var delivery BriefDelivery
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, report_date, attempts FROM report_deliveries
		WHERE state = 'pending' AND next_attempt_at_unix <= ? ORDER BY report_date, id LIMIT 1
	`, now.Unix()).Scan(&delivery.ID, &delivery.Kind, &delivery.ReportDate, &delivery.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	claimed, err := s.db.ExecContext(ctx, `
		UPDATE report_deliveries SET state = 'working', lease_expires_at_unix = ?
		WHERE id = ? AND state = 'pending'
	`, now.Add(briefJobLease).Unix(), delivery.ID)
	if err != nil {
		return err
	}
	if rows, _ := claimed.RowsAffected(); rows != 1 {
		return nil
	}
	reportDate, err := time.ParseInLocation("2006-01-02", delivery.ReportDate, conf.Location)
	if err == nil {
		var brief Brief
		brief, err = s.BuildBrief(ctx, delivery.Kind, reportDate, conf.Location)
		if err == nil {
			subject, body, messageID := RenderBrief(brief)
			err = sender.Send(ctx, subject, body, messageID)
		}
	}
	if err == nil {
		_, err = s.db.ExecContext(ctx, `
			UPDATE report_deliveries SET state = 'done', completed_at_unix = ?, failed_at_unix = NULL,
				lease_expires_at_unix = NULL, last_error = '' WHERE id = ?
		`, now.Unix(), delivery.ID)
		return err
	}

	delivery.Attempts++
	if delivery.Attempts >= maxBriefJobAttempts {
		_, updateErr := s.db.ExecContext(ctx, `
			UPDATE report_deliveries SET state = 'dead', attempts = ?, failed_at_unix = ?,
				lease_expires_at_unix = NULL, last_error = ? WHERE id = ?
		`, delivery.Attempts, now.Unix(), truncateError(err), delivery.ID)
		if updateErr != nil {
			return updateErr
		}
		return err
	}
	delay := time.Duration(1<<min(delivery.Attempts, 10)) * time.Second
	_, updateErr := s.db.ExecContext(ctx, `
		UPDATE report_deliveries SET state = 'pending', attempts = ?, next_attempt_at_unix = ?,
			lease_expires_at_unix = NULL, last_error = ? WHERE id = ?
	`, delivery.Attempts, now.Add(delay).Unix(), truncateError(err), delivery.ID)
	if updateErr != nil {
		return updateErr
	}
	return err
}

func (s *Store) RunBriefWorker(ctx context.Context, conf BriefConfig, sender BriefSender, onError func(error)) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	process := func() {
		if err := s.EnqueueDueBriefs(ctx, conf); err != nil {
			onError(err)
			return
		}
		for i := 0; i < 5; i++ {
			if err := s.ProcessBriefDeliveryOnce(ctx, conf, sender); err != nil {
				onError(err)
				return
			}
		}
	}
	process()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			process()
		}
	}
}

func localMidnight(t time.Time, location *time.Location) time.Time {
	year, month, day := t.In(location).Date()
	return time.Date(year, month, day, 0, 0, 0, 0, location)
}
