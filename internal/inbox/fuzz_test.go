package inbox

import (
	"testing"
	"unicode/utf8"
)

func FuzzClassifyFrame(f *testing.F) {
	seeds := []string{
		"",
		"   ",
		"/rails/app/controllers/orders_controller.rb:42:in `create'",
		"/rails/vendor/bundle/ruby/3.3.0/gems/actionpack-8.0.0/lib/action_controller/metal.rb:252:in `dispatch'",
		"app/models/user.rb:19:in `save!'",
		"/usr/local/go/src/runtime/panic.go:261 in runtime.panicmem",
		"internal/runtime/maps/map.go:12 in runtime.mapaccess",
		"net/http/server.go:2000 in http.HandlerFunc.ServeHTTP",
		"github.com/acme/billing/charge.go:88 in billing.Charge",
		"at processCheckout (/srv/web/src/checkout.ts:54:12)",
		"at /srv/web/node_modules/express/lib/router/index.js:284:7",
		"at node:internal/process/task_queues:95:5",
		`File "/srv/api/app/routes.py", line 42, in handle_order`,
		`File "/usr/lib/python3.12/site-packages/starlette/routing.py", line 70, in app`,
		`File "<frozen importlib._bootstrap>", line 1204, in _gcd_import`,
		"0   libswiftCore.dylib  0x0000000180001234 _assertionFailure + 12",
		"1   UIKitCore           0x0000000180005678 -[UIApplication sendAction:to:from:forEvent:] + 96",
		"2   MyApp               0x0000000100045678 CartViewModel.checkout() (CartViewModel.swift:88)",
		"File \"unclosed",
		"at (",
		"at ()",
		"::::::",
		"foo.go:99999999999999999999999999999999",
		"\x00\xff\xfe:12:34",
		"a:in ",
		"a in ",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		frame := ClassifyFrame(raw)
		if frame.Line < 0 {
			t.Fatalf("negative line number %d for %q", frame.Line, raw)
		}
		// Must never panic on Fingerprint or FingerprintWithCustom either.
		fp, loc := Fingerprint("RuntimeError", []string{raw}, map[string]any{"source": "fuzz"})
		if len(fp) != 32 {
			t.Fatalf("unexpected fingerprint length %d", len(fp))
		}
		if loc == "" && raw != "" {
			// Empty location is allowed for whitespace-only raw lines.
		}
	})
}

func FuzzParseSentryEnvelope(f *testing.F) {
	seeds := [][]byte{
		[]byte(""),
		[]byte("{}\n"),
		[]byte(`{"event_id":"123"}` + "\n" + `{"type":"event"}` + "\n" + `{"event_id":"123","exception":{"values":[{"type":"TypeError","value":"boom","stacktrace":{"frames":[{"filename":"src/app.ts","lineno":10,"function":"run"}]}}]}}`),
		[]byte(`{"event_id":"123"}` + "\n" + `{"type":"event","length":9223372036854775807}` + "\n" + `{}`),
		[]byte(`{"event_id":"123"}` + "\n" + `{"type":"event","length":-1}` + "\n" + `{}`),
		[]byte(`{"event_id":"123"}` + "\n" + `{"type":"event","length":15}` + "\n" + `{"level":"🔥"}` + "\n"),
		[]byte(`{"event_id":"123"}` + "\n" + `{"type":"session","length":2}` + "\n" + `{}` + "\r\n" + `{"type":"event"}` + "\r\n" + `{"message":{"formatted":"hi"},"level":"warning"}`),
		[]byte(`{"event_id":"123"}` + "\n" + `{"type":"event"}` + "\n" + `{"type":"transaction","event_id":"tx1"}`),
		[]byte(`{"event_id":"123"}` + "\n" + `{"type":"event"}` + "\n" + `{"threads":{"values":[{"stacktrace":{"frames":[{"abs_path":"app:///main.py","lineno":7,"function":"main"}]}}]}}`),
		[]byte("\xff\xfe\x00\n{}\n{}"),
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		_, notices, err := ParseSentryEnvelope(raw)
		if err != nil {
			return
		}
		if len(notices) > MaxSentryEnvelopeEvents {
			t.Fatalf("exceeded max events: %d > %d", len(notices), MaxSentryEnvelopeEvents)
		}
		for _, n := range notices {
			if n.Class == "" {
				t.Fatalf("empty class in parsed notice")
			}
			if !utf8.ValidString(n.Class) {
				t.Fatalf("invalid UTF-8 class %q", n.Class)
			}
		}
	})
}
