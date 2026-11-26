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
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const (
	defaultKeepNotices      = 50
	lastUsedDebounceSeconds = 60

	DefaultClaimTTL  = 15 * time.Minute
	MaxClaimTTL      = 2 * time.Hour
	maxClaimAgentLen = 80
	maxClaimNoteLen  = 256
)

var ErrUnauthorized = errors.New("unauthorized")
var ErrNotFound = errors.New("not found")
var ErrLimit = errors.New("limit reached")
var ErrClaimConflict = errors.New("error group is already claimed")
var ErrInvalidClaim = errors.New("invalid claim")

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
	App         string         `json:"app"`
	Env         string         `json:"env"`
	Revision    string         `json:"revision"`
	Handled     bool           `json:"handled"`
	Class       string         `json:"class"`
	Message     string         `json:"message"`
	Backtrace   []string       `json:"backtrace"`
	Location    string         `json:"location"`
	Fingerprint []string       `json:"fingerprint,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
	Request     map[string]any `json:"request,omitempty"`
	ReceivedAt  time.Time      `json:"received_at"`
}

type Claim struct {
	Agent     string    `json:"agent"`
	ClaimedAt time.Time `json:"claimed_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Note      string    `json:"note,omitempty"`
}

type ListOptions struct {
	Compact        bool
	ExcludeClaimed bool
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
	URL               string     `json:"url,omitempty"`
	AckedAt           *time.Time `json:"acked_at,omitempty"`
	SuppressedAt      *time.Time `json:"suppressed_at,omitempty"`
	ResolvedAt        *time.Time `json:"resolved_at,omitempty"`
	Claim             *Claim     `json:"claim,omitempty"`
	Status            string     `json:"status"`
	Sample            Notice     `json:"sample,omitzero"`
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
	db  *sql.DB
	now func() time.Time
	// quotaMu serializes quota-checked writes (project/token creation and
	// archive restore) so concurrent requests from one account cannot pass
	// the count check together and overshoot the cap. The deployment runs a
	// single server process, which is the scope this guards.
	quotaMu                  sync.Mutex
	filer                    IssueFiler
	keepNotices              int
	jobNotify                chan struct{}
	outboundNotify           chan struct{}
	issueJobDead             func(IssueDelivery)
	outboundDead             func(OutboundDelivery)
	secretCipher             SecretCipher
	operatorNtfyOwnerID      int64
	operatorNtfyEnabled      bool
	operatorNtfyMinNotice    string // lowest notice level pushed to the operator's ntfy; default warning
	unlimitedProjectsOwnerID int64
}

func (s *Store) OnIssueJobDead(handler func(IssueDelivery)) {
	s.issueJobDead = handler
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func sqliteDSN(path string) string {
	const (
		memParams  = "_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_txlock=immediate"
		fileParams = "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=journal_size_limit(67108864)&_txlock=immediate"
	)
	trimmed := strings.TrimSpace(path)
	isMem := trimmed == ":memory:" || strings.HasPrefix(trimmed, "file::memory:") || strings.Contains(trimmed, "mode=memory")
	params := fileParams
	if isMem {
		params = memParams
		if trimmed == ":memory:" {
			return "file::memory:?" + params
		}
	}
	if strings.Contains(trimmed, "?") {
		return trimmed + "&" + params
	}
	return trimmed + "?" + params
}

func Open(path string, now func() time.Time, filer IssueFiler) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	db, err := sql.Open("sqlite", sqliteDSN(path))
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
			resolved_at_unix INTEGER,
			claimed_by TEXT NOT NULL DEFAULT '',
			claimed_at_unix INTEGER,
			lease_expires_at_unix INTEGER,
			claim_note TEXT NOT NULL DEFAULT '',
			sample_json TEXT NOT NULL
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
	if err := s.ensureTenantIsolatedGroups(ctx); err != nil {
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
	var lastUsed sql.NullInt64
	tokenHash := HashToken(token)

	// Indexed primary lookup on project_tokens(token_hash) joined to apps(project_id).
	err := s.db.QueryRowContext(ctx, `
		SELECT a.project_id, a.slug, a.display_name, a.token_hash, a.github_repo, a.owner_user_id,
		       a.github_installation_id, a.github_repository_id, a.archived_at_unix,
		       pt.last_used_at_unix
		FROM project_tokens pt
		JOIN apps a ON a.project_id = pt.project_id
		WHERE pt.token_hash = ? AND pt.revoked_at_unix IS NULL AND a.archived_at_unix IS NULL
		LIMIT 1
	`, tokenHash).Scan(
		&app.ID, &app.Name, &app.DisplayName, &app.TokenHash, &app.GitHubRepo, &app.OwnerUserID,
		&app.GitHubInstallationID, &app.GitHubRepositoryID, &archived, &lastUsed,
	)
	fromProjectToken := err == nil
	if errors.Is(err, sql.ErrNoRows) {
		// Fallback for legacy apps.token_hash (used in internal/unit tests via UpsertApp).
		err = s.db.QueryRowContext(ctx, `
			SELECT project_id, slug, display_name, token_hash, github_repo, owner_user_id,
			       github_installation_id, github_repository_id, archived_at_unix
			FROM apps
			WHERE token_hash = ? AND token_hash != '' AND archived_at_unix IS NULL
			LIMIT 1
		`, tokenHash).Scan(
			&app.ID, &app.Name, &app.DisplayName, &app.TokenHash, &app.GitHubRepo, &app.OwnerUserID,
			&app.GitHubInstallationID, &app.GitHubRepositoryID, &archived,
		)
	}
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
	if fromProjectToken {
		now := s.now().UTC().Unix()
		if !lastUsed.Valid || now-lastUsed.Int64 >= lastUsedDebounceSeconds {
			_, _ = s.db.ExecContext(ctx, `UPDATE project_tokens SET last_used_at_unix = ? WHERE token_hash = ?`, now, tokenHash)
		}
	}
	return app, nil
}

func (s *Store) Ingest(ctx context.Context, token string, notice Notice) (IngestResult, error) {
	app, err := s.AppByToken(ctx, token)
	if err != nil {
		return IngestResult{}, err
	}
	return s.IngestForApp(ctx, app, notice)
}

func (s *Store) IngestForApp(ctx context.Context, app App, notice Notice) (IngestResult, error) {
	if err := ValidateFingerprintParts(notice.Fingerprint); err != nil {
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
	fp, location := FingerprintWithCustom(notice.Class, notice.Backtrace, notice.Context, notice.Fingerprint)
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
		resolvedUnix   sql.NullInt64
		sampleRaw      string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, project_id, app_name, env, fingerprint, class, location, message, count,
		       first_seen_unix, last_seen_unix, last_revision, github_issue_number, github_issue_url,
		       acked_at_unix, suppressed_at_unix, resolved_at_unix, sample_json
		FROM error_groups WHERE project_id = ? AND env = ? AND fingerprint = ?
	`, app.ID, notice.Env, fp).Scan(
		&group.ID, &group.ProjectID, &group.App, &group.Env, &group.Fingerprint, &group.Class, &group.Location, &group.Message, &group.Count,
		newUnix(&group.FirstSeen), newUnix(&group.LastSeen), &group.LastRevision, &group.GitHubIssueNumber, &group.GitHubIssueURL,
		&ackedUnix, &suppressedUnix, &resolvedUnix, &sampleRaw,
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
		if resolvedUnix.Valid {
			t := time.Unix(resolvedUnix.Int64, 0).UTC()
			group.ResolvedAt = &t
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
			group.ResolvedAt = nil
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE error_groups
			SET count = ?, last_seen_unix = ?, last_revision = ?, message = ?,
			    acked_at_unix = CASE WHEN ? THEN NULL ELSE acked_at_unix END,
			    resolved_at_unix = CASE WHEN ? THEN NULL ELSE resolved_at_unix END
			WHERE id = ?
		`, group.Count, nowUnix, notice.Revision, notice.Message, boolToInt(regressed), boolToInt(regressed), group.ID)
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

	issueJobEnqueued := false
	if s.filer != nil && app.GitHubRepo != "" && app.GitHubInstallationID != 0 && !suppressed {
		action := ""
		if created {
			action = "open"
		} else if regressed {
			action = "reopen"
		}
		if action != "" {
			enqueued, err := s.enqueueIssueJobTx(ctx, tx, group.ID, action)
			if err != nil {
				return IngestResult{}, err
			}
			issueJobEnqueued = enqueued
		}
	}

	if err := tx.Commit(); err != nil {
		return IngestResult{}, err
	}
	if (created || regressed) && !suppressed {
		s.wakeOutbound()
	}
	if issueJobEnqueued {
		s.wakeIssues()
	}

	group.Status = group.ComputeStatus()
	return IngestResult{Group: group, Created: created, Regressed: regressed}, nil
}

// List is tenant-unsafe: it reads every account's error groups. HTTP
// handlers must use ListForUser; direct callers are limited to the ingest
// pipeline and other internal/system use. It is deliberately absent from the
// httpserver Store interface so a handler cannot be wired to it.
func (s *Store) List(ctx context.Context, app string, unacked bool) ([]Group, error) {
	query := `
		SELECT id, project_id, app_name, env, fingerprint, class, location, message, count,
		       first_seen_unix, last_seen_unix, last_revision, github_issue_number, github_issue_url,
		       acked_at_unix, suppressed_at_unix, resolved_at_unix,
		       claimed_by, claimed_at_unix, lease_expires_at_unix, claim_note, sample_json
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
	now := s.now().UTC()
	var groups []Group
	for rows.Next() {
		group, err := scanGroup(rows, now)
		if err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// Get is tenant-unsafe: it reads any error group by ID. HTTP handlers must
// use GetForUser (or a ForUser wrapper, which checks ownership first);
// direct callers are limited to internal/system use (scoped wrappers, the
// ingest pipeline, background jobs). It is deliberately absent from the
// httpserver Store interface so a handler cannot be wired to it.
func (s *Store) Get(ctx context.Context, id int64) (Group, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, project_id, app_name, env, fingerprint, class, location, message, count,
		       first_seen_unix, last_seen_unix, last_revision, github_issue_number, github_issue_url,
		       acked_at_unix, suppressed_at_unix, resolved_at_unix,
		       claimed_by, claimed_at_unix, lease_expires_at_unix, claim_note, sample_json
		FROM error_groups WHERE id = ?
	`, id)
	group, err := scanGroup(row, s.now().UTC())
	if errors.Is(err, sql.ErrNoRows) {
		return Group{}, ErrNotFound
	}
	return group, err
}

// Notices is tenant-unsafe: it reads any group's notices by ID. HTTP
// handlers must use NoticesForUser. It is deliberately absent from the
// httpserver Store interface so a handler cannot be wired to it.
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

// Ack is tenant-unsafe: it acknowledges any error group by ID. HTTP
// handlers must use AckForUser. It is deliberately absent from the
// httpserver Store interface so a handler cannot be wired to it.
func (s *Store) Ack(ctx context.Context, id int64) (Group, error) {
	now := s.now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE error_groups
		SET acked_at_unix = ?, claimed_by = '', claimed_at_unix = NULL, lease_expires_at_unix = NULL, claim_note = ''
		WHERE id = ?
	`, now, id)
	if err != nil {
		return Group{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Group{}, ErrNotFound
	}
	return s.Get(ctx, id)
}

// Suppress is tenant-unsafe: it suppresses any error group by ID. HTTP
// handlers must use SuppressForUser. It is deliberately absent from the
// httpserver Store interface so a handler cannot be wired to it.
func (s *Store) Suppress(ctx context.Context, id int64) (Group, error) {
	now := s.now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE error_groups
		SET suppressed_at_unix = ?, claimed_by = '', claimed_at_unix = NULL, lease_expires_at_unix = NULL, claim_note = ''
		WHERE id = ?
	`, now, id)
	if err != nil {
		return Group{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Group{}, ErrNotFound
	}
	return s.Get(ctx, id)
}

// Unsuppress is tenant-unsafe: it unsuppresses any error group by ID. HTTP
// handlers must use UnsuppressForUser. It is deliberately absent from the
// httpserver Store interface so a handler cannot be wired to it.
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

// Resolve is tenant-unsafe: it marks any error group resolved store-only
// (setting resolved_at_unix and preserving or setting acked_at_unix) without
// making outbound GitHub API calls. HTTP handlers must use ResolveForUser.
func (s *Store) Resolve(ctx context.Context, id int64) (Group, error) {
	now := s.now().UTC().Unix()
	res, err := s.db.ExecContext(ctx, `
		UPDATE error_groups
		SET resolved_at_unix = ?, acked_at_unix = COALESCE(acked_at_unix, ?),
		    claimed_by = '', claimed_at_unix = NULL, lease_expires_at_unix = NULL, claim_note = ''
		WHERE id = ?
	`, now, now, id)
	if err != nil {
		return Group{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Group{}, ErrNotFound
	}
	return s.Get(ctx, id)
}

// Reopen is tenant-unsafe: it reopens any error group store-only by clearing
// both resolved_at_unix and acked_at_unix so its status returns to open.
// HTTP handlers must use ReopenForUser.
func (s *Store) Reopen(ctx context.Context, id int64) (Group, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE error_groups
		SET resolved_at_unix = NULL, acked_at_unix = NULL
		WHERE id = ?
	`, id)
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

func buildActiveClaim(now time.Time, claimedBy string, claimedAtUnix, leaseExpiresUnix sql.NullInt64, claimNote string) *Claim {
	if !leaseExpiresUnix.Valid || leaseExpiresUnix.Int64 <= now.Unix() || claimedBy == "" {
		return nil
	}
	var claimedAt time.Time
	if claimedAtUnix.Valid {
		claimedAt = time.Unix(claimedAtUnix.Int64, 0).UTC()
	}
	return &Claim{
		Agent:     claimedBy,
		ClaimedAt: claimedAt,
		ExpiresAt: time.Unix(leaseExpiresUnix.Int64, 0).UTC(),
		Note:      claimNote,
	}
}

func scanGroup(row rowScanner, now time.Time) (Group, error) {
	var (
		group            Group
		ackedUnix        sql.NullInt64
		suppressedUnix   sql.NullInt64
		resolvedUnix     sql.NullInt64
		claimedBy        string
		claimedAtUnix    sql.NullInt64
		leaseExpiresUnix sql.NullInt64
		claimNote        string
		sampleRaw        string
	)
	err := row.Scan(
		&group.ID, &group.ProjectID, &group.App, &group.Env, &group.Fingerprint, &group.Class, &group.Location, &group.Message, &group.Count,
		newUnix(&group.FirstSeen), newUnix(&group.LastSeen), &group.LastRevision, &group.GitHubIssueNumber, &group.GitHubIssueURL,
		&ackedUnix, &suppressedUnix, &resolvedUnix,
		&claimedBy, &claimedAtUnix, &leaseExpiresUnix, &claimNote, &sampleRaw,
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
	if resolvedUnix.Valid {
		t := time.Unix(resolvedUnix.Int64, 0).UTC()
		group.ResolvedAt = &t
	}
	group.Claim = buildActiveClaim(now, claimedBy, claimedAtUnix, leaseExpiresUnix, claimNote)
	group.Status = group.ComputeStatus()
	return group, nil
}

func scanGroupCompact(row rowScanner, now time.Time) (Group, error) {
	var (
		group            Group
		ackedUnix        sql.NullInt64
		suppressedUnix   sql.NullInt64
		resolvedUnix     sql.NullInt64
		claimedBy        string
		claimedAtUnix    sql.NullInt64
		leaseExpiresUnix sql.NullInt64
		claimNote        string
	)
	err := row.Scan(
		&group.ID, &group.ProjectID, &group.App, &group.Env, &group.Fingerprint, &group.Class, &group.Location, &group.Message, &group.Count,
		newUnix(&group.FirstSeen), newUnix(&group.LastSeen), &group.LastRevision, &group.GitHubIssueNumber, &group.GitHubIssueURL,
		&ackedUnix, &suppressedUnix, &resolvedUnix,
		&claimedBy, &claimedAtUnix, &leaseExpiresUnix, &claimNote,
	)
	if err != nil {
		return Group{}, err
	}
	if ackedUnix.Valid {
		t := time.Unix(ackedUnix.Int64, 0).UTC()
		group.AckedAt = &t
	}
	if suppressedUnix.Valid {
		t := time.Unix(suppressedUnix.Int64, 0).UTC()
		group.SuppressedAt = &t
	}
	if resolvedUnix.Valid {
		t := time.Unix(resolvedUnix.Int64, 0).UTC()
		group.ResolvedAt = &t
	}
	group.Claim = buildActiveClaim(now, claimedBy, claimedAtUnix, leaseExpiresUnix, claimNote)
	group.Status = group.ComputeStatus()
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
	title := fmt.Sprintf("[%s] %s in %s", group.Env, group.Class, group.Location)
	// The title is a single GitHub issue line: flatten newlines and cap
	// length (by rune, so multi-byte text is never split).
	title = strings.ReplaceAll(strings.ReplaceAll(title, "\r", " "), "\n", " ")
	if runes := []rune(title); len(runes) > 256 {
		title = string(runes[:256])
	}
	return title
}

// FenceFor returns a fenced-code-block fence one run longer than any backtick
// run in the value (at least three backticks), so the value cannot break out of
// its fenced block.
func FenceFor(value string) string {
	return fenceFor(value)
}

// fenceFor returns a fenced-code-block fence one run longer than any
// backtick run in the value (at least three backticks), so the value cannot
// break out of its fenced block.
func fenceFor(value string) string {
	longest, run := 0, 0
	for _, r := range value {
		if r == '`' {
			run++
			if run > longest {
				longest = run
			}
		} else {
			run = 0
		}
	}
	if n := longest + 1; n >= 3 {
		return strings.Repeat("`", n)
	}
	return "```"
}

// fencedField renders one client-supplied value as an inert fenced block
// under a Raises-generated label. Everything the ingestor accepted — Class,
// Location, Message, App, Env, Revision — renders as plain text: mentions,
// links, images, headings and inline code cannot survive GitHub rendering.
func fencedField(label, value string) string {
	fence := fenceFor(value)
	return fmt.Sprintf("%s:\n%s\n%s\n%s", label, fence, value, fence)
}

func issueBody(group Group, deliveryMarker string) string {
	return fmt.Sprintf("%s\n<!-- raises:%s -->\n\n%s\n\n%s\n\n%s\n\n%s\n\n%s\n\n%s\n\n- Raises error: `%d`\n\nAsk your agent to inspect this error through the Raises API.\n",
		deliveryMarker, group.Fingerprint,
		fencedField("Class", group.Class),
		fencedField("Location", group.Location),
		fencedField("Message", group.Message),
		fencedField("Project", group.App),
		fencedField("Environment", group.Env),
		fencedField("Revision", group.LastRevision),
		group.ID)
}
