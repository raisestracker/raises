package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/raisestracker/raises/internal/inbox"
)

// Static legacy agent tokens are retired: no server configuration accepts
// them, so any static-style bearer on agent routes is unauthorized. Only
// DB-backed agent and account keys authenticate.
func TestStaticAgentTokenAlwaysRejected(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler := NewWithConfig(store, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20, Config{}).Handler()

	req := httptest.NewRequest(http.MethodGet, "/v1/errors?unacked=1", nil)
	req.Header.Set("Authorization", "Bearer legacy-agent-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("retired legacy token status=%d want 401", rec.Code)
	}
}
