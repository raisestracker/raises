package inbox

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const defaultKeepNotices = 50

var ErrUnauthorized = errors.New("unauthorized")
var ErrNotFound = errors.New("not found")
var ErrLimit = errors.New("limit reached")

type App struct {
	ID                   string
	Name                 string
	DisplayName          string
	TokenHash            string
	GitHubRepo           string
	OwnerUserID          int64
	GitHubInstallationID int64
	GitHubRepositoryID   int64
	ArchivedAt           *time.Time
}

type Notice struct {
	App        string         `json:"app"`
	Env        string         `json:"env"`
	Revision   string         `json:"revision"`
	Handled    bool           `json:"handled"`
	Class      string         `json:"class"`
	Message    string         `json:"message"`
	Backtrace  []string       `json:"backtrace"`
	Location   string         `json:"location"`
	Context    map[string]any `json:"context,omitempty"`
	Request    map[string]any `json:"request,omitempty"`
	ReceivedAt time.Time      `json:"received_at"`
}

type Group struct {
	ID                int64      `json:"id"`
	ProjectID         string     `json:"project_id"`
	App               string     `json:"app"`
	Env               string     `json:"env"`
	Fingerprint       string     `json:"fingerprint"`
	Class             string     `json:"class"`
	Location          string     `json:"location"`
	Message           string     `json:"message"`
	Count             int        `json:"count"`
	FirstSeen         time.Time  `json:"first_seen"`
	LastSeen          time.Time  `json:"last_seen"`
	LastRevision      string     `json:"last_revision"`
	GitHubIssueNumber int        `json:"github_issue_number,omitempty"`
	GitHubIssueURL    string     `json:"github_issue_url,omitempty"`
	AckedAt           *time.Time `json:"acked_at,omitempty"`
	SuppressedAt      *time.Time `json:"suppressed_at,omitempty"`
	Sample            Notice     `json:"sample"`
}

type IngestResult struct {
	Group     Group
	Created   bool
	Regressed bool
}

type IssueDelivery struct {
	ID          int64
	ProjectID   string
	ProjectName string
	Repository  string
	Action      string
	State       string
	Attempts    int
	LastError   string
	CreatedAt   time.Time
}

type IssueDeliveryHealth struct {
	Retrying int
	Dead     int
	Oldest   *time.Time
	Problems []IssueDelivery
}

type IssueFiler interface {
	FindByMarker(ctx context.Context, installationID int64, repo, marker string, since time.Time) (number int, url string, found bool, err error)
	Open(ctx context.Context, installationID int64, repo, title, body string) (number int, url string, err error)
	Reopen(ctx context.Context, installationID int64, repo string, number int) error
}

type Store struct {
	db                  *sql.DB
	now                 func() time.Time
	filer               IssueFiler
	keepNotices         int
	jobNotify           chan struct{}
	outboundNotify      chan struct{}
	issueJobDead        func(IssueDelivery)
	outboundDead        func(OutboundDelivery)
	secretCipher        SecretCipher
	operatorNtfyOwnerID int64
	operatorNtfyEnabled bool
}

func (s *Store) OnIssueJobDead(handler func(IssueDelivery)) {
	s.issueJobDead = handler
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func Open(path string, now func() time.Time, filer IssueFiler) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, now: now, filer: filer, keepNotices: defaultKeepNotices, jobNotify: make(chan struct{}, 1), outboundNotify: make(chan struct{}, 1)}
	if err := store.initialize(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func (s *Store) initialize(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode = WAL`,
		`PRAGMA busy_timeout = 5000`,
		`CREATE TABLE IF NOT EXISTS apps (
			name TEXT PRIMARY KEY,
			slug TEXT NOT NULL DEFAULT '',
			token_hash TEXT NOT NULL,
			github_repo TEXT NOT NULL DEFAULT '',
			project_id TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '',
			owner_user_id INTEGER NOT NULL DEFAULT 0,
			github_installation_id INTEGER NOT NULL DEFAULT 0,
			github_repository_id INTEGER NOT NULL DEFAULT 0,
			archived_at_unix INTEGER,
			created_at_unix INTEGER NOT NULL DEFAULT 0,
			updated_at_unix INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS error_groups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			project_id TEXT NOT NULL DEFAULT '',
			app_name TEXT NOT NULL,
			env TEXT NOT NULL,
			fingerprint TEXT NOT NULL,
			class TEXT NOT NULL,
			location TEXT NOT NULL,
			message TEXT NOT NULL,
			count INTEGER NOT NULL,
			first_seen_unix INTEGER NOT NULL,
			last_seen_unix INTEGER NOT NULL,
			last_revision TEXT NOT NULL DEFAULT '',
			github_issue_number INTEGER NOT NULL DEFAULT 0,
			github_issue_url TEXT NOT NULL DEFAULT '',
			acked_at_unix INTEGER,
			suppressed_at_unix INTEGER,
			sample_json TEXT NOT NULL,
			UNIQUE(app_name, env, fingerprint)
		)`,
		`CREATE TABLE IF NOT EXISTS notices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id INTEGER NOT NULL,
			received_at_unix INTEGER NOT NULL,
			revision TEXT NOT NULL DEFAULT '',
			payload_json TEXT NOT NULL,
			FOREIGN KEY(group_id) REFERENCES error_groups(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notices_group ON notices(group_id, id DESC)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize store: %w", err)
		}
	}
	if err := s.ensureLegacyColumns(ctx); err != nil {
		return err
	}
	if err := s.initializeControlPlane(ctx); err != nil {
		return err
	}
	if err := s.ensureIssueJobColumns(ctx); err != nil {
		return err
	}
	if err := s.initializeEvents(ctx); err != nil {
		return err
	}
	return s.initializeReporting(ctx)
}

func (s *Store) UpsertApp(ctx context.Context, name, token, repo string) error {
	name = strings.TrimSpace(name)
	if name == "" || token == "" {
		return fmt.Errorf("app name and token are required")
	}
	projectID := "prj_" + name
	now := s.now().UTC().Unix()
	installationID := int64(0)
	if strings.TrimSpace(repo) != "" {
		installationID = -1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO apps (name, slug, token_hash, github_repo, project_id, display_name, github_installation_id, created_at_unix, updated_at_unix)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET token_hash = excluded.token_hash, github_repo = excluded.github_repo,
			slug = excluded.slug,
			project_id = CASE WHEN apps.project_id = '' THEN excluded.project_id ELSE apps.project_id END,
			display_name = CASE WHEN apps.display_name = '' THEN excluded.display_name ELSE apps.display_name END,
			github_installation_id = CASE WHEN apps.github_installation_id = 0 AND excluded.github_repo != '' THEN -1 ELSE apps.github_installation_id END,
			updated_at_unix = excluded.updated_at_unix
	`, name, name, HashToken(token), strings.TrimSpace(repo), projectID, name, installationID, now, now)
	if err != nil {
		return fmt.Errorf("upsert app: %w", err)
	}
	return nil
}

func (s *Store) AppByToken(ctx context.Context, token string) (App, error) {
	var app App
	var archived sql.NullInt64
	err := s.db.QueryRowContext(ctx, `
		SELECT a.project_id, a.slug, a.display_name, a.token_hash, a.github_repo, a.owner_user_id,
		       a.github_installation_id, a.github_repository_id, a.archived_at_unix
		FROM apps a
		LEFT JOIN project_tokens pt ON pt.project_id = a.project_id AND pt.revoked_at_unix IS NULL
		WHERE a.archived_at_unix IS NULL AND (a.token_hash = ? OR pt.token_hash = ?)
		LIMIT 1
	`, HashToken(token), HashToken(token)).Scan(
		&app.ID, &app.Name, &app.DisplayName, &app.TokenHash, &app.GitHubRepo, &app.OwnerUserID,
		&app.GitHubInstallationID, &app.GitHubRepositoryID, &archived,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return App{}, ErrUnauthorized
	}
	if err != nil {
		return App{}, err
	}
	if archived.Valid {
		t := time.Unix(archived.Int64, 0).UTC()
		app.ArchivedAt = &t
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE project_tokens SET last_used_at_unix = ? WHERE token_hash = ?`, s.now().UTC().Unix(), HashToken(token))
	return app, nil
}

func (s *Store) Ingest(ctx context.Context, token string, notice Notice) (IngestResult, error) {
	app, err := s.AppByToken(ctx, token)
	if err != nil {
		return IngestResult{}, err
	}
	if notice.Class == "" {
		notice.Class = "RuntimeError"
	}
	if notice.Env == "" {
		notice.Env = "production"
	}
	notice.App = app.Name
	notice.ReceivedAt = s.now().UTC()
	fp, location := Fingerprint(notice.Class, notice.Backtrace, notice.Context)
	notice.Location = location

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return IngestResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		group          Group
		ackedUnix      sql.NullInt64
		suppressedUnix sql.NullInt64
		sampleRaw      string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, project_id, app_name, env, fingerprint, class, location, message, count,
		       first_seen_unix, last_seen_unix, last_revision, github_issue_number, github_issue_url,
		       acked_at_unix, suppressed_at_unix, sample_json
		FROM error_groups WHERE app_name = ? AND env = ? AND fingerprint = ?
	`, app.Name, notice.Env, fp).Scan(
		&group.ID, &group.ProjectID, &group.App, &group.Env, &group.Fingerprint, &group.Class, &group.Location, &group.Message, &group.Count,
		newUnix(&group.FirstSeen), newUnix(&group.LastSeen), &group.LastRevision, &group.GitHubIssueNumber, &group.GitHubIssueURL,
		&ackedUnix, &suppressedUnix, &sampleRaw,
	)

	created := errors.Is(err, sql.ErrNoRows)
	if err != nil && !created {
		return IngestResult{}, err
	}
	if !created {
		_ = json.Unmarshal([]byte(sampleRaw), &group.Sample)
		if ackedUnix.Valid {
			t := time.Unix(ackedUnix.Int64, 0).UTC()
			group.AckedAt = &t
		}
		if suppressedUnix.Valid {
			t := time.Unix(suppressedUnix.Int64, 0).UTC()
			group.SuppressedAt = &t
		}
	}
	if created && app.OwnerUserID > 0 {
		var groupCount int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM error_groups g JOIN apps a ON a.project_id = g.project_id WHERE a.owner_user_id = ?`, app.OwnerUserID).Scan(&groupCount); err != nil {
			return IngestResult{}, err
		}
		if groupCount >= MaxErrorGroupsPerAccount {
			return IngestResult{}, ErrLimit
		}
	}

	payload, err := json.Marshal(notice)
	if err != nil {
		return IngestResult{}, err
	}

	suppressed := group.SuppressedAt != nil
	regressed := !created && !suppressed && group.AckedAt != nil && notice.Revision != "" && notice.Revision != group.LastRevision
	nowUnix := notice.ReceivedAt.Unix()

	if created {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO error_groups (
				project_id, app_name, env, fingerprint, class, location, message, count,
				first_seen_unix, last_seen_unix, last_revision, sample_json
			) VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?, ?, ?)
		`, app.ID, app.Name, notice.Env, fp, notice.Class, location, notice.Message, nowUnix, nowUnix, notice.Revision, string(payload))
		if err != nil {
			return IngestResult{}, err
		}
		id, _ := res.LastInsertId()
		group = Group{
			ID:           id,
			ProjectID:    app.ID,
			App:          app.Name,
			Env:          notice.Env,
			Fingerprint:  fp,
			Class:        notice.Class,
			Location:     location,
			Message:      notice.Message,
			Count:        1,
			FirstSeen:    notice.ReceivedAt,
			LastSeen:     notice.ReceivedAt,
			LastRevision: notice.Revision,
			Sample:       notice,
		}
	} else {
		group.Count++
		group.LastSeen = notice.ReceivedAt
		group.LastRevision = notice.Revision
		group.Message = notice.Message
		if regressed {
			group.AckedAt = nil
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE error_groups
			SET count = ?, last_seen_unix = ?, last_revision = ?, message = ?,
			    acked_at_unix = CASE WHEN ? THEN NULL ELSE acked_at_unix END
			WHERE id = ?
		`, group.Count, nowUnix, notice.Revision, notice.Message, boolToInt(regressed), group.ID)
		if err != nil {
			return IngestResult{}, err
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO notices (group_id, received_at_unix, revision, payload_json)
		VALUES (?, ?, ?, ?)
	`, group.ID, nowUnix, notice.Revision, string(payload)); err != nil {
		return IngestResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO error_occurrence_buckets (hour_start_unix, project_id, group_id, occurrences)
		VALUES (?, ?, ?, 1)
		ON CONFLICT(hour_start_unix, project_id, group_id)
		DO UPDATE SET occurrences = occurrences + 1
	`, notice.ReceivedAt.UTC().Truncate(time.Hour).Unix(), app.ID, group.ID); err != nil {
		return IngestResult{}, err
	}
	if (created || regressed) && !suppressed {
		eventType := "error.created"
		sourceKey := fmt.Sprintf("error:%d:created", group.ID)
		if regressed {
			eventType = "error.regressed"
			regressionID, idErr := randomID("reg_", 12)
			if idErr != nil {
				return IngestResult{}, idErr
			}
			sourceKey = fmt.Sprintf("error:%d:regressed:%s", group.ID, regressionID)
		}
		payload, _ := json.Marshal(map[string]any{"error": errorEventPayload(group)})
		if err := s.enqueueOutboundEventTx(ctx, tx, sourceKey, app.OwnerUserID, app.ID, app.Name, eventType, payload, group.ID, true); err != nil {
			return IngestResult{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM notices WHERE group_id = ? AND id NOT IN (
			SELECT id FROM notices WHERE group_id = ? ORDER BY id DESC LIMIT ?
		)
	`, group.ID, group.ID, s.keepNotices); err != nil {
		return IngestResult{}, err
	}

	if err := tx.Commit(); err != nil {
		return IngestResult{}, err
	}
	if (created || regressed) && !suppressed {
		s.wakeOutbound()
	}

	if s.filer != nil && app.GitHubRepo != "" && app.GitHubInstallationID != 0 && !suppressed {
		action := ""
		if created {
			action = "open"
		} else if regressed {
			action = "reopen"
		}
		if action != "" {
			_ = s.enqueueIssueJob(ctx, group.ID, action)
			_ = s.ProcessIssueJobsOnce(ctx)
			if refreshed, refreshErr := s.Get(ctx, group.ID); refreshErr == nil {
				group = refreshed
			}
		}
	}

	return IngestResult{Group: group, Created: created, Regressed: regressed}, nil
}

func (s *Store) List(ctx context.Context, app string, unacked bool) ([]Group, error) {
	query := `
		SELECT id, project_id, app_name, env, fingerprint, class, location, message, count,
		       first_seen_unix, last_seen_unix, last_revision, github_issue_number, github_issue_url,
		       acked_at_unix, suppressed_at_unix, sample_json
		FROM error_groups
	`
	args := []any{}
	wheres := []string{}
	if app != "" {
		wheres = append(wheres, "app_name = ?")
		args = append(args, app)
	}
	if unacked {
		wheres = append(wheres, "acked_at_unix IS NULL", "suppressed_at_unix IS NULL")
	}
	if len(wheres) > 0 {
		query += " WHERE " + strings.Join(wheres, " AND ")
	}
	query += " ORDER BY last_seen_unix DESC, id DESC"
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

func (s *Store) Get(ctx context.Context, id int64) (Group, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, project_id, app_name, env, fingerprint, class, location, message, count,
		       first_seen_unix, last_seen_unix, last_revision, github_issue_number, github_issue_url,
		       acked_at_unix, suppressed_at_unix, sample_json
		FROM error_groups WHERE id = ?
	`, id)
	group, err := scanGroup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	return group, err
}

func (s *Store) Notices(ctx context.Context, id int64, limit int) ([]Notice, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	if _, err := s.Get(ctx, id); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload_json FROM notices WHERE group_id = ? ORDER BY id DESC LIMIT ?
	`, id, limit)
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
	return notices, rows.Err()
}

func (s *Store) Ack(ctx context.Context, id int64) (Group, error) {
	now := s.now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `UPDATE error_groups SET acked_at_unix = ? WHERE id = ?`, now, id)
	if err != nil {
		return Group{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Group{}, ErrNotFound
	}
	return s.Get(ctx, id)
}

func (s *Store) Suppress(ctx context.Context, id int64) (Group, error) {
	now := s.now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `UPDATE error_groups SET suppressed_at_unix = ? WHERE id = ?`, now, id)
	if err != nil {
		return Group{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Group{}, ErrNotFound
	}
	return s.Get(ctx, id)
}

func (s *Store) Unsuppress(ctx context.Context, id int64) (Group, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE error_groups SET suppressed_at_unix = NULL WHERE id = ?`, id)
	if err != nil {
		return Group{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Group{}, ErrNotFound
	}
	return s.Get(ctx, id)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanGroup(row rowScanner) (Group, error) {
	var (
		group          Group
		ackedUnix      sql.NullInt64
		suppressedUnix sql.NullInt64
		sampleRaw      string
	)
	err := row.Scan(
		&group.ID, &group.ProjectID, &group.App, &group.Env, &group.Fingerprint, &group.Class, &group.Location, &group.Message, &group.Count,
		newUnix(&group.FirstSeen), newUnix(&group.LastSeen), &group.LastRevision, &group.GitHubIssueNumber, &group.GitHubIssueURL,
		&ackedUnix, &suppressedUnix, &sampleRaw,
	)
	if err != nil {
		return Group{}, err
	}
	_ = json.Unmarshal([]byte(sampleRaw), &group.Sample)
	if ackedUnix.Valid {
		t := time.Unix(ackedUnix.Int64, 0).UTC()
		group.AckedAt = &t
	}
	if suppressedUnix.Valid {
		t := time.Unix(suppressedUnix.Int64, 0).UTC()
		group.SuppressedAt = &t
	}
	return group, nil
}

type unixTime struct{ t *time.Time }

func newUnix(t *time.Time) *unixTime { return &unixTime{t: t} }

func (u *unixTime) Scan(src any) error {
	switch v := src.(type) {
	case int64:
		*u.t = time.Unix(v, 0).UTC()
	case int:
		*u.t = time.Unix(int64(v), 0).UTC()
	default:
		return fmt.Errorf("unsupported unix time %T", src)
	}
	return nil
}

func boolToInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func issueTitle(group Group) string {
	return fmt.Sprintf("[%s] %s in %s", group.Env, group.Class, group.Location)
}

func issueBody(group Group, deliveryMarker string) string {
	return fmt.Sprintf("%s\n<!-- raises:%s -->\n\n**%s** in `%s`\n\n%s\n\n- Project: `%s`\n- Environment: `%s`\n- Revision: `%s`\n- Raises error: `%d`\n\nAsk your agent to inspect this error through the Raises API.\n",
		deliveryMarker, group.Fingerprint, group.Class, group.Location, group.Message, group.App, group.Env, group.LastRevision, group.ID)
}
