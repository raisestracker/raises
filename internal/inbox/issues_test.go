package inbox

import (
	"strings"
	"testing"
)

// The issue body is posted verbatim to a customer's GitHub repo, so every
// client-supplied field must render as inert text: no mentions, links,
// images, headings or inline code can survive outside a fence the payload
// cannot break out of. Fingerprint: raises/github/issue-body-markdown-injection.
func TestIssueBodyFencesUntrustedText(t *testing.T) {
	group := Group{
		ID:           7,
		App:          "shop\n# evil-app",
		Env:          "prod",
		Fingerprint:  "fp123",
		Class:        "nil `ptr` ``dbl``",
		Location:     "@dummyorg/dummy-team handler",
		Message:      "boom see [login](https://phish.invalid) ![i](https://img.invalid/p.png)\n# Heading\n```\nbreakout",
		LastRevision: "rev ```1",
	}
	body := issueBody(group, "<!-- marker -->")

	outside := stripFencedBlocks(t, body)
	for _, payload := range []string{
		"@dummyorg/dummy-team",
		"[login](https://phish.invalid)",
		"![i](https://img.invalid/p.png)",
		"# Heading",
		"```",
		"`ptr`",
	} {
		if strings.Contains(outside, payload) {
			t.Fatalf("payload %q renders outside a fence:\n%s", payload, body)
		}
	}
	// Raises-generated framing stays outside the fence and intact.
	for _, want := range []string{
		"<!-- marker -->",
		"<!-- raises:fp123 -->",
		"Ask your agent to inspect this error through the Raises API.",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing Raises-generated framing %q:\n%s", want, body)
		}
	}
}

// stripFencedBlocks removes ```+ ... ```+ regions so the test can assert on
// what GitHub would render as live markdown.
func stripFencedBlocks(t *testing.T, body string) string {
	t.Helper()
	var out strings.Builder
	openLen := 0
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) >= 3 && strings.Trim(trimmed, "`") == "" {
			// CommonMark: a closing fence is at least as long as the
			// opening one, so a shorter backtick-only value line stays
			// inside the block.
			if openLen == 0 {
				openLen = len(trimmed)
			} else if len(trimmed) >= openLen {
				openLen = 0
			}
			continue
		}
		if openLen == 0 {
			out.WriteString(line + "\n")
		}
	}
	if openLen != 0 {
		t.Fatalf("unbalanced fence in body:\n%s", body)
	}
	return out.String()
}

func TestIssueTitleStrippedAndTruncated(t *testing.T) {
	group := Group{Env: "prod", Class: "A\nB", Location: "loc\r\nline"}
	if title := issueTitle(group); strings.ContainsAny(title, "\n\r") {
		t.Fatalf("title contains newline: %q", title)
	}
	long := Group{Env: "prod", Class: strings.Repeat("C", 300), Location: "loc"}
	if title := issueTitle(long); len([]rune(title)) > 256 {
		t.Fatalf("title exceeds 256 characters: %d", len([]rune(title)))
	}
}
