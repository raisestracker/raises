// Command raises-acceptance runs the real Raises HTTP server and SQLite store
// for the acceptance suite in acceptance/. It replaces only the edges that
// would reach the network: GitHub OAuth signs in a seeded user without leaving
// the machine, and outbound webhooks are recorded in memory instead of sent.
//
// It is a test harness. Never run it as a production server: anyone who can
// reach it can sign in as the seeded user.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	gh "github.com/raisestracker/raises/internal/github"
	"github.com/raisestracker/raises/internal/httpserver"
	"github.com/raisestracker/raises/internal/inbox"
	"github.com/raisestracker/raises/internal/secretbox"
)

// Seeded accounts. The primary user signs in through the fake GitHub flow;
// the other exists so tests can check that one account cannot see another's data.
var (
	primaryUser = gh.User{ID: 900001, Login: "acceptance-owner", Name: "Acceptance Owner"}
	otherUser   = gh.User{ID: 900002, Login: "acceptance-other", Name: "Acceptance Other"}
)

// A fixed, test-only key so webhook endpoints can be created. It protects nothing.
const webhookKey = "YWNjZXB0YW5jZS1vbmx5LXdlYmhvb2sta2V5LTMyYnk"

func main() {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if err := run(logger); err != nil {
		fmt.Fprintln(os.Stderr, "raises-acceptance:", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	addr := envOr("ACCEPTANCE_ADDR", "127.0.0.1:18080")
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("ACCEPTANCE_ADDR: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("refusing to listen on %s: the acceptance harness signs anyone in and must stay on loopback", addr)
	}
	baseURL := "http://" + addr

	dir := os.Getenv("ACCEPTANCE_DATA_DIR")
	if dir == "" {
		dir, err = os.MkdirTemp("", "raises-acceptance-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
	}
	dbPath := filepath.Join(dir, "raises.db")
	// Every run starts from the same seeded state.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}

	store, err := inbox.Open(dbPath, nil, nil)
	if err != nil {
		return err
	}
	defer store.Close()
	cipher, err := secretbox.New(webhookKey)
	if err != nil {
		return err
	}
	store.SetSecretCipher(cipher)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	owner, err := store.UpsertGitHubUser(ctx, primaryUser.ID, primaryUser.Login, primaryUser.Name, "")
	if err != nil {
		return err
	}
	if _, err := store.UpsertGitHubUser(ctx, otherUser.ID, otherUser.Login, otherUser.Name, ""); err != nil {
		return err
	}
	// Both browser projects share one database, so lift the five-project cap.
	store.ConfigureUnlimitedProjects(owner.ID)

	recorder := &deliveryRecorder{}
	go store.RunOutboundWorker(ctx, recorder, func(err error) { logger.Warn("outbound worker", "error", err) })

	server := httpserver.NewWithConfig(store, logger, 1<<20, httpserver.Config{
		BaseURL:       baseURL,
		GitHubAppSlug: "raises",
		GitHubApp:     fakeGitHub{},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /__acceptance/deliveries", recorder.serve)
	mux.HandleFunc("GET /__acceptance/sign-in", signInAs(store))
	mux.Handle("/", server.Handler())

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "raises-acceptance listening on %s (data %s)\n", baseURL, dir)
	httpServer := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdown)
	}()
	if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// signInAs gives the API suite a browser session for a seeded account without
// a browser. Browser tests use the real /auth/github redirect flow instead.
func signInAs(store *inbox.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		githubID := primaryUser.ID
		if r.URL.Query().Get("user") == "other" {
			githubID = otherUser.ID
		}
		user, err := store.UserByGitHubID(r.Context(), githubID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		token, session, err := store.CreateSession(r.Context(), user.ID, time.Hour)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"cookie": "raises_session=" + token, "csrf": session.CSRFToken})
	}
}

// fakeGitHub completes OAuth locally: the authorize URL points straight back at
// the callback with a code, and the code always resolves to the primary user.
type fakeGitHub struct{}

func (fakeGitHub) OAuthURL(redirectURI, state, _ string) string {
	return redirectURI + "?code=acceptance&state=" + url.QueryEscape(state)
}

func (fakeGitHub) ExchangeCode(_ context.Context, code, _, verifier string) (string, error) {
	if code != "acceptance" || verifier == "" {
		return "", fmt.Errorf("unexpected OAuth code")
	}
	return "gho_acceptance", nil
}

func (fakeGitHub) CurrentUser(context.Context, string) (gh.User, error) { return primaryUser, nil }

func (fakeGitHub) UserInstallations(context.Context, string) ([]gh.Installation, error) {
	return nil, nil
}

func (fakeGitHub) Installation(context.Context, int64) (gh.Installation, error) {
	return gh.Installation{}, fmt.Errorf("no installations in the acceptance harness")
}

func (fakeGitHub) Repositories(context.Context, int64) ([]gh.Repository, error) { return nil, nil }

func (fakeGitHub) UserInstallationRepositories(context.Context, string, int64) ([]gh.Repository, error) {
	return nil, nil
}

// deliveryRecorder stands in for the outbound HTTP client. Deliveries go
// through the real queue and worker; only the final network send is replaced.
type deliveryRecorder struct {
	mu         sync.Mutex
	deliveries []recordedDelivery
}

type recordedDelivery struct {
	ID         string          `json:"id"`
	EndpointID string          `json:"endpoint_id"`
	URL        string          `json:"url"`
	Type       string          `json:"type"`
	Project    string          `json:"project"`
	Data       json.RawMessage `json:"data"`
}

func (d *deliveryRecorder) Send(_ context.Context, delivery inbox.OutboundDelivery) error {
	if delivery.DestinationKind != "webhook" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deliveries = append(d.deliveries, recordedDelivery{
		ID: delivery.ID, EndpointID: delivery.EndpointID, URL: delivery.URL,
		Type: delivery.Event.Type, Project: delivery.Event.Project, Data: delivery.Event.Payload,
	})
	return nil
}

func (d *deliveryRecorder) serve(w http.ResponseWriter, r *http.Request) {
	endpoint := r.URL.Query().Get("endpoint_id")
	d.mu.Lock()
	out := []recordedDelivery{}
	for _, delivery := range d.deliveries {
		if endpoint == "" || delivery.EndpointID == endpoint {
			out = append(out, delivery)
		}
	}
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}
