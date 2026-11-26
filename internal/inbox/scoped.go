package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Store) ListForUser(ctx context.Context, userID int64, project string, unacked bool) ([]Group, error) {
	return s.ListForUserWithOptions(ctx, userID, project, unacked, ListOptions{})
}

func (s *Store) ListForUserCompact(ctx context.Context, userID int64, project string, unacked, compact bool) ([]Group, error) {
	return s.ListForUserWithOptions(ctx, userID, project, unacked, ListOptions{Compact: compact})
}

func (s *Store) ListForUserWithOptions(ctx context.Context, userID int64, project string, unacked bool, opts ListOptions) ([]Group, error) {
	cols := `g.id, g.project_id, g.app_name, g.env, g.fingerprint, g.class, g.location, g.message, g.count,
		       g.first_seen_unix, g.last_seen_unix, g.last_revision, g.github_issue_number, g.github_issue_url,
		       g.acked_at_unix, g.suppressed_at_unix, g.resolved_at_unix,
		       g.claimed_by, g.claimed_at_unix, g.lease_expires_at_unix, g.claim_note, g.sample_json`
	if opts.Compact {
		cols = `g.id, g.project_id, g.app_name, g.env, g.fingerprint, g.class, g.location, g.message, g.count,
		       g.first_seen_unix, g.last_seen_unix, g.last_revision, g.github_issue_number, g.github_issue_url,
		       g.acked_at_unix, g.suppressed_at_unix, g.resolved_at_unix,
		       g.claimed_by, g.claimed_at_unix, g.lease_expires_at_unix, g.claim_note`
	}
	query := `
		SELECT ` + cols + `
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
	now := s.now().UTC()
	if opts.ExcludeClaimed {
		query += ` AND (g.lease_expires_at_unix IS NULL OR g.lease_expires_at_unix <= ?)`
		args = append(args, now.Unix())
	}
	query += ` ORDER BY g.last_seen_unix DESC, g.id DESC`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []Group
	for rows.Next() {
		var (
			group Group
			err   error
		)
		if opts.Compact {
			group, err = scanGroupCompact(rows, now)
		} else {
			group, err = scanGroup(rows, now)
		}
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
		       g.acked_at_unix, g.suppressed_at_unix, g.resolved_at_unix,
		       g.claimed_by, g.claimed_at_unix, g.lease_expires_at_unix, g.claim_note, g.sample_json
		FROM error_groups g JOIN apps a ON a.project_id = g.project_id
		WHERE a.owner_user_id = ? AND g.id = ?
	`, userID, id)
	group, err := scanGroup(row, s.now().UTC())
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

func (s *Store) ResolveForUser(ctx context.Context, userID, id int64) (Group, error) {
	now := s.now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE error_groups
		SET resolved_at_unix = ?, acked_at_unix = COALESCE(acked_at_unix, ?),
		    claimed_by = '', claimed_at_unix = NULL, lease_expires_at_unix = NULL, claim_note = ''
		WHERE id = ? AND EXISTS (
			SELECT 1 FROM apps a WHERE a.project_id = error_groups.project_id AND a.owner_user_id = ?
		)
	`, now, now, id, userID)
	if err != nil {
		return Group{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Group{}, ErrNotFound
	}
	return s.GetForUser(ctx, userID, id)
}

func (s *Store) ReopenForUser(ctx context.Context, userID, id int64) (Group, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE error_groups
		SET resolved_at_unix = NULL, acked_at_unix = NULL
		WHERE id = ? AND EXISTS (
			SELECT 1 FROM apps a WHERE a.project_id = error_groups.project_id AND a.owner_user_id = ?
		)
	`, id, userID)
	if err != nil {
		return Group{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Group{}, ErrNotFound
	}
	return s.GetForUser(ctx, userID, id)
}

// ClaimForUser atomically acquires or renews a TTL lease on an error group via
// one tenant-scoped SQL UPDATE. Returns ErrNotFound on missing/cross-tenant IDs
// and (currentGroup, ErrClaimConflict) when another agent holds an active lease.
func (s *Store) ClaimForUser(ctx context.Context, userID, id int64, agent, note string, ttl time.Duration) (Group, error) {
	agent = strings.TrimSpace(agent)
	note = strings.TrimSpace(note)
	if utf8.RuneCountInString(agent) > maxClaimAgentLen {
		return Group{}, fmt.Errorf("%w: agent name exceeds %d characters", ErrInvalidClaim, maxClaimAgentLen)
	}
	if utf8.RuneCountInString(note) > maxClaimNoteLen {
		return Group{}, fmt.Errorf("%w: note exceeds %d characters", ErrInvalidClaim, maxClaimNoteLen)
	}
	if ttl < 0 {
		return Group{}, fmt.Errorf("%w: ttl must be positive", ErrInvalidClaim)
	}
	if ttl == 0 {
		ttl = DefaultClaimTTL
	}
	if ttl > MaxClaimTTL {
		ttl = MaxClaimTTL
	}
	storedAgent := agent
	renewMatch := agent
	if storedAgent == "" {
		storedAgent = "anonymous"
		renewMatch = ""
	}
	now := s.now().UTC()
	nowUnix := now.Unix()
	expiresUnix := now.Add(ttl).Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE error_groups
		SET claimed_by = ?, claimed_at_unix = ?, lease_expires_at_unix = ?, claim_note = ?
		WHERE id = ?
		  AND EXISTS (
			SELECT 1 FROM apps a WHERE a.project_id = error_groups.project_id AND a.owner_user_id = ?
		  )
		  AND (
			lease_expires_at_unix IS NULL
			OR lease_expires_at_unix <= ?
			OR (? != '' AND claimed_by = ?)
		  )
	`, storedAgent, nowUnix, expiresUnix, note, id, userID, nowUnix, renewMatch, renewMatch)
	if err != nil {
		return Group{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		current, getErr := s.GetForUser(ctx, userID, id)
		if getErr != nil {
			return Group{}, getErr
		}
		return current, ErrClaimConflict
	}
	return s.GetForUser(ctx, userID, id)
}

// ReleaseForUser atomically clears an active claim lease on an error group.
// When agent is non-empty, only the current holder (or an already expired/empty
// lease) can release; otherwise ErrClaimConflict is returned with holder metadata.
func (s *Store) ReleaseForUser(ctx context.Context, userID, id int64, agent string) (Group, error) {
	agent = strings.TrimSpace(agent)
	nowUnix := s.now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE error_groups
		SET claimed_by = '', claimed_at_unix = NULL, lease_expires_at_unix = NULL, claim_note = ''
		WHERE id = ?
		  AND EXISTS (
			SELECT 1 FROM apps a WHERE a.project_id = error_groups.project_id AND a.owner_user_id = ?
		  )
		  AND (? = '' OR claimed_by = ? OR lease_expires_at_unix IS NULL OR lease_expires_at_unix <= ?)
	`, id, userID, agent, agent, nowUnix)
	if err != nil {
		return Group{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		current, getErr := s.GetForUser(ctx, userID, id)
		if getErr != nil {
			return Group{}, getErr
		}
		return current, ErrClaimConflict
	}
	return s.GetForUser(ctx, userID, id)
}

func normalizeProjectSlug(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}
