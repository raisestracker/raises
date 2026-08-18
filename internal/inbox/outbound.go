package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	outboundJobLease       = 2 * time.Minute
	maxOutboundJobAttempts = 20
)

type OutboundSender interface {
	Send(context.Context, OutboundDelivery) error
}

type permanentError interface{ Permanent() bool }

func (s *Store) OnOutboundDeliveryDead(handler func(OutboundDelivery)) { s.outboundDead = handler }

func (s *Store) wakeOutbound() {
	select {
	case s.outboundNotify <- struct{}{}:
	default:
	}
}

func errorEventPayload(group Group) map[string]any {
	return map[string]any{
		"id": group.ID, "project_id": group.ProjectID, "app": group.App, "environment": group.Env,
		"class": group.Class, "message": group.Message, "location": group.Location, "count": group.Count,
		"revision": group.LastRevision, "github_issue_url": group.GitHubIssueURL,
	}
}

func containsEvent(eventsJSON, eventType string) bool {
	var events []string
	_ = json.Unmarshal([]byte(eventsJSON), &events)
	for _, event := range events {
		if event == eventType {
			return true
		}
	}
	return false
}

func (s *Store) enqueueOutboundEventTx(ctx context.Context, tx *sql.Tx, sourceKey string, ownerID int64, projectID, appName, eventType string, payload []byte, groupID int64, delayNtfy bool) error {
	eventID, err := randomID("obe_", 18)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbound_events(id,source_key,owner_user_id,project_id,app_name,event_type,payload_json,group_id,created_at_unix) VALUES(?,?,?,?,?,?,?,?,?)`, eventID, sourceKey, ownerID, projectID, appName, eventType, string(payload), groupID, now.Unix())
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id,events_json FROM webhook_endpoints WHERE owner_user_id=? AND active=1`, ownerID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var endpointID, eventsJSON string
		if err := rows.Scan(&endpointID, &eventsJSON); err != nil {
			return err
		}
		if !containsEvent(eventsJSON, eventType) {
			continue
		}
		if err := s.insertOutboundDeliveryTx(ctx, tx, eventID, ownerID, "webhook", endpointID, now); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if s.operatorNtfyEnabled && ownerID == s.operatorNtfyOwnerID && (eventType == "notice.created" || eventType == "error.created" || eventType == "error.regressed") {
		next := now
		if delayNtfy {
			next = now.Add(5 * time.Second)
		}
		if err := s.insertOutboundDeliveryTx(ctx, tx, eventID, ownerID, "ntfy", "operator", next); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) insertOutboundDeliveryTx(ctx context.Context, tx *sql.Tx, eventID string, ownerID int64, kind, endpointID string, next time.Time) error {
	id, err := randomID("obd_", 18)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO outbound_deliveries(id,event_id,owner_user_id,destination_kind,endpoint_id,state,next_attempt_at_unix,created_at_unix) VALUES(?,?,?,?,?,'pending',?,?)`, id, eventID, ownerID, kind, endpointID, next.Unix(), s.now().UTC().Unix())
	return err
}

func (s *Store) EnqueueGitHubLifecycleEvent(ctx context.Context, group Group, app App, action, sourceKey string) error {
	eventType := "github_issue.opened"
	if action == "reopen" {
		eventType = "github_issue.reopened"
	}
	payload, _ := json.Marshal(map[string]any{
		"error":        errorEventPayload(group),
		"github_issue": map[string]any{"number": group.GitHubIssueNumber, "url": group.GitHubIssueURL, "repository": app.GitHubRepo},
	})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.enqueueOutboundEventTx(ctx, tx, "github:"+sourceKey, app.OwnerUserID, app.ID, app.Name, eventType, payload, group.ID, false); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.wakeOutbound()
	return nil
}

func (s *Store) EnqueueWebhookTest(ctx context.Context, userID int64, endpointID string) error {
	endpoint, err := s.webhookEndpointForUser(ctx, userID, endpointID)
	if err != nil {
		return err
	}
	eventID, err := randomID("obe_", 18)
	if err != nil {
		return err
	}
	deliveryID, err := randomID("obd_", 18)
	if err != nil {
		return err
	}
	now := s.now().UTC()
	payload, _ := json.Marshal(map[string]any{"message": "Raises webhook delivery is connected."})
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbound_events(id,source_key,owner_user_id,project_id,app_name,event_type,payload_json,created_at_unix) VALUES(?,?,?,?,?,?,?,?)`, eventID, "test:"+eventID, userID, "", "Raises", "webhook.test", string(payload), now.Unix()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO outbound_deliveries(id,event_id,owner_user_id,destination_kind,endpoint_id,state,next_attempt_at_unix,created_at_unix) VALUES(?,?,?,'webhook',?,'pending',?,?)`, deliveryID, eventID, userID, endpoint.ID, now.Unix(), now.Unix()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.wakeOutbound()
	return nil
}

func (s *Store) ProcessOutboundDeliveriesOnce(ctx context.Context, sender OutboundSender) error {
	now := s.now().UTC()
	if _, err := s.db.ExecContext(ctx, `UPDATE outbound_deliveries SET state='pending',lease_expires_at_unix=NULL WHERE state='working' AND lease_expires_at_unix<=?`, now.Unix()); err != nil {
		return err
	}
	var delivery OutboundDelivery
	var eventCreated int64
	var payloadJSON string
	err := s.db.QueryRowContext(ctx, `
		SELECT d.id,d.destination_kind,d.endpoint_id,d.attempts,
		       e.id,e.owner_user_id,e.project_id,e.app_name,e.event_type,e.payload_json,e.group_id,e.created_at_unix
		FROM outbound_deliveries d JOIN outbound_events e ON e.id=d.event_id
		WHERE d.state='pending' AND d.next_attempt_at_unix<=? ORDER BY d.created_at_unix,d.id LIMIT 1
	`, now.Unix()).Scan(&delivery.ID, &delivery.DestinationKind, &delivery.EndpointID, &delivery.Attempts,
		&delivery.Event.ID, &delivery.Event.OwnerUserID, &delivery.Event.ProjectID, &delivery.Event.Project, &delivery.Event.Type, &payloadJSON, &delivery.Event.GroupID, &eventCreated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	delivery.Event.Payload = json.RawMessage(payloadJSON)
	if err != nil {
		return err
	}
	delivery.Event.CreatedAt = time.Unix(eventCreated, 0).UTC()
	claimed, err := s.db.ExecContext(ctx, `UPDATE outbound_deliveries SET state='working',lease_expires_at_unix=? WHERE id=? AND state='pending'`, now.Add(outboundJobLease).Unix(), delivery.ID)
	if err != nil {
		return err
	}
	if rows, _ := claimed.RowsAffected(); rows != 1 {
		return nil
	}
	if delivery.DestinationKind == "webhook" {
		var encryptedURL, encryptedSecret string
		var active int
		err = s.db.QueryRowContext(ctx, `SELECT url_encrypted,signing_secret_encrypted,active FROM webhook_endpoints WHERE id=? AND owner_user_id=?`, delivery.EndpointID, delivery.Event.OwnerUserID).Scan(&encryptedURL, &encryptedSecret, &active)
		if errors.Is(err, sql.ErrNoRows) || active == 0 {
			return s.completeOutbound(ctx, delivery.ID, now)
		}
		if err == nil {
			if s.secretCipher == nil {
				err = fmt.Errorf("webhook encryption is unavailable")
			} else if delivery.URL, err = s.secretCipher.Decrypt(encryptedURL); err == nil {
				delivery.SigningSecret, err = s.secretCipher.Decrypt(encryptedSecret)
			}
		}
	} else if delivery.Event.GroupID > 0 {
		if group, groupErr := s.Get(ctx, delivery.Event.GroupID); groupErr == nil {
			delivery.Group = &group
		}
	}
	if err == nil {
		err = sender.Send(ctx, delivery)
	}
	if err == nil {
		return s.completeOutbound(ctx, delivery.ID, now)
	}
	permanent := false
	if typed, ok := err.(permanentError); ok {
		permanent = typed.Permanent()
	}
	delivery.Attempts++
	lastError := truncateError(err)
	delivery.LastError = lastError
	if permanent || delivery.Attempts >= maxOutboundJobAttempts {
		_, updateErr := s.db.ExecContext(ctx, `UPDATE outbound_deliveries SET state='dead',attempts=?,failed_at_unix=?,lease_expires_at_unix=NULL,last_error=? WHERE id=?`, delivery.Attempts, now.Unix(), lastError, delivery.ID)
		if updateErr == nil && s.outboundDead != nil {
			s.outboundDead(delivery)
		}
		return updateErr
	}
	delay := time.Duration(1<<min(delivery.Attempts, 10)) * time.Second
	_, updateErr := s.db.ExecContext(ctx, `UPDATE outbound_deliveries SET state='pending',attempts=?,next_attempt_at_unix=?,lease_expires_at_unix=NULL,last_error=? WHERE id=?`, delivery.Attempts, now.Add(delay).Unix(), lastError, delivery.ID)
	return updateErr
}

func (s *Store) completeOutbound(ctx context.Context, id string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbound_deliveries SET state='done',completed_at_unix=?,lease_expires_at_unix=NULL,last_error='' WHERE id=?`, now.Unix(), id)
	return err
}

func (s *Store) RunOutboundWorker(ctx context.Context, sender OutboundSender, onError func(error)) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.outboundNotify:
		}
		if err := s.PruneOutboundHistory(ctx); err != nil && onError != nil {
			onError(err)
		}
		for i := 0; i < 20; i++ {
			if err := s.ProcessOutboundDeliveriesOnce(ctx, sender); err != nil {
				if onError != nil {
					onError(err)
				}
				break
			}
			var pending int
			_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_deliveries WHERE state='pending' AND next_attempt_at_unix<=?`, s.now().UTC().Unix()).Scan(&pending)
			if pending == 0 {
				break
			}
		}
	}
}

func (s *Store) WebhookDeliveryHealthForUser(ctx context.Context, userID int64) (WebhookDeliveryHealth, error) {
	var health WebhookDeliveryHealth
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN state IN ('pending','working') THEN 1 ELSE 0 END),0),COALESCE(SUM(CASE WHEN state='dead' THEN 1 ELSE 0 END),0) FROM outbound_deliveries WHERE owner_user_id=? AND destination_kind='webhook'`, userID).Scan(&health.Retrying, &health.Dead)
	return health, err
}

func (s *Store) ListWebhookDeliveriesForUser(ctx context.Context, userID int64, state string, limit int) ([]WebhookDelivery, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	query := `SELECT d.id,d.endpoint_id,e.event_type,d.state,d.attempts,d.last_error,d.created_at_unix FROM outbound_deliveries d JOIN outbound_events e ON e.id=d.event_id WHERE d.owner_user_id=? AND d.destination_kind='webhook'`
	args := []any{userID}
	if state != "" {
		query += ` AND d.state=?`
		args = append(args, state)
	}
	query += ` ORDER BY d.created_at_unix DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deliveries []WebhookDelivery
	for rows.Next() {
		var item WebhookDelivery
		var created int64
		if err := rows.Scan(&item.ID, &item.EndpointID, &item.EventType, &item.State, &item.Attempts, &item.LastError, &created); err != nil {
			return nil, err
		}
		item.CreatedAt = time.Unix(created, 0).UTC()
		deliveries = append(deliveries, item)
	}
	return deliveries, rows.Err()
}

func (s *Store) RetryWebhookDeliveryForUser(ctx context.Context, userID int64, id string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE outbound_deliveries SET state='pending',attempts=0,next_attempt_at_unix=?,lease_expires_at_unix=NULL,last_error='',failed_at_unix=NULL,completed_at_unix=NULL WHERE id=? AND owner_user_id=? AND destination_kind='webhook' AND state='dead'`, s.now().UTC().Unix(), id, userID)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrNotFound
	}
	s.wakeOutbound()
	return nil
}

func (s *Store) PruneOutboundHistory(ctx context.Context) error {
	cutoff := s.now().UTC().Add(-eventRetention).Unix()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM outbound_deliveries WHERE state IN ('done','dead') AND created_at_unix<?`, cutoff); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM outbound_events WHERE created_at_unix<? AND NOT EXISTS (SELECT 1 FROM outbound_deliveries d WHERE d.event_id=outbound_events.id)`, cutoff)
	return err
}
