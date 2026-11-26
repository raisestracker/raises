package inbox

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var explainScanPattern = regexp.MustCompile(`\bSCAN\b`)

func TestHotQueriesExplainPlan(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "plans.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	owner, err := store.UpsertGitHubUser(ctx, 8801, "plan-owner", "", "")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := store.CreateProject(ctx, owner.ID, "PlanApp", "plan-app")
	if err != nil {
		t.Fatal(err)
	}
	projTok, _, err := store.CreateProjectToken(ctx, owner.ID, proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	app, err := store.AppByToken(ctx, projTok)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		if _, err := store.IngestForApp(ctx, app, Notice{
			Env:       "production",
			Class:     fmt.Sprintf("PlanError%d", i),
			Message:   "boom",
			Backtrace: []string{fmt.Sprintf("app/models/plan_%d.rb:%d", i, i+1)},
		}); err != nil {
			t.Fatal(err)
		}
	}

	queries := []struct {
		name string
		sql  string
		args []any
	}{
		{
			name: "AppByToken",
			sql: `SELECT a.project_id, a.slug, a.display_name, a.token_hash, a.github_repo, a.owner_user_id,
			             a.github_installation_id, a.github_repository_id, a.archived_at_unix
			      FROM project_tokens pt
			      JOIN apps a ON a.project_id = pt.project_id
			      WHERE pt.token_hash = ? AND pt.revoked_at_unix IS NULL AND a.archived_at_unix IS NULL
			      LIMIT 1`,
			args: []any{HashToken(projTok)},
		},
		{
			name: "ListForUser",
			sql: `SELECT g.id, g.project_id, g.app_name, g.env, g.fingerprint, g.class, g.location, g.message, g.count,
			             g.first_seen_unix, g.last_seen_unix, g.last_revision, g.github_issue_number, g.github_issue_url,
			             g.acked_at_unix, g.suppressed_at_unix, g.resolved_at_unix
			      FROM error_groups g JOIN apps a ON a.project_id = g.project_id
			      WHERE a.owner_user_id = ? AND g.acked_at_unix IS NULL AND g.suppressed_at_unix IS NULL
			      ORDER BY g.last_seen_unix DESC, g.id DESC`,
			args: []any{owner.ID},
		},
		{
			name: "ListPageForUser",
			sql: `SELECT g.id, g.project_id, g.app_name, g.env, g.fingerprint, g.class, g.location, g.message, g.count,
			             g.first_seen_unix, g.last_seen_unix, g.last_revision, g.github_issue_number, g.github_issue_url,
			             g.acked_at_unix, g.suppressed_at_unix, g.resolved_at_unix
			      FROM error_groups g JOIN apps a ON a.project_id = g.project_id
			      WHERE a.owner_user_id = ? AND g.acked_at_unix IS NULL AND g.resolved_at_unix IS NULL AND g.suppressed_at_unix IS NULL
			      ORDER BY g.last_seen_unix DESC, g.id DESC LIMIT 25 OFFSET 0`,
			args: []any{owner.ID},
		},
		{
			name: "ProjectCounts",
			sql: `SELECT COALESCE(SUM(CASE WHEN g.acked_at_unix IS NULL AND g.resolved_at_unix IS NULL AND g.suppressed_at_unix IS NULL THEN 1 ELSE 0 END),0),
			             COALESCE(SUM(CASE WHEN g.resolved_at_unix IS NOT NULL AND g.suppressed_at_unix IS NULL THEN 1 ELSE 0 END),0),
			             COUNT(*)
			      FROM error_groups g WHERE g.project_id = ?`,
			args: []any{proj.ID},
		},
		{
			name: "DashboardForUser_Q3",
			sql: `SELECT g.first_seen_unix, g.acked_at_unix, g.resolved_at_unix
			      FROM error_groups g JOIN apps a ON a.project_id = g.project_id
			      WHERE a.owner_user_id = ? AND a.archived_at_unix IS NULL AND g.suppressed_at_unix IS NULL`,
			args: []any{owner.ID},
		},
		{
			name: "ResolveByIssue",
			sql: `UPDATE error_groups SET resolved_at_unix = ?, acked_at_unix = COALESCE(acked_at_unix, ?)
			      WHERE github_issue_number = ? AND resolved_at_unix IS NULL
			        AND project_id IN (SELECT project_id FROM apps WHERE github_repository_id = ? AND github_installation_id = ?)`,
			args: []any{1700000000, 1700000000, 42, 999, 101},
		},
		{
			name: "IssueDeliveryHealthForUser",
			sql: `SELECT COALESCE(SUM(CASE WHEN j.state IN ('pending','working') THEN 1 ELSE 0 END),0),
			             COALESCE(SUM(CASE WHEN j.state='dead' THEN 1 ELSE 0 END),0),
			             MIN(CASE WHEN j.state IN ('pending','working','dead') THEN j.created_at_unix END)
			      FROM issue_jobs j
			      JOIN error_groups g ON g.id=j.group_id
			      JOIN apps a ON a.project_id=g.project_id
			      WHERE a.owner_user_id=?`,
			args: []any{owner.ID},
		},
		{
			name: "WebhookDeliveryHealthForUser",
			sql: `SELECT COALESCE(SUM(CASE WHEN state IN ('pending','working') THEN 1 ELSE 0 END),0),
			             COALESCE(SUM(CASE WHEN state='dead' THEN 1 ELSE 0 END),0)
			      FROM outbound_deliveries WHERE owner_user_id=? AND destination_kind='webhook'`,
			args: []any{owner.ID},
		},
		{
			name: "PruneOutboundHistory",
			sql: `DELETE FROM outbound_events
			      WHERE created_at_unix < ?
			        AND NOT EXISTS (SELECT 1 FROM outbound_deliveries d WHERE d.event_id = outbound_events.id)`,
			args: []any{1700000000},
		},
		{
			name: "ListAgentKeys",
			sql: `SELECT id, name, prefix, created_at_unix, last_used_at_unix, revoked_at_unix, can_mint, minted_by_key_id
			      FROM agent_keys WHERE user_id = ? ORDER BY created_at_unix DESC`,
			args: []any{owner.ID},
		},
	}

	for _, q := range queries {
		rows, err := store.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+q.sql, q.args...)
		if err != nil {
			t.Fatalf("%s explain failed: %v", q.name, err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				rows.Close()
				t.Fatalf("%s scan failed: %v", q.name, err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		for _, line := range plan {
			if explainScanPattern.MatchString(line) {
				t.Errorf("%s has unindexed SCAN: %s\nfull plan:\n%s", q.name, line, strings.Join(plan, "\n"))
			}
			if strings.Contains(line, "AUTOMATIC") {
				t.Errorf("%s uses AUTOMATIC INDEX: %s\nfull plan:\n%s", q.name, line, strings.Join(plan, "\n"))
			}
		}
	}
}

func TestUserErrorCountsAndNoticesForUser(t *testing.T) {
	ctx := context.Background()
	store, err := Open(filepath.Join(t.TempDir(), "e08.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	owner, _ := store.UpsertGitHubUser(ctx, 8810, "e08-owner", "", "")
	other, _ := store.UpsertGitHubUser(ctx, 8811, "e08-other", "", "")
	proj1, err := store.CreateProject(ctx, owner.ID, "App One", "app-one")
	if err != nil {
		t.Fatal(err)
	}
	proj2, err := store.CreateProject(ctx, owner.ID, "App Two", "app-two")
	if err != nil {
		t.Fatal(err)
	}
	tok1, _, _ := store.CreateProjectToken(ctx, owner.ID, proj1.ID)
	tok2, _, _ := store.CreateProjectToken(ctx, owner.ID, proj2.ID)

	g1, err := store.Ingest(ctx, tok1, Notice{Class: "ErrA", Message: "m1", Backtrace: []string{"app/a.rb:1"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(ctx, tok2, Notice{Class: "ErrB", Message: "m2", Backtrace: []string{"app/b.rb:1"}}); err != nil {
		t.Fatal(err)
	}

	// Archive proj2: UserErrorCounts must exclude archived projects.
	if _, err := store.SetProjectArchived(ctx, owner.ID, proj2.ID, true); err != nil {
		t.Fatal(err)
	}
	open, fixed, total, err := store.UserErrorCounts(ctx, owner.ID)
	if err != nil || open != 1 || fixed != 0 || total != 1 {
		t.Fatalf("UserErrorCounts = (%d, %d, %d, %v), want (1, 0, 1, nil)", open, fixed, total, err)
	}

	// NoticesForUser returns owner's notices and rejects cross-tenant with ErrNotFound.
	notices, err := store.NoticesForUser(ctx, owner.ID, g1.Group.ID, 10)
	if err != nil || len(notices) != 1 {
		t.Fatalf("owner NoticesForUser = (%d, %v)", len(notices), err)
	}
	if _, err := store.NoticesForUser(ctx, other.ID, g1.Group.ID, 10); err != ErrNotFound {
		t.Fatalf("cross-tenant NoticesForUser err = %v, want ErrNotFound", err)
	}
}
