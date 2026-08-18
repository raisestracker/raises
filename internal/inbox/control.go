package inbox

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	MaxActiveProjects        = 5
	MaxActiveAgentKeys       = 5
	MaxActiveProjectTokens   = 3
	MaxErrorGroupsPerAccount = 10_000
)

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

const (
	maxAgentNameLength   = 80
	maxProjectNameLength = 100
	maxProjectSlugLength = 63
)

type User struct {
	ID        int64     `json:"id"`
	GitHubID  int64     `json:"github_id"`
	Login     string    `json:"login"`
	Name      string    `json:"name,omitempty"`
	AvatarURL string    `json:"avatar_url,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Session struct {
	User      User
	CSRFToken string
	ExpiresAt time.Time
}

type AgentKey struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type Project struct {
	ID                   string     `json:"id"`
	Slug                 string     `json:"slug"`
	Name                 string     `json:"name"`
	GitHubRepo           string     `json:"github_repository,omitempty"`
	GitHubInstallationID int64      `json:"github_installation_id,omitempty"`
	GitHubRepositoryID   int64      `json:"github_repository_id,omitempty"`
	ArchivedAt           *time.Time `json:"archived_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

type ProjectToken struct {
	ID         string     `json:"id"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type GitHubInstallation struct {
	ID                  int64     `json:"id"`
	AccountLogin        string    `json:"account_login"`
	TargetType          string    `json:"target_type"`
	RepositorySelection string    `json:"repository_selection"`
	Status              string    `json:"status"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type GitHubRepository struct {
	ID             int64  `json:"id"`
	InstallationID int64  `json:"installation_id"`
	FullName       string `json:"full_name"`
}

func randomID(prefix string, bytes int) (string, error) {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func (s *Store) ensureLegacyColumns(ctx context.Context) error {
	columns := map[string]string{
		"apps.slug":                       "ALTER TABLE apps ADD COLUMN slug TEXT NOT NULL DEFAULT ''",
		"apps.project_id":                 "ALTER TABLE apps ADD COLUMN project_id TEXT NOT NULL DEFAULT ''",
		"apps.display_name":               "ALTER TABLE apps ADD COLUMN display_name TEXT NOT NULL DEFAULT ''",
		"apps.owner_user_id":              "ALTER TABLE apps ADD COLUMN owner_user_id INTEGER NOT NULL DEFAULT 0",
		"apps.github_installation_id":     "ALTER TABLE apps ADD COLUMN github_installation_id INTEGER NOT NULL DEFAULT 0",
		"apps.github_repository_id":       "ALTER TABLE apps ADD COLUMN github_repository_id INTEGER NOT NULL DEFAULT 0",
		"apps.archived_at_unix":           "ALTER TABLE apps ADD COLUMN archived_at_unix INTEGER",
		"apps.created_at_unix":            "ALTER TABLE apps ADD COLUMN created_at_unix INTEGER NOT NULL DEFAULT 0",
		"apps.updated_at_unix":            "ALTER TABLE apps ADD COLUMN updated_at_unix INTEGER NOT NULL DEFAULT 0",
		"error_groups.project_id":         "ALTER TABLE error_groups ADD COLUMN project_id TEXT NOT NULL DEFAULT ''",
		"error_groups.suppressed_at_unix": "ALTER TABLE error_groups ADD COLUMN suppressed_at_unix INTEGER",
	}
	for key, statement := range columns {
		parts := strings.SplitN(key, ".", 2)
		exists, err := s.columnExists(ctx, parts[0], parts[1])
		if err != nil {
			return err
		}
		if !exists {
			if _, err := s.db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("add %s: %w", key, err)
			}
		}
	}
	now := s.now().UTC().Unix()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE apps SET project_id = 'prj_' || name WHERE project_id = '';
	`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE apps SET display_name = name WHERE display_name = ''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE apps SET slug = name WHERE slug = ''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE apps SET created_at_unix = ?, updated_at_unix = ? WHERE created_at_unix = 0`, now, now); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		UPDATE error_groups SET project_id = COALESCE((SELECT project_id FROM apps WHERE apps.name = error_groups.app_name), '')
		WHERE project_id = ''
	`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_apps_project_id ON apps(project_id) WHERE project_id != ''`); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_apps_owner_slug ON apps(owner_user_id,slug) WHERE owner_user_id != 0`)
	return err
}

func (s *Store) columnExists(ctx context.Context, table, column string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) initializeControlPlane(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			github_id INTEGER NOT NULL UNIQUE,
			login TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			avatar_url TEXT NOT NULL DEFAULT '',
			created_at_unix INTEGER NOT NULL,
			updated_at_unix INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			token_hash TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			csrf_token TEXT NOT NULL,
			expires_at_unix INTEGER NOT NULL,
			created_at_unix INTEGER NOT NULL,
			FOREIGN KEY(user_id) REFERENCES users(id)
		)`,
		`CREATE TABLE IF NOT EXISTS agent_keys (
			id TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			prefix TEXT NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			created_at_unix INTEGER NOT NULL,
			last_used_at_unix INTEGER,
			revoked_at_unix INTEGER,
			FOREIGN KEY(user_id) REFERENCES users(id)
		)`,
		`CREATE TABLE IF NOT EXISTS bootstrap_tokens (
			token_hash TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			expires_at_unix INTEGER NOT NULL,
			used_at_unix INTEGER,
			created_at_unix INTEGER NOT NULL,
			FOREIGN KEY(user_id) REFERENCES users(id)
		)`,
		`CREATE TABLE IF NOT EXISTS project_tokens (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			prefix TEXT NOT NULL,
			token_hash TEXT NOT NULL UNIQUE,
			created_at_unix INTEGER NOT NULL,
			last_used_at_unix INTEGER,
			revoked_at_unix INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS github_installations (
			installation_id INTEGER PRIMARY KEY,
			user_id INTEGER NOT NULL,
			account_login TEXT NOT NULL,
			target_type TEXT NOT NULL,
			repository_selection TEXT NOT NULL,
			status TEXT NOT NULL,
			updated_at_unix INTEGER NOT NULL,
			FOREIGN KEY(user_id) REFERENCES users(id)
		)`,
		`CREATE TABLE IF NOT EXISTS github_repositories (
			repository_id INTEGER PRIMARY KEY,
			installation_id INTEGER NOT NULL,
			full_name TEXT NOT NULL,
			active INTEGER NOT NULL DEFAULT 1,
			updated_at_unix INTEGER NOT NULL,
			FOREIGN KEY(installation_id) REFERENCES github_installations(installation_id)
		)`,
		`CREATE TABLE IF NOT EXISTS issue_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id INTEGER NOT NULL,
			action TEXT NOT NULL,
			delivery_key TEXT NOT NULL DEFAULT '',
			state TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at_unix INTEGER NOT NULL,
			lease_expires_at_unix INTEGER,
			last_error TEXT NOT NULL DEFAULT '',
			created_at_unix INTEGER NOT NULL,
			completed_at_unix INTEGER,
			failed_at_unix INTEGER,
			FOREIGN KEY(group_id) REFERENCES error_groups(id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_expiry ON sessions(expires_at_unix)`,
		`CREATE INDEX IF NOT EXISTS idx_agent_keys_hash ON agent_keys(token_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_project_tokens_hash ON project_tokens(token_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_issue_jobs_pending ON issue_jobs(state, next_attempt_at_unix)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("initialize control plane: %w", err)
		}
	}
	return nil
}

func (s *Store) ensureIssueJobColumns(ctx context.Context) error {
	columns := map[string]string{
		"delivery_key":          "ALTER TABLE issue_jobs ADD COLUMN delivery_key TEXT NOT NULL DEFAULT ''",
		"lease_expires_at_unix": "ALTER TABLE issue_jobs ADD COLUMN lease_expires_at_unix INTEGER",
		"failed_at_unix":        "ALTER TABLE issue_jobs ADD COLUMN failed_at_unix INTEGER",
	}
	for column, statement := range columns {
		exists, err := s.columnExists(ctx, "issue_jobs", column)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := s.db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("add issue_jobs.%s: %w", column, err)
			}
		}
	}
	now := s.now().UTC().Unix()
	if _, err := s.db.ExecContext(ctx, `UPDATE issue_jobs SET delivery_key = 'legacy-' || id || '-' || created_at_unix WHERE delivery_key = ''`); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE issue_jobs SET state='pending',next_attempt_at_unix=?,lease_expires_at_unix=NULL WHERE state='working' AND lease_expires_at_unix IS NULL`, now); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_issue_jobs_delivery_key ON issue_jobs(delivery_key) WHERE delivery_key != ''`)
	return err
}

func (s *Store) UpsertGitHubUser(ctx context.Context, githubID int64, login, name, avatar string) (User, error) {
	if githubID < 1 || strings.TrimSpace(login) == "" {
		return User{}, fmt.Errorf("github identity is required")
	}
	now := s.now().UTC().Unix()
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (github_id, login, name, avatar_url, created_at_unix, updated_at_unix)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(github_id) DO UPDATE SET login = excluded.login, name = excluded.name,
			avatar_url = excluded.avatar_url, updated_at_unix = excluded.updated_at_unix
	`, githubID, login, name, avatar, now, now)
	if err != nil {
		return User{}, err
	}
	return s.UserByGitHubID(ctx, githubID)
}

func (s *Store) UserByGitHubID(ctx context.Context, githubID int64) (User, error) {
	var u User
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id, github_id, login, name, avatar_url, created_at_unix, updated_at_unix FROM users WHERE github_id = ?`, githubID).
		Scan(&u.ID, &u.GitHubID, &u.Login, &u.Name, &u.AvatarURL, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	u.CreatedAt = time.Unix(created, 0).UTC()
	u.UpdatedAt = time.Unix(updated, 0).UTC()
	return u, err
}

func (s *Store) AssignLegacyProjects(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE apps SET owner_user_id = ? WHERE owner_user_id = 0`, userID)
	return err
}

func (s *Store) ClearLegacyTokens(ctx context.Context, userID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE apps SET token_hash='' WHERE owner_user_id=?`, userID)
	return err
}

func (s *Store) CreateSession(ctx context.Context, userID int64, ttl time.Duration) (token string, session Session, err error) {
	token, err = randomID("ses_", 32)
	if err != nil {
		return "", Session{}, err
	}
	csrf, err := randomID("csrf_", 24)
	if err != nil {
		return "", Session{}, err
	}
	now := s.now().UTC()
	expires := now.Add(ttl)
	_, err = s.db.ExecContext(ctx, `INSERT INTO sessions (token_hash, user_id, csrf_token, expires_at_unix, created_at_unix) VALUES (?, ?, ?, ?, ?)`, HashToken(token), userID, csrf, expires.Unix(), now.Unix())
	if err != nil {
		return "", Session{}, err
	}
	u, err := s.userByID(ctx, userID)
	return token, Session{User: u, CSRFToken: csrf, ExpiresAt: expires}, err
}

func (s *Store) SessionByToken(ctx context.Context, token string) (Session, error) {
	var userID, expires int64
	var csrf string
	err := s.db.QueryRowContext(ctx, `SELECT user_id, csrf_token, expires_at_unix FROM sessions WHERE token_hash = ? AND expires_at_unix > ?`, HashToken(token), s.now().UTC().Unix()).Scan(&userID, &csrf, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrUnauthorized
	}
	if err != nil {
		return Session{}, err
	}
	u, err := s.userByID(ctx, userID)
	return Session{User: u, CSRFToken: csrf, ExpiresAt: time.Unix(expires, 0).UTC()}, err
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, HashToken(token))
	return err
}

func (s *Store) userByID(ctx context.Context, id int64) (User, error) {
	var u User
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT id, github_id, login, name, avatar_url, created_at_unix, updated_at_unix FROM users WHERE id = ?`, id).
		Scan(&u.ID, &u.GitHubID, &u.Login, &u.Name, &u.AvatarURL, &created, &updated)
	if err != nil {
		return User{}, err
	}
	u.CreatedAt, u.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	return u, nil
}

func (s *Store) CreateBootstrapToken(ctx context.Context, userID int64, name string, ttl time.Duration) (string, error) {
	name = strings.TrimSpace(name)
	if len(name) > maxAgentNameLength {
		return "", fmt.Errorf("agent name is too long")
	}
	token, err := randomID("boot_", 32)
	if err != nil {
		return "", err
	}
	now := s.now().UTC()
	_, err = s.db.ExecContext(ctx, `INSERT INTO bootstrap_tokens (token_hash, user_id, name, expires_at_unix, created_at_unix) VALUES (?, ?, ?, ?, ?)`, HashToken(token), userID, name, now.Add(ttl).Unix(), now.Unix())
	return token, err
}

func (s *Store) ExchangeBootstrapToken(ctx context.Context, bootstrapToken, name string) (string, AgentKey, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", AgentKey{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var userID int64
	var storedName string
	err = tx.QueryRowContext(ctx, `SELECT user_id, name FROM bootstrap_tokens WHERE token_hash = ? AND used_at_unix IS NULL AND expires_at_unix > ?`, HashToken(bootstrapToken), s.now().UTC().Unix()).Scan(&userID, &storedName)
	if errors.Is(err, sql.ErrNoRows) {
		return "", AgentKey{}, ErrUnauthorized
	}
	if err != nil {
		return "", AgentKey{}, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_keys WHERE user_id = ? AND revoked_at_unix IS NULL`, userID).Scan(&count); err != nil {
		return "", AgentKey{}, err
	}
	if count >= MaxActiveAgentKeys {
		return "", AgentKey{}, fmt.Errorf("agent key limit reached")
	}
	if strings.TrimSpace(name) == "" {
		name = storedName
	}
	if strings.TrimSpace(name) == "" {
		name = "Agent"
	}
	if len(strings.TrimSpace(name)) > maxAgentNameLength {
		return "", AgentKey{}, fmt.Errorf("agent name is too long")
	}
	token, key, err := createAgentKeyTx(ctx, tx, userID, name, s.now().UTC())
	if err != nil {
		return "", AgentKey{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE bootstrap_tokens SET used_at_unix = ? WHERE token_hash = ?`, s.now().UTC().Unix(), HashToken(bootstrapToken)); err != nil {
		return "", AgentKey{}, err
	}
	if err := tx.Commit(); err != nil {
		return "", AgentKey{}, err
	}
	return token, key, nil
}

func createAgentKeyTx(ctx context.Context, tx *sql.Tx, userID int64, name string, now time.Time) (string, AgentKey, error) {
	secret, err := randomID("", 32)
	if err != nil {
		return "", AgentKey{}, err
	}
	id, err := randomID("key_", 12)
	if err != nil {
		return "", AgentKey{}, err
	}
	prefix := secret[:8]
	token := "raises_agent_" + prefix + "_" + secret[8:]
	_, err = tx.ExecContext(ctx, `INSERT INTO agent_keys (id, user_id, name, prefix, token_hash, created_at_unix) VALUES (?, ?, ?, ?, ?, ?)`, id, userID, strings.TrimSpace(name), prefix, HashToken(token), now.Unix())
	return token, AgentKey{ID: id, Name: strings.TrimSpace(name), Prefix: prefix, CreatedAt: now}, err
}

func (s *Store) CreateAgentKey(ctx context.Context, userID int64, name string) (string, AgentKey, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", AgentKey{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM agent_keys WHERE user_id = ? AND revoked_at_unix IS NULL`, userID).Scan(&count); err != nil {
		return "", AgentKey{}, err
	}
	if count >= MaxActiveAgentKeys {
		return "", AgentKey{}, fmt.Errorf("agent key limit reached")
	}
	if strings.TrimSpace(name) == "" {
		name = "Agent"
	}
	if len(strings.TrimSpace(name)) > maxAgentNameLength {
		return "", AgentKey{}, fmt.Errorf("agent name is too long")
	}
	token, key, err := createAgentKeyTx(ctx, tx, userID, name, s.now().UTC())
	if err != nil {
		return "", AgentKey{}, err
	}
	return token, key, tx.Commit()
}

func (s *Store) AuthenticateAgent(ctx context.Context, token string) (int64, error) {
	var userID int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM agent_keys WHERE token_hash = ? AND revoked_at_unix IS NULL`, HashToken(token)).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrUnauthorized
	}
	if err != nil {
		return 0, err
	}
	_, _ = s.db.ExecContext(ctx, `UPDATE agent_keys SET last_used_at_unix = ? WHERE token_hash = ?`, s.now().UTC().Unix(), HashToken(token))
	return userID, nil
}

func (s *Store) ListAgentKeys(ctx context.Context, userID int64) ([]AgentKey, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, prefix, created_at_unix, last_used_at_unix, revoked_at_unix FROM agent_keys WHERE user_id = ? ORDER BY created_at_unix DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []AgentKey
	for rows.Next() {
		var k AgentKey
		var created int64
		var last, revoked sql.NullInt64
		if err := rows.Scan(&k.ID, &k.Name, &k.Prefix, &created, &last, &revoked); err != nil {
			return nil, err
		}
		k.CreatedAt = time.Unix(created, 0).UTC()
		if last.Valid {
			t := time.Unix(last.Int64, 0).UTC()
			k.LastUsedAt = &t
		}
		if revoked.Valid {
			t := time.Unix(revoked.Int64, 0).UTC()
			k.RevokedAt = &t
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

func (s *Store) RevokeAgentKey(ctx context.Context, userID int64, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE agent_keys SET revoked_at_unix = ? WHERE id = ? AND user_id = ? AND revoked_at_unix IS NULL`, s.now().UTC().Unix(), id, userID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListProjects(ctx context.Context, userID int64, includeArchived bool) ([]Project, error) {
	query := `SELECT project_id, slug, display_name, github_repo, github_installation_id, github_repository_id, archived_at_unix, created_at_unix, updated_at_unix FROM apps WHERE owner_user_id = ?`
	if !includeArchived {
		query += ` AND archived_at_unix IS NULL`
	}
	query += ` ORDER BY created_at_unix, name`
	rows, err := s.db.QueryContext(ctx, query, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var projects []Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

func scanProject(row rowScanner) (Project, error) {
	var p Project
	var archived sql.NullInt64
	var created, updated int64
	err := row.Scan(&p.ID, &p.Slug, &p.Name, &p.GitHubRepo, &p.GitHubInstallationID, &p.GitHubRepositoryID, &archived, &created, &updated)
	if err != nil {
		return Project{}, err
	}
	p.CreatedAt, p.UpdatedAt = time.Unix(created, 0).UTC(), time.Unix(updated, 0).UTC()
	if archived.Valid {
		t := time.Unix(archived.Int64, 0).UTC()
		p.ArchivedAt = &t
	}
	return p, nil
}

func (s *Store) GetProject(ctx context.Context, userID int64, id string) (Project, error) {
	row := s.db.QueryRowContext(ctx, `SELECT project_id,slug,display_name,github_repo,github_installation_id,github_repository_id,archived_at_unix,created_at_unix,updated_at_unix FROM apps WHERE owner_user_id=? AND project_id=?`, userID, id)
	p, err := scanProject(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	return p, err
}

func (s *Store) CreateProject(ctx context.Context, userID int64, name, slug string) (Project, error) {
	name = strings.TrimSpace(name)
	slug = strings.ToLower(strings.TrimSpace(slug))
	if name == "" || len(name) > maxProjectNameLength || len(slug) > maxProjectSlugLength || !slugPattern.MatchString(slug) {
		return Project{}, fmt.Errorf("valid name and slug are required")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM apps WHERE owner_user_id=? AND archived_at_unix IS NULL`, userID).Scan(&count); err != nil {
		return Project{}, err
	}
	if count >= MaxActiveProjects {
		return Project{}, fmt.Errorf("project limit reached")
	}
	id, err := randomID("prj_", 12)
	if err != nil {
		return Project{}, err
	}
	now := s.now().UTC().Unix()
	internalName := fmt.Sprintf("%d:%s", userID, slug)
	_, err = s.db.ExecContext(ctx, `INSERT INTO apps(name,slug,token_hash,github_repo,project_id,display_name,owner_user_id,created_at_unix,updated_at_unix) VALUES(?,?, '', '', ?, ?, ?, ?, ?)`, internalName, slug, id, name, userID, now, now)
	if err != nil {
		return Project{}, err
	}
	return s.GetProject(ctx, userID, id)
}

func (s *Store) UpdateProject(ctx context.Context, userID int64, id, name string) (Project, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > maxProjectNameLength {
		return Project{}, fmt.Errorf("name is required")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET display_name=?,updated_at_unix=? WHERE owner_user_id=? AND project_id=?`, name, s.now().UTC().Unix(), userID, id)
	if err != nil {
		return Project{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Project{}, ErrNotFound
	}
	return s.GetProject(ctx, userID, id)
}

func (s *Store) SetProjectArchived(ctx context.Context, userID int64, id string, archived bool) (Project, error) {
	project, err := s.GetProject(ctx, userID, id)
	if err != nil {
		return Project{}, err
	}
	if !archived && project.ArchivedAt != nil {
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM apps WHERE owner_user_id=? AND archived_at_unix IS NULL`, userID).Scan(&count); err != nil {
			return Project{}, err
		}
		if count >= MaxActiveProjects {
			return Project{}, fmt.Errorf("project limit reached")
		}
	}
	var value any
	if archived {
		value = s.now().UTC().Unix()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET archived_at_unix=?,updated_at_unix=? WHERE owner_user_id=? AND project_id=?`, value, s.now().UTC().Unix(), userID, id)
	if err != nil {
		return Project{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Project{}, ErrNotFound
	}
	return s.GetProject(ctx, userID, id)
}

func (s *Store) CreateProjectToken(ctx context.Context, userID int64, projectID string) (string, ProjectToken, error) {
	project, err := s.GetProject(ctx, userID, projectID)
	if err != nil {
		return "", ProjectToken{}, err
	}
	if project.ArchivedAt != nil {
		return "", ProjectToken{}, fmt.Errorf("project is archived")
	}
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM project_tokens WHERE project_id=? AND revoked_at_unix IS NULL`, projectID).Scan(&count); err != nil {
		return "", ProjectToken{}, err
	}
	if count >= MaxActiveProjectTokens {
		return "", ProjectToken{}, fmt.Errorf("ingestion token limit reached")
	}
	secret, err := randomID("", 32)
	if err != nil {
		return "", ProjectToken{}, err
	}
	id, err := randomID("ptk_", 12)
	if err != nil {
		return "", ProjectToken{}, err
	}
	prefix := secret[:8]
	token := "raises_ingest_" + prefix + "_" + secret[8:]
	now := s.now().UTC()
	_, err = s.db.ExecContext(ctx, `INSERT INTO project_tokens(id,project_id,prefix,token_hash,created_at_unix) VALUES(?,?,?,?,?)`, id, projectID, prefix, HashToken(token), now.Unix())
	return token, ProjectToken{ID: id, Prefix: prefix, CreatedAt: now}, err
}

func (s *Store) RevokeProjectToken(ctx context.Context, userID int64, projectID, tokenID string) error {
	if _, err := s.GetProject(ctx, userID, projectID); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE project_tokens SET revoked_at_unix=? WHERE id=? AND project_id=? AND revoked_at_unix IS NULL`, s.now().UTC().Unix(), tokenID, projectID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpsertInstallation(ctx context.Context, userID int64, installationID int64, account, targetType, selection, status string, repos []GitHubRepository) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := s.now().UTC().Unix()
	_, err = tx.ExecContext(ctx, `INSERT INTO github_installations(installation_id,user_id,account_login,target_type,repository_selection,status,updated_at_unix) VALUES(?,?,?,?,?,?,?) ON CONFLICT(installation_id) DO UPDATE SET user_id=excluded.user_id,account_login=excluded.account_login,target_type=excluded.target_type,repository_selection=excluded.repository_selection,status=excluded.status,updated_at_unix=excluded.updated_at_unix`, installationID, userID, account, targetType, selection, status, now)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE github_repositories SET active=0,updated_at_unix=? WHERE installation_id=?`, now, installationID); err != nil {
		return err
	}
	for _, repo := range repos {
		if _, err = tx.ExecContext(ctx, `INSERT INTO github_repositories(repository_id,installation_id,full_name,active,updated_at_unix) VALUES(?,?,?,1,?) ON CONFLICT(repository_id) DO UPDATE SET installation_id=excluded.installation_id,full_name=excluded.full_name,active=1,updated_at_unix=excluded.updated_at_unix`, repo.ID, installationID, repo.FullName, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListInstallations(ctx context.Context, userID int64) ([]GitHubInstallation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT installation_id,account_login,target_type,repository_selection,status,updated_at_unix FROM github_installations WHERE user_id=? ORDER BY account_login`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GitHubInstallation
	for rows.Next() {
		var i GitHubInstallation
		var updated int64
		if err := rows.Scan(&i.ID, &i.AccountLogin, &i.TargetType, &i.RepositorySelection, &i.Status, &updated); err != nil {
			return nil, err
		}
		i.UpdatedAt = time.Unix(updated, 0).UTC()
		out = append(out, i)
	}
	return out, rows.Err()
}

func (s *Store) InstallationUserID(ctx context.Context, installationID int64) (int64, error) {
	var userID int64
	err := s.db.QueryRowContext(ctx, `SELECT user_id FROM github_installations WHERE installation_id=?`, installationID).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return userID, err
}
func (s *Store) SetInstallationStatus(ctx context.Context, installationID int64, status string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE github_installations SET status=?,updated_at_unix=? WHERE installation_id=?`, status, s.now().UTC().Unix(), installationID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	if status != "active" {
		_, _ = s.db.ExecContext(ctx, `UPDATE apps SET github_installation_id=0,github_repository_id=0,github_repo='' WHERE github_installation_id=?`, installationID)
	}
	return nil
}

func (s *Store) ListGitHubRepositories(ctx context.Context, userID int64) ([]GitHubRepository, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.repository_id,r.installation_id,r.full_name FROM github_repositories r JOIN github_installations i ON i.installation_id=r.installation_id WHERE i.user_id=? AND i.status='active' AND r.active=1 ORDER BY r.full_name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GitHubRepository
	for rows.Next() {
		var r GitHubRepository
		if err := rows.Scan(&r.ID, &r.InstallationID, &r.FullName); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) BindProjectRepository(ctx context.Context, userID int64, projectID string, repositoryID int64) (Project, error) {
	project, err := s.GetProject(ctx, userID, projectID)
	if err != nil {
		return Project{}, err
	}
	if project.ArchivedAt != nil {
		return Project{}, fmt.Errorf("project is archived")
	}
	var installationID int64
	var fullName string
	err = s.db.QueryRowContext(ctx, `SELECT r.installation_id,r.full_name FROM github_repositories r JOIN github_installations i ON i.installation_id=r.installation_id WHERE r.repository_id=? AND r.active=1 AND i.user_id=? AND i.status='active'`, repositoryID, userID).Scan(&installationID, &fullName)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, ErrNotFound
	}
	if err != nil {
		return Project{}, err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET github_installation_id=?,github_repository_id=?,github_repo=?,updated_at_unix=? WHERE project_id=? AND owner_user_id=?`, installationID, repositoryID, fullName, s.now().UTC().Unix(), projectID, userID)
	if err != nil {
		return Project{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Project{}, ErrNotFound
	}
	return s.GetProject(ctx, userID, projectID)
}

func (s *Store) UnbindProjectRepository(ctx context.Context, userID int64, projectID string) (Project, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE apps SET github_installation_id=0,github_repository_id=0,github_repo='',updated_at_unix=? WHERE project_id=? AND owner_user_id=?`, s.now().UTC().Unix(), projectID, userID)
	if err != nil {
		return Project{}, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return Project{}, ErrNotFound
	}
	return s.GetProject(ctx, userID, projectID)
}
