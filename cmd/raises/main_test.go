package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

func TestLoadConfigUsesGenericDisabledDefaults(t *testing.T) {
	setGenericConfigEnv(t)

	conf, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if conf.githubAppID != 0 || conf.initialOwnerGitHubID != 0 || conf.reportEnabled || conf.alertsEnabled || conf.indexNowEnabled {
		t.Fatalf("unexpected enabled defaults: %#v", conf)
	}
	if conf.reportEmailFrom != "" || conf.reportEmailTo != "" || conf.awsRegion != "" || conf.reportTimezone != "UTC" || conf.reportSendHour != 7 || conf.reportSendMinute != 0 {
		t.Fatalf("unexpected reporting defaults: %#v", conf)
	}
}

func TestLoadConfigRejectsPartialGitHubApp(t *testing.T) {
	setGenericConfigEnv(t)
	t.Setenv("GITHUB_APP_ID", "123")
	t.Setenv("GITHUB_APP_CLIENT_ID", "")
	t.Setenv("GITHUB_APP_CLIENT_SECRET", "")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "")
	t.Setenv("GITHUB_APP_WEBHOOK_SECRET", "")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected partial GitHub App configuration error")
	}
}

func TestLoadConfigRejectsEnabledReportingWithoutRequiredValues(t *testing.T) {
	setGenericConfigEnv(t)
	t.Setenv("GITHUB_APP_ID", "")
	t.Setenv("GITHUB_APP_CLIENT_ID", "")
	t.Setenv("GITHUB_APP_CLIENT_SECRET", "")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", "")
	t.Setenv("GITHUB_APP_WEBHOOK_SECRET", "")
	t.Setenv("REPORT_ENABLED", "1")
	t.Setenv("REPORT_EMAIL_FROM", "sender@example.com")
	t.Setenv("REPORT_EMAIL_TO", "")
	t.Setenv("AWS_REGION", "us-east-1")
	if _, err := loadConfig(); err == nil {
		t.Fatal("expected enabled reporting configuration error")
	}
}

func TestLoadConfigIgnoresRetiredLegacyCredentialEnv(t *testing.T) {
	setGenericConfigEnv(t)
	t.Setenv("AGENT_TOKEN", "legacy-agent-token")
	t.Setenv("APP_WIDGET_TOKEN", "legacy-app-token")
	t.Setenv("APP_WIDGET_REPO", "example/widget")
	t.Setenv("DISABLE_LEGACY_TOKENS", "1")
	if _, err := loadConfig(); err != nil {
		t.Fatalf("retired legacy credential env must be ignored, got error: %v", err)
	}
}

func TestLoadConfigRejectsNtfyWithoutConfiguredOwner(t *testing.T) {
	setGenericConfigEnv(t)
	t.Setenv("NTFY_ENABLED", "1")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "INITIAL_OWNER_GITHUB_ID") {
		t.Fatalf("expected ntfy owner configuration error, got %v", err)
	}
}

func TestResolveLegacyOwnerDoesNotCreateConfiguredUser(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ownerID, err := resolveLegacyOwner(context.Background(), store, 6334, false)
	if err != nil || ownerID != 0 {
		t.Fatalf("missing owner id=%d err=%v", ownerID, err)
	}
	if _, err := store.UserByGitHubID(context.Background(), 6334); !errors.Is(err, inbox.ErrNotFound) {
		t.Fatalf("configured owner was created: %v", err)
	}

	user, err := store.UpsertGitHubUser(context.Background(), 6334, "demo", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertApp(context.Background(), "widget", "token", ""); err != nil {
		t.Fatal(err)
	}
	ownerID, err = resolveLegacyOwner(context.Background(), store, 6334, true)
	if err != nil || ownerID != user.ID {
		t.Fatalf("resolved owner id=%d want=%d err=%v", ownerID, user.ID, err)
	}
}

func TestResolveLegacyOwnerRejectsMissingRequiredOwner(t *testing.T) {
	store, err := inbox.Open(filepath.Join(t.TempDir(), "raises.db"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := resolveLegacyOwner(context.Background(), store, 6334, true); err == nil || !strings.Contains(err.Error(), "existing signed-in owner") {
		t.Fatalf("expected missing ntfy owner error, got %v", err)
	}
}

func setGenericConfigEnv(t *testing.T) {
	t.Helper()
	for key, value := range map[string]string{
		"GITHUB_APP_ID":              "",
		"GITHUB_APP_CLIENT_ID":       "",
		"GITHUB_APP_CLIENT_SECRET":   "",
		"GITHUB_APP_PRIVATE_KEY":     "",
		"GITHUB_APP_WEBHOOK_SECRET":  "",
		"REPORT_ENABLED":             "0",
		"OPERATIONAL_ALERTS_ENABLED": "0",
		"REPORT_EMAIL_FROM":          "",
		"REPORT_EMAIL_TO":            "",
		"AWS_REGION":                 "",
		"REPORT_TIMEZONE":            "",
		"INITIAL_OWNER_GITHUB_ID":    "",
		"NTFY_ENABLED":               "0",
		"NTFY_TOPIC":                 "",
		"NTFY_TOKEN":                 "",
		"BASE_URL":                   "",
		"INDEXNOW_ENABLED":           "",
	} {
		t.Setenv(key, value)
	}
}

func TestLoadConfigEnablesIndexNowOnProductionHost(t *testing.T) {
	setGenericConfigEnv(t)
	t.Setenv("BASE_URL", "https://raises.dev")
	conf, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !conf.indexNowEnabled {
		t.Fatal("expected indexNowEnabled=true for https://raises.dev")
	}
}

type slowShutdownFiler struct {
	started chan struct{}
	delay   time.Duration
}

func (f *slowShutdownFiler) FindByMarker(context.Context, int64, string, string, time.Time) (int, string, bool, error) {
	select {
	case f.started <- struct{}{}:
	default:
	}
	time.Sleep(f.delay)
	return 0, "", false, nil
}

func (f *slowShutdownFiler) Open(context.Context, int64, string, string, string) (int, string, error) {
	return 1, "https://github.com/example/widget/issues/1", nil
}

func (f *slowShutdownFiler) Reopen(context.Context, int64, string, int) error { return nil }

func TestDrainWorkersWaitsBeforeStoreClose(t *testing.T) {
	filer := &slowShutdownFiler{started: make(chan struct{}, 1), delay: 120 * time.Millisecond}
	store, err := inbox.Open(filepath.Join(t.TempDir(), "drain.db"), nil, filer)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertApp(context.Background(), "widget", "tok", "example/widget"); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var workerErrs []error

	wg.Add(1)
	go func() {
		defer wg.Done()
		store.RunIssueWorker(runCtx, func(err error) {
			errMu.Lock()
			workerErrs = append(workerErrs, err)
			errMu.Unlock()
		})
	}()

	// Trigger an in-flight worker job (use a separate goroutine because Ingest
	// may still run ProcessIssueJobsOnce synchronously before E05).
	ingestDone := make(chan error, 1)
	go func() {
		_, err := store.Ingest(context.Background(), "tok", inbox.Notice{
			Class:     "RuntimeError",
			Message:   "drain test",
			Backtrace: []string{"app/models/drain.rb:1"},
		})
		ingestDone <- err
	}()

	select {
	case <-filer.started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for filer call to start")
	}

	// Simulate SIGTERM cancellation mid-flight, then drain before closing store.
	cancel()
	if !drainWorkers(&wg, 2*time.Second) {
		t.Fatal("drainWorkers timed out before worker exited")
	}
	if err := <-ingestDone; err != nil {
		t.Fatalf("ingest error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("store close: %v", err)
	}

	errMu.Lock()
	defer errMu.Unlock()
	for _, werr := range workerErrs {
		if strings.Contains(strings.ToLower(werr.Error()), "database is closed") {
			t.Fatalf("worker hit database-is-closed error during drain: %v", werr)
		}
	}
}

func TestDrainWorkersRespectsBoundedTimeout(t *testing.T) {
	var wg sync.WaitGroup
	release := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-release
	}()
	defer close(release)

	start := time.Now()
	if drainWorkers(&wg, 40*time.Millisecond) {
		t.Fatal("expected drainWorkers to return false when worker blocks past timeout")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("drainWorkers took too long: %s", elapsed)
	}
}
