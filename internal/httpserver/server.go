package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/raisestracker/raises/internal/github"
	"github.com/raisestracker/raises/internal/inbox"
	"github.com/raisestracker/raises/web"
)

type Store interface {
	Ping(context.Context) error
	Ingest(context.Context, string, inbox.Notice) (inbox.IngestResult, error)
	List(context.Context, string, bool) ([]inbox.Group, error)
	Get(context.Context, int64) (inbox.Group, error)
	Notices(context.Context, int64, int) ([]inbox.Notice, error)
	Ack(context.Context, int64) (inbox.Group, error)
	UpsertGitHubUser(context.Context, int64, string, string, string) (inbox.User, error)
	AssignLegacyProjects(context.Context, int64) error
	CreateSession(context.Context, int64, time.Duration) (string, inbox.Session, error)
	SessionByToken(context.Context, string) (inbox.Session, error)
	DeleteSession(context.Context, string) error
	CreateBootstrapToken(context.Context, int64, string, time.Duration) (string, error)
	ExchangeBootstrapToken(context.Context, string, string) (string, inbox.AgentKey, error)
	CreateAgentKey(context.Context, int64, string) (string, inbox.AgentKey, error)
	AuthenticateAgent(context.Context, string) (int64, error)
	ListAgentKeys(context.Context, int64) ([]inbox.AgentKey, error)
	RevokeAgentKey(context.Context, int64, string) error
	ListProjects(context.Context, int64, bool) ([]inbox.Project, error)
	GetProject(context.Context, int64, string) (inbox.Project, error)
	CreateProject(context.Context, int64, string, string) (inbox.Project, error)
	UpdateProject(context.Context, int64, string, string) (inbox.Project, error)
	SetProjectArchived(context.Context, int64, string, bool) (inbox.Project, error)
	CreateProjectToken(context.Context, int64, string) (string, inbox.ProjectToken, error)
	RevokeProjectToken(context.Context, int64, string, string) error
	ListForUser(context.Context, int64, string, bool) ([]inbox.Group, error)
	GetForUser(context.Context, int64, int64) (inbox.Group, error)
	NoticesForUser(context.Context, int64, int64, int) ([]inbox.Notice, error)
	AckForUser(context.Context, int64, int64) (inbox.Group, error)
	SuppressForUser(context.Context, int64, int64) (inbox.Group, error)
	UnsuppressForUser(context.Context, int64, int64) (inbox.Group, error)
	UpsertInstallation(context.Context, int64, int64, string, string, string, string, []inbox.GitHubRepository) error
	ListInstallations(context.Context, int64) ([]inbox.GitHubInstallation, error)
	InstallationUserID(context.Context, int64) (int64, error)
	SetInstallationStatus(context.Context, int64, string) error
	ListGitHubRepositories(context.Context, int64) ([]inbox.GitHubRepository, error)
	BindProjectRepository(context.Context, int64, string, int64) (inbox.Project, error)
	UnbindProjectRepository(context.Context, int64, string) (inbox.Project, error)
	IssueDeliveryHealthForUser(context.Context, int64) (inbox.IssueDeliveryHealth, error)
	RetryIssueJobForUser(context.Context, int64, int64) error
	CreateEvent(context.Context, string, inbox.EventInput) (inbox.Event, error)
	ListEventsForUser(context.Context, int64, string, string, time.Time, int) ([]inbox.Event, error)
	GetEventForUser(context.Context, int64, string) (inbox.Event, error)
	CreateWebhookEndpoint(context.Context, int64, string, []string) (inbox.WebhookEndpoint, string, error)
	ListWebhookEndpoints(context.Context, int64) ([]inbox.WebhookEndpoint, error)
	UpdateWebhookEndpoint(context.Context, int64, string, string, []string, bool) (inbox.WebhookEndpoint, error)
	DeleteWebhookEndpoint(context.Context, int64, string) error
	RotateWebhookSecret(context.Context, int64, string) (string, error)
	EnqueueWebhookTest(context.Context, int64, string) error
	WebhookDeliveryHealthForUser(context.Context, int64) (inbox.WebhookDeliveryHealth, error)
	ListWebhookDeliveriesForUser(context.Context, int64, string, int) ([]inbox.WebhookDelivery, error)
	RetryWebhookDeliveryForUser(context.Context, int64, string) error
}

type GitHubApp interface {
	OAuthURL(redirectURI, state, challenge string) string
	ExchangeCode(context.Context, string, string, string) (string, error)
	CurrentUser(context.Context, string) (gh.User, error)
	UserInstallations(context.Context, string) ([]gh.Installation, error)
	Installation(context.Context, int64) (gh.Installation, error)
	Repositories(context.Context, int64) ([]gh.Repository, error)
}

type OperationalReporter interface {
	Report(context.Context, string, string, string) error
}

type Config struct {
	LegacyAgentToken     string
	LegacyOwnerID        int64
	InitialOwnerGitHubID int64
	BaseURL              string
	GitHubAppSlug        string
	GitHubWebhookSecret  string
	GitHubApp            GitHubApp
	SecureCookies        bool
	OperationalReporter  OperationalReporter
}

type Server struct {
	store      Store
	agentToken string
	logger     *slog.Logger
	bodyLimit  int64
	handler    http.Handler
	config     Config
	stylePath  string
	limitMu    sync.Mutex
	limits     map[string]requestWindow
}

type requestWindow struct {
	Started time.Time
	Count   int
}

func New(store Store, agentToken string, logger *slog.Logger, bodyLimit int64) *Server {
	return NewWithConfig(store, logger, bodyLimit, Config{LegacyAgentToken: agentToken})
}

func NewWithConfig(store Store, logger *slog.Logger, bodyLimit int64, config Config) *Server {
	s := &Server{
		store:      store,
		agentToken: config.LegacyAgentToken,
		logger:     logger,
		bodyLimit:  bodyLimit,
		config:     config,
		stylePath:  embeddedStylesheetPath(),
		limits:     map[string]requestWindow{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.root)
	mux.HandleFunc("GET /style.css", s.stylesheet)
	mux.HandleFunc("GET /assets/{name}", s.stylesheet)
	mux.HandleFunc("GET /app-icon.png", s.appIcon)
	mux.HandleFunc("GET /llms.txt", s.llmsText)
	mux.HandleFunc("GET /skill.md", s.skillText)
	mux.HandleFunc("GET /migration.md", s.migrationText)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("POST /v1/notices", s.createNotice)
	mux.HandleFunc("POST /v1/events", s.createEvent)
	mux.HandleFunc("GET /v1/events", s.listEvents)
	mux.HandleFunc("GET /v1/events/{id}", s.getEvent)
	mux.HandleFunc("GET /v1/errors", s.listErrors)
	mux.HandleFunc("GET /v1/errors/{id}", s.getError)
	mux.HandleFunc("GET /v1/errors/{id}/notices", s.listNotices)
	mux.HandleFunc("POST /v1/errors/{id}/ack", s.ackError)
	mux.HandleFunc("POST /v1/errors/{id}/suppress", s.suppressError)
	mux.HandleFunc("POST /v1/errors/{id}/unsuppress", s.unsuppressError)
	mux.HandleFunc("POST /v1/bootstrap/exchange", s.exchangeBootstrap)
	mux.HandleFunc("GET /v1/projects", s.listProjects)
	mux.HandleFunc("POST /v1/projects", s.createProject)
	mux.HandleFunc("GET /v1/projects/{id}", s.getProject)
	mux.HandleFunc("PATCH /v1/projects/{id}", s.updateProject)
	mux.HandleFunc("POST /v1/projects/{id}/archive", s.archiveProject)
	mux.HandleFunc("POST /v1/projects/{id}/restore", s.restoreProject)
	mux.HandleFunc("POST /v1/projects/{id}/ingestion-tokens", s.createProjectToken)
	mux.HandleFunc("DELETE /v1/projects/{id}/ingestion-tokens/{token_id}", s.revokeProjectToken)
	mux.HandleFunc("GET /v1/github/repositories", s.listGitHubRepositories)
	mux.HandleFunc("PUT /v1/projects/{id}/github-repository", s.bindGitHubRepository)
	mux.HandleFunc("DELETE /v1/projects/{id}/github-repository", s.unbindGitHubRepository)
	mux.HandleFunc("GET /v1/webhook-endpoints", s.listWebhookEndpoints)
	mux.HandleFunc("POST /v1/webhook-endpoints", s.createWebhookEndpoint)
	mux.HandleFunc("PATCH /v1/webhook-endpoints/{id}", s.updateWebhookEndpoint)
	mux.HandleFunc("DELETE /v1/webhook-endpoints/{id}", s.deleteWebhookEndpoint)
	mux.HandleFunc("POST /v1/webhook-endpoints/{id}/rotate-secret", s.rotateWebhookSecret)
	mux.HandleFunc("POST /v1/webhook-endpoints/{id}/test", s.testWebhookEndpoint)
	mux.HandleFunc("GET /v1/webhook-deliveries", s.listWebhookDeliveries)
	mux.HandleFunc("POST /v1/webhook-deliveries/{id}/retry", s.retryWebhookDelivery)
	mux.HandleFunc("GET /auth/github", s.beginGitHubAuth)
	mux.HandleFunc("GET /auth/github/callback", s.finishGitHubAuth)
	mux.HandleFunc("GET /settings", s.settings)
	mux.HandleFunc("GET /errors/{id}", s.errorDetail)
	mux.HandleFunc("POST /errors/{id}/suppress", s.browserSuppressError)
	mux.HandleFunc("POST /errors/{id}/unsuppress", s.browserUnsuppressError)
	mux.HandleFunc("POST /settings/bootstrap", s.createBootstrap)
	mux.HandleFunc("POST /settings/agent-keys", s.createBrowserAgentKey)
	mux.HandleFunc("POST /settings/agent-keys/{id}/revoke", s.revokeBrowserAgentKey)
	mux.HandleFunc("POST /settings/issue-jobs/{id}/retry", s.retryBrowserIssueJob)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /github/setup", s.githubSetup)
	mux.HandleFunc("POST /webhooks/github", s.githubWebhook)
	s.handler = requestLogger(logger, recoverPanics(logger, config.OperationalReporter, mux))
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) root(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	body, err := web.FS.ReadFile("index.html")
	if err != nil {
		http.Error(w, "missing index", http.StatusInternalServerError)
		return
	}
	tmpl, err := template.New("index").Parse(string(body))
	if err != nil {
		http.Error(w, "invalid index", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := tmpl.Execute(w, struct{ StylePath string }{StylePath: s.stylePath}); err != nil {
		s.logger.Error("render index", "error", err)
	}
}

func embeddedStylesheetPath() string {
	body, err := web.FS.ReadFile("style.css")
	if err != nil {
		return "/assets/style.css"
	}
	sum := sha256.Sum256(body)
	return fmt.Sprintf("/assets/style-%x.css", sum[:8])
}

func (s *Server) stylesheet(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/style.css" && r.URL.Path != s.stylePath {
		http.NotFound(w, r)
		return
	}
	body, err := web.FS.ReadFile("style.css")
	if err != nil {
		http.Error(w, "missing stylesheet", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	if r.URL.Path == s.stylePath {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	_, _ = w.Write(body)
}

func (s *Server) appIcon(w http.ResponseWriter, r *http.Request) {
	body, err := web.FS.ReadFile("app-icon.png")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	_, _ = w.Write(body)
}

func (s *Server) llmsText(w http.ResponseWriter, _ *http.Request) {
	s.serveTextAsset(w, "llms.txt", "text/plain; charset=utf-8")
}
func (s *Server) skillText(w http.ResponseWriter, _ *http.Request) {
	s.serveTextAsset(w, "skill.md", "text/markdown; charset=utf-8")
}
func (s *Server) migrationText(w http.ResponseWriter, _ *http.Request) {
	s.serveTextAsset(w, "migration.md", "text/markdown; charset=utf-8")
}
func (s *Server) serveTextAsset(w http.ResponseWriter, name, contentType string) {
	body, err := web.FS.ReadFile(name)
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(body)
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), time.Second)
	defer cancel()
	if err := s.store.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type noticeRequest struct {
	App      string         `json:"app"`
	Env      string         `json:"env"`
	Revision string         `json:"revision"`
	Handled  bool           `json:"handled"`
	Error    noticeError    `json:"error"`
	Context  map[string]any `json:"context"`
	Request  map[string]any `json:"request"`
}

type noticeError struct {
	Class     string   `json:"class"`
	Message   string   `json:"message"`
	Backtrace []string `json:"backtrace"`
}

func (s *Server) createNotice(w http.ResponseWriter, r *http.Request) {
	token, ok := bearer(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing token"})
		return
	}
	if !s.allowRequest("ingest:"+inbox.HashToken(token), 120) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.bodyLimit))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "request body too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not read request body"})
		return
	}
	var req noticeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	if req.Error.Class == "" && req.Error.Message == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "error is required"})
		return
	}
	result, err := s.store.Ingest(r.Context(), token, inbox.Notice{
		App:       req.App,
		Env:       req.Env,
		Revision:  req.Revision,
		Handled:   req.Handled,
		Class:     req.Error.Class,
		Message:   req.Error.Message,
		Backtrace: req.Error.Backtrace,
		Context:   req.Context,
		Request:   req.Request,
	})
	if errors.Is(err, inbox.ErrUnauthorized) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
		return
	}
	if errors.Is(err, inbox.ErrLimit) {
		w.Header().Set("Retry-After", "3600")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "account error-group limit reached"})
		return
	}
	if err != nil {
		s.logger.Error("ingest notice", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not store notice"})
		return
	}
	status := http.StatusCreated
	if !result.Created {
		status = http.StatusOK
	}
	writeJSON(w, status, result)
}

func (s *Server) listErrors(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	unacked := r.URL.Query().Get("unacked") == "1" || r.URL.Query().Get("unacked") == "true"
	groups, err := s.store.ListForUser(r.Context(), userID, r.URL.Query().Get("app"), unacked)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not list errors"})
		return
	}
	if groups == nil {
		groups = []inbox.Group{}
	}
	writeJSON(w, http.StatusOK, groups)
}

func (s *Server) allowRequest(key string, limit int) bool {
	s.limitMu.Lock()
	defer s.limitMu.Unlock()
	now := time.Now()
	window := s.limits[key]
	if window.Started.IsZero() || now.Sub(window.Started) >= time.Minute {
		window = requestWindow{Started: now}
	}
	if window.Count >= limit {
		return false
	}
	window.Count++
	s.limits[key] = window
	if len(s.limits) > 10000 {
		for k, v := range s.limits {
			if now.Sub(v.Started) > 2*time.Minute {
				delete(s.limits, k)
			}
		}
	}
	return true
}

func (s *Server) getError(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	group, err := s.store.GetForUser(r.Context(), userID, id)
	if errors.Is(err, inbox.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not load error"})
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) listNotices(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	notices, err := s.store.NoticesForUser(r.Context(), userID, id, limit)
	if errors.Is(err, inbox.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not load notices"})
		return
	}
	if notices == nil {
		notices = []inbox.Notice{}
	}
	writeJSON(w, http.StatusOK, notices)
}

func (s *Server) ackError(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	group, err := s.store.AckForUser(r.Context(), userID, id)
	if errors.Is(err, inbox.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not ack error"})
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) suppressError(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	group, err := s.store.SuppressForUser(r.Context(), userID, id)
	if errors.Is(err, inbox.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not suppress error"})
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) unsuppressError(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid id"})
		return
	}
	group, err := s.store.UnsuppressForUser(r.Context(), userID, id)
	if errors.Is(err, inbox.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not unsuppress error"})
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) agentUser(w http.ResponseWriter, r *http.Request) (int64, bool) {
	token, ok := bearer(r)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
		return 0, false
	}
	if !s.allowRequest("agent:"+inbox.HashToken(token), 60) {
		w.Header().Set("Retry-After", "60")
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return 0, false
	}
	if s.agentToken != "" && token == s.agentToken {
		return s.config.LegacyOwnerID, true
	}
	userID, err := s.store.AuthenticateAgent(r.Context(), token)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
		return 0, false
	}
	return userID, true
}

func bearer(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	return token, token != ""
}

func parseID(raw string) (int64, error) {
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("invalid id")
	}
	return id, nil
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func requestLogger(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("request", "method", r.Method, "path", r.URL.Path, "duration_ms", time.Since(started).Milliseconds())
	})
}

func recoverPanics(logger *slog.Logger, reporter OperationalReporter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				details := fmt.Sprintf("Method: %s\nPath: %s\nPanic: %v\n\n%s", r.Method, r.URL.Path, recovered, debug.Stack())
				logger.Error("HTTP panic", "method", r.Method, "path", r.URL.Path, "panic", recovered)
				if reporter != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if err := reporter.Report(ctx, "http-panic:"+r.Method+":"+r.URL.Path, "HTTP panic", details); err != nil {
						logger.Error("send operational alert", "error", err)
					}
				}
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func ListenAndServe(ctx context.Context, address string, handler http.Handler, logger *slog.Logger) error {
	server := &http.Server{
		Addr:              address,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		logger.Info("HTTP server listening", "address", address)
		errCh <- server.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown HTTP server: %w", err)
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
