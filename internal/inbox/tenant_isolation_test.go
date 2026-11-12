package inbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

func testStoreWithFiler(t *testing.T, filer IssueFiler) (*Store, *fakeFiler) {
	t.Helper()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	store, err := Open(filepath.Join(t.TempDir(), "raises.db"), func() time.Time { return now }, filer)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	fake, _ := filer.(*fakeFiler)
	return store, fake
}

// Two unrelated owners create projects with the same slug. Ingestion,
// grouping and every observable side effect must stay per project identity.
func tenantProjectWithToken(t *testing.T, store *Store, ctx context.Context, user User, display, slug string) (Project, string) {
	t.Helper()
	project, err := store.CreateProject(ctx, user.ID, display, slug)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.CreateProjectToken(ctx, user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	return project, token
}

func sameNotice(contextValue string) Notice {
	return Notice{
		Env:       "production",
		Revision:  "aaa",
		Class:     "RuntimeError",
		Message:   "boom",
		Backtrace: []string{"app/jobs/work_job.rb:4:in `perform`"},
		Context:   map[string]any{"api_key": contextValue},
	}
}

func TestSameSlugAcrossOwnersGetsIsolatedGroups(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	ownerA, _ := store.UpsertGitHubUser(ctx, 101, "owner-a", "", "")
	ownerB, _ := store.UpsertGitHubUser(ctx, 102, "owner-b", "", "")
	projectA, tokenA := tenantProjectWithToken(t, store, ctx, ownerA, "Web A", "web")
	projectB, tokenB := tenantProjectWithToken(t, store, ctx, ownerB, "Web B", "web")

	first, err := store.Ingest(ctx, tokenA, sameNotice("DUMMY-A-VALUE"))
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created {
		t.Fatal("first ingest should create a group")
	}

	second, err := store.Ingest(ctx, tokenB, sameNotice("DUMMY-B-VALUE"))
	if err != nil {
		t.Fatal(err)
	}
	if !second.Created {
		t.Fatal("second tenant must get its own group, not join the first tenant's")
	}
	if second.Group.ID == first.Group.ID {
		t.Fatal("tenants share one group row")
	}
	if second.Group.ProjectID != projectB.ID {
		t.Fatalf("tenant B group project=%q want %q", second.Group.ProjectID, projectB.ID)
	}
	if first.Group.ProjectID != projectA.ID {
		t.Fatalf("tenant A group project=%q want %q", first.Group.ProjectID, projectA.ID)
	}
	if got := second.Group.Sample.Context["api_key"]; got != "DUMMY-B-VALUE" {
		t.Fatalf("tenant B sample leaks another tenant: %v", second.Group.Sample.Context)
	}
	if second.Group.GitHubIssueURL != "" {
		t.Fatalf("tenant B group carries another tenant's issue: %q", second.Group.GitHubIssueURL)
	}

	third, err := store.Ingest(ctx, tokenA, sameNotice("DUMMY-A-VALUE"))
	if err != nil {
		t.Fatal(err)
	}
	if third.Created || third.Group.ID != first.Group.ID || third.Group.Count != 2 {
		t.Fatalf("same-project notices must still deduplicate: %#v", third.Group)
	}
	if third.Group.ProjectID != projectA.ID {
		t.Fatalf("repeat ingest moved project=%q", third.Group.ProjectID)
	}
}

func TestCollisionInsertionOrderReversed(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	ownerA, _ := store.UpsertGitHubUser(ctx, 111, "owner-a", "", "")
	ownerB, _ := store.UpsertGitHubUser(ctx, 112, "owner-b", "", "")
	_, tokenA := tenantProjectWithToken(t, store, ctx, ownerA, "Web A", "web")
	_, tokenB := tenantProjectWithToken(t, store, ctx, ownerB, "Web B", "web")

	// Attacker-shaped order: B ingests first, then the victim.
	bFirst, err := store.Ingest(ctx, tokenB, sameNotice("DUMMY-B-VALUE"))
	if err != nil {
		t.Fatal(err)
	}
	aNext, err := store.Ingest(ctx, tokenA, sameNotice("DUMMY-A-VALUE"))
	if err != nil {
		t.Fatal(err)
	}
	if !aNext.Created || aNext.Group.ID == bFirst.Group.ID {
		t.Fatalf("victim joined attacker's group: %#v", aNext.Group)
	}

	visible, err := store.ListForUser(ctx, ownerA.ID, "web", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(visible) != 1 || visible[0].ID != aNext.Group.ID {
		t.Fatalf("victim listing=%v", visible)
	}
	if _, err := store.GetForUser(ctx, ownerA.ID, bFirst.Group.ID); err == nil {
		t.Fatal("victim can read attacker's group")
	}
	notices, err := store.NoticesForUser(ctx, ownerB.ID, bFirst.Group.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notices {
		if got := n.Context["api_key"]; got == "DUMMY-A-VALUE" {
			t.Fatalf("attacker reads victim notice context: %v", n.Context)
		}
	}
}

func TestCollisionDoesNotMutateOtherTenant(t *testing.T) {
	filer := &fakeFiler{}
	store, _ := testStoreWithFiler(t, filer)
	ctx := context.Background()
	ownerA, _ := store.UpsertGitHubUser(ctx, 121, "owner-a", "", "")
	ownerB, _ := store.UpsertGitHubUser(ctx, 122, "owner-b", "", "")
	projectA, tokenA := tenantProjectWithToken(t, store, ctx, ownerA, "Web A", "web")
	_, tokenB := tenantProjectWithToken(t, store, ctx, ownerB, "Web B", "web")
	if err := store.UpsertInstallation(ctx, ownerA.ID, 4242, "owner-a", "Organization", "all", "active",
		[]GitHubRepository{{ID: 777, InstallationID: 4242, FullName: "owner-a/web"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.BindProjectRepository(ctx, ownerA.ID, projectA.ID, 777); err != nil {
		t.Fatal(err)
	}

	first, err := store.Ingest(ctx, tokenA, sameNotice("DUMMY-A-VALUE"))
	if err != nil {
		t.Fatal(err)
	}
	if filer.opened != 1 || first.Group.GitHubIssueNumber == 0 {
		t.Fatalf("victim issue was not filed: opened=%d group=%#v", filer.opened, first.Group)
	}
	if _, err := store.AckForUser(ctx, ownerA.ID, first.Group.ID); err != nil {
		t.Fatal(err)
	}

	// Same fingerprint, new revision from the other tenant: must create and
	// regress only its own group, never touch A's group or A's repo.
	other := sameNotice("DUMMY-B-VALUE")
	other.Revision = "bbb"
	second, err := store.Ingest(ctx, tokenB, other)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Created {
		t.Fatal("other tenant must create its own group")
	}

	again, err := store.Get(ctx, first.Group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.AckedAt == nil {
		t.Fatal("other tenant cleared victim acknowledgement")
	}
	if again.Message != "boom" || again.Count != 1 {
		t.Fatalf("other tenant rewrote victim group: %#v", again)
	}
	if filer.reopened != 0 {
		t.Fatalf("reopened=%d on victim repo", filer.reopened)
	}

	var payload string
	var payloadOwner int64
	var payloadProject string
	err = store.db.QueryRowContext(ctx, `SELECT owner_user_id, project_id, payload_json FROM outbound_events WHERE group_id = ? ORDER BY created_at_unix LIMIT 1`, second.Group.ID).Scan(&payloadOwner, &payloadProject, &payload)
	if err != nil {
		t.Fatalf("no outbound event for tenant B group: %v", err)
	}
	if payloadOwner != ownerB.ID {
		t.Fatalf("event owner=%d want %d", payloadOwner, ownerB.ID)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatal(err)
	}
	eventErr, _ := decoded["error"].(map[string]any)
	if eventErr["project_id"] != payloadProject {
		t.Fatalf("event payload project=%v", eventErr["project_id"])
	}
	var groupProject string
	if err := store.db.QueryRowContext(ctx, `SELECT project_id FROM error_groups WHERE id = ?`, second.Group.ID).Scan(&groupProject); err != nil {
		t.Fatal(err)
	}
	if payloadProject != groupProject {
		t.Fatalf("event project=%q group project=%q", payloadProject, groupProject)
	}
}

func TestLegacyUnambiguousGroupKeepsProjectContinuity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	fp, location := Fingerprint("RuntimeError", []string{"app/jobs/work_job.rb:4:in `perform`"}, nil)
	path := testPreFixDB(t, []preFixApp{
		{name: "141:solo", slug: "solo", projectID: "prj_solo_141", ownerID: 141, tokenHash: HashToken("solo-token")},
	}, []preFixGroup{
		{appName: "solo", projectID: "", env: "production", fingerprint: fp, location: location},
	}, now)
	store, err := Open(path, func() time.Time { return now }, &fakeFiler{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertTenantUniqueKey(t, store, ctx)

	// Unambiguous slug resolves the legacy row to its project; ingest joins it.
	owner, _ := store.UpsertGitHubUser(ctx, 141, "owner-solo", "", "")
	adoptLegacyApp(t, store, ctx, owner, "prj_solo_141")
	token := issueTokenForProject(t, store, ctx, owner, "prj_solo_141")
	result, err := store.Ingest(ctx, token, sameNotice("DUMMY-SOLO"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Created || result.Group.ID != 1 || result.Group.Count != 2 {
		t.Fatalf("legacy continuity broken: created=%v group=%#v", result.Created, result.Group)
	}
	if result.Group.ProjectID != "prj_solo_141" {
		t.Fatalf("project=%q", result.Group.ProjectID)
	}
}

func TestLegacyCollidingGroupsMigrateWithoutMerging(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	fp, location := Fingerprint("RuntimeError", []string{"app/jobs/work_job.rb:4:in `perform`"}, nil)
	path := testPreFixDB(t, []preFixApp{
		{name: "131:web", slug: "web", projectID: "prj_web_131", ownerID: 131, tokenHash: HashToken("token-a")},
		{name: "132:web", slug: "web", projectID: "prj_web_132", ownerID: 132, tokenHash: HashToken("token-b")},
	}, []preFixGroup{
		{appName: "web", projectID: "", env: "production", fingerprint: fp, location: location},
	}, now)
	store, err := Open(path, func() time.Time { return now }, &fakeFiler{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertTenantUniqueKey(t, store, ctx)

	// Ambiguous history is quarantined, never reassigned: it keeps its row,
	// stays invisible to both tenants, and neither tenant joins it.
	var projectID string
	if err := store.db.QueryRowContext(ctx, `SELECT project_id FROM error_groups WHERE id = 1`).Scan(&projectID); err != nil {
		t.Fatal(err)
	}
	if projectID != "" {
		t.Fatalf("ambiguous row was reassigned to %q", projectID)
	}

	ownerA, _ := store.UpsertGitHubUser(ctx, 131, "owner-a", "", "")
	ownerB, _ := store.UpsertGitHubUser(ctx, 132, "owner-b", "", "")
	adoptLegacyApp(t, store, ctx, ownerA, "prj_web_131")
	adoptLegacyApp(t, store, ctx, ownerB, "prj_web_132")
	tokenA := issueTokenForProject(t, store, ctx, ownerA, "prj_web_131")
	tokenB := issueTokenForProject(t, store, ctx, ownerB, "prj_web_132")

	aRes, err := store.Ingest(ctx, tokenA, sameNotice("DUMMY-A-VALUE"))
	if err != nil {
		t.Fatal(err)
	}
	bRes, err := store.Ingest(ctx, tokenB, sameNotice("DUMMY-B-VALUE"))
	if err != nil {
		t.Fatal(err)
	}
	if !aRes.Created || !bRes.Created || aRes.Group.ID == bRes.Group.ID {
		t.Fatalf("tenants must each start isolated history: A=%#v B=%#v", aRes.Group, bRes.Group)
	}
	if aRes.Group.ProjectID != "prj_web_131" || bRes.Group.ProjectID != "prj_web_132" {
		t.Fatalf("A=%q B=%q", aRes.Group.ProjectID, bRes.Group.ProjectID)
	}
}

type preFixApp struct {
	name, slug, projectID string
	ownerID               int64
	tokenHash             string
}

type preFixGroup struct {
	appName, projectID, env, fingerprint, location string
}

// testPreFixDB builds a database as it looked before tenant isolation: the
// group unique key is (app_name, env, fingerprint) and legacy rows carry no
// project_id.
func testPreFixDB(t *testing.T, apps []preFixApp, groups []preFixGroup, now time.Time) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "prefix.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		`CREATE TABLE apps (name TEXT PRIMARY KEY, slug TEXT NOT NULL DEFAULT '', token_hash TEXT NOT NULL, github_repo TEXT NOT NULL DEFAULT '', project_id TEXT NOT NULL DEFAULT '', display_name TEXT NOT NULL DEFAULT '', owner_user_id INTEGER NOT NULL DEFAULT 0, github_installation_id INTEGER NOT NULL DEFAULT 0, github_repository_id INTEGER NOT NULL DEFAULT 0, archived_at_unix INTEGER, created_at_unix INTEGER NOT NULL DEFAULT 0, updated_at_unix INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE error_groups (id INTEGER PRIMARY KEY AUTOINCREMENT, project_id TEXT NOT NULL DEFAULT '', app_name TEXT NOT NULL, env TEXT NOT NULL, fingerprint TEXT NOT NULL, class TEXT NOT NULL, location TEXT NOT NULL, message TEXT NOT NULL, count INTEGER NOT NULL, first_seen_unix INTEGER NOT NULL, last_seen_unix INTEGER NOT NULL, last_revision TEXT NOT NULL DEFAULT '', github_issue_number INTEGER NOT NULL DEFAULT 0, github_issue_url TEXT NOT NULL DEFAULT '', acked_at_unix INTEGER, suppressed_at_unix INTEGER, resolved_at_unix INTEGER, sample_json TEXT NOT NULL, UNIQUE(app_name, env, fingerprint))`,
		`CREATE TABLE notices (id INTEGER PRIMARY KEY AUTOINCREMENT, group_id INTEGER NOT NULL, received_at_unix INTEGER NOT NULL, revision TEXT NOT NULL DEFAULT '', payload_json TEXT NOT NULL)`,
		`CREATE TABLE issue_jobs (id INTEGER PRIMARY KEY AUTOINCREMENT, group_id INTEGER NOT NULL, action TEXT NOT NULL, delivery_key TEXT NOT NULL DEFAULT '', state TEXT NOT NULL DEFAULT 'pending', attempts INTEGER NOT NULL DEFAULT 0, next_attempt_at_unix INTEGER NOT NULL, lease_expires_at_unix INTEGER, last_error TEXT NOT NULL DEFAULT '', created_at_unix INTEGER NOT NULL, completed_at_unix INTEGER, failed_at_unix INTEGER)`,
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, app := range apps {
		if _, err := db.Exec(`INSERT INTO apps(name,slug,token_hash,project_id,display_name,owner_user_id,created_at_unix,updated_at_unix) VALUES(?,?,?,?,?,?,?,?)`,
			app.name, app.slug, app.tokenHash, app.projectID, app.slug, app.ownerID, now.Unix(), now.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	for _, group := range groups {
		if _, err := db.Exec(`INSERT INTO error_groups(project_id,app_name,env,fingerprint,class,location,message,count,first_seen_unix,last_seen_unix,sample_json) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			group.projectID, group.appName, group.env, group.fingerprint, "RuntimeError", group.location, "boom", 1, now.Unix(), now.Unix(), `{"app":"web"}`); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

// adoptLegacyApp links a fixture app row (which predates its owner row) to a
// freshly created user, mirroring AssignLegacyProjects for single-owner hosts.
func adoptLegacyApp(t *testing.T, store *Store, ctx context.Context, user User, projectID string) {
	t.Helper()
	if _, err := store.db.ExecContext(ctx, `UPDATE apps SET owner_user_id = ? WHERE project_id = ?`, user.ID, projectID); err != nil {
		t.Fatal(err)
	}
}

func issueTokenForProject(t *testing.T, store *Store, ctx context.Context, user User, projectID string) string {
	t.Helper()
	token, _, err := store.CreateProjectToken(ctx, user.ID, projectID)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// assertTenantUniqueKey proves the constraint behaviorally: the same slug may
// repeat across projects, but one project can never hold a duplicate key.
func assertTenantUniqueKey(t *testing.T, store *Store, ctx context.Context) {
	t.Helper()
	if _, err := store.db.ExecContext(ctx, `INSERT INTO error_groups(project_id,app_name,env,fingerprint,class,location,message,count,first_seen_unix,last_seen_unix,sample_json) VALUES('prj_probe_a','web','production','probe-fp','E','loc','m',1,1,1,'{}'),('prj_probe_b','web','production','probe-fp','E','loc','m',1,1,1,'{}')`); err != nil {
		t.Fatalf("same slug must repeat across projects: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO error_groups(project_id,app_name,env,fingerprint,class,location,message,count,first_seen_unix,last_seen_unix,sample_json) VALUES('prj_probe_a','web','production','probe-fp','E','loc','m',1,1,1,'{}')`); err == nil {
		t.Fatal("duplicate key within one project was accepted")
	}
	if _, err := store.db.ExecContext(ctx, `DELETE FROM error_groups WHERE fingerprint = 'probe-fp'`); err != nil {
		t.Fatal(err)
	}
}
