package ui

import "regexp"

// redact.go scrubs secret-looking values out of tool args BEFORE they render in the
// approval sheet (argsBlock) or the expanded ^X activity row (compactArgs). The tool
// layer already refuses to READ credential-bearing files (safety.IsSensitivePath), but
// the ARGS themselves reach the display verbatim — a terminal.write of
// `export TOKEN=sk-…`, a daintree.call carrying an Authorization header, a git remote
// with an inline token. This is a display-only defense-in-depth pass.
//
// It is deliberately PRECISE — keyed field names plus high-signal token shapes — so it
// never masks load-bearing approval detail (a push target, a shell command, a path).
// Erring toward masking a rare benign field (e.g. a "tokenCount") is acceptable; leaking
// a credential into scrollback is not.

const redactionMark = "[redacted]"

// sensitiveKeyValue matches a JSON `"key": "value"` whose KEY names a credential and
// captures the key half so only the value is masked. Case-insensitive; the key may
// contain the marker as a substring ("client_secret", "x-api-key", "sessionToken").
var sensitiveKeyValue = regexp.MustCompile(`(?i)("[^"]*(?:password|passwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|authorization|bearer|credential|client[_-]?secret|cookie|session[_-]?(?:id|token)|signature)[^"]*"\s*:\s*)"[^"]*"`)

// secretValuePatterns match high-signal secret SHAPES embedded anywhere — including
// inside a shell command string passed to terminal.write — so each whole token is masked
// in place while the surrounding text survives.
var secretValuePatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),                                           // OpenAI / Anthropic style
	regexp.MustCompile(`\bgh[opsu]_[A-Za-z0-9]{20,}\b`),                                   // GitHub PAT / OAuth tokens
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),                                // GitHub fine-grained PAT
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                            // AWS access key id
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),                                // Slack token
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\b`), // JWT (header.payload.sig)
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{12,}`),                               // Authorization: Bearer …
}

// redactArgs returns s with credential values masked. Safe on empty / non-JSON input
// (the value-shape pass runs over any string; the keyed pass simply no-ops without JSON).
func redactArgs(s string) string {
	if s == "" {
		return s
	}
	s = sensitiveKeyValue.ReplaceAllString(s, `${1}"`+redactionMark+`"`)
	for _, re := range secretValuePatterns {
		s = re.ReplaceAllString(s, redactionMark)
	}
	return s
}
