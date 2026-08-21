package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/raisestracker/raises/internal/brief"
	gh "github.com/raisestracker/raises/internal/github"
	"github.com/raisestracker/raises/internal/httpserver"
	"github.com/raisestracker/raises/internal/inbox"
	"github.com/raisestracker/raises/internal/ntfy"
	"github.com/raisestracker/raises/internal/operational"
	"github.com/raisestracker/raises/internal/outbound"
	"github.com/raisestracker/raises/internal/secretbox"
)

type config struct {
	listenAddress        string
	databasePath         string
	agentToken           string
	githubToken          string
	githubAppID          int64
	githubClientID       string
	githubClientSecret   string
	githubPrivateKey     string
	githubWebhookSecret  string
	githubAppSlug        string
	baseURL              string
	initialOwnerGitHubID int64
	disableLegacyTokens  bool
	bodyLimit            int64
	apps                 []appConfig
	reportEnabled        bool
	reportEmailFrom      string
	reportEmailTo        string
	reportTimezone       string
	reportSendHour       int
	reportSendMinute     int
	awsRegion            string
	alertsEnabled        bool
	ntfyEnabled          bool
	ntfyBaseURL          string
	ntfyTopic            string
	ntfyToken            string
	webhookEncryptionKey string
}

type appConfig struct {
	name  string
	token string
	repo  string
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("raises stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	conf, err := loadConfig()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(conf.databasePath), 0o755); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}

	var filer inbox.IssueFiler
	var githubApp *gh.AppClient
	if conf.githubAppID > 0 && conf.githubClientID != "" && conf.githubClientSecret != "" && conf.githubPrivateKey != "" {
		githubApp, err = gh.NewApp(conf.githubAppID, conf.githubClientID, conf.githubClientSecret, gh.ParsePrivateKeyEnv(conf.githubPrivateKey))
		if err != nil {
			return err
		}
		filer = githubApp
	} else if conf.githubToken != "" {
		filer = gh.New(conf.githubToken)
	}

	store, err := inbox.Open(conf.databasePath, nil, filer)
	if err != nil {
		return err
	}
	defer store.Close()
	if conf.webhookEncryptionKey != "" {
		cipher, err := secretbox.New(conf.webhookEncryptionKey)
		if err != nil {
			return err
		}
		store.SetSecretCipher(cipher)
	}

	ctx := context.Background()
	legacyOwnerID, err := resolveLegacyOwner(ctx, store, conf.initialOwnerGitHubID, conf.ntfyEnabled)
	if err != nil {
		return err
	}
	store.ConfigureUnlimitedProjects(legacyOwnerID)
	for _, app := range conf.apps {
		if err := store.UpsertApp(ctx, app.name, app.token, app.repo); err != nil {
			return err
		}
		logger.Info("configured app", "app", app.name, "repo", app.repo)
	}
	if conf.disableLegacyTokens && legacyOwnerID > 0 {
		if err := store.ClearLegacyTokens(ctx, legacyOwnerID); err != nil {
			return err
		}
	}

	location, err := time.LoadLocation(conf.reportTimezone)
	if err != nil {
		return fmt.Errorf("load report timezone: %w", err)
	}
	briefConfig := inbox.BriefConfig{
		Location: location, SendHour: conf.reportSendHour, SendMinute: conf.reportSendMinute,
	}
	command := strings.Join(os.Args[1:], " ")
	if command == "daily-brief render" || command == "daily-brief send" {
		today := time.Now().In(location)
		kind := inbox.BriefDaily
		if today.Weekday() == time.Friday {
			kind = inbox.BriefWeekly
		}
		report, err := store.BuildBrief(ctx, kind, today, location)
		if err != nil {
			return err
		}
		subject, body, messageID := inbox.RenderBrief(report)
		if command == "daily-brief render" {
			fmt.Printf("Subject: %s\n\n%s", subject, body)
			return nil
		}
		alreadySent, err := store.BriefWasDelivered(ctx, kind, report.ReportDate)
		if err != nil {
			return err
		}
		if alreadySent {
			return fmt.Errorf("%s brief for %s was already sent", kind, report.ReportDate.Format("2006-01-02"))
		}
		sender, err := newReportSender(ctx, conf)
		if err != nil {
			return err
		}
		if err := sender.Send(ctx, subject, body, messageID); err != nil {
			return err
		}
		if err := store.RecordBriefDelivered(ctx, kind, report.ReportDate); err != nil {
			return fmt.Errorf("record sent report: %w", err)
		}
		logger.Info("sent report canary", "kind", kind, "report_date", today.Format("2006-01-02"))
		return nil
	}
	if command == "operational-alert send" {
		sender, err := newReportSender(ctx, conf)
		if err != nil {
			return err
		}
		alerter := operational.New(sender, 30*time.Minute)
		if err := alerter.Report(ctx, "canary", "monitoring canary", "Raises can send immediate operational alerts through Amazon SES."); err != nil {
			return err
		}
		logger.Info("sent operational alert canary")
		return nil
	}
	if command == "ntfy-alert send" {
		notifier, err := newNtfyClient(conf)
		if err != nil {
			return err
		}
		if err := notifier.Notify(ctx, inbox.Group{App: "Raises", Env: "production", Class: "Canary", Location: "notification delivery", Message: "New and regressed production errors will appear here.", GitHubIssueURL: conf.baseURL}, "open", time.Now().UTC()); err != nil {
			return err
		}
		logger.Info("sent ntfy alert canary")
		return nil
	}
	if command != "" {
		return fmt.Errorf("unknown command %q", command)
	}

	runCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var sender *brief.SESSender
	if conf.reportEnabled || conf.alertsEnabled {
		sender, err = newReportSender(ctx, conf)
		if err != nil {
			return err
		}
	}
	var alerter *operational.Alerter
	if conf.alertsEnabled {
		alerter = operational.New(sender, 30*time.Minute)
		logger.Info("operational alerts enabled")
	}
	reportOperational := func(key, title, details string) {
		if alerter == nil {
			return
		}
		alertCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := alerter.Report(alertCtx, key, title, details); err != nil {
			logger.Error("send operational alert", "key", key, "error", err)
		}
	}
	runProtected := func(key, title string, worker func()) {
		go func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					details := fmt.Sprintf("Panic: %v\n\n%s", recovered, debug.Stack())
					logger.Error("background worker panic", "worker", key, "panic", recovered)
					reportOperational("panic:"+key, title, details)
					panic(recovered)
				}
			}()
			worker()
		}()
	}
	store.OnIssueJobDead(func(delivery inbox.IssueDelivery) {
		details := fmt.Sprintf("Project: %s\nRepository: %s\nAction: %s\nAttempts: %d\nError: %s", delivery.ProjectName, delivery.Repository, delivery.Action, delivery.Attempts, delivery.LastError)
		reportOperational("issue-job-dead:"+strconv.FormatInt(delivery.ID, 10), "GitHub issue delivery is dead", details)
	})
	var notifier *ntfy.Client
	if conf.ntfyEnabled {
		notifier, err = newNtfyClient(conf)
		if err != nil {
			return err
		}
		store.ConfigureOperatorNtfy(legacyOwnerID, true)
		logger.Info("ntfy notifications enabled", "owner_user_id", legacyOwnerID)
	}
	outboundSender := &outbound.Sender{Webhooks: outbound.New(), Ntfy: notifier}
	store.OnOutboundDeliveryDead(func(delivery inbox.OutboundDelivery) {
		if delivery.DestinationKind == "ntfy" {
			reportOperational("ntfy-delivery-dead:"+delivery.ID, "ntfy delivery is dead", fmt.Sprintf("Event: %s\nAttempts: %d\nError: %s", delivery.Event.Type, delivery.Attempts, delivery.LastError))
		}
	})
	runProtected("issue-worker", "issue worker panicked", func() {
		store.RunIssueWorker(runCtx, func(err error) {
			logger.Error("issue worker failed", "error", err)
			reportOperational("issue-worker", "issue worker failed", err.Error())
		})
	})
	runProtected("outbound-worker", "outbound delivery worker panicked", func() {
		store.RunOutboundWorker(runCtx, outboundSender, func(err error) {
			logger.Error("outbound delivery failed", "error", err)
			reportOperational("outbound-worker", "outbound delivery worker failed", err.Error())
		})
	})
	if conf.reportEnabled {
		runProtected("report-worker", "KPI report worker panicked", func() {
			store.RunBriefWorker(runCtx, briefConfig, sender, func(err error) {
				logger.Error("report delivery failed", "error", err)
				reportOperational("report-worker", "KPI email delivery failed", err.Error())
			})
		})
		logger.Info("report delivery enabled", "timezone", conf.reportTimezone, "send_at", fmt.Sprintf("%02d:%02d", conf.reportSendHour, conf.reportSendMinute))
	}

	server := httpserver.NewWithConfig(store, logger, conf.bodyLimit, httpserver.Config{
		LegacyAgentToken:     conf.agentToken,
		LegacyOwnerID:        legacyOwnerID,
		InitialOwnerGitHubID: conf.initialOwnerGitHubID,
		BaseURL:              conf.baseURL,
		GitHubAppSlug:        conf.githubAppSlug,
		GitHubWebhookSecret:  conf.githubWebhookSecret,
		GitHubApp:            githubApp,
		SecureCookies:        strings.HasPrefix(conf.baseURL, "https://"),
		OperationalReporter:  alerter,
	})
	if err := httpserver.ListenAndServe(runCtx, conf.listenAddress, server.Handler(), logger); err != nil {
		reportOperational("http-server", "HTTP server stopped", err.Error())
		return err
	}
	return nil
}

func loadConfig() (config, error) {
	conf := config{
		listenAddress:        envOr("LISTEN_ADDRESS", ":8080"),
		databasePath:         envOr("DATABASE_PATH", "./data/raises.db"),
		agentToken:           os.Getenv("AGENT_TOKEN"),
		githubToken:          os.Getenv("GITHUB_TOKEN"),
		bodyLimit:            1 << 20,
		githubClientID:       strings.TrimSpace(os.Getenv("GITHUB_APP_CLIENT_ID")),
		githubClientSecret:   strings.TrimSpace(os.Getenv("GITHUB_APP_CLIENT_SECRET")),
		githubPrivateKey:     strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY")),
		githubWebhookSecret:  strings.TrimSpace(os.Getenv("GITHUB_APP_WEBHOOK_SECRET")),
		githubAppSlug:        envOr("GITHUB_APP_SLUG", "raises"),
		baseURL:              envOr("BASE_URL", "http://localhost:8080"),
		reportEnabled:        os.Getenv("REPORT_ENABLED") == "1",
		reportEmailFrom:      strings.TrimSpace(os.Getenv("REPORT_EMAIL_FROM")),
		reportEmailTo:        strings.TrimSpace(os.Getenv("REPORT_EMAIL_TO")),
		reportTimezone:       envOr("REPORT_TIMEZONE", "UTC"),
		awsRegion:            strings.TrimSpace(os.Getenv("AWS_REGION")),
		alertsEnabled:        os.Getenv("OPERATIONAL_ALERTS_ENABLED") == "1",
		ntfyEnabled:          os.Getenv("NTFY_ENABLED") == "1",
		ntfyBaseURL:          envOr("NTFY_BASE_URL", "https://ntfy.sh"),
		ntfyTopic:            os.Getenv("NTFY_TOPIC"),
		ntfyToken:            os.Getenv("NTFY_TOKEN"),
		webhookEncryptionKey: os.Getenv("WEBHOOK_ENCRYPTION_KEY"),
	}
	parsedSendAt, err := time.Parse("15:04", envOr("REPORT_SEND_AT", "07:00"))
	if err != nil {
		return config{}, fmt.Errorf("REPORT_SEND_AT must use HH:MM: %w", err)
	}
	conf.reportSendHour, conf.reportSendMinute = parsedSendAt.Hour(), parsedSendAt.Minute()
	githubAppID := strings.TrimSpace(os.Getenv("GITHUB_APP_ID"))
	githubAppValues := []string{githubAppID, conf.githubClientID, conf.githubClientSecret, conf.githubPrivateKey, conf.githubWebhookSecret}
	configuredGitHubApp := 0
	for _, value := range githubAppValues {
		if value != "" {
			configuredGitHubApp++
		}
	}
	if configuredGitHubApp != 0 && configuredGitHubApp != len(githubAppValues) {
		return config{}, fmt.Errorf("GitHub App configuration requires GITHUB_APP_ID, GITHUB_APP_CLIENT_ID, GITHUB_APP_CLIENT_SECRET, GITHUB_APP_PRIVATE_KEY, and GITHUB_APP_WEBHOOK_SECRET together")
	}
	if configuredGitHubApp == len(githubAppValues) {
		var parseErr error
		conf.githubAppID, parseErr = strconv.ParseInt(githubAppID, 10, 64)
		if parseErr != nil || conf.githubAppID < 1 {
			return config{}, fmt.Errorf("GITHUB_APP_ID must be a positive integer")
		}
	}
	initialOwnerGitHubID := envOr("INITIAL_OWNER_GITHUB_ID", "0")
	var parseErr error
	conf.initialOwnerGitHubID, parseErr = strconv.ParseInt(initialOwnerGitHubID, 10, 64)
	if parseErr != nil || conf.initialOwnerGitHubID < 0 {
		return config{}, fmt.Errorf("INITIAL_OWNER_GITHUB_ID must be a nonnegative integer")
	}
	if conf.ntfyEnabled && conf.initialOwnerGitHubID == 0 {
		return config{}, fmt.Errorf("NTFY_ENABLED requires a positive INITIAL_OWNER_GITHUB_ID for an existing signed-in owner")
	}
	conf.disableLegacyTokens = os.Getenv("DISABLE_LEGACY_TOKENS") == "1"
	if conf.reportEnabled || conf.alertsEnabled {
		if conf.reportEmailFrom == "" || conf.reportEmailTo == "" || conf.awsRegion == "" {
			return config{}, fmt.Errorf("REPORT_ENABLED or OPERATIONAL_ALERTS_ENABLED requires REPORT_EMAIL_FROM, REPORT_EMAIL_TO, and AWS_REGION")
		}
	}

	for _, env := range os.Environ() {
		key, value, ok := strings.Cut(env, "=")
		if !ok || !strings.HasPrefix(key, "APP_") || !strings.HasSuffix(key, "_TOKEN") {
			continue
		}
		name := strings.ToLower(strings.TrimSuffix(strings.TrimPrefix(key, "APP_"), "_TOKEN"))
		if name == "" || value == "" {
			continue
		}
		repoKey := "APP_" + strings.ToUpper(name) + "_REPO"
		conf.apps = append(conf.apps, appConfig{
			name:  name,
			token: value,
			repo:  os.Getenv(repoKey),
		})
	}
	return conf, nil
}

func newNtfyClient(conf config) (*ntfy.Client, error) {
	return ntfy.New(conf.ntfyBaseURL, conf.ntfyTopic, conf.ntfyToken)
}

func resolveLegacyOwner(ctx context.Context, store *inbox.Store, githubID int64, required bool) (int64, error) {
	if githubID == 0 {
		if required {
			return 0, fmt.Errorf("ntfy requires a configured legacy owner")
		}
		return 0, nil
	}
	owner, err := store.UserByGitHubID(ctx, githubID)
	if errors.Is(err, inbox.ErrNotFound) {
		if required {
			return 0, fmt.Errorf("NTFY_ENABLED requires INITIAL_OWNER_GITHUB_ID %d to match an existing signed-in owner", githubID)
		}
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("look up configured legacy owner: %w", err)
	}
	if err := store.AssignLegacyProjects(ctx, owner.ID); err != nil {
		return 0, err
	}
	return owner.ID, nil
}

func newReportSender(ctx context.Context, conf config) (*brief.SESSender, error) {
	awsConf, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(conf.awsRegion))
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	return brief.NewSESSender(ses.NewFromConfig(awsConf), conf.reportEmailFrom, conf.reportEmailTo)
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
