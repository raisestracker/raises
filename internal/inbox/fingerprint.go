package inbox

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

var fileLine = regexp.MustCompile(`([^:\s]+\.rb):(\d+)`)

func Fingerprint(class string, backtrace []string, context map[string]any) (fingerprint, location string) {
	location = firstInApp(backtrace)
	input := class + "|" + location
	if !hasInAppFrame(backtrace) {
		if source, ok := context["source"].(string); ok {
			source = strings.TrimSpace(source)
			if source != "" {
				input += "|" + source
			}
		}
	}
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:16]), location
}

func hasInAppFrame(backtrace []string) bool {
	for _, frame := range backtrace {
		if isInApp(frame) {
			return true
		}
	}
	return false
}

func firstInApp(backtrace []string) string {
	for _, frame := range backtrace {
		if isInApp(frame) {
			return normalizeFrame(frame)
		}
	}
	if len(backtrace) > 0 {
		return normalizeFrame(backtrace[0])
	}
	return "unknown"
}

func isInApp(frame string) bool {
	if strings.Contains(frame, "/gems/") || strings.Contains(frame, "ruby/gems") {
		return false
	}
	return strings.Contains(frame, "/app/") || strings.HasPrefix(strings.TrimSpace(frame), "app/")
}

func normalizeFrame(frame string) string {
	matches := fileLine.FindStringSubmatch(frame)
	if len(matches) == 3 {
		path := matches[1]
		if idx := strings.Index(path, "/app/"); idx >= 0 {
			path = path[idx+1:]
		}
		return path + ":" + matches[2]
	}
	return strings.TrimSpace(frame)
}
