package brief

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/ses"
)

type fakeSES struct {
	input *ses.SendRawEmailInput
	err   error
}

func (f *fakeSES) SendRawEmail(_ context.Context, input *ses.SendRawEmailInput, _ ...func(*ses.Options)) (*ses.SendRawEmailOutput, error) {
	f.input = input
	return &ses.SendRawEmailOutput{}, f.err
}

func TestSESSenderUsesExactEnvelopeAndPlainText(t *testing.T) {
	client := &fakeSES{}
	sender, err := NewSESSender(client, "sender@example.com", "alerts@example.com")
	if err != nil {
		t.Fatal(err)
	}
	sender.now = func() time.Time { return time.Date(2026, 8, 15, 14, 0, 0, 0, time.UTC) }
	if err := sender.Send(context.Background(), "[Raises] Daily brief - 2026-08-15", "hello\n", "raises-daily-2026-08-15@raises.dev"); err != nil {
		t.Fatal(err)
	}
	if got := *client.input.Source; got != "sender@example.com" {
		t.Fatalf("source=%q", got)
	}
	if len(client.input.Destinations) != 1 || client.input.Destinations[0] != "alerts@example.com" {
		t.Fatalf("destinations=%v", client.input.Destinations)
	}
	raw := string(client.input.RawMessage.Data)
	for _, expected := range []string{
		"From: sender@example.com\r\n",
		"To: alerts@example.com\r\n",
		"Content-Type: text/plain; charset=UTF-8\r\n",
		"Message-ID: <raises-daily-2026-08-15@raises.dev>\r\n",
	} {
		if !strings.Contains(raw, expected) {
			t.Fatalf("missing %q in %q", expected, raw)
		}
	}
}

func TestSESSenderRejectsHeaderInjectionAndWrapsErrors(t *testing.T) {
	if _, err := NewSESSender(&fakeSES{}, "sender@example.com\r\nBcc: attacker@example.com", "alerts@example.com"); err == nil {
		t.Fatal("expected address validation error")
	}
	client := &fakeSES{err: errors.New("unavailable")}
	sender, _ := NewSESSender(client, "sender@example.com", "alerts@example.com")
	if err := sender.Send(context.Background(), "subject", "body", "message@raises.dev"); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("error=%v", err)
	}
}
