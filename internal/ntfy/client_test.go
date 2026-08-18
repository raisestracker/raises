package ntfy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestNotifyPublishesSanitizedGroup(t *testing.T) {
	var got message
	client, err := New("https://ntfy.test", "private-topic", "secret")
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	group := inbox.Group{
		App: "widget", Env: "production", Class: "NoMethodError",
		Location: "app/models/user.rb:12", Message: "undefined\nmethod",
		GitHubIssueURL: "https://github.com/example/widget/issues/42",
	}
	occurred := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	if err := client.Notify(context.Background(), group, "open", occurred); err != nil {
		t.Fatal(err)
	}
	if got.Topic != "private-topic" || got.Title != "widget · New error" || got.Click != group.GitHubIssueURL {
		t.Fatalf("message=%#v", got)
	}
	if strings.Contains(got.Message, "\nmethod") || !strings.Contains(got.Message, "undefined method") {
		t.Fatalf("body=%q", got.Message)
	}
	if !strings.Contains(got.Message, "Occurred 2026-08-14 12:00:00 UTC") {
		t.Fatalf("missing occurrence time: %q", got.Message)
	}
}

func TestNotifyReturnsPublishFailure(t *testing.T) {
	client, err := New("https://ntfy.test", "private-topic", "secret")
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusUnauthorized, Status: "401 Unauthorized", Body: io.NopCloser(strings.NewReader("nope"))}, nil
	})
	if err := client.Notify(context.Background(), inbox.Group{}, "reopen", time.Now().UTC()); err == nil {
		t.Fatal("expected publish error")
	}
}

func TestNotificationBodyMarksDelayedDelivery(t *testing.T) {
	occurred := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	delivered := occurred.Add(ntfyDelayThreshold + time.Second)
	body := notificationBody(inbox.Group{Env: "production", Class: "RuntimeError", Location: "app/jobs/x.rb:1", Message: "boom"}, occurred, delivered)
	if !strings.Contains(body, "Occurred 2026-08-14 12:00:00 UTC") {
		t.Fatalf("body=%q", body)
	}
	if !strings.Contains(body, "Delivery delayed") {
		t.Fatalf("body=%q", body)
	}
	fast := notificationBody(inbox.Group{Env: "production", Class: "RuntimeError", Location: "app/jobs/x.rb:1"}, occurred, occurred.Add(time.Second))
	if strings.Contains(fast, "Delivery delayed") {
		t.Fatalf("fast delivery should not be delayed: %q", fast)
	}
}

func TestSendOutboundPublishesNoticeWithoutEmptySourceSeparator(t *testing.T) {
	var got message
	client, err := New("https://ntfy.test", "private-topic", "secret")
	if err != nil {
		t.Fatal(err)
	}
	client.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	delivery := inbox.OutboundDelivery{Event: inbox.OutboundEvent{
		Type:    "notice.created",
		Payload: []byte(`{"notice":{"project":"Widget","level":"info","message":"Deploy finished"}}`),
	}}
	if err := client.SendOutbound(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	if got.Title != "Widget · Notice" || got.Message != "info\nDeploy finished" || got.Priority != 3 {
		t.Fatalf("message=%#v", got)
	}
}
