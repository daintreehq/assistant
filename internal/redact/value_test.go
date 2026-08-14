package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// mustJSON round-trips through the decoder so the test operates on the same shape a real
// caller does, and proves the output is still parseable.
func redactJSONString(t *testing.T, in string) string {
	t.Helper()
	out := JSONBytes([]byte(in))
	var probe any
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("redacted JSON no longer parses: %v\n  in:  %s\n  out: %s", err, in, out)
	}
	return string(out)
}

// The bug that motivated the structural walker. The env-assignment pattern's value ran
// to the next whitespace, and minified JSON has none — so it swallowed the rest of the
// document and emitted something unparseable.
func TestValueDoesNotDestroyTheRestOfTheDocument(t *testing.T) {
	got := redactJSONString(t, `{"command":"export API_KEY=abcdefghij","path":"internal/agent/session.go"}`)

	if strings.Contains(got, "abcdefghij") {
		t.Errorf("the key survived: %s", got)
	}
	if !strings.Contains(got, "internal/agent/session.go") {
		t.Errorf("a later field was destroyed — this is the corruption bug: %s", got)
	}
}

// The other corruption bug: the keyed-field pattern is not escape-aware, so an escaped
// quote inside a value was read as the closing quote.
func TestValueHandlesEscapedQuotesInValues(t *testing.T) {
	got := redactJSONString(t, `{"password":"abc\"def","other":"keep-me"}`)

	if strings.Contains(got, "abc") || strings.Contains(got, "def") {
		t.Errorf("the password survived: %s", got)
	}
	if !strings.Contains(got, "keep-me") {
		t.Errorf("a later field was lost to a mis-parsed escape: %s", got)
	}
}

// A credential-named KEY is the only evidence available when the value has no
// recognisable shape — which is the common case for a generated secret.
func TestValueMasksBySensitiveKeyEvenWithoutAShape(t *testing.T) {
	got := redactJSONString(t, `{"api_key":"correct horse battery staple","note":"harmless"}`)

	if strings.Contains(got, "correct horse battery staple") {
		t.Errorf("a shapeless secret under a credential key survived: %s", got)
	}
	if !strings.Contains(got, "harmless") {
		t.Errorf("an unrelated field was masked: %s", got)
	}
}

// Substring key matching is what makes "client_secret" and "x-api-key" work, and it is
// also what made `tokenCount` and `signatureAlgorithm` disappear. Those are metadata
// ABOUT a credential, and masking them hides exactly the numbers a reader wants.
func TestValueKeepsBenignCompoundKeys(t *testing.T) {
	got := redactJSONString(t, `{"tokenCount":"1842","promptTokens":"900","signatureAlgorithm":"RS256","tokenPresent":"yes"}`)

	for _, keep := range []string{"1842", "900", "RS256", "yes"} {
		if !strings.Contains(got, keep) {
			t.Errorf("benign metadata %q was masked: %s", keep, got)
		}
	}
}

// Non-string scalars carry no credential and are the durations, counts, and ids that make
// a record worth keeping — EXCEPT under a key that says otherwise, where a numeric PIN is
// still a secret.
func TestValuePreservesScalarsExceptUnderSensitiveKeys(t *testing.T) {
	got := redactJSONString(t, `{"durationMs":38,"ok":true,"lines":200,"secret":12345}`)

	for _, keep := range []string{"38", "true", "200"} {
		if !strings.Contains(got, keep) {
			t.Errorf("scalar %q was masked: %s", keep, got)
		}
	}
	if strings.Contains(got, "12345") {
		t.Errorf("a numeric value under a sensitive key survived: %s", got)
	}
}

// Nesting must not be an escape hatch: a credential three levels down is just as
// exposed as one at the top.
func TestValueWalksNestedStructures(t *testing.T) {
	got := redactJSONString(t, `{"req":{"headers":{"Authorization":"Bearer abc123def456"},"items":[{"token":"deep-secret-value"}]}}`)

	if strings.Contains(got, "abc123def456") || strings.Contains(got, "deep-secret-value") {
		t.Errorf("a nested credential survived: %s", got)
	}
}

// A depth bound keeps a pathological payload from turning an audit write — a
// side-channel that must never break a tool call — into a stack overflow.
func TestValueBoundsRecursionDepth(t *testing.T) {
	deep := any("bottom")
	for i := 0; i < 200; i++ {
		deep = map[string]any{"n": deep}
	}
	// Must return rather than blow the stack.
	if Value(deep) == nil {
		t.Error("Value returned nil for a deeply nested structure")
	}
}

// Input that is not JSON at all must still be redacted, not passed through — a caller
// should never have to choose between "might corrupt it" and "might not redact it".
func TestJSONBytesFallsBackToFreeTextForNonJSON(t *testing.T) {
	got := string(JSONBytes([]byte("plain log line with sk-or-v1-abcdefghijklmnop0123456 in it")))
	if strings.Contains(got, "sk-or-v1-abcdefghijklmnop0123456") {
		t.Errorf("a secret in non-JSON input survived: %s", got)
	}
}

func TestIsSensitiveKey(t *testing.T) {
	for _, k := range []string{
		"password", "passwd", "api_key", "apiKey", "x-api-key", "API-KEY",
		"secret", "client_secret", "authorization", "Authorization",
		"token", "sessionToken", "private_key", "cookie", "signature",
		// The exemption rules must not open a hole. Each of these was exempted at some
		// point by an over-broad rule, and each holds the actual secret:
		"accessTokens",    // a LIST of credentials, not a count
		"refreshTokens",   //
		"privateKeyBytes", // the key material, not its size
		"secretBytes",     //
		"hashedPassword",  // begins with the letters "has" but is not a predicate
		"hasherSecret",    //
	} {
		if !IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = false, want true", k)
		}
	}
	for _, k := range []string{
		"terminalId", "path", "command", "lines", "durationMs",
		// Metadata ABOUT credentials — masking these hides the numbers a reader needs.
		"tokenCount", "promptTokens", "cachedTokens", "maxTokens", "signatureAlgorithm",
		"tokenPresent", "tokenLength", "keyRedacted", "secretPath",
		// The suffix/prefix rule, not an enumeration: these must all survive without
		// anyone adding them to a list.
		"apiKeyPresent", "apiKeyLength", "mcpTokenPresent", "mcpTokenLength",
		"hasToken", "isSecret", "totalTokens", "credentialPath", "tokenSource",
		// A credential-shaped name ending in "id" is a REFERENCE to a credential, not the
		// credential — the thing that authenticates is the cookie or the session TOKEN,
		// both of which stay masked by their own markers.
		"session_id", "sessionId", "apiKeyId", "credentialId",
		// The exact token-COUNT names, spelled out rather than matched by the "tokens"
		// suffix that accessTokens shares.
		"promptTokens", "completionTokens", "totalTokens", "inputTokens", "tokens",
		// Predicates: the marker must start immediately after the prefix.
		"hasToken", "isSecret", "numApiKeys",
	} {
		if IsSensitiveKey(k) {
			t.Errorf("IsSensitiveKey(%q) = true, want false", k)
		}
	}
}

// Overlapping registered secrets: replacing the shorter one first leaves the remainder of
// the longer one exposed, and the longer pattern can then never match. Registration sorts
// longest-first so this cannot happen.
func TestOverlappingRegisteredSecretsDoNotLeakSuffixes(t *testing.T) {
	ResetSecretsForTest()
	t.Cleanup(ResetSecretsForTest)

	const short = "abcdefghijkl"
	const long = "abcdefghijklmnopqrstuvwx"
	RegisterSecret(short) // shorter registered FIRST — the dangerous order
	RegisterSecret(long)

	got := String("key is " + long)
	if strings.Contains(got, "mnopqrstuvwx") {
		t.Errorf("the tail of the longer secret leaked: %s", got)
	}
	if strings.Contains(got, short) {
		t.Errorf("the shorter secret leaked: %s", got)
	}
}
