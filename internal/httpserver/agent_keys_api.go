package httpserver

import (
	"errors"
	"net/http"

	"github.com/raisestracker/raises/internal/inbox"
)

// Account keys are owner-level agent keys created once in the settings UI.
// Their token authenticates the agent-key management API below, so one manual
// round-trip provisions every later app key: mint with POST, list with GET,
// revoke with DELETE. Minted keys are ordinary agent keys; revoking the
// account key leaves keys it minted usable.

func (s *Server) accountKeyUser(w http.ResponseWriter, r *http.Request) (userID int64, keyID string, ok bool) {
	token, present := bearer(r)
	if !present {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
		return 0, "", false
	}
	if !s.allowRequest("preauth:agentkeys:"+clientIP(r), preAuthAgentPerIPLimit) {
		tooManyRequests(w)
		return 0, "", false
	}
	userID, keyID, err := s.store.AuthenticateAccountKey(r.Context(), token)
	if err == nil {
		if !s.allowRequest("agentkeys:token:"+inbox.HashToken(token), postAuthAgentPerTokenLimit) {
			tooManyRequests(w)
			return 0, "", false
		}
		return userID, keyID, true
	}
	if !errors.Is(err, inbox.ErrUnauthorized) {
		s.logger.Error("authenticate account key", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request failed"})
		return 0, "", false
	}
	if agentID, agentErr := s.store.AuthenticateAgent(r.Context(), token); agentErr == nil && agentID != 0 {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "account key required"})
		return 0, "", false
	}
	if s.failedAuthLimited(r) {
		tooManyRequests(w)
		return 0, "", false
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
	return 0, "", false
}

func (s *Server) listAgentKeys(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.accountKeyUser(w, r)
	if !ok {
		return
	}
	keys, err := s.store.ListAgentKeys(r.Context(), userID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if keys == nil {
		keys = []inbox.AgentKey{}
	}
	writeJSON(w, http.StatusOK, keys)
}

func (s *Server) mintAgentKey(w http.ResponseWriter, r *http.Request) {
	userID, keyID, ok := s.accountKeyUser(w, r)
	if !ok {
		return
	}
	var req struct {
		Name string `json:"name"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	token, key, err := s.store.MintAgentKey(r.Context(), userID, keyID, req.Name)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"token": token, "key": key})
}

func (s *Server) revokeAgentKey(w http.ResponseWriter, r *http.Request) {
	userID, _, ok := s.accountKeyUser(w, r)
	if !ok {
		return
	}
	if err := s.store.RevokeAgentKey(r.Context(), userID, r.PathValue("id")); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
