package inbox

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func TestFingerprintUsesInAppFrame(t *testing.T) {
	fp, loc := Fingerprint("NoMethodError", []string{
		"/usr/local/bundle/gems/activesupport-8.0.0/lib/active_support.rb:10:in `foo`",
		"/rails/app/models/user.rb:42:in `name`",
	}, nil)
	if loc != "app/models/user.rb:42" {
		t.Fatalf("location = %q", loc)
	}
	if fp == "" {
		t.Fatal("expected fingerprint")
	}
	fp2, _ := Fingerprint("NoMethodError", []string{"app/models/user.rb:42:in `name`"}, nil)
	if fp != fp2 {
		t.Fatalf("fingerprints should match: %s vs %s", fp, fp2)
	}
}

func TestFingerprintUsesGoApplicationFrame(t *testing.T) {
	fp, loc := Fingerprint("*errors.errorString", []string{
		"net/http/server.go:3340 in net/http.(*conn).serve",
		"app/internal/jobs/worker.go:42 in example.com/widget/internal/jobs.Run",
	}, nil)
	if loc != "app/internal/jobs/worker.go:42" {
		t.Fatalf("location = %q", loc)
	}
	if fp == "" {
		t.Fatal("expected fingerprint")
	}
}

func TestFingerprintIgnoresMessage(t *testing.T) {
	bt := []string{"app/jobs/foo.rb:3:in `perform`"}
	a, _ := Fingerprint("RuntimeError", bt, nil)
	b, _ := Fingerprint("RuntimeError", bt, nil)
	if a != b {
		t.Fatal("same class and location should match")
	}
}

func TestFingerprintUsesSourceWithoutApplicationFrame(t *testing.T) {
	bt := []string{"/usr/local/bundle/gems/railties-8.0.0/lib/rails/commands/runner/runner_command.rb:49:in `perform`"}
	a, locA := Fingerprint("RuntimeError", bt, map[string]any{"source": "daily_brief"})
	b, locB := Fingerprint("RuntimeError", bt, map[string]any{"source": "operational_alert"})
	c, _ := Fingerprint("RuntimeError", bt, map[string]any{"source": "daily_brief"})
	if locA != locB {
		t.Fatalf("locations = %q vs %q", locA, locB)
	}
	if a == b {
		t.Fatal("different sources should not group together without app frames")
	}
	if a != c {
		t.Fatal("same source should group together")
	}
	inApp, _ := Fingerprint("RuntimeError", []string{"app/jobs/foo.rb:3:in `perform`"}, map[string]any{"source": "daily_brief"})
	withoutSource, _ := Fingerprint("RuntimeError", []string{"app/jobs/foo.rb:3:in `perform`"}, nil)
	if inApp != withoutSource {
		t.Fatal("application frames should ignore context source")
	}
}

func TestLegacyRailsAndGoFingerprintsBitIdentical(t *testing.T) {
	cases := []struct {
		name      string
		class     string
		backtrace []string
		wantInput string
		wantLoc   string
	}{
		{
			name:  "rails_order_model",
			class: "NoMethodError",
			backtrace: []string{
				"/usr/local/bundle/ruby/3.3.0/gems/rack-3.0/lib/rack.rb:12:in `call'",
				"/srv/rails/app/models/order.rb:42:in `total'",
			},
			wantInput: "NoMethodError|app/models/order.rb:42",
			wantLoc:   "app/models/order.rb:42",
		},
		{
			name:  "rails_with_earlier_non_gem_lib_frame",
			class: "ArgumentError",
			backtrace: []string{
				"/srv/rails/lib/custom_middleware.rb:19:in `call'",
				"/srv/rails/app/controllers/orders_controller.rb:77:in `create'",
			},
			wantInput: "ArgumentError|app/controllers/orders_controller.rb:77",
			wantLoc:   "app/controllers/orders_controller.rb:77",
		},
		{
			name:  "go_prefixed_app_frame",
			class: "*errors.errorString",
			backtrace: []string{
				"net/http/server.go:3340 in net/http.(*conn).serve",
				"app/internal/importer/run.go:42 in example.com/widget/internal/importer.Run",
			},
			wantInput: "*errors.errorString|app/internal/importer/run.go:42",
			wantLoc:   "app/internal/importer/run.go:42",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sum := sha256.Sum256([]byte(tc.wantInput))
			wantFP := hex.EncodeToString(sum[:16])
			gotFP, gotLoc := Fingerprint(tc.class, tc.backtrace, nil)
			if gotLoc != tc.wantLoc {
				t.Fatalf("location = %q, want %q", gotLoc, tc.wantLoc)
			}
			if gotFP != wantFP {
				t.Fatalf("fingerprint = %q, want %q (legacy bit-identity broken)", gotFP, wantFP)
			}
		})
	}
}

func TestMultiStackFallbackPreventsStdlibHijack(t *testing.T) {
	cases := []struct {
		name      string
		class     string
		backtrace []string
		wantSub   string
		forbidSub string
	}{
		{
			name:  "go_unprefixed_internal_after_runtime",
			class: "runtime.errorString",
			backtrace: []string{
				"/usr/local/go/src/runtime/panic.go:770 in runtime.gopanic",
				"internal/runtime/maps/map.go:100 in internal/runtime/maps.mapaccess1",
				"/usr/local/go/src/net/http/server.go:2220 in net/http.HandlerFunc.ServeHTTP",
				"internal/billing/charge.go:88 in example.com/acme/internal/billing.Charge",
			},
			wantSub:   "internal/billing/charge.go:88",
			forbidSub: "runtime/",
		},
		{
			name:  "node_src_after_node_modules",
			class: "TypeError",
			backtrace: []string{
				"at node:internal/process/task_queues:95:5",
				"at Layer.handle (/srv/web/node_modules/express/lib/router/layer.js:95:5)",
				"at next (/srv/web/node_modules/express/lib/router/route.js:144:13)",
				"at createSession (/srv/web/src/checkout.ts:54:12)",
			},
			wantSub:   "/srv/web/src/checkout.ts:54",
			forbidSub: "node_modules",
		},
		{
			name:  "python_service_after_site_packages_and_stdlib",
			class: "KeyError",
			backtrace: []string{
				`File "/usr/lib/python3.11/json/__init__.py", line 346, in loads`,
				`File "/app/.venv/lib/python3.11/site-packages/requests/api.py", line 59, in request`,
				`File "/srv/payments/services/payments.py", line 42, in charge`,
			},
			wantSub:   "/srv/payments/services/payments.py:42",
			forbidSub: "site-packages",
		},
		{
			name:  "swift_app_after_libswift_uikit_foundation",
			class: "FatalError",
			backtrace: []string{
				"0   libswiftCore.dylib                  0x000000018a123456 _assertionFailure + 123",
				"1   UIKitCore                           0x000000018b123456 -[UIApplication sendAction:] + 96",
				"2   Foundation                          0x000000018c123456 -[NSRunLoop run] + 44",
				"3   MyApp                               0x0000000100045678 CartViewModel.checkout() (CartViewModel.swift:88)",
			},
			wantSub:   "CartViewModel.swift:88",
			forbidSub: "libswift",
		},
		{
			name:  "rails_in_app_after_gems",
			class: "NoMethodError",
			backtrace: []string{
				"/usr/local/bundle/ruby/3.3.0/gems/actionpack-7.1.3/lib/action_controller/metal.rb:227:in `dispatch'",
				"/srv/orders/app/models/order.rb:11:in `total_cents'",
			},
			wantSub:   "app/models/order.rb:11",
			forbidSub: "gems/",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp, loc := Fingerprint(tc.class, tc.backtrace, nil)
			if fp == "" {
				t.Fatal("empty fingerprint")
			}
			if !strings.Contains(loc, tc.wantSub) {
				t.Fatalf("location = %q, want substring %q", loc, tc.wantSub)
			}
			if strings.Contains(loc, tc.forbidSub) {
				t.Fatalf("location = %q, must not contain %q", loc, tc.forbidSub)
			}
		})
	}
}

func TestClassifyFrameParsesMetadata(t *testing.T) {
	f := ClassifyFrame(`at createSession (/srv/web/src/checkout.ts:54:12)`)
	if !f.InApp || f.File != "/srv/web/src/checkout.ts" || f.Line != 54 || f.Function != "createSession" {
		t.Fatalf("unexpected JS frame: %+v", f)
	}
	py := ClassifyFrame(`File "/srv/payments/services/payments.py", line 42, in charge`)
	if !py.InApp || py.File != "/srv/payments/services/payments.py" || py.Line != 42 || py.Function != "charge" {
		t.Fatalf("unexpected Python frame: %+v", py)
	}
	pyStd := ClassifyFrame(`File "/usr/lib/python3.11/json/__init__.py", line 346, in loads`)
	if pyStd.InApp {
		t.Fatalf("python stdlib frame should not be in_app: %+v", pyStd)
	}
	goMod := ClassifyFrame(`/go/pkg/mod/github.com/lib/pq@v1.10.9/conn.go:50 in github.com/lib/pq.(*conn).Query`)
	if goMod.InApp {
		t.Fatalf("go pkg/mod frame should not be in_app: %+v", goMod)
	}
	swiftUIKit := ClassifyFrame(`1   UIKit                               0x000000018b123456 -[UIView layoutSubviews] + 44`)
	if swiftUIKit.InApp {
		t.Fatalf("UIKit frame should not be in_app: %+v", swiftUIKit)
	}
}

func BenchmarkClassifyFrameNotice(b *testing.B) {
	trace := []string{
		"/usr/local/go/src/runtime/panic.go:770 in runtime.gopanic",
		"internal/runtime/maps/map.go:100 in internal/runtime/maps.mapaccess1",
		"/usr/local/go/src/net/http/server.go:2220 in net/http.HandlerFunc.ServeHTTP",
		"at Layer.handle (/srv/web/node_modules/express/lib/router/layer.js:95:5)",
		`File "/usr/lib/python3.11/json/__init__.py", line 346, in loads`,
		`File "/opt/venv/lib/python3.11/site-packages/requests/api.py", line 59, in request`,
		"0   libswiftCore.dylib                  0x000000018a123456 _assertionFailure + 123",
		"1   UIKitCore                           0x000000018b123456 -[UIApplication sendAction:] + 96",
		"2   Foundation                          0x000000018c123456 -[NSRunLoop run] + 44",
		"internal/billing/charge.go:88 in example.com/acme/internal/billing.Charge",
	}
	// 30-frame realistic trace
	full := make([]string, 0, 30)
	for i := 0; i < 3; i++ {
		full = append(full, trace...)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, frame := range full {
			_ = ClassifyFrame(frame)
		}
	}
}

func TestCustomFingerprintValidationAndConsolidation(t *testing.T) {
	// Valid cases
	if err := ValidateFingerprintParts(nil); err != nil {
		t.Fatalf("nil should be valid: %v", err)
	}
	if err := ValidateFingerprintParts([]string{"db-pool-exhausted"}); err != nil {
		t.Fatalf("single part should be valid: %v", err)
	}
	if err := ValidateFingerprintParts([]string{"{{ default }}", "checkout"}); err != nil {
		t.Fatalf("{{ default }} should be valid: %v", err)
	}
	tenParts := []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10"}
	if err := ValidateFingerprintParts(tenParts); err != nil {
		t.Fatalf("10 parts should be valid: %v", err)
	}
	exact256 := []string{strings.Repeat("a", 256)}
	if err := ValidateFingerprintParts(exact256); err != nil {
		t.Fatalf("256 runes should be valid: %v", err)
	}

	// Invalid cases
	invalid := [][]string{
		{},
		{""},
		{"   "},
		{"ok", ""},
		{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"},
		{strings.Repeat("a", 257)},
		{strings.Repeat("a", 130), strings.Repeat("b", 127)},
	}
	for i, parts := range invalid {
		if err := ValidateFingerprintParts(parts); err == nil {
			t.Fatalf("case %d (%v): expected error", i, parts)
		}
	}

	// Consolidation across distinct files/classes
	fp1, loc1 := FingerprintWithCustom("PG::ConnectionBad", []string{"app/models/order.rb:12"}, nil, []string{"db-pool-exhausted"})
	fp2, loc2 := FingerprintWithCustom("Timeout::Error", []string{"app/jobs/sync.rb:99"}, nil, []string{"db-pool-exhausted"})
	if fp1 != fp2 {
		t.Fatalf("custom fingerprint should consolidate: %s vs %s", fp1, fp2)
	}
	if loc1 == loc2 {
		t.Fatalf("expected distinct preserved locations: %q vs %q", loc1, loc2)
	}

	// {{ default }} alone matches default fingerprint bit-for-bit
	defFP, _ := Fingerprint("NoMethodError", []string{"app/models/order.rb:42:in `total'"}, nil)
	onlyDefFP, _ := FingerprintWithCustom("NoMethodError", []string{"app/models/order.rb:42:in `total'"}, nil, []string{"{{ default }}"})
	if defFP != onlyDefFP {
		t.Fatalf("{{ default }} alone should equal defaultFP: %s vs %s", onlyDefFP, defFP)
	}

	// {{ default }} + discriminator splits same frame into distinct groups
	subA, _ := FingerprintWithCustom("NoMethodError", []string{"app/models/order.rb:42:in `total'"}, nil, []string{"{{ default }}", "stripe"})
	subB, _ := FingerprintWithCustom("NoMethodError", []string{"app/models/order.rb:42:in `total'"}, nil, []string{"{{ default }}", "paypal"})
	if subA == subB || subA == defFP {
		t.Fatalf("{{ default }} + discriminator failed to separate groups: def=%s a=%s b=%s", defFP, subA, subB)
	}

	// Delimiter collision safety: ["a|b", "c"] != ["a", "b|c"]
	colA, _ := FingerprintWithCustom("Err", []string{"app/a.rb:1"}, nil, []string{"a|b", "c"})
	colB, _ := FingerprintWithCustom("Err", []string{"app/a.rb:1"}, nil, []string{"a", "b|c"})
	if colA == colB {
		t.Fatal("delimiter collision between custom fingerprint parts")
	}
}
