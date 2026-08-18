package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/raisestracker/raises/internal/inbox"
)

func TestLoadConfigUsesGenericDisabledDefaults(t *testing.T) {
	setGenericConfigEnv(t)

	conf, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if conf.githubAppID != 0 || conf.initialOwnerGitHubID != 0 || conf.reportEnabled || conf.alertsEnabled {
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
	} {
		t.Setenv(key, value)
	}
}
