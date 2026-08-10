package security

import (
	"net/http"
	"regexp"
	"sort"
	"strings"
)

const RedactedValue = "[REDACTED]"

var sensitiveHeaders = map[string]struct{}{
	"api-key":                {},
	"authentication-info":    {},
	"authorization":          {},
	"cookie":                 {},
	"openai-api-key":         {},
	"proxy-authorization":    {},
	"sec-websocket-protocol": {}, // Commonly (mis)used to transport bearer credentials.
	"set-cookie":             {},
	"x-access-token":         {},
	"x-api-key":              {},
	"x-auth-token":           {},
	"x-codex-api-key":        {},
	"x-goog-api-key":         {},
	"x-openai-api-key":       {},
	"x-refresh-token":        {},
}

var (
	apiKeyPattern              = regexp.MustCompile(`\bcgk_v1_[A-Za-z0-9_-]{16}_[A-Za-z0-9_-]+`)
	opaquePattern              = regexp.MustCompile(`\bcg[isr]_v1_[A-Za-z0-9_-]+`)
	recoveryCodePattern        = regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{4}(?:-[0-9A-HJKMNP-TV-Z]{4}){5}\b`)
	authSchemePattern          = regexp.MustCompile(`(?i)\b(?:Bearer|Basic)\s+[^\s,;]+`)
	sensitiveParamPattern      = regexp.MustCompile(`(?i)(\b(?:access_token|refresh_token|id_token|api[_-]?key|token|session|invitation|recovery|code)=)[^&#\s]+`)
	sensitiveHeaderLinePattern = regexp.MustCompile(`(?im)^((?:authorization|proxy-authorization|authentication-info|cookie|set-cookie|api-key|x-api-key|x-auth-token|x-access-token|x-refresh-token|x-codex-api-key|x-openai-api-key|openai-api-key|x-goog-api-key|sec-websocket-protocol)\s*:\s*).*$`)
)

// SensitiveHeaderNames returns a sorted defensive copy of headers whose values
// must never be logged or forwarded from an untrusted client.
func SensitiveHeaderNames() []string {
	names := make([]string, 0, len(sensitiveHeaders))
	for name := range sensitiveHeaders {
		names = append(names, http.CanonicalHeaderKey(name))
	}
	sort.Strings(names)
	return names
}

// IsSensitiveHeader performs an ASCII case-insensitive lookup after trimming
// surrounding whitespace.
func IsSensitiveHeader(name string) bool {
	_, ok := sensitiveHeaders[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

// RedactedHeaders creates a safe copy for structured logging. Sensitive header
// values are replaced wholesale; recognizable credentials embedded in other
// values are scrubbed as a second line of defense. The input is never mutated.
func RedactedHeaders(headers http.Header) http.Header {
	redacted := make(http.Header, len(headers))
	for name, values := range headers {
		if IsSensitiveHeader(name) {
			redacted[http.CanonicalHeaderKey(name)] = []string{RedactedValue}
			continue
		}
		cloned := make([]string, len(values))
		for i, value := range values {
			cloned[i] = RedactText(value)
		}
		redacted[http.CanonicalHeaderKey(name)] = cloned
	}
	return redacted
}

// RedactText removes credential-shaped values from error messages and other
// metadata before logging. It is a defense-in-depth helper, not permission to
// log request or response bodies.
func RedactText(value string) string {
	value = sensitiveHeaderLinePattern.ReplaceAllString(value, "${1}"+RedactedValue)
	value = apiKeyPattern.ReplaceAllString(value, RedactedValue)
	value = opaquePattern.ReplaceAllString(value, RedactedValue)
	value = recoveryCodePattern.ReplaceAllString(value, RedactedValue)
	value = authSchemePattern.ReplaceAllString(value, RedactedValue)
	value = sensitiveParamPattern.ReplaceAllString(value, "${1}"+RedactedValue)
	return value
}
