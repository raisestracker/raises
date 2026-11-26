package httpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/raisestracker/raises/internal/inbox"
)

func (f dashFixture) seedFacetGroups(t *testing.T) {
	t.Helper()
	specs := []struct {
		env, class, msg string
		count           int
	}{
		{"production", "TimeoutError", "Database Timeout on checkout", 9},
		{"production", "TimeoutError", "Redis Timeout on session", 5},
		{"production", "KeyError", "Missing key in payload", 3},
		{"staging", "TimeoutError", "Staging Timeout on worker", 2},
	}
	for i, s := range specs {
		for n := 0; n < s.count; n++ {
			if _, err := f.store.Ingest(context.Background(), "token", inbox.Notice{Env: s.env, Class: s.class, Message: s.msg, Backtrace: []string{fmt.Sprintf("app/models/burst%d.rb:%d", i, i+1)}}); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestAPIErrorsSearchFacets(t *testing.T) {
	f := newDashFixture(t)
	f.seedFacetGroups(t)
	rec := f.get("/v1/errors?env=production&q=Timeout&sort=count&compact=1", "Authorization", "Bearer "+f.agent)
	if rec.Code != http.StatusOK {
		t.Fatalf("search=%d %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Errors []struct {
			Env, Class, Message string
			Count               int
		} `json:"errors"`
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Errors) != 2 || page.Errors[0].Count != 9 || page.Errors[1].Count != 5 {
		t.Fatalf("search page=%+v", page)
	}
	for _, bad := range []string{"sort=bogus", "sort=count%3BDROP%20TABLE%20apps", "sort=g.count", "status=nope&q=x"} {
		if rec := f.get("/v1/errors?"+bad, "Authorization", "Bearer "+f.agent); rec.Code != http.StatusBadRequest {
			t.Fatalf("%s=%d", bad, rec.Code)
		}
	}
	if rec := f.get("/v1/errors?status=suppressed", "Authorization", "Bearer "+f.agent); rec.Code != http.StatusOK {
		t.Fatalf("status=suppressed=%d", rec.Code)
	}
	// Highest-count production error in one request (DEV-1 shape).
	rec = f.get("/v1/errors?status=open&page=1&per_page=25&compact=1&sort=count", "Authorization", "Bearer "+f.agent)
	if !strings.Contains(rec.Body.String(), `"count":9`) || strings.Index(rec.Body.String(), `"count":9`) > strings.Index(rec.Body.String(), `"count":5`) {
		t.Fatalf("sort=count first row: %s", rec.Body.String()[:200])
	}
	// The legacy flat array stays untouched.
	if rec := f.get("/v1/errors?unacked=1&compact=1", "Authorization", "Bearer "+f.agent); !strings.HasPrefix(strings.TrimSpace(rec.Body.String()), "[") {
		t.Fatal("unacked list must stay a flat array")
	}
}

func TestHTMLErrorListSearchPreservesParams(t *testing.T) {
	f := newDashFixture(t)
	f.seedFacetGroups(t)
	for i := 0; i < 30; i++ {
		if _, err := f.store.Ingest(context.Background(), "token", inbox.Notice{Env: "production", Class: "TimeoutError", Message: fmt.Sprintf("Pool Timeout %d", i), Backtrace: []string{fmt.Sprintf("app/jobs/pool%d.rb:1", i)}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, base := range []string{"/projects/widget", "/errors"} {
		body := f.get(base + "?q=Timeout&env=production&sort=count").Body.String()
		for _, want := range []string{
			`name="q" value="Timeout"`, `name="env" value="production"`, `<option value="count" selected>`,
			base + `?env=production&amp;page=2&amp;q=Timeout&amp;sort=count`,
			base + `?env=production&amp;q=Timeout&amp;sort=count&amp;status=suppressed`,
			"32 matches", "seen 9×", `class="mt-0.5 block text-[13px] text-muted tabular-nums sm:hidden">seen 9× ·`,
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s missing %q", base, want)
			}
		}
		if idx9, idx5 := strings.Index(body, "Database Timeout on checkout"), strings.Index(body, "Redis Timeout on session"); idx9 < 0 || idx5 < 0 || idx9 > idx5 {
			t.Fatalf("%s sort=count order broken", base)
		}
		// Unknown sort falls back without error; escaping holds.
		rec := f.get(base + "?sort=bogus&q=%3Cscript%3E")
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), `value="<script>"`) || !strings.Contains(rec.Body.String(), "match this search") {
			t.Fatalf("%s bogus sort/xss q=%d", base, rec.Code)
		}
	}
}
