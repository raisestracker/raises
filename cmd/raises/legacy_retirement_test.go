package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Legacy static credentials are retired at the config level: the shipped
// binary ignores AGENT_TOKEN and APP_*_TOKEN entirely, so an old agent token
// gets 401 from agent routes and an old ingestion token gets 401 from
// ingestion on every startup shape, including across restarts. No
// DISABLE_LEGACY_TOKENS flag exists anymore.
func TestBinaryAlwaysRejectsLegacyTokens(t *testing.T) {
	binary := buildRaisesBinary(t)
	notice := `{"error":{"class":"RuntimeError","message":"boom"}}`

	serve := func(t *testing.T, db string) (base string, stop func()) {
		t.Helper()
		var cmd *exec.Cmd
		for _, port := range []int{18081, 18082, 18083, 18084} {
			base = fmt.Sprintf("http://127.0.0.1:%d", port)
			ctx, cancel := context.WithCancel(context.Background())
			cmd = exec.CommandContext(ctx, binary)
			cmd.Env = []string{
				"PATH=" + os.Getenv("PATH"),
				"LISTEN_ADDRESS=127.0.0.1:" + fmt.Sprint(port),
				"DATABASE_PATH=" + db,
				// Retired: the binary must ignore these.
				"AGENT_TOKEN=legacy-agent-token",
				"APP_WIDGET_TOKEN=legacy-app-token",
				"DISABLE_LEGACY_TOKENS=1",
			}
			cmd.Stdout = io.Discard
			cmd.Stderr = io.Discard
			if err := cmd.Start(); err != nil {
				cancel()
				t.Fatal(err)
			}
			if waitReady(t, base) {
				return base, cancel
			}
			cancel()
		}
		t.Fatal("binary never became ready")
		return "", nil
	}
	agentStatus := func(t *testing.T, base string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, base+"/v1/errors?unacked=1", nil)
		req.Header.Set("Authorization", "Bearer legacy-agent-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	ingestStatus := func(t *testing.T, base string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/notices", bytes.NewBufferString(notice))
		req.Header.Set("Authorization", "Bearer legacy-app-token")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}

	db := filepath.Join(t.TempDir(), "retired.db")
	base, stop := serve(t, db)
	if code := agentStatus(t, base); code != http.StatusUnauthorized {
		t.Fatalf("legacy agent route status=%d want 401", code)
	}
	if code := ingestStatus(t, base); code != http.StatusUnauthorized {
		t.Fatalf("legacy ingest status=%d want 401", code)
	}
	stop()

	// A second boot with the same legacy environment must not resurrect them.
	base, stop = serve(t, db)
	defer stop()
	if code := agentStatus(t, base); code != http.StatusUnauthorized {
		t.Fatalf("agent route resurrected after restart: status=%d", code)
	}
	if code := ingestStatus(t, base); code != http.StatusUnauthorized {
		t.Fatalf("ingest credential resurrected after restart: status=%d", code)
	}
}

func buildRaisesBinary(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "raises")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	return binary
}

func waitReady(t *testing.T, base string) bool {
	t.Helper()
	for i := 0; i < 100; i++ {
		resp, err := http.Get(base + "/healthz")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}
