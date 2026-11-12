package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Error status values. Suppressed groups are known false positives and only
// appear under the "all" filter.
const (
	StatusOpen         = "open"
	StatusAcknowledged = "acknowledged"
	StatusFixed        = "fixed"
	StatusSuppressed   = "suppressed"
	StatusAll          = "all"
)

const DefaultPageSize = 25

// ComputeStatus derives the display status. Fixed beats acknowledged, because
// a fix implies an agent acknowledged the error first.
func (g Group) ComputeStatus() string {
	switch {
	case g.SuppressedAt != nil:
		return StatusSuppressed
	case g.ResolvedAt != nil:
		return StatusFixed
	case g.AckedAt != nil:
		return StatusAcknowledged
	default:
		return StatusOpen
	}
}

// NormalizeStatus maps a query value to a known status filter; unknown or
// empty values fall back to the supplied default.
func NormalizeStatus(value, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case StatusOpen, StatusAcknowledged, StatusFixed, StatusAll:
		return strings.ToLower(strings.TrimSpace(value))
	case "acked", "ack":
		return StatusAcknowledged
	}
	return fallback
}

// statusClause returns a SQL condition on error_groups columns prefixed by p.
func statusClause(status, p string) string {
	switch status {
	case StatusOpen:
		return p + "acked_at_unix IS NULL AND " + p + "resolved_at_unix IS NULL AND " + p + "suppressed_at_unix IS NULL"
	case StatusAcknowledged:
		return p + "acked_at_unix IS NOT NULL AND " + p + "resolved_at_unix IS NULL AND " + p + "suppressed_at_unix IS NULL"
	case StatusFixed:
		return p + "resolved_at_unix IS NOT NULL AND " + p + "suppressed_at_unix IS NULL"
	default:
		return ""
	}
}

type ErrorPage struct {
	Groups   []Group `json:"errors"`
	Page     int     `json:"page"`
	PageSize int     `json:"page_size"`
	Total    int     `json:"total"`
	Pages    int     `json:"pages"`
}

// ListPageForUser returns one page of a user's errors, newest activity first.
// project may be a project id or slug; empty means all projects.
func (s *Store) ListPageForUser(ctx context.Context, userID int64, project, status string, page, pageSize int) (ErrorPage, error) {
	if pageSize < 1 || pageSize > 100 {
		pageSize = DefaultPageSize
	}
	if page < 1 {
		page = 1
	}
	where := ` WHERE a.owner_user_id = ?`
	args := []any{userID}
	if project != "" {
		where += ` AND (a.project_id = ? OR a.slug = ?)`
		args = append(args, project, project)
	}
	if clause := statusClause(status, "g."); clause != "" {
		where += ` AND ` + clause
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM error_groups g JOIN apps a ON a.project_id = g.project_id`+where, args...).Scan(&total); err != nil {
		return ErrorPage{}, err
	}
	pages := (total + pageSize - 1) / pageSize
	if pages < 1 {
		pages = 1
	}
	if page > pages {
		page = pages
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.id, g.project_id, g.app_name, g.env, g.fingerprint, g.class, g.location, g.message, g.count,
		       g.first_seen_unix, g.last_seen_unix, g.last_revision, g.github_issue_number, g.github_issue_url,
		       g.acked_at_unix, g.suppressed_at_unix, g.resolved_at_unix, g.sample_json
		FROM error_groups g JOIN apps a ON a.project_id = g.project_id`+where+`
		ORDER BY g.last_seen_unix DESC, g.id DESC LIMIT ? OFFSET ?`,
		append(args, pageSize, (page-1)*pageSize)...)
	if err != nil {
		return ErrorPage{}, err
	}
	defer rows.Close()
	out := ErrorPage{Groups: []Group{}, Page: page, PageSize: pageSize, Total: total, Pages: pages}
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return ErrorPage{}, err
		}
		out.Groups = append(out.Groups, group)
	}
	return out, rows.Err()
}

// ResolveByIssue marks groups fixed when their GitHub issue is closed, and
// returns how many groups changed. Resolving also acknowledges.
func (s *Store) ResolveByIssue(ctx context.Context, repo string, number int) (int, error) {
	return s.setResolvedByIssue(ctx, repo, number, true)
}

// ReopenByIssue clears the fixed state when a GitHub issue is reopened.
func (s *Store) ReopenByIssue(ctx context.Context, repo string, number int) (int, error) {
	return s.setResolvedByIssue(ctx, repo, number, false)
}

func (s *Store) setResolvedByIssue(ctx context.Context, repo string, number int, resolved bool) (int, error) {
	repo = strings.TrimSpace(repo)
	if repo == "" || number < 1 {
		return 0, nil
	}
	now := s.now().UTC().Unix()
	query := `UPDATE error_groups SET resolved_at_unix = NULL
		WHERE github_issue_number = ? AND resolved_at_unix IS NOT NULL
		AND project_id IN (SELECT project_id FROM apps WHERE lower(github_repo) = lower(?))`
	args := []any{number, repo}
	if resolved {
		query = `UPDATE error_groups SET resolved_at_unix = ?, acked_at_unix = COALESCE(acked_at_unix, ?)
		WHERE github_issue_number = ? AND resolved_at_unix IS NULL
		AND project_id IN (SELECT project_id FROM apps WHERE lower(github_repo) = lower(?))`
		args = []any{now, now, number, repo}
	}
	res, err := s.db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

const SparkDays = 14

type Spark []int

func (s Spark) Max() int {
	m := 0
	for _, v := range s {
		if v > m {
			m = v
		}
	}
	return m
}

type DashboardStats struct {
	Open           int
	NewToday       int
	Events24h      int
	Fixed7d        int
	OpenSpark      Spark // open backlog at the end of each day
	NewSpark       Spark // errors first seen per day
	EventsSpark    Spark // occurrences per day
	FixedSpark     Spark // errors fixed per day
	ProjectSummary []ProjectSummary
}

type ProjectSummary struct {
	Project
	Open      int
	Fixed     int
	Total     int
	LastError *time.Time
	Spark     Spark
}

func dayIndex(now, t time.Time) int {
	today := now.UTC().Truncate(24 * time.Hour)
	diff := int(today.Sub(t.UTC().Truncate(24*time.Hour)) / (24 * time.Hour))
	idx := SparkDays - 1 - diff
	if idx < 0 || idx >= SparkDays {
		return -1
	}
	return idx
}

// DashboardForUser aggregates the KPI tiles and per-project summaries with a
// fixed number of queries, independent of the project count.
func (s *Store) DashboardForUser(ctx context.Context, userID int64) (DashboardStats, error) {
	now := s.now().UTC()
	windowStart := now.Truncate(24*time.Hour).AddDate(0, 0, -(SparkDays - 1)).Unix()
	stats := DashboardStats{
		OpenSpark: make(Spark, SparkDays), NewSpark: make(Spark, SparkDays),
		EventsSpark: make(Spark, SparkDays), FixedSpark: make(Spark, SparkDays),
	}

	projects, err := s.ListProjects(ctx, userID, false)
	if err != nil {
		return stats, err
	}
	byID := map[string]*ProjectSummary{}
	stats.ProjectSummary = make([]ProjectSummary, len(projects))
	for i, p := range projects {
		stats.ProjectSummary[i] = ProjectSummary{Project: p, Spark: make(Spark, SparkDays)}
		byID[p.ID] = &stats.ProjectSummary[i]
	}

	// Per-project counts and last error.
	rows, err := s.db.QueryContext(ctx, `
		SELECT g.project_id,
		  SUM(CASE WHEN `+statusClause(StatusOpen, "g.")+` THEN 1 ELSE 0 END),
		  SUM(CASE WHEN `+statusClause(StatusFixed, "g.")+` THEN 1 ELSE 0 END),
		  COUNT(*), MAX(g.last_seen_unix)
		FROM error_groups g JOIN apps a ON a.project_id = g.project_id
		WHERE a.owner_user_id = ? AND a.archived_at_unix IS NULL GROUP BY g.project_id`, userID)
	if err != nil {
		return stats, err
	}
	for rows.Next() {
		var id string
		var open, fixed, total int
		var last int64
		if err := rows.Scan(&id, &open, &fixed, &total, &last); err != nil {
			rows.Close()
			return stats, err
		}
		if p := byID[id]; p != nil {
			p.Open, p.Fixed, p.Total = open, fixed, total
			t := time.Unix(last, 0).UTC()
			p.LastError = &t
		}
		stats.Open += open
	}
	if err := rows.Close(); err != nil {
		return stats, err
	}

	// Occurrences per hour bucket, folded into days.
	rows, err = s.db.QueryContext(ctx, `
		SELECT b.project_id, b.hour_start_unix, b.occurrences
		FROM error_occurrence_buckets b JOIN apps a ON a.project_id = b.project_id
		WHERE a.owner_user_id = ? AND a.archived_at_unix IS NULL AND b.hour_start_unix >= ?`, userID, windowStart)
	if err != nil {
		return stats, err
	}
	for rows.Next() {
		var id string
		var hour int64
		var n int
		if err := rows.Scan(&id, &hour, &n); err != nil {
			rows.Close()
			return stats, err
		}
		t := time.Unix(hour, 0).UTC()
		if idx := dayIndex(now, t); idx >= 0 {
			stats.EventsSpark[idx] += n
			if p := byID[id]; p != nil {
				p.Spark[idx] += n
			}
		}
		if now.Sub(t) < 24*time.Hour {
			stats.Events24h += n
		}
	}
	if err := rows.Close(); err != nil {
		return stats, err
	}

	// One pass over group timestamps yields new, fixed and open-backlog series.
	rows, err = s.db.QueryContext(ctx, `
		SELECT g.first_seen_unix, g.acked_at_unix, g.resolved_at_unix
		FROM error_groups g JOIN apps a ON a.project_id = g.project_id
		WHERE a.owner_user_id = ? AND a.archived_at_unix IS NULL AND g.suppressed_at_unix IS NULL`, userID)
	if err != nil {
		return stats, err
	}
	today := now.Truncate(24 * time.Hour)
	for rows.Next() {
		var first int64
		var acked, resolved sql.NullInt64
		if err := rows.Scan(&first, &acked, &resolved); err != nil {
			rows.Close()
			return stats, err
		}
		if idx := dayIndex(now, time.Unix(first, 0)); idx >= 0 {
			stats.NewSpark[idx]++
		}
		if resolved.Valid {
			if idx := dayIndex(now, time.Unix(resolved.Int64, 0)); idx >= 0 {
				stats.FixedSpark[idx]++
			}
		}
		// Open at the end of each day: seen by then and not yet acknowledged.
		for i := 0; i < SparkDays; i++ {
			end := today.AddDate(0, 0, i-(SparkDays-1)+1).Unix()
			if first < end && (!acked.Valid || acked.Int64 >= end) {
				stats.OpenSpark[i]++
			}
		}
	}
	if err := rows.Close(); err != nil {
		return stats, err
	}
	stats.NewToday = stats.NewSpark[SparkDays-1]
	for _, v := range stats.FixedSpark[SparkDays-7:] {
		stats.Fixed7d += v
	}
	return stats, nil
}

// ProjectBySlug resolves a project the user owns by slug or id.
func (s *Store) ProjectBySlug(ctx context.Context, userID int64, slug string) (Project, error) {
	row := s.db.QueryRowContext(ctx, `SELECT project_id,slug,display_name,github_repo,github_installation_id,github_repository_id,archived_at_unix,created_at_unix,updated_at_unix FROM apps WHERE owner_user_id=? AND (slug=? OR project_id=?) LIMIT 1`, userID, slug, slug)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

// ProjectCounts returns open/fixed/total error counts for one project.
func (s *Store) ProjectCounts(ctx context.Context, projectID string) (open, fixed, total int, err error) {
	err = s.db.QueryRowContext(ctx, fmt.Sprintf(`
		SELECT COALESCE(SUM(CASE WHEN %s THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN %s THEN 1 ELSE 0 END),0), COUNT(*)
		FROM error_groups g WHERE g.project_id = ?`, statusClause(StatusOpen, "g."), statusClause(StatusFixed, "g.")), projectID).Scan(&open, &fixed, &total)
	return
}

// SetIssueForTest links a group to a GitHub issue number without calling GitHub.
func (s *Store) SetIssueForTest(ctx context.Context, id int64, number int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE error_groups SET github_issue_number = ? WHERE id = ?`, number, id)
	return err
}
