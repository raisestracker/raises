package ntfy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

type Client struct {
	baseURL string
	topic   string
	token   string
	client  *http.Client
}

type message struct {
	Topic    string   `json:"topic"`
	Title    string   `json:"title"`
	Message  string   `json:"message"`
	Priority int      `json:"priority"`
	Tags     []string `json:"tags"`
	Click    string   `json:"click,omitempty"`
}

func New(baseURL, topic, token string) (*Client, error) {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	topic = strings.TrimSpace(topic)
	token = strings.TrimSpace(token)
	if baseURL == "" || topic == "" || token == "" {
		return nil, fmt.Errorf("ntfy base URL, topic, and token are required")
	}
	return &Client{
		baseURL: baseURL,
		topic:   topic,
		token:   token,
		client:  &http.Client{Timeout: 5 * time.Second},
	}, nil
}

const ntfyDelayThreshold = 5 * time.Minute

func (c *Client) Notify(ctx context.Context, group inbox.Group, action string, occurred time.Time) error {
	event, tag := "New error", "rotating_light"
	if action == "reopen" {
		event, tag = "Error regressed", "repeat"
	}
	payload := message{
		Topic:    c.topic,
		Title:    fmt.Sprintf("%s · %s", displayName(group), event),
		Message:  notificationBody(group, occurred, time.Now().UTC()),
		Priority: 4,
		Tags:     []string{tag},
		Click:    group.GitHubIssueURL,
	}
	return c.publish(ctx, payload)
}

func (c *Client) SendOutbound(ctx context.Context, delivery inbox.OutboundDelivery) error {
	if delivery.Event.Type == "notice.created" {
		var payload struct {
			Notice inbox.Event `json:"notice"`
		}
		if err := json.Unmarshal(delivery.Event.Payload, &payload); err != nil {
			return err
		}
		priority := 3
		tag := "information_source"
		if payload.Notice.Level == "warning" {
			priority, tag = 4, "warning"
		} else if payload.Notice.Level == "error" {
			priority, tag = 5, "rotating_light"
		}
		body := payload.Notice.Level
		if source := strings.TrimSpace(payload.Notice.Source); source != "" {
			body += " · " + source
		}
		body += "\n" + payload.Notice.Message
		return c.publish(ctx, message{
			Topic: c.topic, Title: fmt.Sprintf("%s · Notice", payload.Notice.Project),
			Message:  body,
			Priority: priority, Tags: []string{tag},
		})
	}
	if delivery.Group == nil {
		return fmt.Errorf("ntfy error delivery is missing its group")
	}
	action := "open"
	if delivery.Event.Type == "error.regressed" {
		action = "reopen"
	}
	return c.Notify(ctx, *delivery.Group, action, delivery.Event.CreatedAt)
}

func (c *Client) publish(ctx context.Context, payload message) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("publish ntfy notification: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("publish ntfy notification: %s: %s", res.Status, strings.TrimSpace(string(raw)))
	}
	return nil
}

func displayName(group inbox.Group) string {
	if name := strings.TrimSpace(group.App); name != "" {
		return name
	}
	return "Raises"
}

func notificationBody(group inbox.Group, occurred, deliveredAt time.Time) string {
	location := strings.TrimSpace(group.Location)
	if location == "" {
		location = "unknown location"
	}
	message := singleLine(group.Message)
	if runes := []rune(message); len(runes) > 240 {
		message = string(runes[:240]) + "…"
	}
	first := fmt.Sprintf("%s · %s in %s", group.Env, group.Class, location)
	lines := []string{first}
	if message != "" {
		lines = append(lines, message)
	}
	lines = append(lines, fmt.Sprintf("Occurred %s UTC", occurred.UTC().Format("2006-01-02 15:04:05")))
	if deliveredAt.Sub(occurred) > ntfyDelayThreshold {
		lines = append(lines, "Delivery delayed")
	}
	return strings.Join(lines, "\n")
}

func singleLine(value string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(value))
}
