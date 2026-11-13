package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Deliberate response policy: every response carries the baseline security
// headers and a CSP that allowlists exactly the third parties the console
// uses (Google Fonts everywhere, Tag Manager on the public homepage).
func TestSecurityHeadersOnPublicAndAPIResponses(t *testing.T) {
	handler := testHandler(t)
	for _, path := range []string{"/", "/healthz", "/readyz", "/v1/errors"} {
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
			"frame-ancestors 'none'",
			"object-src 'none'",
			"https://www.googletagmanager.com",
			"https://fonts.googleapis.com",
			"https://fonts.gstatic.com",
		} {
			if !strings.Contains(csp, want) {
				t.Fatalf("%s: CSP missing %q: %q", path, want, csp)
			}
		}
	}
}

// The headers must survive redirects too (e.g. /settings when logged out),
// otherwise the policy has gaps on exactly the authenticated pages.
func TestSecurityHeadersOnRedirect(t *testing.T) {
	handler := testHandler(t)
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
