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

// The legacy static agent token works only while the server is configured
// with it (flag off). A server started without it — the retirement wiring —
// rejects it on agent routes.
func TestLegacyAgentTokenAcceptedOnlyWhenConfigured(t *testing.T) {
	newHandler := func(agentToken string) http.Handler {
		t.Helper()
		store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return New(store, agentToken, slog.New(slog.NewTextHandler(io.Discard, nil)), 1<<20).Handler()
	}

	withToken := newHandler("legacy-agent-token")
	req := httptest.NewRequest(http.MethodGet, "/v1/errors?unacked=1", nil)
	req.Header.Set("Authorization", "Bearer legacy-agent-token")
	rec := httptest.NewRecorder()
	withToken.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("configured legacy token status=%d body=%s", rec.Code, rec.Body.String())
	}

	retired := newHandler("")
	req = httptest.NewRequest(http.MethodGet, "/v1/errors?unacked=1", nil)
	req.Header.Set("Authorization", "Bearer legacy-agent-token")
	rec = httptest.NewRecorder()
	retired.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("retired legacy token status=%d want 401", rec.Code)
	}
}
