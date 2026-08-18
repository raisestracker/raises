package inbox

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	issueJobLease       = 2 * time.Minute
	maxIssueJobAttempts = 20
)

func (s *Store) enqueueIssueJob(ctx context.Context, groupID int64, action string) error {
	var exists int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_jobs WHERE group_id = ? AND action = ? AND state IN ('pending','working')`, groupID, action).Scan(&exists)
	if err != nil {
		return err
	}
	if exists > 0 {
		return nil
	}
	deliveryKey, err := randomID("ij_", 18)
	if err != nil {
		return err
	}
	now := s.now().UTC().Unix()
	_, err = s.db.ExecContext(ctx, `INSERT INTO issue_jobs(group_id,action,delivery_key,state,next_attempt_at_unix,created_at_unix) VALUES(?,?,?,'pending',?,?)`, groupID, action, deliveryKey, now, now)
	if err == nil {
		select {
		case s.jobNotify <- struct{}{}:
		default:
		}
	}
	return err
}

func (s *Store) ProcessIssueJobsOnce(ctx context.Context) error {
	if s.filer == nil {
		return nil
	}
	now := s.now().UTC()
	if _, err := s.db.ExecContext(ctx, `UPDATE issue_jobs SET state='pending',next_attempt_at_unix=?,lease_expires_at_unix=NULL WHERE state='working' AND lease_expires_at_unix <= ?`, now.Unix(), now.Unix()); err != nil {
		return err
	}
	var id, groupID, createdAtUnix int64
	var action, deliveryKey string
	err := s.db.QueryRowContext(ctx, `SELECT id,group_id,action,delivery_key,created_at_unix FROM issue_jobs WHERE state='pending' AND next_attempt_at_unix <= ? ORDER BY id LIMIT 1`, now.Unix()).Scan(&id, &groupID, &action, &deliveryKey, &createdAtUnix)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	claimed, err := s.db.ExecContext(ctx, `UPDATE issue_jobs SET state='working',lease_expires_at_unix=? WHERE id=? AND state='pending'`, now.Add(issueJobLease).Unix(), id)
	if err != nil {
		return err
	}
	if rows, _ := claimed.RowsAffected(); rows != 1 {
		return nil
	}
	group, app, err := s.issueJobData(ctx, groupID)
	emitLifecycle := true
	if err == nil && (app.ArchivedAt != nil || app.GitHubInstallationID == 0 || app.GitHubRepo == "") {
		err = fmt.Errorf("project is not connected")
	}
	if err == nil {
		switch action {
		case "open":
			var recent int
			_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_jobs j JOIN error_groups g ON g.id=j.group_id WHERE g.project_id=? AND j.action='open' AND j.state='done' AND j.completed_at_unix>?`, group.ProjectID, s.now().UTC().Add(-time.Hour).Unix()).Scan(&recent)
			if recent >= 30 {
				_, _ = s.db.ExecContext(ctx, `UPDATE issue_jobs SET state='pending',next_attempt_at_unix=?,lease_expires_at_unix=NULL WHERE id=?`, now.Add(time.Hour).Unix(), id)
				return nil
			}
			if group.GitHubIssueNumber == 0 {
				var number int
				var url string
				marker := issueDeliveryMarker(deliveryKey)
				var found bool
				number, url, found, err = s.filer.FindByMarker(ctx, app.GitHubInstallationID, app.GitHubRepo, marker, time.Unix(createdAtUnix, 0).UTC().Add(-time.Minute))
				if err == nil && !found {
					number, url, err = s.filer.Open(ctx, app.GitHubInstallationID, app.GitHubRepo, issueTitle(group), issueBody(group, marker))
				}
				if err == nil {
					_, err = s.db.ExecContext(ctx, `UPDATE error_groups SET github_issue_number=?,github_issue_url=? WHERE id=?`, number, url, group.ID)
				}
			}
		case "reopen":
			if group.GitHubIssueNumber > 0 {
				err = s.filer.Reopen(ctx, app.GitHubInstallationID, app.GitHubRepo, group.GitHubIssueNumber)
			} else {
				err = s.enqueueIssueJob(ctx, group.ID, "open")
				emitLifecycle = false
			}
		default:
			err = fmt.Errorf("unknown issue action %q", action)
		}
	}
	if err == nil && emitLifecycle {
		if refreshed, refreshErr := s.Get(ctx, group.ID); refreshErr == nil {
			group = refreshed
		}
		err = s.EnqueueGitHubLifecycleEvent(ctx, group, app, action, deliveryKey)
	}
	if err == nil {
		_, err = s.db.ExecContext(ctx, `UPDATE issue_jobs SET state='done',completed_at_unix=?,failed_at_unix=NULL,lease_expires_at_unix=NULL,last_error='' WHERE id=?`, now.Unix(), id)
		return err
	}
	var attempts int
	_ = s.db.QueryRowContext(ctx, `SELECT attempts FROM issue_jobs WHERE id=?`, id).Scan(&attempts)
	attempts++
	if attempts >= maxIssueJobAttempts {
		_, updateErr := s.db.ExecContext(ctx, `UPDATE issue_jobs SET state='dead',attempts=?,failed_at_unix=?,lease_expires_at_unix=NULL,last_error=? WHERE id=?`, attempts, now.Unix(), truncateError(err), id)
		if updateErr == nil && s.issueJobDead != nil {
			s.issueJobDead(IssueDelivery{
				ID: id, ProjectID: group.ProjectID, ProjectName: app.DisplayName,
				Repository: app.GitHubRepo, Action: action, State: "dead",
				Attempts: attempts, LastError: truncateError(err), CreatedAt: time.Unix(createdAtUnix, 0).UTC(),
			})
		}
		return updateErr
	}
	delay := time.Duration(1<<min(attempts, 10)) * time.Second
	_, updateErr := s.db.ExecContext(ctx, `UPDATE issue_jobs SET state='pending',attempts=?,next_attempt_at_unix=?,lease_expires_at_unix=NULL,last_error=? WHERE id=?`, attempts, now.Add(delay).Unix(), truncateError(err), id)
	return updateErr
}

func (s *Store) IssueDeliveryHealthForUser(ctx context.Context, userID int64) (IssueDeliveryHealth, error) {
	var health IssueDeliveryHealth
	var oldest sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN j.state IN ('pending','working') THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN j.state='dead' THEN 1 ELSE 0 END),0),
			MIN(CASE WHEN j.state IN ('pending','working','dead') THEN j.created_at_unix END)
		FROM issue_jobs j
		JOIN error_groups g ON g.id=j.group_id
		JOIN apps a ON a.project_id=g.project_id
		WHERE a.owner_user_id=?
	`, userID).Scan(&health.Retrying, &health.Dead, &oldest)
	if err != nil {
		return health, err
	}
	if oldest.Valid {
		t := time.Unix(oldest.Int64, 0).UTC()
		health.Oldest = &t
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT j.id,g.project_id,a.display_name,a.github_repo,j.action,j.state,j.attempts,j.last_error,j.created_at_unix
		FROM issue_jobs j
		JOIN error_groups g ON g.id=j.group_id
		JOIN apps a ON a.project_id=g.project_id
		WHERE a.owner_user_id=? AND j.state IN ('pending','working','dead') AND j.last_error != ''
		ORDER BY CASE WHEN j.state='dead' THEN 0 ELSE 1 END,j.created_at_unix
		LIMIT 5
	`, userID)
	if err != nil {
		return health, err
	}
	defer rows.Close()
	for rows.Next() {
		var item IssueDelivery
		var created int64
		if err := rows.Scan(&item.ID, &item.ProjectID, &item.ProjectName, &item.Repository, &item.Action, &item.State, &item.Attempts, &item.LastError, &created); err != nil {
			return health, err
		}
		item.LastError = strings.TrimSpace(item.LastError)
		if runes := []rune(item.LastError); len(runes) > 180 {
			item.LastError = string(runes[:180]) + "…"
		}
		item.CreatedAt = time.Unix(created, 0).UTC()
		health.Problems = append(health.Problems, item)
	}
	return health, rows.Err()
}

func (s *Store) RetryIssueJobForUser(ctx context.Context, userID, jobID int64) error {
	now := s.now().UTC().Unix()
	result, err := s.db.ExecContext(ctx, `
		UPDATE issue_jobs SET state='pending',attempts=0,next_attempt_at_unix=?,lease_expires_at_unix=NULL,last_error='',failed_at_unix=NULL,completed_at_unix=NULL
		WHERE id=? AND state='dead' AND EXISTS (
			SELECT 1 FROM error_groups g JOIN apps a ON a.project_id=g.project_id
			WHERE g.id=issue_jobs.group_id AND a.owner_user_id=?
		)
	`, now, jobID, userID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrNotFound
	}
	select {
	case s.jobNotify <- struct{}{}:
	default:
	}
	return nil
}

func (s *Store) issueJobData(ctx context.Context, groupID int64) (Group, App, error) {
	group, err := s.Get(ctx, groupID)
	if err != nil {
		return Group{}, App{}, err
	}
	var app App
	var archived sql.NullInt64
	err = s.db.QueryRowContext(ctx, `SELECT project_id,slug,display_name,token_hash,github_repo,owner_user_id,github_installation_id,github_repository_id,archived_at_unix FROM apps WHERE project_id=?`, group.ProjectID).Scan(&app.ID, &app.Name, &app.DisplayName, &app.TokenHash, &app.GitHubRepo, &app.OwnerUserID, &app.GitHubInstallationID, &app.GitHubRepositoryID, &archived)
	if archived.Valid {
		t := time.Unix(archived.Int64, 0).UTC()
		app.ArchivedAt = &t
	}
	return group, app, err
}

func truncateError(err error) string {
	runes := []rune(err.Error())
	if len(runes) > 1000 {
		return string(runes[:1000])
	}
	return string(runes)
}

func issueDeliveryMarker(deliveryKey string) string {
	return "<!-- raises-delivery:" + deliveryKey + " -->"
}

func (s *Store) RunIssueWorker(ctx context.Context, onError func(error)) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.jobNotify:
		}
		for i := 0; i < 20; i++ {
			if err := s.ProcessIssueJobsOnce(ctx); err != nil {
				if onError != nil {
					onError(err)
				}
				break
			}
			var pending int
			_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_jobs WHERE state='pending' AND next_attempt_at_unix <= ?`, s.now().UTC().Unix()).Scan(&pending)
			if pending == 0 {
				break
			}
		}
	}
}
