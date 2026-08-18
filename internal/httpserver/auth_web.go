package httpserver

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/raisestracker/raises/internal/github"
	"github.com/raisestracker/raises/internal/inbox"
	"github.com/raisestracker/raises/web"
)

const sessionCookie = "raises_session"

type settingsData struct {
	User            inbox.User
	CSRF            string
	Keys            []inbox.AgentKey
	Installations   []settingsInstallation
	InstallURL      string
	StylePath       string
	BootstrapPrompt string
	NewAgentToken   string
	Delivery        settingsDeliveryHealth
	Webhook         inbox.WebhookDeliveryHealth
	RecentErrors    []inbox.Group
}

type settingsDeliveryHealth struct {
	Healthy     bool
	Retrying    int
	Dead        int
	OldestLabel string
	Problems    []inbox.IssueDelivery
}

type settingsInstallation struct {
	inbox.GitHubInstallation
	Repositories []inbox.GitHubRepository
	ManageURL    string
}

type errorDetailData struct {
	User      inbox.User
	CSRF      string
	StylePath string
	Group     inbox.Group
	Notices   []inbox.Notice
}

func (s *Server) beginGitHubAuth(w http.ResponseWriter, r *http.Request) {
	if s.config.GitHubApp == nil {
		http.Error(w, "GitHub sign-in is not configured", http.StatusServiceUnavailable)
		return
	}
	state, err := github.RandomOAuthValue()
	if err != nil {
		http.Error(w, "could not begin sign-in", 500)
		return
	}
	verifier, err := github.RandomOAuthValue()
	if err != nil {
		http.Error(w, "could not begin sign-in", 500)
		return
	}
	purpose := r.URL.Query().Get("purpose")
	if purpose != "install" {
		purpose = "login"
	}
	s.setShortCookie(w, "raises_oauth_state", state)
	s.setShortCookie(w, "raises_oauth_verifier", verifier)
	s.setShortCookie(w, "raises_oauth_purpose", purpose)
	callback := strings.TrimRight(s.config.BaseURL, "/") + "/auth/github/callback"
	http.Redirect(w, r, s.config.GitHubApp.OAuthURL(callback, state, github.PKCEChallenge(verifier)), http.StatusFound)
}

func (s *Server) finishGitHubAuth(w http.ResponseWriter, r *http.Request) {
	state, _ := r.Cookie("raises_oauth_state")
	verifier, _ := r.Cookie("raises_oauth_verifier")
	purpose, _ := r.Cookie("raises_oauth_purpose")
	if state == nil || verifier == nil || purpose == nil || !github.ConstantTimeEqual(state.Value, r.URL.Query().Get("state")) {
		http.Error(w, "invalid OAuth state", http.StatusBadRequest)
		return
	}
	callback := strings.TrimRight(s.config.BaseURL, "/") + "/auth/github/callback"
	token, err := s.config.GitHubApp.ExchangeCode(r.Context(), r.URL.Query().Get("code"), callback, verifier.Value)
	if err != nil {
		s.logger.Error("github oauth exchange", "error", err)
		http.Error(w, "GitHub sign-in failed", http.StatusBadGateway)
		return
	}
	ghUser, err := s.config.GitHubApp.CurrentUser(r.Context(), token)
	if err != nil {
		s.logger.Error("github user", "error", err)
		http.Error(w, "GitHub sign-in failed", http.StatusBadGateway)
		return
	}
	user, err := s.store.UpsertGitHubUser(r.Context(), ghUser.ID, ghUser.Login, ghUser.Name, ghUser.AvatarURL)
	if err != nil {
		http.Error(w, "could not save account", 500)
		return
	}
	if ghUser.ID == s.config.InitialOwnerGitHubID {
		_ = s.store.AssignLegacyProjects(r.Context(), user.ID)
	}
	if purpose.Value == "install" {
		if err := s.finishInstallation(r, token, user.ID); err != nil {
			s.logger.Error("finish github installation", "error", err)
			http.Error(w, "could not connect GitHub installation", http.StatusBadRequest)
			return
		}
	}
	sessionToken, _, err := s.store.CreateSession(r.Context(), user.ID, 30*24*time.Hour)
	if err != nil {
		http.Error(w, "could not create session", 500)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: sessionToken, Path: "/", HttpOnly: true, Secure: s.config.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: int((30 * 24 * time.Hour).Seconds())})
	s.clearCookie(w, "raises_oauth_state")
	s.clearCookie(w, "raises_oauth_verifier")
	s.clearCookie(w, "raises_oauth_purpose")
	s.clearCookie(w, "raises_pending_installation")
	http.Redirect(w, r, "/settings", http.StatusFound)
}

func (s *Server) finishInstallation(r *http.Request, userToken string, userID int64) error {
	pending, err := r.Cookie("raises_pending_installation")
	if err != nil {
		return fmt.Errorf("missing installation")
	}
	installationID, err := strconv.ParseInt(pending.Value, 10, 64)
	if err != nil {
		return err
	}
	accessible, err := s.config.GitHubApp.UserInstallations(r.Context(), userToken)
	if err != nil {
		return err
	}
	allowed := false
	for _, item := range accessible {
		if item.ID == installationID {
			allowed = true
			break
		}
	}
	if !allowed {
		return fmt.Errorf("installation is not accessible")
	}
	return s.syncInstallation(r, userID, installationID)
}

func (s *Server) syncInstallation(r *http.Request, userID, installationID int64) error {
	installation, err := s.config.GitHubApp.Installation(r.Context(), installationID)
	if err != nil {
		return err
	}
	repos, err := s.config.GitHubApp.Repositories(r.Context(), installationID)
	if err != nil {
		return err
	}
	mapped := make([]inbox.GitHubRepository, 0, len(repos))
	for _, repo := range repos {
		mapped = append(mapped, inbox.GitHubRepository{ID: repo.ID, InstallationID: installationID, FullName: repo.FullName})
	}
	status := "active"
	if installation.SuspendedAt != nil {
		status = "suspended"
	}
	return s.store.UpsertInstallation(r.Context(), userID, installationID, installation.Account.Login, installation.TargetType, installation.RepositorySelection, status, mapped)
}

func (s *Server) githubSetup(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.browserSession(w, r); !ok {
		return
	}
	id := r.URL.Query().Get("installation_id")
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		http.Error(w, "invalid installation", http.StatusBadRequest)
		return
	}
	s.setShortCookie(w, "raises_pending_installation", id)
	http.Redirect(w, r, "/auth/github?purpose=install", http.StatusFound)
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserSession(w, r)
	if !ok {
		return
	}
	s.renderSettingsRequest(w, r, session, "", "")
}

// renderSettingsRequest keeps request context available while rendering store-backed data.
func (s *Server) renderSettingsRequest(w http.ResponseWriter, r *http.Request, session inbox.Session, prompt, newToken string) {
	keys, err := s.store.ListAgentKeys(r.Context(), session.User.ID)
	if err != nil {
		http.Error(w, "could not load keys", 500)
		return
	}
	installations, err := s.store.ListInstallations(r.Context(), session.User.ID)
	if err != nil {
		http.Error(w, "could not load GitHub connections", 500)
		return
	}
	repositories, err := s.store.ListGitHubRepositories(r.Context(), session.User.ID)
	if err != nil {
		http.Error(w, "could not load GitHub repositories", 500)
		return
	}
	delivery, err := s.store.IssueDeliveryHealthForUser(r.Context(), session.User.ID)
	if err != nil {
		http.Error(w, "could not load issue delivery health", 500)
		return
	}
	deliveryView := settingsDeliveryHealth{
		Healthy:  delivery.Retrying == 0 && delivery.Dead == 0,
		Retrying: delivery.Retrying,
		Dead:     delivery.Dead,
		Problems: delivery.Problems,
	}
	if delivery.Oldest != nil {
		deliveryView.OldestLabel = ageLabel(s.storeNow().Sub(*delivery.Oldest))
	}
	webhookHealth, err := s.store.WebhookDeliveryHealthForUser(r.Context(), session.User.ID)
	if err != nil {
		http.Error(w, "could not load webhook delivery health", 500)
		return
	}
	recentErrors, err := s.store.ListForUser(r.Context(), session.User.ID, "", false)
	if err != nil {
		http.Error(w, "could not load recent errors", 500)
		return
	}
	if len(recentErrors) > 5 {
		recentErrors = recentErrors[:5]
	}
	installationViews := make([]settingsInstallation, 0, len(installations))
	for _, installation := range installations {
		view := settingsInstallation{
			GitHubInstallation: installation,
			ManageURL:          fmt.Sprintf("https://github.com/settings/installations/%d", installation.ID),
		}
		for _, repository := range repositories {
			if repository.InstallationID == installation.ID {
				view.Repositories = append(view.Repositories, repository)
			}
		}
		installationViews = append(installationViews, view)
	}
	body, err := web.FS.ReadFile("settings.html")
	if err != nil {
		http.Error(w, "missing settings template", 500)
		return
	}
	tmpl, err := template.New("settings").Parse(string(body))
	if err != nil {
		http.Error(w, "invalid settings template", 500)
		return
	}
	data := settingsData{User: session.User, CSRF: session.CSRFToken, Keys: keys, Installations: installationViews, InstallURL: "https://github.com/apps/" + url.PathEscape(s.config.GitHubAppSlug) + "/installations/new", StylePath: s.stylePath, BootstrapPrompt: prompt, NewAgentToken: newToken, Delivery: deliveryView, Webhook: webhookHealth, RecentErrors: recentErrors}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		s.logger.Error("render settings", "error", err)
	}
}

func (s *Server) createBootstrap(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserPOST(w, r)
	if !ok {
		return
	}
	token, err := s.store.CreateBootstrapToken(r.Context(), session.User.ID, "Bootstrap agent", 10*time.Minute)
	if err != nil {
		http.Error(w, "could not create bootstrap", 500)
		return
	}
	prompt := fmt.Sprintf("Set up Raises for me. Read %s/llms.txt and authenticate once with bootstrap token %s. Store the returned agent key securely and never print it again.", strings.TrimRight(s.config.BaseURL, "/"), token)
	s.renderSettingsRequest(w, r, session, prompt, "")
}

func (s *Server) createBrowserAgentKey(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserPOST(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", 400)
		return
	}
	token, _, err := s.store.CreateAgentKey(r.Context(), session.User.ID, r.FormValue("name"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	s.renderSettingsRequest(w, r, session, "", token)
}

func (s *Server) revokeBrowserAgentKey(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserPOST(w, r)
	if !ok {
		return
	}
	if err := s.store.RevokeAgentKey(r.Context(), session.User.ID, r.PathValue("id")); err != nil {
		s.writeStoreError(w, err)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) retryBrowserIssueJob(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserPOST(w, r)
	if !ok {
		return
	}
	jobID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || jobID < 1 {
		http.NotFound(w, r)
		return
	}
	if err := s.store.RetryIssueJobForUser(r.Context(), session.User.ID, jobID); err != nil {
		s.writeStoreError(w, err)
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

func (s *Server) errorDetail(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserSession(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	group, err := s.store.GetForUser(r.Context(), session.User.ID, id)
	if errors.Is(err, inbox.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not load error", 500)
		return
	}
	notices, err := s.store.NoticesForUser(r.Context(), session.User.ID, id, 5)
	if err != nil {
		http.Error(w, "could not load notices", 500)
		return
	}
	body, err := web.FS.ReadFile("error.html")
	if err != nil {
		http.Error(w, "missing error template", 500)
		return
	}
	tmpl, err := template.New("error").Parse(string(body))
	if err != nil {
		http.Error(w, "invalid error template", 500)
		return
	}
	data := errorDetailData{User: session.User, CSRF: session.CSRFToken, StylePath: s.stylePath, Group: group, Notices: notices}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		s.logger.Error("render error detail", "error", err)
	}
}

func (s *Server) browserSuppressError(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserPOST(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.SuppressForUser(r.Context(), session.User.ID, id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/errors/%d", id), http.StatusSeeOther)
}

func (s *Server) browserUnsuppressError(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserPOST(w, r)
	if !ok {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		http.NotFound(w, r)
		return
	}
	if _, err := s.store.UnsuppressForUser(r.Context(), session.User.ID, id); err != nil {
		s.writeStoreError(w, err)
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/errors/%d", id), http.StatusSeeOther)
}

func (s *Server) storeNow() time.Time { return time.Now().UTC() }

func ageLabel(age time.Duration) string {
	if age < time.Minute {
		return "just now"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm", int(age.Minutes()))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh", int(age.Hours()))
	}
	return fmt.Sprintf("%dd", int(age.Hours()/24))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserPOST(w, r)
	if !ok {
		return
	}
	cookie, _ := r.Cookie(sessionCookie)
	if cookie != nil {
		_ = s.store.DeleteSession(r.Context(), cookie.Value)
	}
	s.clearCookie(w, sessionCookie)
	_ = session
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) browserSession(w http.ResponseWriter, r *http.Request) (inbox.Session, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		http.Redirect(w, r, "/auth/github", http.StatusFound)
		return inbox.Session{}, false
	}
	session, err := s.store.SessionByToken(r.Context(), cookie.Value)
	if err != nil {
		s.clearCookie(w, sessionCookie)
		http.Redirect(w, r, "/auth/github", http.StatusFound)
		return inbox.Session{}, false
	}
	return session, true
}

func (s *Server) browserPOST(w http.ResponseWriter, r *http.Request) (inbox.Session, bool) {
	session, ok := s.browserSession(w, r)
	if !ok {
		return inbox.Session{}, false
	}
	if err := r.ParseForm(); err != nil || !github.ConstantTimeEqual(session.CSRFToken, r.FormValue("csrf")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return inbox.Session{}, false
	}
	return session, true
}

func (s *Server) setShortCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: s.config.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: 600})
}
func (s *Server) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", HttpOnly: true, Secure: s.config.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

func (s *Server) githubWebhook(w http.ResponseWriter, r *http.Request) {
	if s.config.GitHubWebhookSecret == "" {
		http.Error(w, "webhook unavailable", 503)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid payload", 400)
		return
	}
	mac := hmac.New(sha256.New, []byte(s.config.GitHubWebhookSecret))
	_, _ = mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(r.Header.Get("X-Hub-Signature-256"))) {
		http.Error(w, "invalid signature", 401)
		return
	}
	var payload struct {
		Action       string `json:"action"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "installation" && (payload.Action == "deleted" || payload.Action == "suspend") {
		status := "deleted"
		if payload.Action == "suspend" {
			status = "suspended"
		}
		_ = s.store.SetInstallationStatus(r.Context(), payload.Installation.ID, status)
	} else if event == "installation" || event == "installation_repositories" {
		if userID, err := s.store.InstallationUserID(r.Context(), payload.Installation.ID); err == nil {
			_ = s.syncInstallation(r, userID, payload.Installation.ID)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}
