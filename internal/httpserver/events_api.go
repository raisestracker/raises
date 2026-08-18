package httpserver

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
	"github.com/raisestracker/raises/internal/outbound"
)

func (s *Server) createEvent(w http.ResponseWriter, r *http.Request) {
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
	var req inbox.EventInput
	if !decodeJSON(w, r, &req) {
		return
	}
	event, err := s.store.CreateEvent(r.Context(), token, req)
	if errors.Is(err, inbox.ErrUnauthorized) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid token"})
		return
	}
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, event)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	var since time.Time
	if raw := r.URL.Query().Get("since"); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since must be RFC3339"})
			return
		}
		since = parsed
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	events, err := s.store.ListEventsForUser(r.Context(), userID, r.URL.Query().Get("project"), r.URL.Query().Get("level"), since, limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if events == nil {
		events = []inbox.Event{}
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) getEvent(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	event, err := s.store.GetEventForUser(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, event)
}

func (s *Server) listWebhookEndpoints(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	endpoints, err := s.store.ListWebhookEndpoints(r.Context(), userID)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if endpoints == nil {
		endpoints = []inbox.WebhookEndpoint{}
	}
	writeJSON(w, http.StatusOK, endpoints)
}

func (s *Server) createWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := outbound.ValidateURL(req.URL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	endpoint, secret, err := s.store.CreateWebhookEndpoint(r.Context(), userID, req.URL, req.Events)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"endpoint": endpoint, "signing_secret": secret})
}

func (s *Server) updateWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	var req struct {
		URL    string   `json:"url"`
		Events []string `json:"events"`
		Active bool     `json:"active"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := outbound.ValidateURL(req.URL); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	endpoint, err := s.store.UpdateWebhookEndpoint(r.Context(), userID, r.PathValue("id"), req.URL, req.Events, req.Active)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, endpoint)
}

func (s *Server) deleteWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteWebhookEndpoint(r.Context(), userID, r.PathValue("id")); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) rotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	secret, err := s.store.RotateWebhookSecret(r.Context(), userID, r.PathValue("id"))
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"signing_secret": secret})
}

func (s *Server) testWebhookEndpoint(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	if err := s.store.EnqueueWebhookTest(r.Context(), userID, r.PathValue("id")); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) listWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	deliveries, err := s.store.ListWebhookDeliveriesForUser(r.Context(), userID, r.URL.Query().Get("state"), limit)
	if err != nil {
		s.writeStoreError(w, err)
		return
	}
	if deliveries == nil {
		deliveries = []inbox.WebhookDelivery{}
	}
	writeJSON(w, http.StatusOK, deliveries)
}

func (s *Server) retryWebhookDelivery(w http.ResponseWriter, r *http.Request) {
	userID, ok := s.agentUser(w, r)
	if !ok {
		return
	}
	if err := s.store.RetryWebhookDeliveryForUser(r.Context(), userID, r.PathValue("id")); err != nil {
		s.writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}
