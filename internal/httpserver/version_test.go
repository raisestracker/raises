package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The fleet-deployer probes GET /version and requires exactly
// {"revision":"<40-hex deployed sha>"} as application/json with
// Cache-Control: no-store and no redirect.
func TestVersionServesDeployedRevisionExactly(t *testing.T) {
	const sha = "9f2c4b1a8d3e6f70987654321abcdef012345678"
	server := NewWithConfig(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{Revision: sha})
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want %q", got, "application/json")
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want %q", got, "no-store")
	}
	if want := `{"revision":"` + sha + `"}`; rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
}

func TestVersionDefaultsToUnknownWithoutInjectedRevision(t *testing.T) {
	server := NewWithConfig(nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{})
	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if want := `{"revision":"unknown"}`; rec.Body.String() != want {
		t.Fatalf("body = %q, want %q", rec.Body.String(), want)
	}
}
