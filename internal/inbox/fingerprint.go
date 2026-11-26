package inbox

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	MaxCustomFingerprintParts = 10
	MaxCustomFingerprintRunes = 256
)

// ErrInvalidFingerprint is returned when a notice supplies a malformed or
// oversized custom fingerprint array.
var ErrInvalidFingerprint = errors.New("invalid fingerprint")

var fileLine = regexp.MustCompile(`([^:\s]+\.[[:alnum:]]+):(\d+)`)

// Frame is a parsed, classified stacktrace line shared across fingerprinting,
// remediation context, and UI folding.
type Frame struct {
	Raw      string `json:"raw"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Function string `json:"function,omitempty"`
	InApp    bool   `json:"in_app"`
}

func Fingerprint(class string, backtrace []string, context map[string]any) (fingerprint, location string) {
	location, inApp := selectFingerprintLocation(backtrace)
	input := class + "|" + location
	if !inApp {
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

// ValidateFingerprintParts checks an optional client-supplied custom
// fingerprint array. A nil slice means omitted (valid); an explicit empty
// slice, >10 parts, empty/whitespace-only parts, or >256 total runes returns
// ErrInvalidFingerprint.
func ValidateFingerprintParts(parts []string) error {
	if parts == nil {
		return nil
	}
	if len(parts) == 0 {
		return fmt.Errorf("%w: fingerprint array must not be empty", ErrInvalidFingerprint)
	}
	if len(parts) > MaxCustomFingerprintParts {
		return fmt.Errorf("%w: fingerprint may have at most %d parts", ErrInvalidFingerprint, MaxCustomFingerprintParts)
	}
	totalRunes := 0
	for _, p := range parts {
		if !utf8.ValidString(p) {
			return fmt.Errorf("%w: fingerprint parts must be valid UTF-8", ErrInvalidFingerprint)
		}
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			return fmt.Errorf("%w: fingerprint parts must be non-empty", ErrInvalidFingerprint)
		}
		totalRunes += utf8.RuneCountInString(p)
		if totalRunes > MaxCustomFingerprintRunes {
			return fmt.Errorf("%w: fingerprint exceeds %d runes", ErrInvalidFingerprint, MaxCustomFingerprintRunes)
		}
	}
	return nil
}

func isDefaultFingerprintToken(part string) bool {
	trimmed := strings.TrimSpace(part)
	return trimmed == "{{ default }}" || trimmed == "{{default}}"
}

// FingerprintWithCustom computes the group fingerprint and code location,
// honoring an optional validated custom fingerprint array with {{ default }}
// substitution. When custom is nil or empty, it is identical to Fingerprint.
func FingerprintWithCustom(class string, backtrace []string, context map[string]any, custom []string) (fingerprint, location string) {
	defaultFP, location := Fingerprint(class, backtrace, context)
	if len(custom) == 0 {
		return defaultFP, location
	}
	if len(custom) == 1 && isDefaultFingerprintToken(custom[0]) {
		return defaultFP, location
	}
	resolved := make([]string, len(custom))
	for i, part := range custom {
		if isDefaultFingerprintToken(part) {
			resolved[i] = defaultFP
		} else {
			resolved[i] = strings.TrimSpace(part)
		}
	}
	sum := sha256.Sum256([]byte("custom|" + strings.Join(resolved, "\x1f")))
	return hex.EncodeToString(sum[:16]), location
}

// selectFingerprintLocation preserves 100% bit-identity for shipped Rails/Go
// traces containing a legacy app/ frame, and falls back to multi-stack
// ClassifyFrame in-app detection only when legacy app/ frames are absent.
func selectFingerprintLocation(backtrace []string) (location string, inApp bool) {
	// Pass 1: exact legacy app/ preference for shipped Rails/Go continuity.
	for _, frame := range backtrace {
		if isLegacyInApp(frame) {
			return normalizeFrame(frame), true
		}
	}
	// Pass 2: fallback multi-stack classification when legacy app/ is absent.
	for _, frame := range backtrace {
		classified := ClassifyFrame(frame)
		if classified.InApp {
			return formatClassifiedLocation(classified), true
		}
	}
	if len(backtrace) > 0 {
		return normalizeFrame(backtrace[0]), false
	}
	return "unknown", false
}

func isLegacyInApp(frame string) bool {
	if strings.Contains(frame, "/gems/") || strings.Contains(frame, "ruby/gems") ||
		strings.Contains(frame, "node_modules/") || strings.Contains(frame, "site-packages/") ||
		strings.Contains(frame, "dist-packages/") {
		return false
	}
	return strings.Contains(frame, "/app/") || strings.HasPrefix(strings.TrimSpace(frame), "app/")
}

// ClassifyFrame parses a single raw stacktrace line across Rails, Go, JS/TS,
// Python, and Swift and classifies whether it belongs to application code or
// stdlib/vendor/framework runtime code.
func ClassifyFrame(raw string) Frame {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Frame{Raw: raw, InApp: false}
	}
	file, line, fn := parseFrameParts(trimmed)
	inApp := !isVendorOrStdlib(trimmed, file)
	return Frame{
		Raw:      trimmed,
		File:     file,
		Line:     line,
		Function: fn,
		InApp:    inApp,
	}
}

func formatClassifiedLocation(f Frame) string {
	if f.File != "" && f.Line > 0 {
		path := f.File
		if idx := strings.Index(path, "/app/"); idx >= 0 {
			path = path[idx+1:]
		}
		return path + ":" + strconv.Itoa(f.Line)
	}
	return normalizeFrame(f.Raw)
}

func parseFrameParts(trimmed string) (file string, line int, fn string) {
	// Python traceback format: File "/path/to/file.py", line 42, in func_name
	if strings.HasPrefix(trimmed, `File "`) {
		rest := trimmed[len(`File "`):]
		if endQuote := strings.IndexByte(rest, '"'); endQuote > 0 {
			file = rest[:endQuote]
			after := rest[endQuote+1:]
			if idx := strings.Index(after, "line "); idx >= 0 {
				numStr := after[idx+len("line "):]
				end := 0
				for end < len(numStr) && numStr[end] >= '0' && numStr[end] <= '9' {
					end++
				}
				if end > 0 {
					line, _ = strconv.Atoi(numStr[:end])
				}
			}
			if idx := strings.Index(after, ", in "); idx >= 0 {
				fn = strings.TrimSpace(after[idx+len(", in "):])
			}
			return file, line, fn
		}
	}

	// JavaScript / Node / TypeScript format: at fnName (/path/file.ts:54:12) or at /path/file.ts:54:12
	if strings.HasPrefix(trimmed, "at ") {
		body := strings.TrimSpace(trimmed[3:])
		if open := strings.LastIndexByte(body, '('); open >= 0 && strings.HasSuffix(body, ")") {
			fn = strings.TrimSpace(body[:open])
			loc := body[open+1 : len(body)-1]
			file, line = splitFileLineCol(loc)
			return file, line, fn
		}
		file, line = splitFileLineCol(body)
		return file, line, ""
	}

	// Ruby format: /path/file.rb:42:in `method'
	if idx := strings.Index(trimmed, ":in "); idx > 0 {
		left := trimmed[:idx]
		fn = strings.Trim(strings.TrimSpace(trimmed[idx+len(":in "):]), "`'\"")
		file, line = splitFileLineCol(left)
		return file, line, fn
	}

	// Go / generic format: path/file.go:88 in pkg.Func
	if idx := strings.Index(trimmed, " in "); idx > 0 {
		left := strings.TrimSpace(trimmed[:idx])
		fn = strings.TrimSpace(trimmed[idx+len(" in "):])
		file, line = splitFileLineCol(left)
		if file != "" {
			return file, line, fn
		}
	}

	// Swift / Apple frame with trailing parenthesized file:line:
	// e.g. "3   MyApp  0x0000000100045678 CartViewModel.checkout() (CartViewModel.swift:88)"
	if open := strings.LastIndexByte(trimmed, '('); open >= 0 && strings.HasSuffix(trimmed, ")") {
		inner := trimmed[open+1 : len(trimmed)-1]
		if f, l := splitFileLineCol(inner); f != "" && l > 0 {
			file, line = f, l
			fn = afterThirdField(strings.TrimSpace(trimmed[:open]))
			return file, line, fn
		}
	}

	// Fallback: extract file:line from arbitrary string.
	if f, l := extractFileLineFast(trimmed); f != "" {
		return f, l, ""
	}
	return "", 0, ""
}

func splitFileLineCol(s string) (string, int) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", 0
	}
	lastColon := strings.LastIndexByte(s, ':')
	if lastColon <= 0 {
		return "", 0
	}
	n, ok := parseUintFast(s[lastColon+1:])
	if !ok {
		return "", 0
	}
	prefix := s[:lastColon]
	if prevColon := strings.LastIndexByte(prefix, ':'); prevColon > 0 {
		if prevNum, ok2 := parseUintFast(prefix[prevColon+1:]); ok2 {
			cand := strings.TrimLeft(prefix[:prevColon], `("'<`)
			if looksLikeFilePath(cand) {
				return cand, prevNum
			}
		}
	}
	cand := strings.TrimLeft(prefix, `("'<`)
	if looksLikeFilePath(cand) {
		return cand, n
	}
	return "", 0
}

func parseUintFast(s string) (int, bool) {
	if len(s) == 0 || len(s) > 9 {
		return 0, false
	}
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func extractFileLineFast(s string) (string, int) {
	for i := 0; i < len(s); i++ {
		if s[i] != ':' {
			continue
		}
		j := i + 1
		for j < len(s) && s[j] >= '0' && s[j] <= '9' {
			j++
		}
		if j == i+1 {
			continue
		}
		start := i
		for start > 0 {
			c := s[start-1]
			if c == ' ' || c == '\t' || c == '(' || c == '"' || c == '\'' || c == '<' {
				break
			}
			start--
		}
		cand := s[start:i]
		if looksLikeFilePath(cand) {
			line, _ := parseUintFast(s[i+1 : j])
			return cand, line
		}
	}
	return "", 0
}

func looksLikeFilePath(s string) bool {
	if s == "" {
		return false
	}
	dot := strings.LastIndexByte(s, '.')
	if dot <= 0 || dot == len(s)-1 {
		return false
	}
	for i := dot + 1; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
			return false
		}
	}
	return true
}

func isGoStdlibPath(path string) bool {
	path = strings.TrimPrefix(path, "./")
	slash := strings.IndexByte(path, '/')
	if slash <= 0 {
		return false
	}
	first := path[:slash]
	switch first {
	case "runtime", "net", "encoding", "crypto", "syscall", "reflect",
		"fmt", "io", "os", "sync", "time", "context", "testing",
		"bufio", "bytes", "strings", "strconv", "errors", "log",
		"math", "path", "sort", "unicode", "html", "text", "mime",
		"compress", "archive", "flag", "vendor":
		return true
	case "database":
		return strings.HasPrefix(path, "database/sql/")
	case "internal":
		rest := path[slash+1:]
		slash2 := strings.IndexByte(rest, '/')
		if slash2 <= 0 {
			return false
		}
		switch rest[:slash2] {
		case "runtime", "poll", "syscall", "abi", "reflectlite", "godebug", "sync", "cpu", "bytealg", "fmtsort", "bisect":
			return true
		}
	}
	return false
}

func isAppleVendorToken(tok string) bool {
	switch tok {
	case "UIKit", "UIKitCore", "Foundation", "CoreFoundation", "SwiftUI",
		"Combine", "AppKit", "QuartzCore", "CoreData", "CFNetwork",
		"GraphicsServices", "FrontBoardServices", "dyld":
		return true
	}
	return false
}

func isVendorOrStdlib(raw, file string) bool {
	// Fast path for path-based vendor / stdlib checks.
	target := file
	if target == "" {
		target = raw
	}

	// Check unprefixed Go stdlib paths in O(1) via first path segment.
	if isGoStdlibPath(target) {
		return true
	}

	// Check common vendor/runtime path substrings.
	if strings.Contains(raw, "node_modules/") || strings.Contains(raw, "site-packages/") ||
		strings.Contains(raw, "dist-packages/") || strings.Contains(raw, "/gems/") ||
		strings.Contains(raw, "ruby/gems") || strings.Contains(raw, "/usr/lib/python") ||
		strings.Contains(raw, "/usr/local/lib/python") || strings.Contains(raw, "/lib/python") ||
		strings.Contains(raw, "/usr/local/go/src/") || strings.Contains(raw, "/go/src/") ||
		strings.Contains(raw, "/goroot/src/") || strings.Contains(raw, "/pkg/mod/") ||
		strings.Contains(raw, "/vendor/bundle/") || strings.Contains(raw, "/lib/ruby/") ||
		strings.Contains(raw, "node:internal/") || strings.Contains(raw, "(node:") ||
		strings.Contains(raw, "internal/modules/") || strings.Contains(raw, "libswift") ||
		strings.Contains(raw, "libdispatch") || strings.Contains(raw, "libsystem_") ||
		strings.Contains(raw, "/System/Library/") || strings.Contains(raw, "/usr/lib/swift/") ||
		strings.HasPrefix(raw, "<internal:") || strings.Contains(raw, "<frozen ") {
		return true
	}

	return isAppleFrameworkFrame(raw, file)
}

func isAppleFrameworkFrame(raw, file string) bool {
	if strings.Contains(raw, ".framework") {
		return true
	}
	if file != "" && (strings.HasSuffix(file, ".swift") || strings.HasSuffix(file, ".m") || strings.HasSuffix(file, ".mm")) {
		if slash := strings.IndexByte(file, '/'); slash > 0 && isAppleVendorToken(file[:slash]) {
			return true
		}
		return false
	}
	f0, f1 := firstTwoFields(raw)
	if isAppleVendorToken(f1) {
		return true
	}
	if dot := strings.IndexByte(f1, '.'); dot > 0 && isAppleVendorToken(f1[:dot]) {
		return true
	}
	if isAppleVendorToken(f0) {
		return true
	}
	if sep := strings.IndexAny(f0, ":/"); sep > 0 && isAppleVendorToken(f0[:sep]) {
		return true
	}
	return false
}

// firstTwoFields extracts the first two whitespace-separated tokens without allocating.
func firstTwoFields(s string) (string, string) {
	i := 0
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	start0 := i
	for i < len(s) && s[i] != ' ' && s[i] != '\t' {
		i++
	}
	f0 := s[start0:i]
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	start1 := i
	for i < len(s) && s[i] != ' ' && s[i] != '\t' {
		i++
	}
	return f0, s[start1:i]
}

// afterThirdField returns the substring starting at the 4th whitespace-delimited field without allocating.
func afterThirdField(s string) string {
	i := 0
	for field := 0; field < 3; field++ {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		for i < len(s) && s[i] != ' ' && s[i] != '\t' {
			i++
		}
	}
	for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
		i++
	}
	return s[i:]
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
