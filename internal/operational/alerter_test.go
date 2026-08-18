package operational

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeSender struct {
	sends   int
	subject string
	body    string
	err     error
}

func (f *fakeSender) Send(_ context.Context, subject, body, _ string) error {
	f.sends++
	f.subject, f.body = subject, body
	return f.err
}

func TestAlerterSendsAndRateLimitsByKey(t *testing.T) {
	sender := &fakeSender{}
	now := time.Date(2026, 8, 15, 17, 0, 0, 0, time.UTC)
	alerter := New(sender, 30*time.Minute)
	alerter.now = func() time.Time { return now }
	if err := alerter.Report(context.Background(), "panic:/v1/notices", "HTTP panic", "boom"); err != nil {
		t.Fatal(err)
	}
	if err := alerter.Report(context.Background(), "panic:/v1/notices", "HTTP panic", "again"); err != nil {
		t.Fatal(err)
	}
	if sender.sends != 1 {
		t.Fatalf("sends=%d", sender.sends)
	}
	if !strings.Contains(sender.subject, "[Raises] ALERT") || !strings.Contains(sender.body, "boom") {
		t.Fatalf("subject=%q body=%q", sender.subject, sender.body)
	}
	now = now.Add(31 * time.Minute)
	if err := alerter.Report(context.Background(), "panic:/v1/notices", "HTTP panic", "later"); err != nil {
		t.Fatal(err)
	}
	if sender.sends != 2 {
		t.Fatalf("sends=%d", sender.sends)
	}
}

func TestAlerterRetriesAfterDeliveryFailure(t *testing.T) {
	sender := &fakeSender{err: errors.New("SES unavailable")}
	alerter := New(sender, time.Hour)
	if err := alerter.Report(context.Background(), "worker", "Worker failed", "boom"); err == nil {
		t.Fatal("expected send error")
	}
	sender.err = nil
	if err := alerter.Report(context.Background(), "worker", "Worker failed", "boom"); err != nil {
		t.Fatal(err)
	}
	if sender.sends != 2 {
		t.Fatalf("sends=%d", sender.sends)
	}
}
