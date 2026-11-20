package inbox

import (
	"context"
	"fmt"
)

func (s *Store) initializeIndexNow(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS indexnow_submissions (
			url TEXT PRIMARY KEY,
			fingerprint TEXT NOT NULL,
			submitted_at_unix INTEGER NOT NULL
		)
	`)
	if err != nil {
		return fmt.Errorf("initialize indexnow table: %w", err)
	}
	return nil
}

func (s *Store) IndexNowSubmitted(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT url, fingerprint FROM indexnow_submissions`)
	if err != nil {
		return nil, fmt.Errorf("query indexnow submissions: %w", err)
	}
	defer rows.Close()

	result := map[string]string{}
	for rows.Next() {
		var u, fp string
		if err := rows.Scan(&u, &fp); err != nil {
			return nil, fmt.Errorf("scan indexnow submission: %w", err)
		}
		result[u] = fp
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate indexnow submissions: %w", err)
	}
	return result, nil
}

func (s *Store) RecordIndexNowSubmitted(ctx context.Context, entries map[string]string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin indexnow transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM indexnow_submissions`); err != nil {
		return fmt.Errorf("clear indexnow submissions: %w", err)
	}
	now := s.now().UTC().Unix()
	for u, fp := range entries {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO indexnow_submissions (url, fingerprint, submitted_at_unix)
			VALUES (?, ?, ?)
		`, u, fp, now); err != nil {
			return fmt.Errorf("insert indexnow submission: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit indexnow submissions: %w", err)
	}
	return nil
}
