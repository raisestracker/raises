package outbound

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
	"github.com/raisestracker/raises/internal/ntfy"
)

type DeliveryError struct {
	message   string
	permanent bool
}

func (e *DeliveryError) Error() string   { return e.message }
func (e *DeliveryError) Permanent() bool { return e.permanent }

type Client struct {
	httpClient *http.Client
	resolver   *net.Resolver
}

type Sender struct {
	Webhooks *Client
	Ntfy     *ntfy.Client
}

func New() *Client {
	client := &Client{resolver: net.DefaultResolver}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		ResponseHeaderTimeout: 5 * time.Second,
	}
	transport.DialContext = client.dialContext
	client.httpClient = &http.Client{
		Transport: transport,
		Timeout:   8 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return client
}

func ValidateURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" {
		return fmt.Errorf("webhook URL must be a valid HTTPS URL")
	}
	if parsed.User != nil {
		return fmt.Errorf("webhook URL must not contain credentials")
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return fmt.Errorf("webhook URL must use port 443")
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") {
		return fmt.Errorf("webhook URL must use a public host")
	}
	if ip := net.ParseIP(host); ip != nil && !publicIP(ip) {
		return fmt.Errorf("webhook URL must use a public address")
	}
	return nil
}

func (s *Sender) Send(ctx context.Context, delivery inbox.OutboundDelivery) error {
	switch delivery.DestinationKind {
	case "webhook":
		return s.Webhooks.Send(ctx, delivery)
	case "ntfy":
		if s.Ntfy == nil {
			return &DeliveryError{message: "ntfy is unavailable"}
		}
		return s.Ntfy.SendOutbound(ctx, delivery)
	default:
		return &DeliveryError{message: "unknown destination kind", permanent: true}
	}
}

func (c *Client) Send(ctx context.Context, delivery inbox.OutboundDelivery) error {
	if err := ValidateURL(delivery.URL); err != nil {
		return &DeliveryError{message: err.Error(), permanent: true}
	}
	envelope := map[string]any{
		"id":         delivery.Event.ID,
		"type":       delivery.Event.Type,
		"created_at": delivery.Event.CreatedAt,
		"project": map[string]any{
			"id": delivery.Event.ProjectID, "name": delivery.Event.Project,
		},
		"data": json.RawMessage(delivery.Event.Payload),
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return &DeliveryError{message: err.Error(), permanent: true}
	}
	timestamp := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(delivery.SigningSecret))
	_, _ = mac.Write([]byte(timestamp + "."))
	_, _ = mac.Write(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, delivery.URL, bytes.NewReader(body))
	if err != nil {
		return &DeliveryError{message: err.Error(), permanent: true}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "raises-webhooks/0.3")
	req.Header.Set("X-Raises-Delivery", delivery.ID)
	req.Header.Set("X-Raises-Event", delivery.Event.Type)
	req.Header.Set("X-Raises-Timestamp", timestamp)
	req.Header.Set("X-Raises-Signature", "v1="+hex.EncodeToString(mac.Sum(nil)))
	res, err := c.httpClient.Do(req)
	if err != nil {
		return &DeliveryError{message: "deliver webhook: " + err.Error()}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return nil
	}
	permanent := res.StatusCode >= 400 && res.StatusCode < 500 && res.StatusCode != 408 && res.StatusCode != 425 && res.StatusCode != 429
	return &DeliveryError{message: fmt.Sprintf("deliver webhook: %s: %s", res.Status, strings.TrimSpace(string(raw))), permanent: permanent}
}

func (c *Client) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := c.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	for _, address := range addresses {
		if !publicIP(address.IP) {
			continue
		}
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, net.JoinHostPort(address.IP.String(), port))
	}
	return nil, fmt.Errorf("webhook host does not resolve to a public address")
}

func publicIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return false
	}
	cgnat := &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}
	return !cgnat.Contains(ip)
}
