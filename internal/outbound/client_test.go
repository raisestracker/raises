package outbound

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestValidateURL(t *testing.T) {
	for _, value := range []string{"http://example.com", "https://user:pass@example.com", "https://example.com:8443", "https://127.0.0.1/hook", "https://localhost/hook"} {
		if err := ValidateURL(value); err == nil {
			t.Fatalf("expected %q to fail", value)
		}
	}
	if err := ValidateURL("https://example.com/hook"); err != nil {
		t.Fatal(err)
	}
}

func TestPublicIP(t *testing.T) {
	for _, value := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "::1"} {
		if publicIP(net.ParseIP(value)) {
			t.Fatalf("expected %s to be blocked", value)
		}
	}
	if !publicIP(net.ParseIP("1.1.1.1")) {
		t.Fatal("public IP was blocked")
	}
}

func TestSendSignsExactWebhookBody(t *testing.T) {
	client := New()
	var request *http.Request
	var body []byte
	client.httpClient.Transport = roundTripFunc(func(got *http.Request) (*http.Response, error) {
		request = got
		var err error
		body, err = io.ReadAll(got.Body)
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Status: "204 No Content", Body: io.NopCloser(bytes.NewReader(nil))}, nil
	})
	delivery := inbox.OutboundDelivery{
		ID: "obd_test", URL: "https://example.com/hook", SigningSecret: "whsec_test",
		Event: inbox.OutboundEvent{ID: "obe_test", ProjectID: "prj_test", Project: "Widget", Type: "notice.created", Payload: []byte(`{"notice":{"message":"done"}}`), CreatedAt: time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)},
	}
	if err := client.Send(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	timestamp := request.Header.Get("X-Raises-Timestamp")
	if _, err := strconv.ParseInt(timestamp, 10, 64); err != nil {
		t.Fatalf("timestamp=%q", timestamp)
	}
	mac := hmac.New(sha256.New, []byte(delivery.SigningSecret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	want := "v1=" + hex.EncodeToString(mac.Sum(nil))
	if got := request.Header.Get("X-Raises-Signature"); got != want {
		t.Fatalf("signature=%q want=%q", got, want)
	}
	if request.Header.Get("X-Raises-Delivery") != delivery.ID || request.Header.Get("X-Raises-Event") != delivery.Event.Type {
		t.Fatalf("headers=%v", request.Header)
	}
}
