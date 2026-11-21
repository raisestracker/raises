package inbox

import (
	"context"
	"path/filepath"
	"testing"
)

func TestIndexNowSubmissionPersistence(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	initial, err := store.IndexNowSubmitted(ctx)
	if err != nil {
		t.Fatalf("initial query: %v", err)
	}
	if len(initial) != 0 {
		t.Fatalf("expected empty initial state, got %+v", initial)
	}

	want := map[string]string{
		"https://raises.dev/":         "sha256:aaa",
		"https://raises.dev/llms.txt": "lastmod:2026-11-19",
	}
	if err := store.RecordIndexNowSubmitted(ctx, want); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := store.IndexNowSubmitted(ctx)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(got) != len(want) || got["https://raises.dev/"] != want["https://raises.dev/"] || got["https://raises.dev/llms.txt"] != want["https://raises.dev/llms.txt"] {
		t.Fatalf("got %+v, want %+v", got, want)
	}

	updated := map[string]string{
		"https://raises.dev/": "sha256:bbb",
	}
	if err := store.RecordIndexNowSubmitted(ctx, updated); err != nil {
		t.Fatalf("update: %v", err)
	}
	got, err = store.IndexNowSubmitted(ctx)
	if err != nil {
		t.Fatalf("reload updated: %v", err)
	}
	if len(got) != 1 || got["https://raises.dev/"] != "sha256:bbb" {
		t.Fatalf("got %+v, want %+v", got, updated)
	}
}
