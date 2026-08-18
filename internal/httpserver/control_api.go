package httpserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/raisestracker/raises/internal/inbox"
)

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return false
	}
	return true
}

func (s *Server) exchangeBootstrap(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token string `json:"token"`
		Name  string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	token, key, err := s.store.ExchangeBootstrapToken(r.Context(), req.Token, req.Name)
	if errors.Is(err, inbox.ErrUnauthorized) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid or expired bootstrap token"})
		return
	}
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "key": key})
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	projects, err := s.store.ListProjects(r.Context(), userID, r.URL.Query().Get("archived") == "1")
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if projects == nil {
		projects = []inbox.Project{}
	}
	writeJSON(w, http.StatusOK, projects)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	project, err := s.store.CreateProject(r.Context(), userID, req.Name, req.Slug)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, project)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	project, err := s.store.GetProject(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) updateProject(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	project, err := s.store.UpdateProject(r.Context(), userID, r.PathValue("id"), req.Name)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) archiveProject(w http.ResponseWriter, r *http.Request) { s.setArchive(w, r, true) }
func (s *Server) restoreProject(w http.ResponseWriter, r *http.Request) { s.setArchive(w, r, false) }
func (s *Server) setArchive(w http.ResponseWriter, r *http.Request, archived bool) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	project, err := s.store.SetProjectArchived(r.Context(), userID, r.PathValue("id"), archived)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) createProjectToken(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	token, record, err := s.store.CreateProjectToken(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "credential": record})
}

func (s *Server) revokeProjectToken(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	err := s.store.RevokeProjectToken(r.Context(), userID, r.PathValue("id"), r.PathValue("token_id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listGitHubRepositories(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	repos, err := s.store.ListGitHubRepositories(r.Context(), userID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if repos == nil {
		repos = []inbox.GitHubRepository{}
	}
	writeJSON(w, http.StatusOK, repos)
}

func (s *Server) bindGitHubRepository(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		RepositoryID int64 `json:"repository_id"`
	}
	if !decodeJSON(w, r, &req) || req.RepositoryID < 1 {
		if req.RepositoryID < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repository_id is required"})
		}
		return
	}
	project, err := s.store.BindProjectRepository(r.Context(), userID, r.PathValue("id"), req.RepositoryID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) unbindGitHubRepository(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	project, err := s.store.UnbindProjectRepository(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, project)
}

func (s *Server) writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, inbox.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, inbox.ErrUnauthorized):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
	case strings.Contains(err.Error(), "limit reached"):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	case strings.Contains(err.Error(), "required") || strings.Contains(err.Error(), "valid") || strings.Contains(err.Error(), "too long"):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case strings.Contains(err.Error(), "archived"):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
	case strings.Contains(strings.ToLower(err.Error()), "unique"):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "already exists"})
	default:
		s.logger.Error("store request", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request failed"})
	}
}

func int64Query(value string) int64 { v, _ := strconv.ParseInt(value, 10, 64); return v }
