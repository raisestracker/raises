package inbox

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"
)

func TestHardeningSessionByTokenSingleQueryAndExpiry(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	store, err := Open(filepath.Join(t.TempDir(), "sess.db"), func() time.Time { return now }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	user, err := store.UpsertGitHubUser(ctx, 7701, "sessuser", "Session User", "https://example.com/a.png")
	if err != nil {
		t.Fatal(err)
	}
	tok, created, err := store.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.SessionByToken(ctx, tok)
	if err != nil {
		t.Fatalf("SessionByToken valid: %v", err)
	}
	if loaded.User.ID != user.ID || loaded.User.Login != "sessuser" || loaded.CSRFToken != created.CSRFToken {
		t.Fatalf("unexpected loaded session: %+v", loaded)
	}

	// Advance past expiry -> must return ErrUnauthorized and delete expired row.
	now = now.Add(2 * time.Hour)
	if _, err := store.SessionByToken(ctx, tok); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired SessionByToken err = %v, want ErrUnauthorized", err)
	}

	// Missing token -> ErrUnauthorized.
	if _, err := store.SessionByToken(ctx, "nonexistent"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("missing SessionByToken err = %v, want ErrUnauthorized", err)
	}

	// DeleteSession removes an active session.
	tok2, _, err := store.CreateSession(ctx, user.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteSession(ctx, tok2); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SessionByToken(ctx, tok2); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("deleted SessionByToken err = %v, want ErrUnauthorized", err)
	}
}

func TestHardeningNormalizeStatusAndSentryEdgeCases(t *testing.T) {
	cases := []struct {
		in, fb, want string
	}{
		{"open", StatusAll, StatusOpen},
		{"  ACKED ", StatusAll, StatusAcknowledged},
		{"ack", StatusAll, StatusAcknowledged},
		{"acknowledged", StatusOpen, StatusAcknowledged},
		{"fixed", StatusOpen, StatusFixed},
		{"suppressed", StatusOpen, StatusSuppressed},
		{"all", StatusOpen, StatusAll},
		{"unknown", StatusOpen, StatusOpen},
		{"", StatusAll, StatusAll},
	}
	for _, tc := range cases {
		if got := NormalizeStatus(tc.in, tc.fb); got != tc.want {
			t.Errorf("NormalizeStatus(%q, %q) = %q, want %q", tc.in, tc.fb, got, tc.want)
		}
	}

	// Sentry envelope: int64 length overflow must return ErrInvalidSentryEnvelope, not panic.
	rawOverflow := []byte(`{"event_id":"e1"}` + "\n" + `{"type":"event","length":9223372036854775807}` + "\n" + `{}`)
	if _, _, err := ParseSentryEnvelope(rawOverflow); !errors.Is(err, ErrInvalidSentryEnvelope) {
		t.Fatalf("int64 overflow err = %v, want ErrInvalidSentryEnvelope", err)
	}

	// Sentry envelope: multi-byte UTF-8 level must preserve valid UTF-8.
	rawUTF8 := []byte(`{"event_id":"e2"}` + "\n" + `{"type":"event"}` + "\n" + `{"level":"érror","message":"m"}`)
	_, notices, err := ParseSentryEnvelope(rawUTF8)
	if err != nil || len(notices) != 1 {
		t.Fatalf("utf8 level parse failed: notices=%d err=%v", len(notices), err)
	}
	if !utf8.ValidString(notices[0].Class) || notices[0].Class != "Érror" {
		t.Fatalf("unexpected utf8 level class: %q", notices[0].Class)
	}
}
