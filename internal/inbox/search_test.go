package inbox

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

func seedSearchGroups(t *testing.T, store *Store, owner int64, slug string, specs []struct {
	env, class, msg string
	count           int
}) {
	t.Helper()
	ctx := context.Background()
	proj, err := store.CreateProject(ctx, owner, slug, slug)
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := store.CreateProjectToken(ctx, owner, proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	app, err := store.AppByToken(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range specs {
		for n := 0; n < s.count; n++ {
			if _, err := store.IngestForApp(ctx, app, Notice{Env: s.env, Class: s.class, Message: s.msg, Backtrace: []string{fmt.Sprintf("app/models/%s%d.rb:%d", slug, i, i+1)}}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestListPageSearchFacets(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	owner, _ := store.UpsertGitHubUser(ctx, 501, "search-owner", "", "")
	other, _ := store.UpsertGitHubUser(ctx, 502, "search-other", "", "")
	seedSearchGroups(t, store, owner.ID, "burst", []struct {
		env, class, msg string
		count           int
	}{
		{"production", "TimeoutError", "Database Timeout on checkout", 9},
		{"production", "TimeoutError", "Redis Timeout on session", 5},
		{"production", "KeyError", "Missing key in payload", 3},
		{"staging", "TimeoutError", "Staging Timeout on worker", 2},
		{"production", "ParseError", "100% literal_under\\score", 1},
	})
	seedSearchGroups(t, store, other.ID, "foreign", []struct {
		env, class, msg string
		count           int
	}{{"production", "TimeoutError", "Foreign Timeout", 50}})

	list := func(status string, opts ListOptions) []Group {
		t.Helper()
		page, err := store.ListPageForUserWithOptions(ctx, owner.ID, "", status, 1, 25, opts)
		if err != nil {
			t.Fatal(err)
		}
		return page.Groups
	}

	got := list(StatusAll, ListOptions{Env: "production", Query: "timeout", Sort: SortCount, Compact: true})
	if len(got) != 2 || got[0].Count != 9 || got[1].Count != 5 {
		t.Fatalf("env+q+sort=count: %+v", got)
	}
	for _, g := range got {
		if g.Env != "production" || strings.Contains(g.Message, "Foreign") {
			t.Fatalf("facet leak: %+v", g)
		}
	}
	// LIKE wildcards are literal.
	if got := list(StatusAll, ListOptions{Query: "%"}); len(got) != 1 || got[0].Class != "ParseError" {
		t.Fatalf("q=%% must match only a literal percent: %d rows", len(got))
	}
	if got := list(StatusAll, ListOptions{Query: "_"}); len(got) != 1 {
		t.Fatalf("q=_ must match only a literal underscore: %d rows", len(got))
	}
	if got := list(StatusAll, ListOptions{Query: `\`}); len(got) != 1 {
		t.Fatalf(`q=\ must match only a literal backslash: %d rows`, len(got))
	}
	// Location is searchable too.
	if got := list(StatusAll, ListOptions{Query: "burst2.rb"}); len(got) != 1 || got[0].Class != "KeyError" {
		t.Fatalf("location search: %+v", got)
	}
	// Default sort is unchanged (last_seen); first_seen is allowlisted.
	if got := list(StatusAll, ListOptions{}); len(got) != 5 || got[0].Class != "ParseError" {
		t.Fatalf("default order: %+v", got)
	}
	if got := list(StatusAll, ListOptions{Sort: "first_seen"}); len(got) != 5 {
		t.Fatalf("first_seen: %d", len(got))
	}
	// An unknown sort can never reach SQL text.
	if got := list(StatusAll, ListOptions{Sort: "count; DROP TABLE apps"}); len(got) != 5 {
		t.Fatalf("injected sort must fall back: %d", len(got))
	}
	// status=suppressed.
	if _, err := store.SuppressForUser(ctx, owner.ID, got[0].ID); err != nil {
		t.Fatal(err)
	}
	if got := list(StatusSuppressed, ListOptions{}); len(got) != 1 {
		t.Fatalf("suppressed=%d", len(got))
	}
	// The suppressed 9-count Timeout group leaves open: Redis + Staging.
	if got := list(StatusOpen, ListOptions{Query: "timeout"}); len(got) != 2 {
		t.Fatalf("open+q=%d", len(got))
	}
}

func TestSearchNormalizers(t *testing.T) {
	long := strings.Repeat("é", 200)
	if got := NormalizeSearchQuery("  " + long + "  "); len([]rune(got)) != MaxSearchQueryRunes {
		t.Fatalf("q cap=%d", len([]rune(got)))
	}
	for in, want := range map[string]bool{"": true, "count": true, "COUNT": true, "first_seen": true, "last_seen": true, "id": false, "count desc": false, "g.count": false} {
		if _, ok := NormalizeSort(in); ok != want {
			t.Errorf("NormalizeSort(%q) ok=%v", in, ok)
		}
	}
	if escapeLike(`a%b_c\d`) != `a\%b\_c\\d` {
		t.Fatalf("escapeLike=%q", escapeLike(`a%b_c\d`))
	}
	if NormalizeStatus("suppressed", StatusOpen) != StatusSuppressed {
		t.Fatal("suppressed status")
	}
}

// The q facet must stay tenant-index-first (no SCAN of error_groups) and
// answer within 10 ms at 10K groups.
func TestSearchAt10KRowsUsesTenantIndex(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	owner, _ := store.UpsertGitHubUser(ctx, 503, "big-owner", "", "")
	proj, err := store.CreateProject(ctx, owner.ID, "Big", "big")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := store.now().Unix()
	for i := 0; i < 10000; i++ {
		class := fmt.Sprintf("Noise%05dError", i)
		if i%997 == 0 {
			class = fmt.Sprintf("NeedleTimeout%05d", i)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO error_groups(project_id, app_name, env, fingerprint, class, location, message, count, first_seen_unix, last_seen_unix, last_revision, github_issue_url, sample_json)
			VALUES(?, 'big', ?, ?, ?, ?, 'boom', ?, ?, ?, '', '', '{}')`,
			proj.ID, []string{"production", "staging"}[i%2], fmt.Sprintf("fp%05d", i), class, fmt.Sprintf("app/models/m%d.rb:1", i), i%50+1, now-int64(i), now-int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	opts := ListOptions{Query: "needletimeout", Env: "production", Sort: SortCount, Compact: true}
	var best time.Duration
	for i := 0; i < 5; i++ {
		start := time.Now()
		page, err := store.ListPageForUserWithOptions(ctx, owner.ID, "", StatusAll, 1, 25, opts)
		elapsed := time.Since(start)
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 6 || len(page.Groups) != 6 {
			t.Fatalf("needles total=%d rows=%d", page.Total, len(page.Groups))
		}
		if i == 0 || elapsed < best {
			best = elapsed
		}
	}
	t.Logf("q= search over 10K groups: best %s (race=%v)", best, raceEnabled)
	// The race detector slows pure-Go SQLite ~30x; the 10 ms budget is
	// asserted on a normal build.
	if !raceEnabled && best > 10*time.Millisecond {
		t.Fatalf("q search too slow: %s", best)
	}

	where, args := searchClause(opts, ` WHERE a.owner_user_id = ?`, []any{owner.ID})
	rows, err := store.db.QueryContext(ctx, `EXPLAIN QUERY PLAN SELECT g.id FROM error_groups g JOIN apps a ON a.project_id = g.project_id`+where+` ORDER BY `+sortOrders[SortCount]+` LIMIT 25`, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, " | ")
	if explainScanPattern.MatchString(joined) || strings.Contains(joined, "AUTOMATIC") || !strings.Contains(joined, "idx_error_groups_project") {
		t.Fatalf("search plan must narrow via tenant/project index: %s", joined)
	}
}
