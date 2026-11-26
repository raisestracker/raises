package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

func (s *Store) ListForUser(ctx context.Context, userID int64, project string, unacked bool) ([]Group, error) {
	query := `
		SELECT g.id, g.project_id, g.app_name, g.env, g.fingerprint, g.class, g.location, g.message, g.count,
		       g.first_seen_unix, g.last_seen_unix, g.last_revision, g.github_issue_number, g.github_issue_url,
		       g.acked_at_unix, g.suppressed_at_unix, g.resolved_at_unix, g.sample_json
		FROM error_groups g JOIN apps a ON a.project_id = g.project_id
		WHERE a.owner_user_id = ?
	`
	args := []any{userID}
	if project != "" {
		query += ` AND (a.project_id = ? OR a.slug = ?)`
		args = append(args, project, project)
	}
	if unacked {
		query += ` AND g.acked_at_unix IS NULL AND g.suppressed_at_unix IS NULL`
	}
	query += ` ORDER BY g.last_seen_unix DESC, g.id DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []Group
	for rows.Next() {
		group, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

func (s *Store) GetForUser(ctx context.Context, userID, id int64) (Group, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT g.id, g.project_id, g.app_name, g.env, g.fingerprint, g.class, g.location, g.message, g.count,
		       g.first_seen_unix, g.last_seen_unix, g.last_revision, g.github_issue_number, g.github_issue_url,
		       g.acked_at_unix, g.suppressed_at_unix, g.resolved_at_unix, g.sample_json
		FROM error_groups g JOIN apps a ON a.project_id = g.project_id
		WHERE a.owner_user_id = ? AND g.id = ?
	`, userID, id)
	group, err := scanGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	return group, err
}

func (s *Store) NoticesForUser(ctx context.Context, userID, id int64, limit int) ([]Notice, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.payload_json
		FROM notices n
		JOIN error_groups g ON g.id = n.group_id
		JOIN apps a ON a.project_id = g.project_id
		WHERE a.owner_user_id = ? AND g.id = ?
		ORDER BY n.id DESC LIMIT ?
	`, userID, id, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var notices []Notice
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var notice Notice
		if err := json.Unmarshal([]byte(raw), &notice); err != nil {
			return nil, err
		}
		notices = append(notices, notice)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(notices) > 0 {
		return notices, nil
	}
	// 0 notices returned: verify ownership so missing or cross-tenant IDs return ErrNotFound.
	var exists int
	err = s.db.QueryRowContext(ctx, `
		SELECT 1 FROM error_groups g JOIN apps a ON a.project_id = g.project_id
		WHERE a.owner_user_id = ? AND g.id = ?
	`, userID, id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return notices, err
}

func (s *Store) AckForUser(ctx context.Context, userID, id int64) (Group, error) {
	if _, err := s.GetForUser(ctx, userID, id); err != nil {
		return Group{}, err
	}
	return s.Ack(ctx, id)
}

func (s *Store) SuppressForUser(ctx context.Context, userID, id int64) (Group, error) {
	if _, err := s.GetForUser(ctx, userID, id); err != nil {
		return Group{}, err
	}
	return s.Suppress(ctx, id)
}

func (s *Store) UnsuppressForUser(ctx context.Context, userID, id int64) (Group, error) {
	if _, err := s.GetForUser(ctx, userID, id); err != nil {
		return Group{}, err
	}
	return s.Unsuppress(ctx, id)
}

func normalizeProjectSlug(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
