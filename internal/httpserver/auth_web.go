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
	Nav             navData
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
	Nav       navData
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
			if errors.Is(err, inbox.ErrInstallationTaken) {
				http.Error(w, "installation is already linked to another account", http.StatusConflict)
				return
			}
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
	http.Redirect(w, r, "/dashboard", http.StatusFound)
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
	return s.syncInstallation(r, userID, installationID, userToken)
}

func (s *Server) syncInstallation(r *http.Request, userID, installationID int64, userToken string) error {
	installation, err := s.config.GitHubApp.Installation(r.Context(), installationID)
	if err != nil {
		return err
	}
	var repos []github.Repository
	if userToken != "" {
		// User-initiated link: list only repos the linker can read.
		repos, err = s.config.GitHubApp.UserInstallationRepositories(r.Context(), userToken, installationID)
	} else {
		// Webhook resync has no user token; refresh names from the full list.
		repos, err = s.config.GitHubApp.Repositories(r.Context(), installationID)
	}
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

// githubSetup receives GitHub's post-install redirect. GET renders a
// confirmation so the link step itself stays a CSRF-protected POST.
func (s *Server) githubSetup(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserSession(w, r)
	if !ok {
		return
	}
	id := r.URL.Query().Get("installation_id")
	if _, err := strconv.ParseInt(id, 10, 64); err != nil {
		http.Error(w, "invalid installation", http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><body><h1>Connect GitHub installation %s?</h1><form method="post" action="/github/setup"><input type="hidden" name="csrf" value="%s"><input type="hidden" name="installation_id" value="%s"><button type="submit">Connect</button></form></body></html>`, template.HTMLEscapeString(id), template.HTMLEscapeString(session.CSRFToken), template.HTMLEscapeString(id))
}

// githubSetupPost commits the pending installation link. CSRF-checked.
func (s *Server) githubSetupPost(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.browserPOST(w, r); !ok {
		return
	}
	id := r.FormValue("installation_id")
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
	data := settingsData{User: session.User, CSRF: session.CSRFToken, Keys: keys, Installations: installationViews, InstallURL: "https://github.com/apps/" + url.PathEscape(s.config.GitHubAppSlug) + "/installations/new", StylePath: s.stylePath, BootstrapPrompt: prompt, NewAgentToken: newToken, Delivery: deliveryView, Webhook: webhookHealth, Nav: s.nav(session, "settings")}
	s.renderPage(w, "settings.html", data)
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
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusCreated, map[string]any{"prompt": prompt})
		return
	}
	s.renderSettingsRequest(w, r, session, prompt, "")
}

func (s *Server) createBrowserAgentKey(w http.ResponseWriter, r *http.Request) {
	wantJSON := strings.Contains(r.Header.Get("Accept"), "application/json")
	session, ok := s.browserKeySession(w, r, wantJSON)
	if !ok {
		return
	}
	// ParseMultipartForm covers both urlencoded and multipart (fetch FormData)
	// bodies. ParseForm alone would leave multipart fields unparsed, and a
	// later FormValue would then miss them.
	if err := r.ParseMultipartForm(10 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) {
		if wantJSON {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid form"})
		} else {
			http.Error(w, "invalid form", 400)
		}
		return
	}
	var (
		token string
		key   inbox.AgentKey
		err   error
	)
	if r.FormValue("account_key") != "" {
		token, key, err = s.store.CreateAccountKey(r.Context(), session.User.ID, r.FormValue("name"))
	} else {
		token, key, err = s.store.CreateAgentKey(r.Context(), session.User.ID, r.FormValue("name"))
	}
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		writeJSON(w, http.StatusCreated, map[string]any{"token": token, "key": key})
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
	data := errorDetailData{Nav: s.nav(session, "dashboard"), User: session.User, CSRF: session.CSRFToken, StylePath: s.stylePath, Group: group, Notices: notices}
	s.renderPage(w, "error.html", data)
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
	// ParseMultipartForm covers both urlencoded and multipart (fetch FormData)
	// bodies. ParseForm alone would leave multipart fields unparsed, and the
	// FormValue below would then miss the CSRF token.
	if err := r.ParseMultipartForm(10 << 20); err != nil && !errors.Is(err, http.ErrNotMultipart) || !github.ConstantTimeEqual(session.CSRFToken, r.FormValue("csrf")) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return inbox.Session{}, false
	}
	return session, true
}

// browserKeySession authenticates the settings agent-key form. Clients that
// asked for JSON (the settings fetch client) get JSON errors instead of a
// redirect to an HTML page or a plain-text body, so response.json() always
// parses and the real status surfaces. Plain form posts keep the old behavior.
func (s *Server) browserKeySession(w http.ResponseWriter, r *http.Request, wantJSON bool) (inbox.Session, bool) {
	if !wantJSON {
		return s.browserPOST(w, r)
	}
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in again to create a key"})
		return inbox.Session{}, false
	}
	session, err := s.store.SessionByToken(r.Context(), cookie.Value)
	if err != nil {
		s.clearCookie(w, sessionCookie)
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "sign in again to create a key"})
		return inbox.Session{}, false
	}
	if ferr := r.ParseMultipartForm(10 << 20); ferr != nil && !errors.Is(ferr, http.ErrNotMultipart) || !github.ConstantTimeEqual(session.CSRFToken, r.FormValue("csrf")) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "could not verify this browser session; reload the page and try again"})
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
		Issue struct {
			Number int `json:"number"`
		} `json:"issue"`
		Repository struct {
			ID       int64  `json:"id"`
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, "invalid json", 400)
		return
	}
	event := r.Header.Get("X-GitHub-Event")
	if event == "issues" && (payload.Action == "closed" || payload.Action == "reopened") {
		// A closed GitHub issue means the error was fixed; reopening undoes it.
		// Match on repository plus installation IDs so a renamed repo, or a
		// different repo reusing the old full_name, never resolves foreign groups.
		var err error
		if payload.Action == "closed" {
			_, err = s.store.ResolveByIssue(r.Context(), payload.Repository.ID, payload.Installation.ID, payload.Issue.Number)
		} else {
			_, err = s.store.ReopenByIssue(r.Context(), payload.Repository.ID, payload.Installation.ID, payload.Issue.Number)
		}
		if err != nil {
			s.logger.Error("apply github issue state", "error", err)
			http.Error(w, "could not apply issue state", 500)
			return
		}
	} else if event == "installation" && (payload.Action == "deleted" || payload.Action == "suspend") {
		status := "deleted"
		if payload.Action == "suspend" {
			status = "suspended"
		}
		_ = s.store.SetInstallationStatus(r.Context(), payload.Installation.ID, status)
	} else if event == "installation" || event == "installation_repositories" {
		if userID, err := s.store.InstallationUserID(r.Context(), payload.Installation.ID); err == nil {
			_ = s.syncInstallation(r, userID, payload.Installation.ID, "")
		}
	}
	w.WriteHeader(http.StatusAccepted)
}
