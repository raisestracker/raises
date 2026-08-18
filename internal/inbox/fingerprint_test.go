package inbox

import "testing"

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
