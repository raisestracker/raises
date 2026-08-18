package brief

import (
	"context"
	"fmt"
	"mime"
	"net/mail"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ses"
	"github.com/aws/aws-sdk-go-v2/service/ses/types"
)

type SESAPI interface {
	SendRawEmail(context.Context, *ses.SendRawEmailInput, ...func(*ses.Options)) (*ses.SendRawEmailOutput, error)
}

type SESSender struct {
	client SESAPI
	from   string
	to     string
	now    func() time.Time
}

func NewSESSender(client SESAPI, from, to string) (*SESSender, error) {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if err := validateAddress(from); err != nil {
		return nil, fmt.Errorf("report sender: %w", err)
	}
	if err := validateAddress(to); err != nil {
		return nil, fmt.Errorf("report recipient: %w", err)
	}
	return &SESSender{client: client, from: from, to: to, now: time.Now}, nil
}

func (s *SESSender) Send(ctx context.Context, subject, body, messageID string) error {
	if strings.ContainsAny(messageID, "\r\n<>") || strings.TrimSpace(messageID) == "" {
		return fmt.Errorf("invalid message ID")
	}
	encodedSubject := mime.QEncoding.Encode("UTF-8", subject)
	raw := strings.Join([]string{
		"Date: " + s.now().UTC().Format(time.RFC1123Z),
		"From: " + s.from,
		"To: " + s.to,
		"Subject: " + encodedSubject,
		"Message-ID: <" + messageID + ">",
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=UTF-8",
		"Content-Transfer-Encoding: 8bit",
		"",
		strings.ReplaceAll(body, "\n", "\r\n"),
	}, "\r\n")
	_, err := s.client.SendRawEmail(ctx, &ses.SendRawEmailInput{
		Source:       aws.String(s.from),
		Destinations: []string{s.to},
		RawMessage:   &types.RawMessage{Data: []byte(raw)},
	})
	if err != nil {
		return fmt.Errorf("send report email: %w", err)
	}
	return nil
}

func validateAddress(value string) error {
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("invalid email address")
	}
	parsed, err := mail.ParseAddress(value)
	if err != nil || parsed.Address != value {
		return fmt.Errorf("invalid email address %q", value)
	}
	return nil
}
