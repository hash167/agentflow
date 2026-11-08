package telemetry

import (
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
)

const (
	redacted        = "[REDACTED]"
	maxMessageBytes = 512
	maxStderrBytes  = 2048
)

// Patterns for tokens, keys, and credential-shaped values. Tests use synthetic
// sentinels that match these; production values matching them are stripped
// before they can land on a span or log field.
var (
	tokenPattern = regexp.MustCompile(`(?i)\b(ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9_]{8,}|github_pat_[A-Za-z0-9_]{8,}|crsr_[A-Za-z0-9_\-]{8,}|sk-[A-Za-z0-9_\-]{8,}`)
	bearerPat    = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-+/=]{8,}`)
	basicPat     = regexp.MustCompile(`(?i)(basic\s+)[A-Za-z0-9+=]{8,}`)
	blockedKey   = regexp.MustCompile(`(?i)(^|[._])(authorization|access[_-]?token|api[_-]?key|secret|password|passwd|cookie|credential|raw[_-]?diff|prompt|webhook[_-]?body|tool[_-]?args|tool[_-]?result)s?$`)
)

var secretEnvNames = []string{
	"CURSOR_API_KEY", "GATEWAY_REVIEWER_KEY", "GATEWAY_ADMIN_KEY",
	"GITHUB_TOKEN", "GITHUB_PAT", "ORIGIN_TOKEN", "WEBHOOK_TOKEN",
	"HONEYCOMB_API_KEY", "HONEYCOMB_API_KEY_TEST", "X_HONEYCOMB_TEAM",
}

// Redact replaces credential-shaped substrings. It is deterministic (regex
// only) so it is safe to call from Temporal workflow code.
func Redact(s string) string {
	if s == "" {
		return s
	}
	s = tokenPattern.ReplaceAllString(s, redacted)
	s = bearerPat.ReplaceAllString(s, "${1}"+redacted)
	s = basicPat.ReplaceAllString(s, "${1}"+redacted)
	return s
}

// RedactSecrets also strips live env secret values. Activity/process code only
// (reads the environment; not replay-safe).
func RedactSecrets(s string) string {
	s = Redact(s)
	for _, name := range secretEnvNames {
		if v := os.Getenv(name); len(v) >= 8 {
			s = strings.ReplaceAll(s, v, redacted)
		}
	}
	return s
}

func Truncate(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if !utf8.ValidString(s[:max]) {
		for max > 0 && !utf8.ValidString(s[:max]) {
			max--
		}
	}
	return s[:max] + "…"
}

func SafeMessage(s string) string {
	return Truncate(RedactSecrets(s), maxMessageBytes)
}

func SafeStderr(s string) string {
	return Truncate(RedactSecrets(s), maxStderrBytes)
}

func blockedAttrKey(key string) bool {
	return blockedKey.MatchString(key)
}

// Sanitize drops secret-shaped keys and redacts values. Never pass diffs,
// prompts, tool args/results, or webhook bodies into attrs in the first place.
func Sanitize(attrs []attribute.KeyValue) []attribute.KeyValue {
	if len(attrs) == 0 {
		return attrs
	}
	out := make([]attribute.KeyValue, 0, len(attrs))
	for _, a := range attrs {
		if blockedAttrKey(string(a.Key)) {
			continue
		}
		if a.Value.Type() == attribute.STRING {
			v := RedactSecrets(a.Value.AsString())
			if v != a.Value.AsString() {
				a = attribute.String(string(a.Key), v)
			}
		}
		out = append(out, a)
	}
	return out
}
