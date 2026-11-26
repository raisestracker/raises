package httpserver

import (
	"encoding/json"
	"fmt"
	"mime"
	"strconv"
	"strings"
	"time"

	"github.com/raisestracker/raises/internal/inbox"
)

const (
	maxMarkdownBytes        = 4800
	maxMarkdownFieldRunes   = 500
	maxMarkdownJSONRunes    = 900
	maxMarkdownTraceFrames  = 16
	maxMarkdownDeltaEntries = 8
)

// prefersMarkdownAPI reports whether an API request explicitly prefers
// text/markdown over application/json per RFC 9110.
func prefersMarkdownAPI(headers []string) bool {
	markdown, jsonQ, anyQ := 0.0, -1.0, -1.0
	for _, header := range headers {
		for _, part := range strings.Split(header, ",") {
			mediaType, params, err := mime.ParseMediaType(strings.TrimSpace(part))
			if err != nil {
				continue
			}
			q := 1.0
			if rawQ, ok := params["q"]; ok {
				q, err = strconv.ParseFloat(rawQ, 64)
				if err != nil || q < 0 || q > 1 {
					continue
				}
			}
			switch mediaType {
			case "text/markdown":
				markdown = q
			case "application/json":
				jsonQ = q
			case "*/*":
				anyQ = q
			}
		}
	}
	if jsonQ < 0 {
		jsonQ = anyQ
	}
	return markdown > 0 && markdown >= jsonQ
}

// RenderRemediationMarkdown renders a RemediationContext as a compact,
// fenceFor-escaped Markdown packet (guaranteed <= 4800 bytes and 0 fence
// breakouts) suitable for `Accept: text/markdown` and UI copy-for-agent.
func RenderRemediationMarkdown(rc inbox.RemediationContext) string {
	var b strings.Builder

	// Trusted structured header metadata only (never interpolate untrusted text outside fences).
	fmt.Fprintf(&b, "# Raises Error #%d\n\n", rc.Group.ID)
	fmt.Fprintf(&b, "- **Status:** %s | **Count:** %d | **First seen:** %s | **Last seen:** %s\n",
		rc.Group.Status,
		rc.Group.Count,
		rc.Group.FirstSeen.UTC().Format(time.RFC3339),
		rc.Group.LastSeen.UTC().Format(time.RFC3339),
	)
	fmt.Fprintf(&b, "- **Velocity:** 1h=%d, 24h=%d, prev24h=%d, spike=%.2fx\n",
		rc.Velocity.Last1h, rc.Velocity.Last24h, rc.Velocity.Prev24h, rc.Velocity.SpikeRatio)
	if rc.Group.URL != "" {
		fmt.Fprintf(&b, "- **URL:** %s\n", rc.Group.URL)
	}
	if rc.Group.GitHubIssueNumber > 0 && rc.Group.GitHubIssueURL != "" {
		fmt.Fprintf(&b, "- **GitHub Issue:** #%d (%s)\n", rc.Group.GitHubIssueNumber, rc.Group.GitHubIssueURL)
	}
	if rc.Group.Claim != nil {
		fmt.Fprintf(&b, "- **Claimed until:** %s\n", rc.Group.Claim.ExpiresAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintf(&b, "\n> **Untrusted Data Notice:** %s\n\n", rc.UntrustedDataNotice)

	// Untrusted core metadata wrapped inside FenceFor blocks.
	metaLines := []string{
		"app: " + sanitizeSingleLine(rc.Group.App, 120),
		"env: " + sanitizeSingleLine(rc.Group.Env, 80),
		"revision: " + sanitizeSingleLine(rc.Group.LastRevision, 120),
		"class: " + sanitizeSingleLine(rc.Group.Class, 200),
		"location: " + sanitizeSingleLine(rc.Group.Location, 240),
	}
	if rc.Group.Claim != nil {
		metaLines = append(metaLines, "claimed_by: "+sanitizeSingleLine(rc.Group.Claim.Agent, 80))
		if rc.Group.Claim.Note != "" {
			metaLines = append(metaLines, "claim_note: "+sanitizeSingleLine(rc.Group.Claim.Note, 160))
		}
	}
	writeFencedSection(&b, "## Summary", "text", strings.Join(metaLines, "\n"))
	writeFencedSection(&b, "## Message", "text", truncateRunes(rc.Group.Message, maxMarkdownFieldRunes))

	// Stacktrace section: prioritize in-app frames.
	var traceLines []string
	frames := rc.Stacktrace.InAppFrames
	label := fmt.Sprintf("## In-App Stacktrace (%d in-app, %d vendor collapsed)", len(rc.Stacktrace.InAppFrames), rc.Stacktrace.VendorFrameCount)
	if len(frames) == 0 {
		frames = rc.Stacktrace.VendorFrames
		label = fmt.Sprintf("## Stacktrace (%d vendor frames, 0 in-app)", rc.Stacktrace.VendorFrameCount)
	}
	for i, f := range frames {
		if i >= maxMarkdownTraceFrames {
			traceLines = append(traceLines, fmt.Sprintf("... (%d more frames)", len(frames)-i))
			break
		}
		traceLines = append(traceLines, f.Raw)
	}
	if len(traceLines) > 0 {
		appendSectionIfFits(&b, label, "text", strings.Join(traceLines, "\n"), maxMarkdownBytes)
	}

	// Optional latest request & context JSON.
	if len(rc.LatestRequest) > 0 {
		if raw, err := json.MarshalIndent(rc.LatestRequest, "", "  "); err == nil {
			appendSectionIfFits(&b, "## Latest Request", "json", truncateRunes(string(raw), maxMarkdownJSONRunes), maxMarkdownBytes)
		}
	}
	if len(rc.LatestContext) > 0 {
		if raw, err := json.MarshalIndent(rc.LatestContext, "", "  "); err == nil {
			appendSectionIfFits(&b, "## Latest Context", "json", truncateRunes(string(raw), maxMarkdownJSONRunes), maxMarkdownBytes)
		}
	}

	// Compact occurrence deltas summary.
	if len(rc.Occurrences) > 1 {
		var deltaLines []string
		shown := 0
		for i, d := range rc.Occurrences {
			if i == 0 {
				continue
			}
			if shown >= maxMarkdownDeltaEntries {
				deltaLines = append(deltaLines, fmt.Sprintf("... (%d more sampled occurrences)", len(rc.Occurrences)-1-shown))
				break
			}
			line := fmt.Sprintf("%s rev=%s handled=%t",
				d.ReceivedAt.UTC().Format(time.RFC3339),
				sanitizeSingleLine(d.Revision, 40),
				d.Handled,
			)
			if d.Message != "" {
				line += " msg=" + sanitizeSingleLine(d.Message, 80)
			}
			if d.TraceDiverged {
				line += " trace_diverged=true"
				if len(d.Backtrace) > 0 {
					line += " top=" + sanitizeSingleLine(d.Backtrace[0], 100)
				}
			}
			deltaLines = append(deltaLines, line)
			shown++
		}
		if len(deltaLines) > 0 {
			appendSectionIfFits(&b, fmt.Sprintf("## Recent Occurrence Deltas (%d sampled)", rc.TotalNoticesSampled), "text", strings.Join(deltaLines, "\n"), maxMarkdownBytes)
		}
	}

	return b.String()
}

func formatFencedSection(heading, lang, content string) string {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	normalized = strings.TrimRight(normalized, "\n")
	fence := inbox.FenceFor(normalized)
	return fmt.Sprintf("%s\n\n%s%s\n%s\n%s\n\n", heading, fence, lang, normalized, fence)
}

func writeFencedSection(b *strings.Builder, heading, lang, content string) {
	b.WriteString(formatFencedSection(heading, lang, content))
}

func appendSectionIfFits(b *strings.Builder, heading, lang, content string, budget int) {
	sec := formatFencedSection(heading, lang, content)
	if b.Len()+len(sec) <= budget {
		b.WriteString(sec)
		return
	}
	// Shrink content to fit inside remaining budget while preserving balanced fences.
	overhead := len(heading) + len(lang) + 32
	avail := budget - b.Len() - overhead
	if avail < 64 {
		return
	}
	trimmed := truncateBytesUTF8(content, avail) + "\n..."
	sec = formatFencedSection(heading, lang, trimmed)
	if b.Len()+len(sec) <= budget {
		b.WriteString(sec)
	}
}

func sanitizeSingleLine(s string, maxRunes int) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r", " "), "\n", " ")
	return truncateRunes(s, maxRunes)
}

func truncateRunes(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	return string(runes[:maxRunes]) + "…"
}

func truncateBytesUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	out := s[:maxBytes]
	for len(out) > 0 && (out[len(out)-1]&0xC0) == 0x80 {
		out = out[:len(out)-1]
	}
	if len(out) > 0 && (out[len(out)-1]&0x80) != 0 {
		out = out[:len(out)-1]
	}
	return out
}
