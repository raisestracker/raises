package inbox

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func buildEnvelope(header string, items ...struct {
	typ     string
	payload string
	withLen bool
}) []byte {
	var b strings.Builder
	b.WriteString(header)
	b.WriteByte('\n')
	for _, it := range items {
		if it.withLen {
			fmt.Fprintf(&b, `{"type":%q,"length":%d}`+"\n", it.typ, len(it.payload))
		} else {
			fmt.Fprintf(&b, `{"type":%q}`+"\n", it.typ)
		}
		b.WriteString(it.payload)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

func TestParseSentryEnvelopeFourSDKFixtures(t *testing.T) {
	fixtures := []struct {
		name       string
		raw        []byte
		wantClass  string
		wantMsg    string
		wantEnv    string
		wantRev    string
		wantLocSub string
		forbidSub  string
		wantHandle bool
	}{
		{
			name: "sentry_node",
			raw: buildEnvelope(
				`{"event_id":"node0001","dsn":"https://tok@raises.dev/1","sdk":{"name":"sentry.javascript.node","version":"8.20.0"}}`,
				struct {
					typ, payload string
					withLen      bool
				}{
					typ: "session", payload: `{"sid":"s1","status":"errored"}`, withLen: true,
				},
				struct {
					typ, payload string
					withLen      bool
				}{
					typ: "event", withLen: true,
					payload: `{"event_id":"node0001","platform":"node","environment":"production","release":"web@1.4.2","exception":{"values":[{"type":"TypeError","value":"Cannot read properties of undefined (reading 'id')","mechanism":{"type":"generic","handled":false},"stacktrace":{"frames":[{"filename":"app:///src/checkout.ts","abs_path":"/srv/web/src/checkout.ts","function":"createSession","lineno":54,"in_app":true},{"filename":"node_modules/express/lib/router/layer.js","abs_path":"/srv/web/node_modules/express/lib/router/layer.js","function":"Layer.handle","lineno":95,"in_app":false}]}}]},"request":{"url":"https://shop.example/checkout","method":"POST"}}`,
				},
			),
			wantClass: "TypeError", wantMsg: "Cannot read properties of undefined (reading 'id')",
			wantEnv: "production", wantRev: "web@1.4.2",
			wantLocSub: "/srv/web/src/checkout.ts:54", forbidSub: "node_modules", wantHandle: false,
		},
		{
			name: "sentry_python",
			raw: buildEnvelope(
				`{"event_id":"py0001","sdk":{"name":"sentry.python","version":"2.12.0"}}`,
				struct {
					typ, payload string
					withLen      bool
				}{
					typ: "event", withLen: false,
					payload: `{"event_id":"py0001","platform":"python","environment":"staging","release":"payments@9ab12","exception":{"values":[{"type":"KeyError","value":"'customer_id'","mechanism":{"type":"generic","handled":true},"stacktrace":{"frames":[{"filename":"services/payments.py","abs_path":"/srv/payments/services/payments.py","function":"charge","lineno":42,"in_app":true},{"filename":"json/__init__.py","abs_path":"/usr/lib/python3.11/json/__init__.py","function":"loads","lineno":346,"in_app":false}]}}]},"tags":{"worker":"billing"}}`,
				},
				struct {
					typ, payload string
					withLen      bool
				}{
					typ: "client_report", payload: `{"discarded_events":[]}`, withLen: false,
				},
			),
			wantClass: "KeyError", wantMsg: "'customer_id'",
			wantEnv: "staging", wantRev: "payments@9ab12",
			wantLocSub: "/srv/payments/services/payments.py:42", forbidSub: "/usr/lib/python", wantHandle: true,
		},
		{
			name: "sentry_go",
			raw: buildEnvelope(
				`{"event_id":"go0001","sdk":{"name":"sentry.go","version":"0.28.1"}}`,
				struct {
					typ, payload string
					withLen      bool
				}{
					typ: "event", withLen: true,
					payload: `{"event_id":"go0001","platform":"go","environment":"production","release":"api@4f3c1","exception":{"values":[{"type":"*errors.errorString","value":"nil customer","mechanism":{"handled":false},"stacktrace":{"frames":[{"filename":"internal/billing/charge.go","function":"example.com/acme/internal/billing.Charge","lineno":88,"in_app":true},{"filename":"net/http/server.go","function":"net/http.HandlerFunc.ServeHTTP","lineno":2220,"in_app":false}]}}]}}`,
				},
			),
			wantClass: "*errors.errorString", wantMsg: "nil customer",
			wantEnv: "production", wantRev: "api@4f3c1",
			wantLocSub: "internal/billing/charge.go:88", forbidSub: "net/http", wantHandle: false,
		},
		{
			name: "sentry_cocoa",
			raw: buildEnvelope(
				`{"event_id":"cocoa0001","sdk":{"name":"sentry.cocoa","version":"8.33.0"}}`,
				struct {
					typ, payload string
					withLen      bool
				}{
					typ: "event", withLen: true,
					payload: `{"event_id":"cocoa0001","platform":"cocoa","environment":"production","release":"ios@2.1.0","exception":{"values":[{"type":"FatalError","value":"Unexpectedly found nil","mechanism":{"handled":false},"stacktrace":{"frames":[{"filename":"CartViewModel.swift","function":"CartViewModel.checkout()","lineno":88,"in_app":true},{"package":"UIKitCore","function":"-[UIApplication sendAction:]","in_app":false},{"package":"libswiftCore.dylib","function":"_assertionFailure","in_app":false}]}}]}}`,
				},
			),
			wantClass: "FatalError", wantMsg: "Unexpectedly found nil",
			wantEnv: "production", wantRev: "ios@2.1.0",
			wantLocSub: "CartViewModel.swift:88", forbidSub: "libswift", wantHandle: false,
		},
	}

	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			eventID, notices, err := ParseSentryEnvelope(tc.raw)
			if err != nil {
				t.Fatalf("ParseSentryEnvelope: %v", err)
			}
			if eventID == "" || len(notices) != 1 {
				t.Fatalf("eventID=%q notices=%d, want 1", eventID, len(notices))
			}
			n := notices[0]
			if n.Class != tc.wantClass || n.Message != tc.wantMsg || n.Env != tc.wantEnv || n.Revision != tc.wantRev || n.Handled != tc.wantHandle {
				t.Fatalf("notice=%+v", n)
			}
			_, loc := Fingerprint(n.Class, n.Backtrace, n.Context)
			if !strings.Contains(loc, tc.wantLocSub) {
				t.Fatalf("location=%q, want substring %q (bt=%v)", loc, tc.wantLocSub, n.Backtrace)
			}
			if strings.Contains(loc, tc.forbidSub) {
				t.Fatalf("location=%q must not contain %q", loc, tc.forbidSub)
			}
		})
	}
}

func TestParseSentryEnvelopeMaxEventsAndSkipsTransactions(t *testing.T) {
	// Skip transaction-typed event items cleanly.
	txRaw := buildEnvelope(
		`{"event_id":"tx1"}`,
		struct {
			typ, payload string
			withLen      bool
		}{typ: "transaction", payload: `{"event_id":"tx1","type":"transaction"}`, withLen: true},
		struct {
			typ, payload string
			withLen      bool
		}{typ: "event", payload: `{"event_id":"tx2","type":"transaction"}`, withLen: true},
	)
	_, notices, err := ParseSentryEnvelope(txRaw)
	if err != nil || len(notices) != 0 {
		t.Fatalf("expected 0 notices for transactions, got notices=%d err=%v", len(notices), err)
	}

	// Reject >10 error event items in one envelope.
	var items []struct {
		typ, payload string
		withLen      bool
	}
	for i := 0; i < 11; i++ {
		items = append(items, struct {
			typ, payload string
			withLen      bool
		}{
			typ:     "event",
			payload: fmt.Sprintf(`{"event_id":"e%d","message":"err %d"}`, i, i),
			withLen: true,
		})
	}
	_, _, err = ParseSentryEnvelope(buildEnvelope(`{"event_id":"batch"}`, items...))
	if !errors.Is(err, ErrSentryTooManyEvents) {
		t.Fatalf("expected ErrSentryTooManyEvents, got %v", err)
	}
}
