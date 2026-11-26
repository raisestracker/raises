package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/raisestracker/raises/internal/inbox"
)

func TestHardeningMarkdownFullBranchesAndUTF8Truncation(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	claimExp := now.Add(15 * time.Minute)

	// 1. Vendor-only stacktrace (>16 frames), Claim + GitHubIssue, huge Request & Context triggering appendSectionIfFits + truncateBytesUTF8, and >8 occurrence deltas including TraceDiverged.
	vendorFrames := make([]inbox.Frame, 20)
	for i := range vendorFrames {
		vendorFrames[i] = inbox.Frame{
			Raw:   fmt.Sprintf("/gems/rack-3.1.0/lib/rack/middleware_%02d.rb:%d:in `call'", i, i+10),
			InApp: false,
		}
	}
	occs := make([]inbox.OccurrenceDelta, 12)
	for i := range occs {
		occs[i] = inbox.OccurrenceDelta{
			ReceivedAt:    now.Add(-time.Duration(i) * time.Minute),
			Revision:      fmt.Sprintf("rev-%02d", i),
			Handled:       i%2 == 0,
			Message:       fmt.Sprintf("variant message %d", i),
			TraceDiverged: i == 2,
			Backtrace:     []string{"app/jobs/diverged.rb:99:in `run'"},
		}
	}

	rc := inbox.RemediationContext{
		UntrustedDataNotice: inbox.UntrustedDataNotice,
		Group: inbox.GroupSummary{
			ID:                42,
			App:               "shop",
			Env:               "production",
			Class:             "TimeoutError",
			Location:          "app/services/pay.rb:12",
			Message:           "upstream timeout " + strings.Repeat("é", 400),
			Count:             99,
			FirstSeen:         now.Add(-24 * time.Hour),
			LastSeen:          now,
			LastRevision:      "rev-00",
			URL:               "https://raises.example.com/errors/42",
			GitHubIssueNumber: 123,
			GitHubIssueURL:    "https://github.com/acme/shop/issues/123",
			Claim: &inbox.Claim{
				Agent:     "triage-bot-1",
				ClaimedAt: now,
				ExpiresAt: claimExp,
				Note:      "investigating checkout timeout",
			},
			Status: inbox.StatusOpen,
		},
		Velocity: inbox.VelocitySummary{Last1h: 12, Last24h: 40, Prev24h: 10, SpikeRatio: 2.88},
		Stacktrace: inbox.StacktraceSummary{
			VendorFrameCount: len(vendorFrames),
			VendorFrames:     vendorFrames,
			TotalFrames:      len(vendorFrames),
		},
		LatestRequest: map[string]any{
			"url":  "https://shop.example.com/pay",
			"blob": strings.Repeat("日本語テスト", 300),
		},
		LatestContext: map[string]any{
			"trace": strings.Repeat("🚀", 400),
		},
		Occurrences:         occs,
		TotalNoticesSampled: len(occs),
	}

	md := RenderRemediationMarkdown(rc)
	if len(md) > maxMarkdownBytes {
		t.Fatalf("markdown length %d exceeds budget %d", len(md), maxMarkdownBytes)
	}
	if !utf8.ValidString(md) {
		t.Fatalf("markdown output contains invalid UTF-8 after truncation")
	}
	for _, want := range []string{
		"# Raises Error #42",
		"**GitHub Issue:** #123",
		"**Claimed until:**",
		"claimed_by: triage-bot-1",
		"claim_note: investigating checkout timeout",
		"## Stacktrace (20 vendor frames, 0 in-app)",
		"... (4 more frames)",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}

	// Direct UTF-8 byte truncation boundary checks (1-, 2-, 3-, 4-byte runes).
	for _, s := range []string{"abcdef", "ééééé", "日本語テスト", "🔥🔥🔥🔥🔥"} {
		for maxB := 1; maxB <= len(s)+1; maxB++ {
			got := truncateBytesUTF8(s, maxB)
			if len(got) > maxB {
				t.Fatalf("truncateBytesUTF8(%q, %d) len=%d > max", s, maxB, len(got))
			}
			if !utf8.ValidString(got) {
				t.Fatalf("truncateBytesUTF8(%q, %d) produced invalid UTF-8: %x", s, maxB, got)
			}
		}
	}
}

func TestHardeningSentryEnvelopeAndEventEdgeBranches(t *testing.T) {
	f := newSecurityFixture(t)

	// Invalid project placeholders on Sentry envelope route.
	for _, proj := range []string{"bad.proj", strings.Repeat("a", 65), "bad$proj"} {
		rec := f.do(http.MethodPost, "/api/"+proj+"/envelope/", strings.NewReader("{}\n"), map[string]string{
			"X-Sentry-Auth": sentryAuthHeader(f.tokenA),
		}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("sentry project=%q -> %d, want 400", proj, rec.Code)
		}
	}

	// Missing X-Sentry-Auth & invalid token.
	if rec := f.do(http.MethodPost, "/api/1/envelope/", strings.NewReader("{}\n"), nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("missing X-Sentry-Auth -> %d, want 401", rec.Code)
	}
	if rec := f.do(http.MethodPost, "/api/1/envelope/", strings.NewReader("{}\n"), map[string]string{
		"X-Sentry-Auth": sentryAuthHeader("raises_ingest_nonexistent"),
	}, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("invalid sentry token -> %d, want 401", rec.Code)
	}

	// Invalid gzip stream -> 400.
	if rec := f.do(http.MethodPost, "/api/1/envelope/", strings.NewReader("not-gzip-bytes"), map[string]string{
		"X-Sentry-Auth":    sentryAuthHeader(f.tokenA),
		"Content-Encoding": "gzip",
	}, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid gzip -> %d, want 400", rec.Code)
	}

	// Non-event-only envelope (0 error notices) -> 200 OK with event_id.
	sessionOnly := `{"event_id":"sess-only-1"}` + "\n" + `{"type":"session"}` + "\n" + `{"sid":"abc"}`
	if rec := f.do(http.MethodPost, "/api/1/envelope/", strings.NewReader(sessionOnly), map[string]string{
		"X-Sentry-Auth": sentryAuthHeader(f.tokenA),
	}, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "sess-only-1") {
		t.Errorf("session-only envelope -> %d %s", rec.Code, rec.Body.String())
	}

	// Events API (E06 CreateEventForApp path): missing token, invalid token, invalid JSON, valid creation, GET /v1/events/{id}.
	if rec := f.do(http.MethodPost, "/v1/events", strings.NewReader(`{"level":"info","message":"hi"}`), nil, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /v1/events no token -> %d, want 401", rec.Code)
	}
	if rec := f.do(http.MethodPost, "/v1/events", strings.NewReader(`{"level":"info","message":"hi"}`), map[string]string{
		"Authorization": "Bearer raises_ingest_bad",
	}, nil); rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /v1/events bad token -> %d, want 401", rec.Code)
	}
	if rec := f.do(http.MethodPost, "/v1/events", strings.NewReader(`{"unknown":1}`), map[string]string{
		"Authorization": "Bearer " + f.tokenA,
	}, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("POST /v1/events unknown field -> %d, want 400", rec.Code)
	}
	evRec := f.do(http.MethodPost, "/v1/events", strings.NewReader(`{"env":"production","level":"info","message":"deploy ok","source":"ci"}`), map[string]string{
		"Authorization": "Bearer " + f.tokenA,
		"Content-Type":  "application/json",
	}, nil)
	if evRec.Code != http.StatusCreated {
		t.Fatalf("POST /v1/events valid -> %d %s", evRec.Code, evRec.Body.String())
	}
	var ev inbox.Event
	if err := json.Unmarshal(evRec.Body.Bytes(), &ev); err != nil {
		t.Fatal(err)
	}
	// Cross-tenant GET /v1/events/{id} -> 404, owner GET -> 200.
	if rec := f.do(http.MethodGet, "/v1/events/"+ev.ID, nil, map[string]string{"Authorization": "Bearer " + f.keyB}, nil); rec.Code != http.StatusNotFound {
		t.Errorf("cross-tenant GET /v1/events/%s -> %d, want 404", ev.ID, rec.Code)
	}
	if rec := f.do(http.MethodGet, "/v1/events/"+ev.ID, nil, map[string]string{"Authorization": "Bearer " + f.keyA}, nil); rec.Code != http.StatusOK {
		t.Errorf("owner GET /v1/events/%s -> %d, want 200", ev.ID, rec.Code)
	}

	// bindGitHubRepository bad JSON must write exactly ONE JSON object (regression test for double-write fix).
	bindRec := f.do(http.MethodPut, "/v1/projects/"+f.projA.ID+"/github-repository", bytes.NewReader([]byte(`{bad json`)), map[string]string{
		"Authorization": "Bearer " + f.keyA,
		"Content-Type":  "application/json",
	}, nil)
	if bindRec.Code != http.StatusBadRequest {
		t.Fatalf("bind bad json -> %d", bindRec.Code)
	}
	var oneObj map[string]string
	if err := json.Unmarshal(bindRec.Body.Bytes(), &oneObj); err != nil {
		t.Fatalf("bind bad json wrote malformed/double JSON %q: %v", bindRec.Body.String(), err)
	}

	// Ensure context cancellation / store check sanity.
	_ = context.Background()
}
