package httpserver

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

func newRateLimitServer(t *testing.T) *Server {
	t.Helper()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertApp(context.Background(), "widget", "app-token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	return New(store, "agent-token", slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20)
}

func postNotice(s *Server, token, remoteAddr, forwardedFor string) int {
	req := httptest.NewRequest(http.MethodPost, "/v1/notices", nil)
	req.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func getErrors(s *Server, token, remoteAddr, forwardedFor string) int {
	req := httptest.NewRequest(http.MethodGet, "/v1/errors", nil)
	req.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

// Distinct invalid tokens must neither bypass the limiter nor grow its state.
func TestDistinctInvalidTokensStayBounded(t *testing.T) {
	s := newRateLimitServer(t)
	const total = 20000
	seen429 := false
	for i := 0; i < total; i++ {
		// Distinct source IPs behind the trusted proxy: a distributed flood.
		ip := fmt.Sprintf("203.0.%d.%d", (i>>8)&0xff, i&0xff)
		code := postNotice(s, fmt.Sprintf("junk-%d", i), "127.0.0.1:1234", ip)
		if code != http.StatusUnauthorized && code != http.StatusTooManyRequests {
			t.Fatalf("request %d: status=%d", i, code)
		}
		if code == http.StatusTooManyRequests {
			seen429 = true
		}
	}
	if !seen429 {
		t.Fatal("limiter never engaged during a 20k-token flood")
	}
	s.limitMu.Lock()
	size := len(s.limits)
	s.limitMu.Unlock()
	if size > rateLimitMaxEntries {
		t.Fatalf("limiter state unbounded: %d entries, cap %d", size, rateLimitMaxEntries)
	}

	// Per-call cost stays flat once the map is at its cap.
	start := time.Now()
	const probes = 2000
	for i := 0; i < probes; i++ {
		s.allowRequest(fmt.Sprintf("probe-%d", i), 1<<30)
	}
	avg := time.Since(start) / probes
	if avg > time.Millisecond {
		t.Fatalf("per-call cost not flat: avg %s over %d fresh keys", avg, probes)
	}
	s.limitMu.Lock()
	size = len(s.limits)
	s.limitMu.Unlock()
	if size > rateLimitMaxEntries {
		t.Fatalf("limiter state unbounded after probes: %d entries", size)
	}

	// A legitimate credential still works after the flood.
	req := httptest.NewRequest(http.MethodPost, "/v1/notices",
		strings.NewReader(`{"error":{"class":"RuntimeError","message":"boom"}}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.250")
	req.Header.Set("Authorization", "Bearer app-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("valid ingest after flood: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// One IP hammering an invalid token is throttled by the pre-auth IP budget.
func TestSingleIPInvalidTokenFloodIsThrottled(t *testing.T) {
	s := newRateLimitServer(t)
	throttled := false
	for i := 0; i < preAuthIngestPerIPLimit+50; i++ {
		if code := postNotice(s, "same-junk", "127.0.0.1:1234", "198.51.100.7"); code == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatal("single-IP invalid-token flood was never throttled")
	}
}

// The proxy header is trusted only from the proxy: a direct client cannot
// rotate X-Forwarded-For to mint fresh buckets, while requests arriving via
// the proxy are keyed by the address the proxy vouches for (last entry).
func TestClientIPTrustGate(t *testing.T) {
	s := newRateLimitServer(t)

	// Direct connection with a rotating header: one shared RemoteAddr bucket.
	if code := getErrors(s, "junk", "198.51.100.9:1234", "10.0.0.1"); code != http.StatusUnauthorized {
		t.Fatalf("first direct request: status=%d, want 401", code)
	}
	throttled := false
	for i := 0; i < preAuthAgentPerIPLimit; i++ {
		if code := getErrors(s, "junk", "198.51.100.9:1234", fmt.Sprintf("10.0.0.%d", i)); code == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatal("rotating X-Forwarded-For from a direct client evaded the limiter")
	}

	// Via the proxy each client IP gets its own budget.
	s2 := newRateLimitServer(t)
	for i := 0; i < preAuthAgentPerIPLimit; i++ {
		ip := fmt.Sprintf("203.0.%d.%d", (i>>8)&0xff, i&0xff)
		if code := getErrors(s2, "junk", "127.0.0.1:8080", ip); code != http.StatusUnauthorized {
			t.Fatalf("proxied request %d: status=%d, want 401", i, code)
		}
	}

	// Only the proxy-appended (last) entry counts: spoofed left entries share
	// the real client's bucket.
	s3 := newRateLimitServer(t)
	throttled = false
	for i := 0; i < preAuthAgentPerIPLimit; i++ {
		xff := fmt.Sprintf("spoof-%d, 203.0.113.44", i)
		code := getErrors(s3, "junk", "127.0.0.1:8080", xff)
		if code == http.StatusTooManyRequests {
			throttled = true
			break
		}
		if code != http.StatusUnauthorized {
			t.Fatalf("proxied request %d: status=%d", i, code)
		}
	}
	if !throttled {
		t.Fatal("spoofed X-Forwarded-For entries evaded the shared client bucket")
	}
}

// A valid credential gets its own generous post-auth window, enforced
// independently of the pre-auth IP budget.
func TestPostAuthTokenWindowLimitsValidToken(t *testing.T) {
	s := newRateLimitServer(t)
	var last int
	for i := 0; i < postAuthAgentPerTokenLimit+1; i++ {
		ip := fmt.Sprintf("203.0.%d.%d", (i>>8)&0xff, i&0xff)
		last = getErrors(s, "agent-token", "127.0.0.1:8080", ip)
		if i < postAuthAgentPerTokenLimit && last != http.StatusOK {
			t.Fatalf("request %d: status=%d, want 200", i, last)
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("valid token over its post-auth window: status=%d, want 429", last)
	}
}

// Limiter state never exceeds its cap, even under distinct-key pressure.
func TestAllowRequestMapStaysCapped(t *testing.T) {
	s := &Server{limits: map[string]requestWindow{}}
	for i := 0; i < 3*rateLimitMaxEntries; i++ {
		s.allowRequest(fmt.Sprintf("key-%d", i), 1<<30)
	}
	s.limitMu.Lock()
	size := len(s.limits)
	s.limitMu.Unlock()
	if size > rateLimitMaxEntries {
		t.Fatalf("map size %d exceeds cap %d", size, rateLimitMaxEntries)
	}
}
