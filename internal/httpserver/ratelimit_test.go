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

func newRateLimitServer(t *testing.T) (*Server, string) {
	t.Helper()
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.UpsertApp(context.Background(), "widget", "app-token", "example/widget"); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertApp(context.Background(), "gadget", "app-token-2", "example/gadget"); err != nil {
		t.Fatal(err)
	}
	// Static agent tokens are retired, so agent traffic authenticates with a
	// DB-backed key minted for a fixture owner.
	owner, err := store.UpsertGitHubUser(context.Background(), 4242, "ratelimit-owner", "", "")
	if err != nil {
		t.Fatal(err)
	}
	agentToken, _, err := store.CreateAgentKey(context.Background(), owner.ID, "ratelimit-agent")
	if err != nil {
		t.Fatal(err)
	}
	return NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{}), agentToken
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

// A distributed junk-token flood must neither bypass the limiter nor grow
// its state, and must not lock out valid credentials: the global failure
// budget applies on the failure path only, after the credential lookup
// fails, so a fresh IP with a valid token still gets through.
func TestDistinctInvalidTokensStayBounded(t *testing.T) {
	s, agentToken := newRateLimitServer(t)
	// Past the 2,000-strong global failure budget from 120 distinct IPs,
	// each staying under its own per-IP budget so only the shared global
	// budget trips.
	const ips, perIP = 120, 20
	seen429 := false
	sent := 0
	for i := 0; i < ips; i++ {
		// Distinct source IPs behind the trusted proxy: a distributed flood.
		ip := fmt.Sprintf("203.0.%d.%d", (i>>8)&0xff, i&0xff)
		for j := 0; j < perIP; j++ {
			code := postNotice(s, fmt.Sprintf("junk-%d-%d", i, j), "127.0.0.1:1234", ip)
			sent++
			if code != http.StatusUnauthorized && code != http.StatusTooManyRequests {
				t.Fatalf("flood request %d: status=%d", sent, code)
			}
			if code == http.StatusTooManyRequests {
				seen429 = true
			}
		}
	}
	if sent <= failedAuthGlobalLimit {
		t.Fatalf("flood sent %d failures, must exceed global budget %d", sent, failedAuthGlobalLimit)
	}
	if !seen429 {
		t.Fatal("global failure budget never engaged during a distributed flood")
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

	// While the flood's minute-window is still active the global failure
	// budget must shed only failing credentials: a valid ingestion
	// token and a valid agent key from fresh IPs still get through, while
	// an invalid token from a fresh IP gets 429 on the failure path.
	req := httptest.NewRequest(http.MethodPost, "/v1/notices",
		strings.NewReader(`{"error":{"class":"RuntimeError","message":"boom"}}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.250")
	req.Header.Set("Authorization", "Bearer app-token")
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("valid ingest mid-flood: status=%d, want 201/200", rec.Code)
	}
	if code := getErrors(s, agentToken, "127.0.0.1:1234", "203.0.113.251"); code != http.StatusOK {
		t.Fatalf("valid agent key mid-flood: status=%d, want 200", code)
	}
	if code := postNotice(s, "junk-fresh-ip", "127.0.0.1:1234", "203.0.113.252"); code != http.StatusTooManyRequests {
		t.Fatalf("invalid token mid-flood: status=%d, want 429", code)
	}

	// Once the flood stops and its windows expire, a legitimate credential
	// works again with no lasting state damage.
	s.limitMu.Lock()
	for key, window := range s.limits {
		window.Started = window.Started.Add(-2 * rateLimitWindow)
		s.limits[key] = window
	}
	s.limitMu.Unlock()
	req = httptest.NewRequest(http.MethodPost, "/v1/notices",
		strings.NewReader(`{"error":{"class":"RuntimeError","message":"boom"}}`))
	req.RemoteAddr = "127.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.250")
	req.Header.Set("Authorization", "Bearer app-token")
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	// 201 first seen, 200 once deduped into the mid-flood group above.
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("valid ingest after flood: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

// One IP hammering an invalid token is throttled by the per-IP failure
// budget, even though successes are never charged pre-auth.
func TestSingleIPInvalidTokenFloodIsThrottled(t *testing.T) {
	s, _ := newRateLimitServer(t)
	throttled := false
	for i := 0; i < failedAuthPerIPLimit+50; i++ {
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
	s, _ := newRateLimitServer(t)

	// Direct connection with a rotating header: one shared RemoteAddr bucket.
	if code := getErrors(s, "junk", "198.51.100.9:1234", "10.0.0.1"); code != http.StatusUnauthorized {
		t.Fatalf("first direct request: status=%d, want 401", code)
	}
	throttled := false
	for i := 0; i < failedAuthPerIPLimit; i++ {
		if code := getErrors(s, "junk", "198.51.100.9:1234", fmt.Sprintf("10.0.0.%d", i)); code == http.StatusTooManyRequests {
			throttled = true
			break
		}
	}
	if !throttled {
		t.Fatal("rotating X-Forwarded-For from a direct client evaded the limiter")
	}

	// Via the proxy each client IP gets its own budget.
	s2, _ := newRateLimitServer(t)
	for i := 0; i < failedAuthPerIPLimit; i++ {
		ip := fmt.Sprintf("203.0.%d.%d", (i>>8)&0xff, i&0xff)
		if code := getErrors(s2, "junk", "127.0.0.1:8080", ip); code != http.StatusUnauthorized {
			t.Fatalf("proxied request %d: status=%d, want 401", i, code)
		}
	}

	// Only the proxy-appended (last) entry counts: spoofed left entries share
	// the real client's bucket.
	s3, _ := newRateLimitServer(t)
	throttled = false
	for i := 0; i < failedAuthPerIPLimit+1; i++ {
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

// IPv6 clients share one rate-limit bucket per /64 subnet: rotating
// addresses within a subnet does not mint fresh budgets, while a different
// /64 gets its own.
func TestClientIPv6BucketedBySlash64(t *testing.T) {
	s, _ := newRateLimitServer(t)
	for i := 0; i < failedAuthPerIPLimit; i++ {
		if code := getErrors(s, "junk", "127.0.0.1:8080", "2001:db8::1"); code != http.StatusUnauthorized {
			t.Fatalf("same-/64 request %d: status=%d, want 401", i, code)
		}
	}
	// Same /64, different address: shares the exhausted per-IP budget.
	if code := getErrors(s, "junk", "127.0.0.1:8080", "2001:db8::2"); code != http.StatusTooManyRequests {
		t.Fatalf("same-/64 rotated address: status=%d, want 429", code)
	}
	// Different /64: fresh budget.
	if code := getErrors(s, "junk", "127.0.0.1:8080", "2001:db8:0:1::1"); code != http.StatusUnauthorized {
		t.Fatalf("other-/64 request: status=%d, want 401", code)
	}
}

// A valid credential gets its own generous post-auth window, enforced
// independently of the pre-auth IP budget.
func TestPostAuthTokenWindowLimitsValidToken(t *testing.T) {
	s, agentToken := newRateLimitServer(t)
	var last int
	for i := 0; i < postAuthAgentPerTokenLimit+1; i++ {
		ip := fmt.Sprintf("203.0.%d.%d", (i>>8)&0xff, i&0xff)
		last = getErrors(s, agentToken, "127.0.0.1:8080", ip)
		if i < postAuthAgentPerTokenLimit && last != http.StatusOK {
			t.Fatalf("request %d: status=%d, want 200", i, last)
		}
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("valid token over its post-auth window: status=%d, want 429", last)
	}
}

// Two valid app tokens behind one egress IP each get their full post-auth
// window: successes are never charged to a shared pre-auth bucket, so one
// app's error storm cannot 429 another app on the same host or NAT.
func TestSharedEgressIPValidTokensUnaffected(t *testing.T) {
	s, _ := newRateLimitServer(t)
	postValidNotice := func(token string) int {
		req := httptest.NewRequest(http.MethodPost, "/v1/notices",
			strings.NewReader(`{"error":{"class":"RuntimeError","message":"boom"}}`))
		req.RemoteAddr = "127.0.0.1:8080"
		req.Header.Set("X-Forwarded-For", "203.0.113.77")
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	const perToken = 150
	for i := 0; i < perToken; i++ {
		// 201 first seen, 200 once deduped into the same group: both prove
		// the request was accepted rather than rate-limited.
		if code := postValidNotice("app-token"); code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("token 1 request %d: status=%d, want 201/200", i, code)
		}
		if code := postValidNotice("app-token-2"); code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("token 2 request %d: status=%d, want 201/200", i, code)
		}
	}
	// Valid traffic mints no success-charging pre-auth state at all.
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	for key := range s.limits {
		if strings.HasPrefix(key, "preauth:") {
			t.Fatalf("valid traffic charged pre-auth bucket %q", key)
		}
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

// One IP flooding POST /v1/bootstrap/exchange with invalid tokens is throttled
// at failedAuthPerIPLimit without blocking a legitimate bootstrap from another IP.
func TestBootstrapExchangeInvalidFloodIsThrottled(t *testing.T) {
	s, _ := newRateLimitServer(t)
	owner, err := s.store.UpsertGitHubUser(context.Background(), 4243, "boot-owner", "", "")
	if err != nil {
		t.Fatal(err)
	}
	validToken, err := s.store.CreateBootstrapToken(context.Background(), owner.ID, "legit-agent", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	postBootstrap := func(token, forwardedFor string) int {
		body := fmt.Sprintf(`{"token":%q,"name":"agent"}`, token)
		req := httptest.NewRequest(http.MethodPost, "/v1/bootstrap/exchange", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", forwardedFor)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}

	throttledAt := 0
	for i := 1; i <= failedAuthPerIPLimit+5; i++ {
		code := postBootstrap(fmt.Sprintf("boot-invalid-%d", i), "198.51.100.42")
		if code == http.StatusTooManyRequests {
			throttledAt = i
			break
		}
		if code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status=%d, want 401 or 429", i, code)
		}
	}
	if throttledAt == 0 || throttledAt > failedAuthPerIPLimit+1 {
		t.Fatalf("bootstrap flood not throttled at expected limit: throttledAt=%d, limit=%d", throttledAt, failedAuthPerIPLimit)
	}

	// Legitimate bootstrap from another IP still succeeds with 201 and replay fails with 401.
	if code := postBootstrap(validToken, "198.51.100.99"); code != http.StatusCreated {
		t.Fatalf("valid bootstrap from other IP: status=%d, want 201", code)
	}
	if code := postBootstrap(validToken, "198.51.100.99"); code != http.StatusUnauthorized {
		t.Fatalf("replayed bootstrap: status=%d, want 401", code)
	}
}
