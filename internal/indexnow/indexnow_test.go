package indexnow

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/raisestracker/raises/web"
)

func TestEnabled(t *testing.T) {
	cases := []struct {
		baseURL string
		flag    string
		want    bool
	}{
		{"https://raises.dev", "", true},
		{"https://raises.dev/", "", true},
		{"https://raises.dev", "0", false},
		{"https://raises.dev", "false", false},
		{"http://localhost:8080", "", false},
		{"http://localhost:8080", "1", true},
		{"http://localhost:8080", "true", true},
		{"http://raises.dev", "", false},
		{"https://staging.raises.dev", "", false},
	}
	for _, tc := range cases {
		if got := Enabled(tc.baseURL, tc.flag); got != tc.want {
			t.Errorf("Enabled(%q, %q) = %v, want %v", tc.baseURL, tc.flag, got, tc.want)
		}
	}
}

func TestEmbeddedKeyFileMatchesConstant(t *testing.T) {
	body, err := web.FS.ReadFile(Key + ".txt")
	if err != nil {
		t.Fatalf("read embedded key file: %v", err)
	}
	if string(body) != Key {
		t.Fatalf("embedded key file = %q, want %q", string(body), Key)
	}
}

func TestClientSubmitFiltersAndPosts(t *testing.T) {
	var received submitPayload
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("content-type = %q", ct)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	client := NewClient()
	client.Endpoint = srv.URL
	client.HTTPClient = srv.Client()

	sent, err := client.Submit(context.Background(), []string{
		"https://raises.dev/llms.txt",
		"https://raises.dev/",
		"https://raises.dev/llms.txt",
		"https://evil.example.com/phish",
		"not-a-url",
	})
	if err != nil {
		t.Fatalf("Submit failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
	want := []string{"https://raises.dev/", "https://raises.dev/llms.txt"}
	if len(sent) != len(want) || sent[0] != want[0] || sent[1] != want[1] {
		t.Fatalf("sent = %v, want %v", sent, want)
	}
	if received.Host != Host || received.Key != Key || received.KeyLocation != KeyLocation {
		t.Fatalf("unexpected payload metadata: %+v", received)
	}
	if len(received.URLList) != 2 {
		t.Fatalf("unexpected urlList: %+v", received.URLList)
	}
}

func TestClientSubmitErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid key", http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	client := NewClient()
	client.Endpoint = srv.URL
	client.HTTPClient = srv.Client()

	if _, err := client.Submit(context.Background(), []string{"https://raises.dev/"}); err == nil {
		t.Fatal("expected error for 422 response")
	} else if !strings.Contains(err.Error(), "422") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type memoryStateStore struct {
	entries map[string]string
}

func (m *memoryStateStore) IndexNowSubmitted(_ context.Context) (map[string]string, error) {
	out := make(map[string]string, len(m.entries))
	for k, v := range m.entries {
		out[k] = v
	}
	return out, nil
}

func (m *memoryStateStore) RecordIndexNowSubmitted(_ context.Context, entries map[string]string) error {
	m.entries = make(map[string]string, len(entries))
	for k, v := range entries {
		m.entries[k] = v
	}
	return nil
}

func TestWorkerSyncOnceDiffsSitemapAndContent(t *testing.T) {
	var batches [][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p submitPayload
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		batches = append(batches, p.URLList)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := NewClient()
	client.Endpoint = srv.URL
	client.HTTPClient = srv.Client()

	fsys := fstest.MapFS{
		"sitemap.xml": &fstest.MapFile{Data: []byte(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://raises.dev/</loc></url>
  <url><loc>https://raises.dev/llms.txt</loc><lastmod>2026-11-19</lastmod></url>
</urlset>`)},
		"index.html": &fstest.MapFile{Data: []byte("<h1>v1</h1>")},
		"index.md":   &fstest.MapFile{Data: []byte("# v1")},
	}
	store := &memoryStateStore{}
	worker := &Worker{Client: client, Store: store, FS: fsys}

	// 1. First run submits both URLs.
	sent, err := worker.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if len(sent) != 2 {
		t.Fatalf("first sync sent %v, want 2 URLs", sent)
	}

	// 2. Second run with no changes submits nothing.
	sent, err = worker.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if len(sent) != 0 || len(batches) != 1 {
		t.Fatalf("second sync should be a no-op; sent=%v batches=%d", sent, len(batches))
	}

	// 3. Updating index.html submits only /.
	fsys["index.html"] = &fstest.MapFile{Data: []byte("<h1>v2</h1>")}
	sent, err = worker.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if len(sent) != 1 || sent[0] != "https://raises.dev/" {
		t.Fatalf("third sync sent %v, want [https://raises.dev/]", sent)
	}

	// 4. Bumping lastmod on llms.txt submits only /llms.txt.
	fsys["sitemap.xml"] = &fstest.MapFile{Data: []byte(`<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>https://raises.dev/</loc></url>
  <url><loc>https://raises.dev/llms.txt</loc><lastmod>2026-11-20</lastmod></url>
</urlset>`)}
	sent, err = worker.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("fourth sync: %v", err)
	}
	if len(sent) != 1 || sent[0] != "https://raises.dev/llms.txt" {
		t.Fatalf("fourth sync sent %v, want [https://raises.dev/llms.txt]", sent)
	}
}

func TestSitemapEntriesEmbedded(t *testing.T) {
	entries, err := SitemapEntries(web.FS)
	if err != nil {
		t.Fatalf("SitemapEntries(web.FS): %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(entries), entries)
	}
}
