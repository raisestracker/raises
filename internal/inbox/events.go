package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	MaxEventsPerAccount           = 10_000
	MaxWebhookEndpointsPerAccount = 3
	eventRetention                = 30 * 24 * time.Hour
)

var supportedEventLevels = map[string]bool{"info": true, "warning": true, "error": true}
var SupportedWebhookEvents = []string{"notice.created", "error.created", "error.regressed", "github_issue.opened", "github_issue.reopened", "webhook.test"}

type SecretCipher interface {
	Encrypt(string) (string, error)
	Decrypt(string) (string, error)
}

type EventInput struct {
	Env      string         `json:"env"`
	Revision string         `json:"revision"`
	Level    string         `json:"level"`
	Message  string         `json:"message"`
	Source   string         `json:"source"`
	Context  map[string]any `json:"context"`
}

type Event struct {
	ID        string         `json:"id"`
	ProjectID string         `json:"project_id"`
	Project   string         `json:"project"`
	Env       string         `json:"env"`
	Revision  string         `json:"revision,omitempty"`
	Level     string         `json:"level"`
	Message   string         `json:"message"`
	Source    string         `json:"source,omitempty"`
	Context   map[string]any `json:"context,omitempty"`
	CreatedAt time.Time      `json:"created_at"`
}

type WebhookEndpoint struct {
	ID        string    `json:"id"`
	URL       string    `json:"url"`
	Events    []string  `json:"events"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type OutboundEvent struct {
	ID          string          `json:"id"`
	OwnerUserID int64           `json:"-"`
	ProjectID   string          `json:"project_id"`
	Project     string          `json:"project"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"data"`
	GroupID     int64           `json:"-"`
	CreatedAt   time.Time       `json:"created_at"`
}

type OutboundDelivery struct {
	ID              string
	Event           OutboundEvent
	DestinationKind string
	EndpointID      string
	URL             string
	SigningSecret   string
	Attempts        int
	LastError       string
	Group           *Group
}

type WebhookDelivery struct {
	ID         string    `json:"id"`
	EndpointID string    `json:"endpoint_id"`
	EventType  string    `json:"event_type"`
	State      string    `json:"state"`
	Attempts   int       `json:"attempts"`
	LastError  string    `json:"last_error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

type WebhookDeliveryHealth struct {
	Retrying int `json:"retrying"`
	Dead     int `json:"dead"`
}

func (s *Store) SetSecretCipher(cipher SecretCipher) { s.secretCipher = cipher }

func (s *Store) ConfigureOperatorNtfy(ownerUserID int64, enabled bool) {
	s.operatorNtfyOwnerID = ownerUserID
	s.operatorNtfyEnabled = enabled
}

func (s *Store) initializeEvents(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS events (
			id TEXT PRIMARY KEY, project_id TEXT NOT NULL, owner_user_id INTEGER NOT NULL,
			app_name TEXT NOT NULL, env TEXT NOT NULL, revision TEXT NOT NULL DEFAULT '',
			level TEXT NOT NULL, message TEXT NOT NULL, source TEXT NOT NULL DEFAULT '',
			context_json TEXT NOT NULL DEFAULT '{}', created_at_unix INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_owner_created ON events(owner_user_id, created_at_unix DESC)`,
		`CREATE TABLE IF NOT EXISTS webhook_endpoints (
			id TEXT PRIMARY KEY, owner_user_id INTEGER NOT NULL, url_encrypted TEXT NOT NULL,
			signing_secret_encrypted TEXT NOT NULL, events_json TEXT NOT NULL,
			active INTEGER NOT NULL DEFAULT 1, created_at_unix INTEGER NOT NULL, updated_at_unix INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_webhook_endpoints_owner ON webhook_endpoints(owner_user_id, active)`,
		`CREATE TABLE IF NOT EXISTS outbound_events (
			id TEXT PRIMARY KEY, source_key TEXT NOT NULL UNIQUE, owner_user_id INTEGER NOT NULL,
			project_id TEXT NOT NULL, app_name TEXT NOT NULL, event_type TEXT NOT NULL,
			payload_json TEXT NOT NULL, group_id INTEGER NOT NULL DEFAULT 0, created_at_unix INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS outbound_deliveries (
			id TEXT PRIMARY KEY, event_id TEXT NOT NULL, owner_user_id INTEGER NOT NULL,
			destination_kind TEXT NOT NULL, endpoint_id TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at_unix INTEGER NOT NULL,
			lease_expires_at_unix INTEGER, last_error TEXT NOT NULL DEFAULT '', created_at_unix INTEGER NOT NULL,
			completed_at_unix INTEGER, failed_at_unix INTEGER,
			UNIQUE(event_id, destination_kind, endpoint_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_outbound_deliveries_pending ON outbound_deliveries(state, next_attempt_at_unix)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize events: %w", err)
		}
	}
	return nil
}

func (s *Store) CreateEvent(ctx context.Context, token string, input EventInput) (Event, error) {
	app, err := s.AppByToken(ctx, token)
	if err != nil {
		return Event{}, err
	}
	input.Message = strings.TrimSpace(input.Message)
	input.Level = strings.ToLower(strings.TrimSpace(input.Level))
	if input.Level == "" {
		input.Level = "info"
	}
	if input.Env == "" {
		input.Env = "production"
	}
	if len([]rune(input.Env)) > 100 {
		return Event{}, fmt.Errorf("environment is too long")
	}
	if len([]rune(input.Revision)) > 200 {
		return Event{}, fmt.Errorf("revision is too long")
	}
	if input.Message == "" {
		return Event{}, fmt.Errorf("message is required")
	}
	if len([]rune(input.Message)) > 2_000 {
		return Event{}, fmt.Errorf("message is too long")
	}
	if !supportedEventLevels[input.Level] {
		return Event{}, fmt.Errorf("level is invalid")
	}
	if len([]rune(input.Source)) > 120 {
		return Event{}, fmt.Errorf("source is too long")
	}
	contextJSON, err := json.Marshal(input.Context)
	if err != nil || len(contextJSON) > 64*1024 {
		return Event{}, fmt.Errorf("context is invalid or too large")
	}
	id, err := randomID("evt_", 18)
	if err != nil {
		return Event{}, err
	}
	now := s.now().UTC()
	event := Event{ID: id, ProjectID: app.ID, Project: app.DisplayName, Env: input.Env, Revision: input.Revision, Level: input.Level, Message: input.Message, Source: input.Source, Context: input.Context, CreatedAt: now}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT INTO events(id,project_id,owner_user_id,app_name,env,revision,level,message,source,context_json,created_at_unix) VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, app.ID, app.OwnerUserID, app.DisplayName, input.Env, input.Revision, input.Level, input.Message, input.Source, string(contextJSON), now.Unix()); err != nil {
		return Event{}, err
	}
	payload, _ := json.Marshal(map[string]any{"notice": event})
	if err := s.enqueueOutboundEventTx(ctx, tx, "notice:"+id, app.OwnerUserID, app.ID, app.DisplayName, "notice.created", payload, 0, false); err != nil {
		return Event{}, err
	}
	if err := s.pruneEventsTx(ctx, tx, app.OwnerUserID, now); err != nil {
		return Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return Event{}, err
	}
	s.wakeOutbound()
	return event, nil
}

func (s *Store) pruneEventsTx(ctx context.Context, tx *sql.Tx, ownerID int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE owner_user_id=? AND created_at_unix<?`, ownerID, now.Add(-eventRetention).Unix()); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM events WHERE owner_user_id=? AND id NOT IN (SELECT id FROM events WHERE owner_user_id=? ORDER BY created_at_unix DESC,id DESC LIMIT ?)`, ownerID, ownerID, MaxEventsPerAccount)
	return err
}

func (s *Store) ListEventsForUser(ctx context.Context, userID int64, project, level string, since time.Time, limit int) ([]Event, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	query := `SELECT e.id,e.project_id,e.app_name,e.env,e.revision,e.level,e.message,e.source,e.context_json,e.created_at_unix FROM events e JOIN apps a ON a.project_id=e.project_id WHERE e.owner_user_id=?`
	args := []any{userID}
	if project != "" {
		query += ` AND (e.project_id=? OR a.slug=?)`
		args = append(args, project, project)
	}
	if level != "" {
		query += ` AND e.level=?`
		args = append(args, level)
	}
	if !since.IsZero() {
		query += ` AND e.created_at_unix>=?`
		args = append(args, since.UTC().Unix())
	}
	query += ` ORDER BY e.created_at_unix DESC,e.id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []Event
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) GetEventForUser(ctx context.Context, userID int64, id string) (Event, error) {
	event, err := scanEvent(s.db.QueryRowContext(ctx, `SELECT id,project_id,app_name,env,revision,level,message,source,context_json,created_at_unix FROM events WHERE owner_user_id=? AND id=?`, userID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Event{}, ErrNotFound
	}
	return event, err
}

func scanEvent(row rowScanner) (Event, error) {
	var event Event
	var contextJSON string
	var created int64
	if err := row.Scan(&event.ID, &event.ProjectID, &event.Project, &event.Env, &event.Revision, &event.Level, &event.Message, &event.Source, &contextJSON, &created); err != nil {
		return Event{}, err
	}
	_ = json.Unmarshal([]byte(contextJSON), &event.Context)
	event.CreatedAt = time.Unix(created, 0).UTC()
	return event, nil
}

func validWebhookEvents(events []string) ([]string, error) {
	if len(events) == 0 {
		events = append([]string(nil), SupportedWebhookEvents[:len(SupportedWebhookEvents)-1]...)
	}
	allowed := map[string]bool{}
	for _, event := range SupportedWebhookEvents {
		allowed[event] = true
	}
	seen := map[string]bool{}
	clean := make([]string, 0, len(events))
	for _, event := range events {
		event = strings.TrimSpace(event)
		if !allowed[event] {
			return nil, fmt.Errorf("webhook event %q is invalid", event)
		}
		if !seen[event] {
			seen[event] = true
			clean = append(clean, event)
		}
	}
	return clean, nil
}

func (s *Store) CreateWebhookEndpoint(ctx context.Context, userID int64, url string, events []string) (WebhookEndpoint, string, error) {
	if s.secretCipher == nil {
		return WebhookEndpoint{}, "", fmt.Errorf("webhook encryption is unavailable")
	}
	events, err := validWebhookEvents(events)
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM webhook_endpoints WHERE owner_user_id=?`, userID).Scan(&count); err != nil {
		return WebhookEndpoint{}, "", err
	}
	if count >= MaxWebhookEndpointsPerAccount {
		return WebhookEndpoint{}, "", fmt.Errorf("webhook endpoint limit reached")
	}
	id, err := randomID("whe_", 12)
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	secret, err := randomID("whsec_", 32)
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	encryptedURL, err := s.secretCipher.Encrypt(url)
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	encryptedSecret, err := s.secretCipher.Encrypt(secret)
	if err != nil {
		return WebhookEndpoint{}, "", err
	}
	eventsJSON, _ := json.Marshal(events)
	now := s.now().UTC()
	if _, err = tx.ExecContext(ctx, `INSERT INTO webhook_endpoints(id,owner_user_id,url_encrypted,signing_secret_encrypted,events_json,active,created_at_unix,updated_at_unix) VALUES(?,?,?,?,?,1,?,?)`, id, userID, encryptedURL, encryptedSecret, string(eventsJSON), now.Unix(), now.Unix()); err != nil {
		return WebhookEndpoint{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return WebhookEndpoint{}, "", err
	}
	return WebhookEndpoint{ID: id, URL: url, Events: events, Active: true, CreatedAt: now, UpdatedAt: now}, secret, nil
}

func (s *Store) ListWebhookEndpoints(ctx context.Context, userID int64) ([]WebhookEndpoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,url_encrypted,events_json,active,created_at_unix,updated_at_unix FROM webhook_endpoints WHERE owner_user_id=? ORDER BY created_at_unix`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var endpoints []WebhookEndpoint
	for rows.Next() {
		endpoint, err := s.scanWebhookEndpoint(rows)
		if err != nil {
			return nil, err
		}
		endpoints = append(endpoints, endpoint)
	}
	return endpoints, rows.Err()
}

func (s *Store) UpdateWebhookEndpoint(ctx context.Context, userID int64, id, url string, events []string, active bool) (WebhookEndpoint, error) {
	if s.secretCipher == nil {
		return WebhookEndpoint{}, fmt.Errorf("webhook encryption is unavailable")
	}
	events, err := validWebhookEvents(events)
	if err != nil {
		return WebhookEndpoint{}, err
	}
	encryptedURL, err := s.secretCipher.Encrypt(url)
	if err != nil {
		return WebhookEndpoint{}, err
	}
	eventsJSON, _ := json.Marshal(events)
	result, err := s.db.ExecContext(ctx, `UPDATE webhook_endpoints SET url_encrypted=?,events_json=?,active=?,updated_at_unix=? WHERE owner_user_id=? AND id=?`, encryptedURL, string(eventsJSON), boolToInt(active), s.now().UTC().Unix(), userID, id)
	if err != nil {
		return WebhookEndpoint{}, err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return WebhookEndpoint{}, ErrNotFound
	}
	return s.webhookEndpointForUser(ctx, userID, id)
}

func (s *Store) DeleteWebhookEndpoint(ctx context.Context, userID int64, id string) error {
	result, err := s.db.ExecContext(ctx, `DELETE FROM webhook_endpoints WHERE owner_user_id=? AND id=?`, userID, id)
	if err != nil {
		return err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) RotateWebhookSecret(ctx context.Context, userID int64, id string) (string, error) {
	if s.secretCipher == nil {
		return "", fmt.Errorf("webhook encryption is unavailable")
	}
	secret, err := randomID("whsec_", 32)
	if err != nil {
		return "", err
	}
	encrypted, err := s.secretCipher.Encrypt(secret)
	if err != nil {
		return "", err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE webhook_endpoints SET signing_secret_encrypted=?,updated_at_unix=? WHERE owner_user_id=? AND id=?`, encrypted, s.now().UTC().Unix(), userID, id)
	if err != nil {
		return "", err
	}
	if rows, _ := result.RowsAffected(); rows != 1 {
		return "", ErrNotFound
	}
	return secret, nil
}

func (s *Store) webhookEndpointForUser(ctx context.Context, userID int64, id string) (WebhookEndpoint, error) {
	endpoint, err := s.scanWebhookEndpoint(s.db.QueryRowContext(ctx, `SELECT id,url_encrypted,events_json,active,created_at_unix,updated_at_unix FROM webhook_endpoints WHERE owner_user_id=? AND id=?`, userID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return WebhookEndpoint{}, ErrNotFound
	}
	return endpoint, err
}

func (s *Store) scanWebhookEndpoint(row rowScanner) (WebhookEndpoint, error) {
	var endpoint WebhookEndpoint
	var encryptedURL, eventsJSON string
	var active int
	var created, updated int64
	if err := row.Scan(&endpoint.ID, &encryptedURL, &eventsJSON, &active, &created, &updated); err != nil {
		return WebhookEndpoint{}, err
	}
	if s.secretCipher == nil {
		return WebhookEndpoint{}, fmt.Errorf("webhook encryption is unavailable")
	}
	url, err := s.secretCipher.Decrypt(encryptedURL)
	if err != nil {
		return WebhookEndpoint{}, err
	}
	endpoint.URL, endpoint.Active = url, active == 1
	_ = json.Unmarshal([]byte(eventsJSON), &endpoint.Events)
	endpoint.CreatedAt, endpoint.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	return endpoint, nil
}
