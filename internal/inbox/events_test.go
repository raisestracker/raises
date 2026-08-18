package inbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type testCipher struct{}

func (testCipher) Encrypt(value string) (string, error) { return "encrypted:" + value, nil }
func (testCipher) Decrypt(value string) (string, error) {
	plain, ok := strings.CutPrefix(value, "encrypted:")
	if !ok {
		return "", errors.New("invalid ciphertext")
	}
	return plain, nil
}

type captureOutboundSender struct {
	deliveries []OutboundDelivery
	err        error
}

func (s *captureOutboundSender) Send(_ context.Context, delivery OutboundDelivery) error {
	s.deliveries = append(s.deliveries, delivery)
	return s.err
}

func TestCreateEventStoresNoticeWithoutCreatingErrorGroup(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	user, _ := store.UpsertGitHubUser(ctx, 10, "owner", "", "")
	project, err := store.CreateProject(ctx, user.ID, "Widget", "widget")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := store.CreateProjectToken(ctx, user.ID, project.ID)
	if err != nil {
		t.Fatal(err)
	}

	event, err := store.CreateEvent(ctx, token, EventInput{Message: "Deploy finished", Source: "deploy", Context: map[string]any{"revision": "abc123"}})
	if err != nil {
		t.Fatal(err)
	}
	if event.Level != "info" || event.Env != "production" || event.Project != "Widget" {
		t.Fatalf("event=%#v", event)
	}
	var groups, jobs int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM error_groups`).Scan(&groups); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM issue_jobs`).Scan(&jobs); err != nil {
		t.Fatal(err)
	}
	if groups != 0 || jobs != 0 {
		t.Fatalf("groups=%d jobs=%d", groups, jobs)
	}
	events, err := store.ListEventsForUser(ctx, user.ID, "widget", "info", time.Time{}, 10)
	if err != nil || len(events) != 1 || events[0].ID != event.ID {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	other, _ := store.UpsertGitHubUser(ctx, 11, "other", "", "")
	if _, err := store.GetEventForUser(ctx, other.ID, event.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-account event=%v", err)
	}
}

func TestEventEnqueuesWebhookAndOperatorNtfy(t *testing.T) {
	store, _ := testStore(t)
	store.SetSecretCipher(testCipher{})
	ctx := context.Background()
	user, _ := store.UpsertGitHubUser(ctx, 12, "owner", "", "")
	project, _ := store.CreateProject(ctx, user.ID, "Widget", "widget")
	token, _, _ := store.CreateProjectToken(ctx, user.ID, project.ID)
	endpoint, secret, err := store.CreateWebhookEndpoint(ctx, user.ID, "https://example.com/raises", []string{"notice.created"})
	if err != nil || secret == "" {
		t.Fatalf("endpoint=%#v secret=%q err=%v", endpoint, secret, err)
	}
	store.ConfigureOperatorNtfy(user.ID, true)
	if _, err := store.CreateEvent(ctx, token, EventInput{Message: "Deploy finished"}); err != nil {
		t.Fatal(err)
	}

	var webhook, ntfy int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_deliveries WHERE destination_kind='webhook'`).Scan(&webhook); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbound_deliveries WHERE destination_kind='ntfy'`).Scan(&ntfy); err != nil {
		t.Fatal(err)
	}
	if webhook != 1 || ntfy != 1 {
		t.Fatalf("webhook=%d ntfy=%d", webhook, ntfy)
	}
	sender := &captureOutboundSender{}
	if err := store.ProcessOutboundDeliveriesOnce(ctx, sender); err != nil {
		t.Fatal(err)
	}
	if err := store.ProcessOutboundDeliveriesOnce(ctx, sender); err != nil {
		t.Fatal(err)
	}
	var webhookDelivery *OutboundDelivery
	for index := range sender.deliveries {
		if sender.deliveries[index].DestinationKind == "webhook" {
			webhookDelivery = &sender.deliveries[index]
		}
	}
	if webhookDelivery == nil || webhookDelivery.URL != endpoint.URL || webhookDelivery.SigningSecret != secret {
		t.Fatalf("deliveries=%#v", sender.deliveries)
	}
}

func TestWebhookDeliveryRetriesAndCanBeRetriedAfterDeadLetter(t *testing.T) {
	store, _ := testStore(t)
	store.SetSecretCipher(testCipher{})
	ctx := context.Background()
	user, _ := store.UpsertGitHubUser(ctx, 13, "owner", "", "")
	project, _ := store.CreateProject(ctx, user.ID, "Widget", "widget")
	token, _, _ := store.CreateProjectToken(ctx, user.ID, project.ID)
	_, _, _ = store.CreateWebhookEndpoint(ctx, user.ID, "https://example.com/raises", []string{"notice.created"})
	if _, err := store.CreateEvent(ctx, token, EventInput{Message: "Deploy finished"}); err != nil {
		t.Fatal(err)
	}
	sender := &captureOutboundSender{err: errors.New("offline")}
	for attempt := 0; attempt < maxOutboundJobAttempts; attempt++ {
		if _, err := store.db.ExecContext(ctx, `UPDATE outbound_deliveries SET next_attempt_at_unix=0`); err != nil {
			t.Fatal(err)
		}
		if err := store.ProcessOutboundDeliveriesOnce(ctx, sender); err != nil {
			t.Fatal(err)
		}
	}
	deliveries, err := store.ListWebhookDeliveriesForUser(ctx, user.ID, "dead", 10)
	if err != nil || len(deliveries) != 1 || deliveries[0].Attempts != maxOutboundJobAttempts {
		t.Fatalf("deliveries=%#v err=%v", deliveries, err)
	}
	if err := store.RetryWebhookDeliveryForUser(ctx, user.ID, deliveries[0].ID); err != nil {
		t.Fatal(err)
	}
	health, err := store.WebhookDeliveryHealthForUser(ctx, user.ID)
	if err != nil || health.Retrying != 1 || health.Dead != 0 {
		t.Fatalf("health=%#v err=%v", health, err)
	}
}

func TestWebhookEndpointLimitIncludesInactiveEndpoints(t *testing.T) {
	store, _ := testStore(t)
	store.SetSecretCipher(testCipher{})
	ctx := context.Background()
	user, _ := store.UpsertGitHubUser(ctx, 15, "owner", "", "")
	var first WebhookEndpoint
	for index := 0; index < MaxWebhookEndpointsPerAccount; index++ {
		endpoint, _, err := store.CreateWebhookEndpoint(ctx, user.ID, "https://example.com/raises", nil)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			first = endpoint
		}
	}
	if _, err := store.UpdateWebhookEndpoint(ctx, user.ID, first.ID, first.URL, first.Events, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateWebhookEndpoint(ctx, user.ID, "https://example.com/another", nil); err == nil {
		t.Fatal("expected endpoint limit to include inactive endpoints")
	}
	if err := store.DeleteWebhookEndpoint(ctx, user.ID, first.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.CreateWebhookEndpoint(ctx, user.ID, "https://example.com/replacement", nil); err != nil {
		t.Fatalf("create replacement: %v", err)
	}
}

func TestCreateEventPrunesExpiredEvents(t *testing.T) {
	store, _ := testStore(t)
	ctx := context.Background()
	user, _ := store.UpsertGitHubUser(ctx, 14, "owner", "", "")
	project, _ := store.CreateProject(ctx, user.ID, "Widget", "widget")
	token, _, _ := store.CreateProjectToken(ctx, user.ID, project.ID)
	if _, err := store.db.ExecContext(ctx, `INSERT INTO events(id,project_id,owner_user_id,app_name,env,level,message,context_json,created_at_unix) VALUES('old',?,?,?,'production','info','old','{}',?)`, project.ID, user.ID, project.Name, store.now().Add(-eventRetention-time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateEvent(ctx, token, EventInput{Message: "new"}); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE id='old'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}
