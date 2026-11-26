package httpserver

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
	"github.com/raisestracker/raises/web"
)

type navData struct {
	User      inbox.User
	CSRF      string
	Active    string
	StylePath string
}

func (s *Server) nav(session inbox.Session, active string) navData {
	return navData{User: session.User, CSRF: session.CSRFToken, Active: active, StylePath: s.stylePath}
}

type tileData struct {
	Value  int
	Label  string
	Series inbox.Spark
	Tone   string
	Alt    string
}

type snippetData struct {
	ID    string
	Label string
	Body  string
}

func ago(t time.Time) string {
	age := time.Since(t)
	if age < time.Minute {
		return "just now"
	}
	return ageLabel(age) + " ago"
}

// spark renders a 14-day series as an inline SVG that inherits currentColor.
// Only integers are interpolated, so the markup is safe.
func spark(series inbox.Spark) template.HTML {
	if len(series) < 2 {
		return ""
	}
	max := series.Max()
	if max == 0 {
		max = 1
	}
	const w, h = 100.0, 28.0
	var points strings.Builder
	for i, v := range series {
		x := w * float64(i) / float64(len(series)-1)
		y := h - 2 - (h-4)*float64(v)/float64(max)
		fmt.Fprintf(&points, "%.1f,%.1f ", x, y)
	}
	p := strings.TrimSpace(points.String())
	return template.HTML(fmt.Sprintf(`<svg class="size-full overflow-visible" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" aria-hidden="true" focusable="false"><polygon points="0,%.0f %s %.0f,%.0f" fill="currentColor" opacity=".12"/><polyline points="%s" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round" stroke-linecap="round" vector-effect="non-scaling-stroke"/></svg>`, w, h, h, p, w, h, p))
}

func (s *Server) renderPage(w http.ResponseWriter, name string, data any) {
	s.renderPageStatus(w, name, data, http.StatusOK)
}

func (s *Server) renderPageStatus(w http.ResponseWriter, name string, data any, status int) {
	nav, err := web.FS.ReadFile("nav.html")
	if err != nil {
		http.Error(w, "missing template", 500)
		return
	}
	page, err := web.FS.ReadFile(name)
	if err != nil {
		http.Error(w, "missing template", 500)
		return
	}
	tmpl, err := template.New(name).Funcs(template.FuncMap{
		"ago":   ago,
		"spark": spark,
		"snippet": func(id, label, body string) snippetData {
			return snippetData{ID: id, Label: label, Body: body}
		},
		"tile": func(value int, label string, series inbox.Spark, tone string) tileData {
			parts := make([]string, len(series))
			for i, v := range series {
				parts[i] = strconv.Itoa(v)
			}
			return tileData{Value: value, Label: label, Series: series, Tone: tone, Alt: strings.Join(parts, ", ")}
		},
	}).Parse(string(nav))
	if err == nil {
		_, err = tmpl.Parse(string(page))
	}
	if err != nil {
		s.logger.Error("parse template", "name", name, "error", err)
		http.Error(w, "invalid template", 500)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		s.logger.Error("render page", "name", name, "error", err)
	}
}

// optionalSession returns a valid browser session without redirecting.
func (s *Server) optionalSession(r *http.Request) (inbox.Session, bool) {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return inbox.Session{}, false
	}
	session, err := s.store.SessionByToken(r.Context(), cookie.Value)
	return session, err == nil
}

type dashboardData struct {
	Nav       navData
	StylePath string
	AppJSPath string
	CSRF      string
	Stats     inbox.DashboardStats
	Recent    []inbox.Group
	Form      dashboardProjectForm
	Reveal    *projectReveal
}

// dashboardProjectForm echoes a rejected self-serve project form back to the
// reader with the server's reason; it never carries a secret.
type dashboardProjectForm struct {
	Name  string
	Slug  string
	Error string
}

// projectReveal is rendered exactly once, in the POST /dashboard/projects
// response (Cache-Control: no-store). The plaintext token is never placed in
// a URL, a redirect, a cookie or any later GET.
type projectReveal struct {
	Project inbox.Project
	Token   string
	Curl    string
	Rails   string
	Go      string
	JS      string
}

const defaultPublicBaseURL = "https://raises.dev"

func (s *Server) publicBaseURL(r *http.Request) string {
	if base := strings.TrimRight(strings.TrimSpace(s.config.BaseURL), "/"); base != "" {
		return base
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func quickstartSnippets(base, token string) (curl, rails, goSnippet, js string) {
	urlLine := ""
	if base != defaultPublicBaseURL {
		urlLine = "RAISES_URL=" + base + "\n"
	}
	curl = "curl -sS -X POST " + base + "/v1/notices \\\n" +
		"  -H \"Authorization: Bearer " + token + "\" \\\n" +
		"  -H \"Content-Type: application/json\" \\\n" +
		`  -d '{"env":"production","error":{"class":"RaisesTestError","message":"Hello from Raises","backtrace":["app/hello.rb:1"]}}'`
	rails = "bundle add raises --version \"~> 0.3\"\n\n" +
		"# Store in your secret manager / environment:\n" +
		"RAISES_TOKEN=" + token + "\n" + urlLine
	goSnippet = "go get github.com/raisestracker/raises-go@v0.1.0\n\n" +
		"# Store in your secret manager / environment:\n" +
		"RAISES_TOKEN=" + token + "\n" + urlLine
	js = "// RAISES_TOKEN=" + token + "\n" +
		"export async function reportError(err) {\n" +
		"  await fetch(\"" + base + "/v1/notices\", {\n" +
		"    method: \"POST\",\n" +
		"    headers: {\n" +
		"      Authorization: `Bearer ${process.env.RAISES_TOKEN}`,\n" +
		"      \"Content-Type\": \"application/json\",\n" +
		"    },\n" +
		"    body: JSON.stringify({\n" +
		"      env: process.env.NODE_ENV || \"production\",\n" +
		"      error: {\n" +
		"        class: err.name,\n" +
		"        message: err.message,\n" +
		"        backtrace: String(err.stack || \"\").split(\"\\n\").slice(1).map((l) => l.trim()),\n" +
		"      },\n" +
		"    }),\n" +
		"  });\n" +
		"}\n"
	return curl, rails, goSnippet, js
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserSession(w, r)
	if !ok {
		return
	}
	s.renderDashboard(w, r, session, http.StatusOK, dashboardProjectForm{}, nil)
}

func (s *Server) renderDashboard(w http.ResponseWriter, r *http.Request, session inbox.Session, status int, form dashboardProjectForm, reveal *projectReveal) {
	stats, err := s.store.DashboardForUser(r.Context(), session.User.ID)
	if err != nil {
		s.logger.Error("dashboard stats", "error", err)
		http.Error(w, "could not load dashboard", 500)
		return
	}
	recent, err := s.store.ListPageForUserCompact(r.Context(), session.User.ID, "", inbox.StatusAll, 1, 10, true)
	if err != nil {
		http.Error(w, "could not load recent errors", 500)
		return
	}
	s.renderPageStatus(w, "dashboard.html", dashboardData{Nav: s.nav(session, "dashboard"), StylePath: s.stylePath, AppJSPath: s.appJSPath, CSRF: session.CSRFToken, Stats: stats, Recent: recent.Groups, Form: form, Reveal: reveal}, status)
}

// createDashboardProject is the self-serve browser onboarding path: one
// CSRF-checked form post creates a project and its first ingestion token in
// a single transaction and reveals the token once with quickstart snippets.
// It works identically with or without JavaScript.
func (s *Server) createDashboardProject(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserPOST(w, r)
	if !ok {
		return
	}
	form := dashboardProjectForm{Name: strings.TrimSpace(r.FormValue("name")), Slug: strings.TrimSpace(r.FormValue("slug"))}
	project, token, _, err := s.store.CreateProjectWithToken(r.Context(), session.User.ID, form.Name, form.Slug)
	if err != nil {
		status := http.StatusInternalServerError
		msg := "Could not create the project. Try again."
		switch {
		case strings.Contains(err.Error(), "limit reached"):
			status, msg = http.StatusUnprocessableEntity, "Project limit reached. Archive a project before creating another."
		case strings.Contains(err.Error(), "valid name and slug"):
			status, msg = http.StatusBadRequest, "Enter a project name (up to 100 characters). Slugs use lowercase letters, numbers and single hyphens."
		case strings.Contains(strings.ToLower(err.Error()), "unique"):
			status, msg = http.StatusConflict, "You already have a project with that slug. Pick another."
		default:
			s.logger.Error("create dashboard project", "error", err)
		}
		form.Error = msg
		s.renderDashboard(w, r, session, status, form, nil)
		return
	}
	reveal := &projectReveal{Project: project, Token: token}
	reveal.Curl, reveal.Rails, reveal.Go, reveal.JS = quickstartSnippets(s.publicBaseURL(r), token)
	w.Header().Set("Referrer-Policy", "no-referrer")
	s.renderDashboard(w, r, session, http.StatusCreated, dashboardProjectForm{}, reveal)
}

type filterLink struct {
	Label  string
	URL    string
	Active bool
}

type projectPageData struct {
	Nav       navData
	StylePath string
	Title     string
	Project   *inbox.Project
	ShowApp   bool
	Open      int
	Fixed     int
	Filters   []filterLink
	Page      inbox.ErrorPage
	Prev      string
	Next      string
	Empty     string
}

func pageURL(base, status string, page int) string {
	q := url.Values{}
	if status != inbox.StatusOpen {
		q.Set("status", status)
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if len(q) == 0 {
		return base
	}
	return base + "?" + q.Encode()
}

func (s *Server) projectPage(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserSession(w, r)
	if !ok {
		return
	}
	project, err := s.store.ProjectBySlug(r.Context(), session.User.ID, r.PathValue("slug"))
	if errors.Is(err, inbox.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "could not load project", 500)
		return
	}
	s.renderErrorList(w, r, session, &project)
}

func (s *Server) allErrorsPage(w http.ResponseWriter, r *http.Request) {
	session, ok := s.browserSession(w, r)
	if !ok {
		return
	}
	s.renderErrorList(w, r, session, nil)
}

func (s *Server) renderErrorList(w http.ResponseWriter, r *http.Request, session inbox.Session, project *inbox.Project) {
	status := inbox.NormalizeStatus(r.URL.Query().Get("status"), inbox.StatusOpen)
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	projectKey, base, title := "", "/errors", "All errors"
	active := "errors"
	if project != nil {
		projectKey, base, title = project.ID, "/projects/"+url.PathEscape(project.Slug), project.Name
		active = "dashboard"
	}
	result, err := s.store.ListPageForUserCompact(r.Context(), session.User.ID, projectKey, status, page, inbox.DefaultPageSize, true)
	if err != nil {
		http.Error(w, "could not load errors", 500)
		return
	}
	var open, fixed int
	if project != nil {
		open, fixed, _, err = s.store.ProjectCounts(r.Context(), project.ID)
	} else {
		open, fixed, _, err = s.store.UserErrorCounts(r.Context(), session.User.ID)
	}
	if err != nil {
		http.Error(w, "could not load counts", 500)
		return
	}
	data := projectPageData{Nav: s.nav(session, active), StylePath: s.stylePath, Title: title, Project: project, ShowApp: project == nil, Open: open, Fixed: fixed, Page: result}
	for _, f := range []struct{ label, value string }{{"Open", inbox.StatusOpen}, {"Acknowledged", inbox.StatusAcknowledged}, {"Fixed", inbox.StatusFixed}, {"All", inbox.StatusAll}} {
		data.Filters = append(data.Filters, filterLink{Label: f.label, URL: pageURL(base, f.value, 1), Active: f.value == status})
	}
	if result.Page > 1 {
		data.Prev = pageURL(base, status, result.Page-1)
	}
	if result.Page < result.Pages {
		data.Next = pageURL(base, status, result.Page+1)
	}
	name := title
	if project == nil {
		name = "your projects"
	}
	switch status {
	case inbox.StatusOpen:
		data.Empty = fmt.Sprintf("No open errors in %s. New errors show up here within seconds.", name)
	case inbox.StatusAcknowledged:
		data.Empty = fmt.Sprintf("No acknowledged errors in %s. Errors move here when an agent picks them up.", name)
	case inbox.StatusFixed:
		data.Empty = fmt.Sprintf("Nothing marked fixed in %s yet. Errors move here when their GitHub issue is closed.", name)
	default:
		data.Empty = fmt.Sprintf("No errors in %s yet. New errors show up here within seconds.", name)
	}
	s.renderPage(w, "project.html", data)
}
