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

// The shipped binary must reject retired legacy credentials on every startup
// shape: old AGENT_TOKEN gets 401 from agent routes and old APP_*_TOKEN gets
// 401 from ingestion when DISABLE_LEGACY_TOKENS=1, across restarts. With the
// flag off the same credentials keep working.
func TestBinaryRetiresLegacyTokensWhenFlagged(t *testing.T) {
	binary := buildRaisesBinary(t)
	notice := `{"error":{"class":"RuntimeError","message":"boom"}}`

	serve := func(t *testing.T, db string, disable string) (base string, stop func()) {
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
				"AGENT_TOKEN=legacy-agent-token",
				"APP_WIDGET_TOKEN=legacy-app-token",
				"DISABLE_LEGACY_TOKENS=" + disable,
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
	base, stop := serve(t, db, "1")
	if code := agentStatus(t, base); code != http.StatusUnauthorized {
		t.Fatalf("flagged agent route status=%d want 401", code)
	}
	if code := ingestStatus(t, base); code != http.StatusUnauthorized {
		t.Fatalf("flagged ingest status=%d want 401", code)
	}
	stop()

	// A second boot with the same legacy environment must not resurrect them.
	base, stop = serve(t, db, "1")
	defer stop()
	if code := agentStatus(t, base); code != http.StatusUnauthorized {
		t.Fatalf("agent route resurrected after restart: status=%d", code)
	}
	if code := ingestStatus(t, base); code != http.StatusUnauthorized {
		t.Fatalf("ingest credential resurrected after restart: status=%d", code)
	}
}

func TestBinaryKeepsLegacyTokensWithoutFlag(t *testing.T) {
	binary := buildRaisesBinary(t)
	db := filepath.Join(t.TempDir(), "compat.db")
	var cmd *exec.Cmd
	var base string
	for _, port := range []int{18085, 18086, 18087, 18088} {
		base = fmt.Sprintf("http://127.0.0.1:%d", port)
		ctx, cancel := context.WithCancel(context.Background())
		cmd = exec.CommandContext(ctx, binary)
		cmd.Env = []string{
			"PATH=" + os.Getenv("PATH"),
			"LISTEN_ADDRESS=127.0.0.1:" + fmt.Sprint(port),
			"DATABASE_PATH=" + db,
			"AGENT_TOKEN=legacy-agent-token",
			"APP_WIDGET_TOKEN=legacy-app-token",
		}
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatal(err)
		}
		if waitReady(t, base) {
			defer cancel()
			break
		}
		cancel()
		cmd = nil
	}
	if cmd == nil {
		t.Fatal("binary never became ready")
	}
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/errors?unacked=1", nil)
	req.Header.Set("Authorization", "Bearer legacy-agent-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unflagged agent route status=%d want 200", resp.StatusCode)
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
