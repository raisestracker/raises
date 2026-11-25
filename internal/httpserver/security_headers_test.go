package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/raisestracker/raises/internal/inbox"
)

// Default GA-off mode must emit a strict script-src 'self' CSP with zero
// 'unsafe-inline' and zero Google Analytics domains in headers or homepage HTML.
func TestSecurityHeadersOnPublicAndAPIResponses(t *testing.T) {
	handler, _, _ := testHandler(t)
	for _, path := range []string{"/", "/healthz", "/readyz", "/version", "/v1/errors"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		h := rec.Header()
		if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("%s: X-Content-Type-Options = %q", path, got)
		}
		if got := h.Get("X-Frame-Options"); got != "DENY" {
			t.Fatalf("%s: X-Frame-Options = %q", path, got)
		}
		if got := h.Get("Referrer-Policy"); got == "" {
			t.Fatalf("%s: missing Referrer-Policy", path)
		}
		csp := h.Get("Content-Security-Policy")
		for _, want := range []string{
			"script-src 'self';",
			"connect-src 'self';",
			"frame-ancestors 'none'",
			"object-src 'none'",
			"https://fonts.googleapis.com",
			"https://fonts.gstatic.com",
		} {
			if !strings.Contains(csp, want) {
				t.Fatalf("%s: CSP missing %q: %q", path, want, csp)
			}
		}
		for _, forbid := range []string{"'unsafe-inline'", "googletagmanager.com", "google-analytics.com"} {
			if strings.Contains(csp, forbid) {
				t.Fatalf("%s: default CSP must not contain %q: %q", path, forbid, csp)
			}
		}
		if path == "/" {
			body := rec.Body.String()
			for _, forbid := range []string{"googletagmanager.com", "G-16MS9N28TE", "<script"} {
				if strings.Contains(body, forbid) {
					t.Fatalf("default homepage must not contain %q", forbid)
				}
			}
		}
	}
}

// Opt-in GA4 mode allows googletagmanager.com via external fingerprinted ga.js
// while still forbidding 'unsafe-inline' in script-src.
func TestSecurityHeadersWithGAEnabled(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	srv := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{
		GAMeasurementID: "G-TEST1234",
	})
	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("home status = %d", rec.Code)
	}
	csp := rec.Header().Get("Content-Security-Policy")
	if strings.Contains(csp, "'unsafe-inline'") {
		t.Fatalf("GA-enabled CSP must still forbid 'unsafe-inline': %q", csp)
	}
	for _, want := range []string{
		"script-src 'self' https://www.googletagmanager.com;",
		"connect-src 'self' https://www.googletagmanager.com https://www.google-analytics.com https://*.google-analytics.com;",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("GA-enabled CSP missing %q: %q", want, csp)
		}
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-ga-id="G-TEST1234"`) {
		t.Fatalf("GA-enabled homepage missing data-ga-id: %s", body)
	}
	gaPath := regexp.MustCompile(`/assets/ga-[a-f0-9]+\.js`).FindString(body)
	if gaPath == "" {
		t.Fatalf("GA-enabled homepage missing fingerprinted ga.js: %s", body)
	}
	for _, assetPath := range []string{gaPath, srv.appJSPath} {
		assetReq := httptest.NewRequest(http.MethodGet, assetPath, nil)
		assetRec := httptest.NewRecorder()
		handler.ServeHTTP(assetRec, assetReq)
		if assetRec.Code != http.StatusOK {
			t.Fatalf("%s status = %d", assetPath, assetRec.Code)
		}
		if !strings.Contains(assetRec.Header().Get("Content-Type"), "javascript") {
			t.Fatalf("%s content-type = %q", assetPath, assetRec.Header().Get("Content-Type"))
		}
		if !strings.Contains(assetRec.Header().Get("Cache-Control"), "immutable") {
			t.Fatalf("%s cache-control = %q", assetPath, assetRec.Header().Get("Cache-Control"))
		}
	}
}

// The headers must survive redirects too (e.g. /settings when logged out),
// otherwise the policy has gaps on exactly the authenticated pages.
func TestSecurityHeadersOnRedirect(t *testing.T) {
	handler, _, _ := testHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code < 300 || rec.Code > 399 {
		t.Fatalf("/settings logged out status = %d, want a redirect", rec.Code)
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Fatal("/settings redirect is missing the Content-Security-Policy")
	}
	if got := rec.Header().Get("X-Frame-Options"); got != "DENY" {
		t.Fatalf("/settings redirect X-Frame-Options = %q", got)
	}
}
