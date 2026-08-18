package operational

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync"
	"time"
)

type Sender interface {
	Send(context.Context, string, string, string) error
}

type Alerter struct {
	sender   Sender
	now      func() time.Time
	cooldown time.Duration

	mu       sync.Mutex
	lastSent map[string]time.Time
}

func New(sender Sender, cooldown time.Duration) *Alerter {
	return &Alerter{
		sender: sender, now: time.Now, cooldown: cooldown,
		lastSent: make(map[string]time.Time),
	}
}

func (a *Alerter) Report(ctx context.Context, key, title, details string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		key = "unknown"
	}
	now := a.now().UTC()
	a.mu.Lock()
	last := a.lastSent[key]
	a.mu.Unlock()
	if !last.IsZero() && now.Sub(last) < a.cooldown {
		return nil
	}

	title = singleLine(title)
	if title == "" {
		title = "operational failure"
	}
	body := fmt.Sprintf("Raises operational alert\n\nTime: %s\nKey: %s\n\n%s\n", now.Format(time.RFC3339), key, strings.TrimSpace(details))
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", key, now.UnixNano())))
	messageID := fmt.Sprintf("raises-alert-%x@raises.dev", sum[:12])
	if err := a.sender.Send(ctx, "[Raises] ALERT - "+title, body, messageID); err != nil {
		return err
	}
	a.mu.Lock()
	a.lastSent[key] = now
	a.mu.Unlock()
	return nil
}

func singleLine(value string) string {
	return strings.TrimSpace(strings.NewReplacer("\r", " ", "\n", " ").Replace(value))
}
