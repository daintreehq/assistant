// Package redact scrubs secret-looking values out of text on its way to somewhere it
// will persist: the debug log, the durable audit rows, the approval sheet, the attached session's
// expanded activity rows.
//
// # Why this is a package and not a helper
//
// The assistant handles other people's secrets constantly and mostly by accident. A tool
// call carries whatever the model put in it — a `terminal.sendCommand` of
// `export TOKEN=…`, a `daintree.call` with an Authorization header, a git remote with an
// inline token. A tool RESULT carries whatever the terminal printed, which routinely
// includes an agent echoing its environment. None of that is the assistant's own
// credential, so no amount of care with our own key protects it.
//
// Where it lands matters. The attached session renders on the terminal's NORMAL screen buffer, so
// anything displayed persists in the host's native scrollback long after the session and
// is never cleared. The debug log is an append-only file that outlives the process by a
// week. The audit table is queryable and exportable by a tool the model itself can call.
// A secret reaching any of those has effectively been published to everything with read
// access to the machine.
//
// # Two layers
//
// SHAPES (this file's patterns) catch credentials we have never seen: keyed JSON fields,
// recognisable token formats, URL userinfo, `KEY=value` assignments, PEM blocks. They are
// deliberately precise rather than aggressive — masking a load-bearing detail out of an
// approval sheet ("push to WHERE?") is its own kind of harm, so a pattern earns its place
// by being specific enough that a false positive is rare and boring.
//
// EXACT SECRETS (RegisterSecret) catch the credentials this process is actually holding:
// the caller's API key, the Daintree MCP bearer token. Shape matching cannot be relied on
// for these — a subscription key or a rotated token format may match nothing here — and
// they are the ones whose disclosure costs money or grants system-tier access. Registering
// them turns "probably caught" into "certainly caught".
//
// # What is redacted, and what deliberately is not
//
// The line is drawn by PURPOSE, not by whether a sink happens to be durable.
//
// REDACTED — copies made for OUR benefit, where a credential serves no purpose:
//
//	the debug log                    every value, at the write boundary
//	durable audit rows               args, result, and the summary column
//	run_events (SQLite, /explain)    via the EventSink source
//	the console + JSONL + host sinks  via the same source
//	the attached session's activity rows and ops previews, which seal into native scrollback
//	the approval sheet and ^X detail
//
// NOT REDACTED — places where the raw value is what makes the thing work:
//
//	the CONVERSATION persisted for resume. The model must see exactly what the
//	terminal printed; a redacted transcript would make the assistant unable to relay
//	a value the user legitimately asked it to read, and would corrupt a resume.
//
//	ARTIFACTS (archived oversized results, pre-compaction transcripts). Same reason —
//	they are the conversation's overflow, read back by artifact.read.
//
//	TIMER payloads and ASYNC commands. These are scheduled WORK: the stored form is
//	replayed to execute later, so redacting it would break the job outright.
//
//	the user's and the assistant's own PROSE. A model can repeat a credential it read;
//	no metadata-level redaction can prevent that, and pretending otherwise would be a
//	guarantee this package cannot keep.
//
// All four live under the 0700 state dir with 0600 files, local to the machine, and
// none is exported anywhere by default. They are protected by SCOPE, not by scrubbing —
// which is why `support-bundle` is a separate, deliberately-narrower artifact rather
// than "zip up the state directory".
//
// The package is intentionally allocation-cheap on the common path: every entry point
// returns the input unchanged when there is nothing to do.
package redact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Mark is what replaces a redacted value. Distinctive enough to grep for when auditing
// whether redaction fired, and obviously not a real value.
const Mark = "[redacted]"

// sensitiveKeyValue matches a JSON `"key": "value"` whose KEY names a credential, and
// captures the key half so only the value is masked. Case-insensitive; the key may carry
// the marker as a substring ("client_secret", "x-api-key", "sessionToken").
var sensitiveKeyValue = regexp.MustCompile(`(?i)("[^"]*(?:password|passwd|secret|token|api[_-]?key|apikey|access[_-]?key|private[_-]?key|authorization|bearer|credential|client[_-]?secret|cookie|session[_-]?token|signature)[^"]*"\s*:\s*)"[^"]*"`)

// envAssignment matches a shell-style `NAME=value` where NAME names a credential — the
// shape a terminal prints when an agent dumps its environment, or when the model sends
// `export DAINTREE_MCP_TOKEN=…`.
//
// Two bounds, each fixing a real failure:
//
// The value must contain at least one NON-DIGIT. Without that, `MAX_TOKENS=4096` and
// `TOKEN_LIMIT=8` were redacted: the name contains "TOKEN", so the pattern fired on a
// plain configuration limit and hid the number the reader wanted. No credential is a
// bare integer.
//
// The value stops at a QUOTE, COMMA, SEMICOLON, or ampersand as well as whitespace.
// "non-whitespace" was catastrophic on minified JSON, which has no whitespace at all:
// `{"command":"export API_KEY=abc","path":"keep"}` matched everything from `abc` to the
// end of the document and emitted unparseable output. Structured payloads should go
// through Value instead — but this pattern still sees serialized text in free-form
// contexts, and destroying the rest of the line is never the right failure.
var envAssignment = regexp.MustCompile(`(?i)\b([A-Z0-9_]*(?:PASSWORD|PASSWD|SECRET|TOKEN|API[_-]?KEY|APIKEY|ACCESS[_-]?KEY|PRIVATE[_-]?KEY|CREDENTIAL)[A-Z0-9_]*)=([^\s"',;&]*[^\s"',;&\d][^\s"',;&]*)`)

// urlUserinfo matches credentials embedded in a URL's AUTHORITY (`https://user:pw@host`).
// Only the userinfo is masked; the host survives, because "which remote?" is usually the
// load-bearing half of the line.
//
// `?` and `#` are excluded from both halves: without that,
// `https://example.com?email=foo@bar.com` was read as userinfo and rewritten to
// `https://[redacted]@bar.com`, mangling an ordinary URL — an at-sign after the query
// delimiter is not authority. The username may be empty, so `https://:token@host` (a
// common CI form) is caught too.
var urlUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@?#]*(?::[^/\s@?#]*)?@`)

// pemBlock matches an entire PEM private key. Matched before the line-oriented patterns
// so a key body — which is just base64 and matches nothing else — is removed whole
// rather than leaking every line that happens not to look like a token.
var pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----`)

// secretValuePatterns match high-signal secret SHAPES embedded anywhere — including
// inside a shell command — so each whole token is masked in place while the surrounding
// text survives.
var secretValuePatterns = []*regexp.Regexp{
	// \b on the sk- prefix. It was omitted at first so a key glued to preceding text would
	// still match — but "sk-" is a two-letter fragment, and the cost showed up immediately
	// in ordinary prose: "risk-class-and-confirmation" contains "sk-class-and-confirmation"
	// and was being masked out of log lines and approval sheets. A redactor that garbles
	// the sentence explaining an approval is not erring safely; it is destroying what the
	// reader needs. A real key is delimited by a quote, space, or `=` essentially always,
	// so the boundary costs almost nothing.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`),                                         // OpenAI / OpenRouter / Anthropic style
	regexp.MustCompile(`\bgh[opsu]_[A-Za-z0-9]{20,}\b`),                                   // GitHub PAT / OAuth
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`),                                // GitHub fine-grained PAT
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),                                            // AWS access key id
	regexp.MustCompile(`\bASIA[0-9A-Z]{16}\b`),                                            // AWS temporary access key id
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}\b`),                                // Slack
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{16,}\b`),                                    // GitLab PAT
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{30,}\b`),                                      // Google API key
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}\b`), // JWT
	// Two bounds, each fixing a real false positive:
	//
	// The credential must LOOK like one — at least one digit or token punctuation.
	// Requiring only length matched the prose "use Bearer authentication for this
	// endpoint", redacting a sentence that explains how to authenticate.
	//
	// The separator is [ \t]+, NOT \s+. \s matches a NEWLINE, so a comment ending in
	// "…the bearer" followed by a line starting "// token requirement…" matched across
	// the line break and masked the start of the next line. A real Authorization header
	// never has a newline between the scheme and the credential, and mangling prose is
	// how a redactor loses the reader's trust.
	regexp.MustCompile(`(?i)\bbearer[ \t]+[A-Za-z0-9._~+/=-]*[0-9._~+/=-][A-Za-z0-9._~+/=-]*`), // Authorization: Bearer …
	regexp.MustCompile(`(?i)\bbasic[ \t]+[A-Za-z0-9+/]*[0-9+/=][A-Za-z0-9+/=]*`),               // Authorization: Basic …
}

// exact holds the literal secrets this process is holding. Guarded because
// RegisterSecret runs at boot and on every MCP credential refresh, while redaction runs from
// tool goroutines, the scheduler, and the async coordinator concurrently.
var exact struct {
	sync.RWMutex
	values []string
}

// minExactLength is the shortest string accepted as an exact secret.
//
// A very short "secret" would match constantly and turn the log into noise — and a
// two-character value being replaced everywhere destroys far more diagnostic signal than
// it protects. Real credentials are comfortably longer; anything shorter is either a
// placeholder or a test fixture.
const minExactLength = 12

// RegisterSecret adds a literal value to be removed from every redacted string.
//
// Call it with each real credential as it becomes known: the Daintree MCP token on
// connect and reconnect, and DAINTREE_API_KEY at boot on the rare install that sets one.
// Registering is additive and idempotent — an old value stays registered after a
// rotation, which is correct, since a log written before it can still contain it.
//
// Safe to call with "" or a short value; both are ignored.
func RegisterSecret(s string) {
	s = strings.TrimSpace(s)
	if len(s) < minExactLength {
		return
	}
	exact.Lock()
	defer exact.Unlock()
	for _, v := range exact.values {
		if v == s {
			return
		}
	}
	exact.values = append(exact.values, s)
	// LONGEST FIRST. With two overlapping secrets — a key and a rotated key sharing a
	// prefix, say — replacing the shorter one first leaves the remainder of the longer
	// one exposed as `[redacted]mnop`, and the longer pattern can then never match. Order
	// once at registration (rare) rather than on every redaction (hot).
	sort.SliceStable(exact.values, func(i, j int) bool {
		return len(exact.values[i]) > len(exact.values[j])
	})
}

// ResetSecretsForTest clears the registered exact secrets. Tests only — the registry is
// process-global by design, and a leaked registration between tests makes one test's
// fixture silently redact another's assertion.
func ResetSecretsForTest() {
	exact.Lock()
	defer exact.Unlock()
	exact.values = nil
}

// String redacts s: registered exact secrets first, then credential shapes.
//
// Exact secrets go first deliberately. A registered key is a certainty while a shape is a
// guess, and removing the certainty first means a partially-shape-matched key cannot be
// left half-visible by an earlier pattern chewing off its prefix.
func String(s string) string {
	if s == "" {
		return s
	}
	// The slice is COPIED under the lock, not aliased. RegisterSecret appends to and
	// re-sorts this same backing array, so holding only a reference and iterating after
	// unlocking races that sort: entries can move, and a value can be skipped entirely —
	// which for this function means a live credential reaching a log. The copy is a few
	// string headers on a path that already scans the whole input.
	exact.RLock()
	values := make([]string, len(exact.values))
	copy(values, exact.values)
	exact.RUnlock()
	for _, v := range values {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, Mark)
		}
	}

	s = pemBlock.ReplaceAllString(s, Mark)
	s = sensitiveKeyValue.ReplaceAllString(s, `${1}"`+Mark+`"`)
	s = envAssignment.ReplaceAllString(s, `${1}=`+Mark)
	s = urlUserinfo.ReplaceAllString(s, `${1}`+Mark+`@`)
	for _, re := range secretValuePatterns {
		s = re.ReplaceAllString(s, Mark)
	}
	return s
}

// Cap bounds a string, keeping its HEAD and its TAIL and eliding the middle.
//
// Unbounded values are their own problem, separate from secrecy: a single terminal dump
// can be megabytes, and a trace that records every one of them stops being readable and
// starts being a disk-space incident.
//
// Head-AND-tail rather than head-only, because of what these values actually are. The
// biggest ones are build and test output, where the interesting part — the failure, the
// summary line, the exit status — is at the END. A head-only cap reliably discards the
// one thing the reader opened the log for and keeps 64 KiB of compilation noise. The
// split is 3:1 in favour of the head, which is where a payload's structure and identity
// live.
//
// The elision records the true byte length and a content hash, so two occurrences of the
// same payload stay recognisably the same without either being stored. Note the size and
// hash describe the value AS CAPPED SEES IT — post-redaction, when called through
// StringCapped — not the raw original; that is deliberate, since a hash of the raw form
// would be a verifier for the credential just removed.
//
// max <= 0 means no cap.
func Cap(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	headMax := max * 3 / 4
	tailMax := max - headMax
	head := s[:runeBoundaryBefore(s, headMax)]
	tail := s[runeBoundaryAfter(s, len(s)-tailMax):]
	sum := sha256.Sum256([]byte(s))
	return head + fmt.Sprintf("\n…[elided %d of %d bytes, sha256:%s]…\n",
		len(s)-len(head)-len(tail), len(s), hex.EncodeToString(sum[:])[:16]) + tail
}

// runeBoundaryBefore returns the largest index <= i that starts a rune.
//
// Slicing on a raw byte index split a multi-byte character and emitted a lone
// continuation byte, leaving the whole log file invalid UTF-8 — enough to break awk,
// iconv, and anything else that validates before parsing.
func runeBoundaryBefore(s string, i int) int {
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// runeBoundaryAfter returns the smallest index >= i that starts a rune.
func runeBoundaryAfter(s string, i int) int {
	if i <= 0 {
		return 0
	}
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}

// StringCapped redacts then caps — the ordering callers almost always want.
//
// Redacting FIRST matters: capping first could cut a secret in half and leave the
// surviving prefix in the output, unmatched by any pattern because it is no longer a
// well-formed token.
func StringCapped(s string, max int) string { return Cap(String(s), max) }

// --- Scanning, as distinct from redacting -------------------------------------------

// highConfidencePatterns are the shapes that essentially never appear except as a REAL
// credential: an issuer-prefixed token, or a PEM block.
//
// This is a deliberately different set from the one String uses, and the difference is
// the point. Redaction runs over a log line and should err toward masking — a false
// positive there costs a little readability. SCANNING runs over source code and should
// err toward silence: the keyed-field, env-assignment, and Authorization-header patterns
// all match ordinary prose that DESCRIBES those shapes, and this repository is full of
// such prose because it implements the redactor. A scanner that cries wolf on its own
// documentation gets switched off, and then it protects nothing.
//
// So a scan asks the narrower question: is there a literal token here?
var highConfidencePatterns = []*regexp.Regexp{
	// \b here, unlike the redaction pattern above. Scanning must be PRECISE: without a
	// left boundary, the ordinary phrase "risk-class-and-confirmation" contains
	// "sk-class-and-confirmation" and trips the scan on this project's own documentation.
	// Redaction keeps the loose form on purpose — there, a glued-on secret matters more
	// than a rare false mask.
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`\bgh[opsu]_[A-Za-z0-9]{30,}\b`),
	regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{30,}\b`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bASIA[0-9A-Z]{16}\b`),
	regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{20,}\b`),
	regexp.MustCompile(`\bglpat-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\.[A-Za-z0-9_-]{20,}\b`),
	// The header PLUS real key material. Matching the header alone flagged every
	// document that MENTIONS a PEM block and every test that writes a stub file — a
	// mention is not a leak, and a scanner that cannot tell the difference is one people
	// route around. A genuine key body runs to hundreds of base64 characters.
	regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.{32,}?-----END [A-Z ]*PRIVATE KEY-----`),
}

// FindLiteralSecrets returns every high-confidence credential found in s.
//
// For scanning a repository, a build artifact, or anything else where a finding must be
// worth acting on. Use String to REDACT; use this to ASK.
func FindLiteralSecrets(s string) []string {
	raw := FindLiteralSecretsRaw(s)
	out := make([]string, 0, len(raw))
	for _, m := range raw {
		// A scanner that prints the secret it found puts it in CI logs, which is the same
		// disclosure by another route. Report enough to locate it, and no more.
		out = append(out, previewSecret(m))
	}
	return out
}

// FindLiteralSecretsRaw is FindLiteralSecrets without the redaction, for the one caller
// that must INSPECT a match to classify it (the repository scan, deciding whether a hit
// is an obviously-invented fixture). Never print its results.
func FindLiteralSecretsRaw(s string) []string {
	var out []string
	seen := map[string]bool{}
	for _, re := range highConfidencePatterns {
		for _, m := range re.FindAllString(s, -1) {
			if !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// previewSecret renders enough of a finding to locate it, and no more.
func previewSecret(s string) string {
	if len(s) <= 12 {
		return "[redacted-short-token]"
	}
	return s[:8] + "…[" + itoaLen(len(s)) + " chars]"
}

func itoaLen(n int) string { return strconv.Itoa(n) }
