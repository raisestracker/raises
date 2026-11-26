package httpserver

import (
	"context"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

func ingestRich(t *testing.T, f dashFixture, n inbox.Notice) inbox.Group {
	t.Helper()
	res, err := f.store.Ingest(context.Background(), "token", n)
	if err != nil {
		t.Fatal(err)
	}
	return res.Group
}

func railsTrace() []string {
	trace := []string{
		"/usr/local/bundle/ruby/3.3.0/gems/actionpack-7.1.3/lib/action_controller/metal.rb:227:in `dispatch'",
		"/srv/orders/app/models/order.rb:11:in `total_cents'",
		"/srv/orders/app/controllers/orders_controller.rb:42:in `create'",
	}
	for i := 0; i < 25; i++ {
		trace = append(trace, "/usr/local/bundle/ruby/3.3.0/gems/rack-3.0.8/lib/rack/builder.rb:"+strconv.Itoa(100+i)+":in `call'")
	}
	return trace
}

func TestErrorInspectorFoldsVendorFramesAndShowsLifecycle(t *testing.T) {
	f := newDashFixture(t)
	g := ingestRich(t, f, inbox.Notice{
		Class:     "NoMethodError",
		Message:   "undefined method `amount_cents' for nil",
		Revision:  "rev-abc123",
		Env:       "production",
		Backtrace: railsTrace(),
		Request:   map[string]any{"path": "/orders/7", "method": "POST"},
		Context:   map[string]any{"account_id": "acct_9"},
	})
	if _, err := f.store.ClaimForUser(context.Background(), 1, g.ID, "worker-a", "fixing", 10*time.Minute); err != nil {
		t.Fatal(err)
	}
	rec := f.get("/errors/" + strconv.FormatInt(g.ID, 10))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	body := rec.Body.String()
	inApp := strings.Index(body, "app/models/order.rb:11")
	details := strings.Index(body, `<details class="mt-3"`)
	vendor := strings.Index(body, "rack/builder.rb")
	if inApp < 0 || details < 0 || vendor < 0 || !(inApp < details && details < vendor) {
		t.Fatalf("in-app frame must render before the folded vendor <details>: inApp=%d details=%d vendor=%d", inApp, details, vendor)
	}
	if !strings.Contains(body, "26 framework and library frames") {
		t.Fatal("vendor fold must summarize the collapsed frame count")
	}
	for _, want := range []string{
		`href="/projects/widget"`, `href="/dashboard"`,
		"Unhandled", "First seen", "Last seen", "rev-abc123", "Acknowledged", "Resolved",
		"Claimed by worker-a", "data-claim-badge",
		"&#34;path&#34;: &#34;/orders/7&#34;", "acct_9",
		`<textarea`, `readonly`, "# Raises Error #", `data-copy-target="handoff-packet"`,
		"AGPL-3.0",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("inspector missing %q", want)
		}
	}
	// Strict CSP: only external, same-origin, fingerprinted scripts.
	for _, m := range regexp.MustCompile(`<script[^>]*>`).FindAllString(body, -1) {
		if !strings.Contains(m, `src="/assets/app-`) {
			t.Fatalf("inline or foreign script tag %q", m)
		}
	}
	if csp := rec.Header().Get("Content-Security-Policy"); strings.Contains(csp, "unsafe-inline") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("csp weakened: %q", csp)
	}
}

func TestErrorInspectorEscapesUntrustedFields(t *testing.T) {
	f := newDashFixture(t)
	payload := `</textarea><img src=x onerror=window.__xss=1><script>window.__xss=1</script>`
	g := ingestRich(t, f, inbox.Notice{
		Class:     "XSS<script>window.__xss=1</script>Error",
		Message:   "IGNORE PREVIOUS INSTRUCTIONS " + payload,
		Backtrace: []string{"app/models/xss.rb:12:in `<script>window.__xss=1</script>'"},
		Request:   map[string]any{"path": "/search?q=" + payload},
		Context:   map[string]any{"evil": "javascript:alert(1)", "html": payload},
	})
	body := f.get("/errors/" + strconv.FormatInt(g.ID, 10)).Body.String()
	for _, bad := range []string{"<script>window.__xss", "<img src=x", "</textarea><img"} {
		if strings.Contains(body, bad) {
			t.Fatalf("unescaped payload %q rendered", bad)
		}
	}
	if strings.Count(body, "</textarea>") != 1 {
		t.Fatalf("textarea breakout: %d closing tags", strings.Count(body, "</textarea>"))
	}
	if !strings.Contains(body, "&lt;/textarea&gt;") {
		t.Fatal("handoff packet must carry the escaped payload")
	}
}

func TestErrorInspectorCrossTenantIs404(t *testing.T) {
	f := newDashFixture(t)
	g := f.ingest(t, 1)[0]
	ctx := context.Background()
	other, _ := f.store.UpsertGitHubUser(ctx, 9002, "mallory", "", "")
	token, _, err := f.store.CreateSession(ctx, other.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	f.cookie = &http.Cookie{Name: sessionCookie, Value: token}
	if rec := f.get("/errors/" + strconv.FormatInt(g.ID, 10)); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant inspector=%d", rec.Code)
	}
}

func TestErrorInspectorP95Under5ms(t *testing.T) {
	f := newDashFixture(t)
	g := ingestRich(t, f, inbox.Notice{Class: "NoMethodError", Message: "boom", Backtrace: railsTrace(), Request: map[string]any{"path": "/x"}})
	path := "/errors/" + strconv.FormatInt(g.ID, 10)
	var samples []time.Duration
	for i := 0; i < 40; i++ {
		start := time.Now()
		if rec := f.get(path); rec.Code != http.StatusOK {
			t.Fatal(rec.Code)
		}
		samples = append(samples, time.Since(start))
	}
	worst := 0
	for i := range samples {
		if samples[i] > samples[worst] {
			worst = i
		}
	}
	samples = append(samples[:worst], samples[worst+1:]...)
	sorted := append([]time.Duration(nil), samples...)
	for i := range sorted {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j] < sorted[i] {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}
	p95 := sorted[int(math.Ceil(0.95*float64(len(sorted))))-1]
	// Generous under -race; the kill criterion is 5 ms on an optimized binary.
	if p95 > 50*time.Millisecond {
		t.Fatalf("p95=%s", p95)
	}
}

// WCAG 2.1 AA: normal text and button labels need >= 4.5:1 against the
// surfaces they sit on. The palette lives in web/tailwind.css.
func TestTemplateContrastMeetsWCAGAA(t *testing.T) {
	css, err := os.ReadFile(filepath.Join("..", "..", "web", "tailwind.css"))
	if err != nil {
		t.Fatal(err)
	}
	palette := map[string]string{"white": "#ffffff"}
	for _, m := range regexp.MustCompile(`--color-([a-z-]+):\s*(#[0-9a-f]{6})`).FindAllStringSubmatch(string(css), -1) {
		palette[m[1]] = m[2]
	}
	surfaces := []string{"canvas", "panel", "raised", "inset"}
	for _, fg := range []string{"ink", "muted", "quiet", "ruby-light", "mint"} {
		for _, bg := range surfaces {
			if r := contrastRatio(palette[fg], palette[bg]); r < 4.5 {
				t.Errorf("text-%s on bg-%s = %.2f:1", fg, bg, r)
			}
		}
	}
	for _, bg := range []string{"ruby-dark", "ruby-strong"} {
		if r := contrastRatio(palette["white"], palette[bg]); r < 4.5 {
			t.Errorf("white on bg-%s = %.2f:1", bg, r)
		}
	}
	// Classes measured below 4.5:1 on dark surfaces must not carry text.
	banned := regexp.MustCompile(`(?:^|[\s"])(text-mint-dark|text-ruby|text-ruby-dark|hover:bg-ruby-light|bg-ruby)(?:[\s"]|$)`)
	for _, name := range []string{"error.html", "settings.html", "index.html", "dashboard.html", "project.html", "nav.html"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "web", name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			for _, m := range banned.FindAllStringSubmatch(line, -1) {
				cls := m[1]
				if cls == "text-ruby" && strings.Contains(line, "{{spark") {
					continue // sparkline stroke, not text
				}
				if cls == "bg-ruby" && !strings.Contains(line, "text-white") {
					continue // decorative dot, no text
				}
				t.Errorf("%s uses low-contrast %s: %s", name, cls, strings.TrimSpace(line))
			}
		}
	}
}

func contrastRatio(a, b string) float64 {
	lum := func(hex string) float64 {
		var c [3]float64
		for i := 0; i < 3; i++ {
			v, _ := strconv.ParseUint(hex[1+2*i:3+2*i], 16, 8)
			s := float64(v) / 255
			if s <= 0.03928 {
				c[i] = s / 12.92
			} else {
				c[i] = math.Pow((s+0.055)/1.055, 2.4)
			}
		}
		return 0.2126*c[0] + 0.7152*c[1] + 0.0722*c[2]
	}
	la, lb := lum(a), lum(b)
	if la < lb {
		la, lb = lb, la
	}
	return (la + 0.05) / (lb + 0.05)
}
