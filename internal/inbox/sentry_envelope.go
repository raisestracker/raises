package inbox

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const MaxSentryEnvelopeEvents = 10

var (
	ErrInvalidSentryEnvelope = errors.New("invalid sentry envelope")
	ErrSentryTooManyEvents   = errors.New("sentry envelope exceeds maximum of 10 events")
)

type sentryEnvelopeHeader struct {
	EventID string         `json:"event_id"`
	SDK     map[string]any `json:"sdk"`
}

type sentryItemHeader struct {
	Type   string `json:"type"`
	Length *int   `json:"length"`
}

type sentryEventPayload struct {
	EventID     string          `json:"event_id"`
	Type        string          `json:"type"`
	Level       string          `json:"level"`
	Platform    string          `json:"platform"`
	Environment string          `json:"environment"`
	Release     string          `json:"release"`
	Message     json.RawMessage `json:"message"`
	Logentry    *struct {
		Formatted string `json:"formatted"`
		Message   string `json:"message"`
	} `json:"logentry"`
	Exception *struct {
		Values []struct {
			Type      string `json:"type"`
			Value     string `json:"value"`
			Mechanism *struct {
				Handled *bool `json:"handled"`
			} `json:"mechanism"`
			Stacktrace *struct {
				Frames []sentryFrame `json:"frames"`
			} `json:"stacktrace"`
		} `json:"values"`
	} `json:"exception"`
	Threads *struct {
		Values []struct {
			Stacktrace *struct {
				Frames []sentryFrame `json:"frames"`
			} `json:"stacktrace"`
		} `json:"values"`
	} `json:"threads"`
	Fingerprint []string       `json:"fingerprint"`
	Tags        map[string]any `json:"tags"`
	Extra       map[string]any `json:"extra"`
	User        map[string]any `json:"user"`
	Contexts    map[string]any `json:"contexts"`
	Request     map[string]any `json:"request"`
	SDK         map[string]any `json:"sdk"`
}

type sentryFrame struct {
	Filename string `json:"filename"`
	AbsPath  string `json:"abs_path"`
	Function string `json:"function"`
	Package  string `json:"package"`
	Lineno   int    `json:"lineno"`
}

// ParseSentryEnvelope parses a Sentry Envelope payload (stdlib-only, error
// items only) into up to MaxSentryEnvelopeEvents Raises Notices. Non-error
// items (sessions, transactions, client reports, attachments) are skipped.
func ParseSentryEnvelope(raw []byte) (string, []Notice, error) {
	line, pos, ok := nextEnvelopeLine(raw, 0)
	if !ok || len(bytes.TrimSpace(line)) == 0 {
		return "", nil, fmt.Errorf("%w: missing envelope header", ErrInvalidSentryEnvelope)
	}
	var envHeader sentryEnvelopeHeader
	if err := json.Unmarshal(line, &envHeader); err != nil {
		return "", nil, fmt.Errorf("%w: invalid envelope header", ErrInvalidSentryEnvelope)
	}

	var notices []Notice
	for pos < len(raw) {
		itemLine, afterHeader, ok := nextEnvelopeLine(raw, pos)
		if !ok {
			break
		}
		if len(bytes.TrimSpace(itemLine)) == 0 {
			pos = afterHeader
			continue
		}
		var itemHeader sentryItemHeader
		if err := json.Unmarshal(itemLine, &itemHeader); err != nil {
			return "", nil, fmt.Errorf("%w: invalid item header", ErrInvalidSentryEnvelope)
		}
		pos = afterHeader

		var payload []byte
		if itemHeader.Length != nil {
			n := *itemHeader.Length
			if n < 0 || pos+n > len(raw) {
				return "", nil, fmt.Errorf("%w: truncated item payload", ErrInvalidSentryEnvelope)
			}
			payload = raw[pos : pos+n]
			pos += n
			if pos < len(raw) && raw[pos] == '\r' {
				pos++
			}
			if pos < len(raw) && raw[pos] == '\n' {
				pos++
			}
		} else {
			payload, pos, _ = nextEnvelopeLine(raw, pos)
		}
		if itemHeader.Type != "event" {
			continue
		}
		notice, eventID, skip, err := sentryEventToNotice(payload, envHeader)
		if err != nil {
			return "", nil, err
		}
		if skip {
			continue
		}
		if len(notices) >= MaxSentryEnvelopeEvents {
			return "", nil, ErrSentryTooManyEvents
		}
		if envHeader.EventID == "" && eventID != "" {
			envHeader.EventID = eventID
		}
		notices = append(notices, notice)
	}
	return envHeader.EventID, notices, nil
}

func nextEnvelopeLine(raw []byte, pos int) (line []byte, next int, ok bool) {
	if pos >= len(raw) {
		return nil, pos, false
	}
	if idx := bytes.IndexByte(raw[pos:], '\n'); idx < 0 {
		line, next = raw[pos:], len(raw)
	} else {
		line, next = raw[pos:pos+idx], pos+idx+1
	}
	return bytes.TrimSuffix(line, []byte{'\r'}), next, true
}

func sentryEventToNotice(payload []byte, envHeader sentryEnvelopeHeader) (Notice, string, bool, error) {
	var ev sentryEventPayload
	if err := json.Unmarshal(payload, &ev); err != nil {
		return Notice{}, "", false, fmt.Errorf("%w: invalid event payload", ErrInvalidSentryEnvelope)
	}
	if ev.Type != "" && ev.Type != "error" && ev.Type != "default" {
		return Notice{}, ev.EventID, true, nil
	}

	className, message, handled := "", extractSentryMessage(ev), false
	var frames []sentryFrame
	if ev.Exception != nil && len(ev.Exception.Values) > 0 {
		exc := ev.Exception.Values[len(ev.Exception.Values)-1]
		for i := len(ev.Exception.Values) - 1; i >= 0; i-- {
			if ev.Exception.Values[i].Stacktrace != nil && len(ev.Exception.Values[i].Stacktrace.Frames) > 0 {
				exc = ev.Exception.Values[i]
				break
			}
		}
		className = strings.TrimSpace(exc.Type)
		if v := strings.TrimSpace(exc.Value); v != "" {
			message = v
		}
		if exc.Mechanism != nil && exc.Mechanism.Handled != nil {
			handled = *exc.Mechanism.Handled
		}
		if exc.Stacktrace != nil {
			frames = exc.Stacktrace.Frames
		}
	} else if ev.Threads != nil {
		for _, th := range ev.Threads.Values {
			if th.Stacktrace != nil && len(th.Stacktrace.Frames) > 0 {
				frames = th.Stacktrace.Frames
				break
			}
		}
	}
	if className == "" {
		if ev.Level != "" {
			className = strings.ToUpper(ev.Level[:1]) + ev.Level[1:]
		} else {
			className = "Error"
		}
	}
	if message == "" {
		message = className
	}

	// Sentry frames are oldest-first; Raises expects newest-first (top of stack first).
	backtrace := make([]string, 0, len(frames))
	for i := len(frames) - 1; i >= 0; i-- {
		if formatted := formatSentryFrame(frames[i]); formatted != "" {
			backtrace = append(backtrace, formatted)
		}
	}

	ctxMap := map[string]any{"source": "sentry_envelope"}
	for k, v := range map[string]string{"sentry_event_id": firstNonEmpty(ev.EventID, envHeader.EventID), "level": ev.Level, "platform": ev.Platform} {
		if v != "" {
			ctxMap[k] = v
		}
	}
	for k, m := range map[string]map[string]any{"tags": ev.Tags, "user": ev.User, "extra": ev.Extra, "contexts": ev.Contexts} {
		if len(m) > 0 {
			ctxMap[k] = m
		}
	}
	if len(ev.SDK) > 0 {
		ctxMap["sdk"] = ev.SDK
	} else if len(envHeader.SDK) > 0 {
		ctxMap["sdk"] = envHeader.SDK
	}

	var customFP []string
	if len(ev.Fingerprint) > 0 && ValidateFingerprintParts(ev.Fingerprint) == nil {
		customFP = ev.Fingerprint
	}
	return Notice{
		Env: strings.TrimSpace(ev.Environment), Revision: strings.TrimSpace(ev.Release),
		Handled: handled, Class: className, Message: message, Backtrace: backtrace,
		Fingerprint: customFP, Context: ctxMap, Request: ev.Request,
	}, ev.EventID, false, nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func extractSentryMessage(ev sentryEventPayload) string {
	if ev.Logentry != nil {
		if s := firstNonEmpty(strings.TrimSpace(ev.Logentry.Formatted), strings.TrimSpace(ev.Logentry.Message)); s != "" {
			return s
		}
	}
	trimmed := bytes.TrimSpace(ev.Message)
	if len(trimmed) == 0 {
		return ""
	}
	var plain string
	if json.Unmarshal(trimmed, &plain) == nil {
		return strings.TrimSpace(plain)
	}
	var obj struct {
		Formatted string `json:"formatted"`
		Message   string `json:"message"`
	}
	if json.Unmarshal(trimmed, &obj) == nil {
		return firstNonEmpty(strings.TrimSpace(obj.Formatted), strings.TrimSpace(obj.Message))
	}
	return ""
}

func formatSentryFrame(f sentryFrame) string {
	path := strings.TrimPrefix(firstNonEmpty(strings.TrimSpace(f.AbsPath), strings.TrimSpace(f.Filename)), "app:///")
	fn, pkg := strings.TrimSpace(f.Function), strings.TrimSpace(f.Package)
	if path != "" && f.Lineno > 0 {
		loc := path + ":" + strconv.Itoa(f.Lineno)
		if fn != "" {
			return loc + " in " + fn
		}
		return loc
	}
	if pkg != "" && fn != "" {
		return pkg + " " + fn
	}
	if path != "" && fn != "" {
		return path + " in " + fn
	}
	return firstNonEmpty(path, fn)
}
