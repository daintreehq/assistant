package redact

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStringMasksCredentialShapes(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		secret string // the substring that must NOT survive
	}{
		{"openai/openrouter key", `run with sk-or-v1-abcdefghijklmnopqrstuvwxyz012345`, "sk-or-v1-abcdefghijklmnopqrstuvwxyz012345"},
		{"github pat", `git remote add o https://ghp_abcdefghijklmnopqrstuvwxyz0123@github.com/x/y`, "ghp_abcdefghijklmnopqrstuvwxyz0123"},
		{"github fine-grained pat", `token github_pat_11ABCDEFG0abcdefghij_klmnopqrstuvwxyz`, "github_pat_11ABCDEFG0abcdefghij_klmnopqrstuvwxyz"},
		{"aws access key", `AWS_ACCESS_KEY_ID is AKIAIOSFODNN7EXAMPLE`, "AKIAIOSFODNN7EXAMPLE"},
		{"aws temp key", `creds ASIAIOSFODNN7EXAMPLE here`, "ASIAIOSFODNN7EXAMPLE"},
		{"slack token", `xoxb-123456789012-abcdefghijklm`, "xoxb-123456789012-abcdefghijklm"},
		{"gitlab pat", `glpat-abcdefghijklmnopqrst`, "glpat-abcdefghijklmnopqrst"},
		{"google api key", `key=AIzaSyA0123456789abcdefghijklmnopqrstuv`, "AIzaSyA0123456789abcdefghijklmnopqrstuv"},
		{"jwt", `Cookie: s=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghij`, "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.abcdefghij"},
		{"bearer header", `-H 'Authorization: Bearer abcdef0123456789xyz'`, "abcdef0123456789xyz"},
		{"basic header", `-H 'Authorization: Basic dXNlcjpwYXNzd29yZDEyMzQ='`, "dXNlcjpwYXNzd29yZDEyMzQ="},
		{"keyed json field", `{"api_key":"totally-not-a-shape-we-know"}`, "totally-not-a-shape-we-know"},
		{"nested keyed field", `{"headers":{"X-Api-Key":"plain-looking-value-here"}}`, "plain-looking-value-here"},
		{"env assignment", `export DAINTREE_MCP_TOKEN=plainvalue-no-shape`, "plainvalue-no-shape"},
		{"env assignment lowercase key", `db_password=hunter2hunter2`, "hunter2hunter2"},
		{"url userinfo", `git clone https://alice:s3cr3tpassword@example.com/repo.git`, "s3cr3tpassword"},
		{"pem private key", "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEAxyz\n-----END RSA PRIVATE KEY-----", "MIIEpAIBAAKCAQEAxyz"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := String(tc.in)
			if strings.Contains(got, tc.secret) {
				t.Errorf("secret survived redaction\n  in:  %s\n  out: %s", tc.in, got)
			}
			if !strings.Contains(got, Mark) {
				t.Errorf("nothing was masked: %s", got)
			}
		})
	}
}

// Redaction that eats load-bearing detail is its own failure. An approval sheet that
// masks the push target answers "do you approve?" with "approve WHAT?", and a log that
// masks the command leaves nothing to debug. Every pattern must be specific enough that
// ordinary operational text passes through untouched.
func TestStringPreservesOrdinaryText(t *testing.T) {
	for _, in := range []string{
		"",
		"git push origin feature/auth-redirect",
		`{"terminalId":"terminal-2b9f4c8e-1234-4a5b-8c9d-0e1f2a3b4c5d","lines":200}`,
		`{"path":"internal/agent/session.go","maxBytes":65536}`,
		"npm run build && npm test",
		`{"tokenCount":1842,"cachedTokens":18000}`,
		"https://github.com/daintreehq/assistant/pull/418",
		"MAX_TOKENS=4096",     // a limit, not a credential
		"RETRY_COUNT=3",       // no credential marker in the name
		"see docs/BACKEND.md", // plain prose
		// "sk-" is a two-letter fragment; without a word boundary it matched inside
		// perfectly ordinary words and garbled the sentence around it.
		"the risk-class-and-confirmation matrix",
		"a task-scheduler-and-supervisor design",
	} {
		if got := String(in); got != in {
			t.Errorf("ordinary text was altered\n  in:  %q\n  out: %q", in, got)
		}
	}
}

// The registered-secret path is the one that matters most: the API key becomes a
// subscription key later and the MCP token has no fixed format, so neither can be relied
// on to match a shape — while being the two values whose disclosure actually costs
// money or grants system-tier access.
func TestRegisteredSecretsAreRemovedRegardlessOfShape(t *testing.T) {
	ResetSecretsForTest()
	t.Cleanup(ResetSecretsForTest)

	const token = "an-entirely-unremarkable-opaque-value"
	if got := String("token is " + token); strings.Contains(got, Mark) {
		t.Fatalf("test premise broken: %q already matches a shape pattern", token)
	}

	RegisterSecret(token)
	got := String("connecting with token " + token + " now")
	if strings.Contains(got, token) {
		t.Errorf("a registered secret survived: %s", got)
	}
	if !strings.Contains(got, "connecting with token") || !strings.Contains(got, "now") {
		t.Errorf("redaction destroyed the surrounding text: %s", got)
	}
}

// Registration is additive on purpose: a log line written before a key rotation still
// contains the OLD key, so un-protecting it would expose that key the next time the file
// is read or bundled.
func TestRegisterSecretIsAdditiveAndIdempotent(t *testing.T) {
	ResetSecretsForTest()
	t.Cleanup(ResetSecretsForTest)

	RegisterSecret("first-secret-value-long-enough")
	RegisterSecret("second-secret-value-long-enough")
	RegisterSecret("first-secret-value-long-enough") // duplicate

	out := String("a=first-secret-value-long-enough b=second-secret-value-long-enough")
	if strings.Contains(out, "first-secret-value-long-enough") || strings.Contains(out, "second-secret-value-long-enough") {
		t.Errorf("a registered secret survived: %s", out)
	}

	exact.RLock()
	n := len(exact.values)
	exact.RUnlock()
	if n != 2 {
		t.Errorf("duplicate registration should be a no-op, got %d entries", n)
	}
}

// A too-short "secret" would match constantly and destroy far more diagnostic signal
// than it protects. Real credentials are comfortably longer.
func TestRegisterSecretIgnoresEmptyAndShortValues(t *testing.T) {
	ResetSecretsForTest()
	t.Cleanup(ResetSecretsForTest)

	RegisterSecret("")
	RegisterSecret("   ")
	RegisterSecret("short")
	RegisterSecret("elevenchars") // 11 < minExactLength

	if got := String("the word short appears here, and elevenchars too"); strings.Contains(got, Mark) {
		t.Errorf("a too-short value was registered and is now masking ordinary text: %s", got)
	}
}

func TestCapReplacesOverflowWithSizeAndHash(t *testing.T) {
	long := strings.Repeat("x", 5000)
	got := Cap(long, 100)
	if len(got) >= len(long) {
		t.Fatalf("Cap did not shorten: %d bytes", len(got))
	}
	if !strings.Contains(got, "of 5000 bytes") {
		t.Errorf("the original size must survive: %s", got)
	}
	if !strings.Contains(got, "sha256:") {
		t.Errorf("a content hash must survive so two occurrences stay comparable: %s", got)
	}
	// The same payload must hash identically; a different one must not.
	if Cap(long, 100) != got {
		t.Error("Cap is not deterministic")
	}
	if Cap(strings.Repeat("y", 5000), 100) == got {
		t.Error("different payloads produced the same capped form")
	}
	// Under the cap, and with no cap, the value is untouched.
	if Cap("short", 100) != "short" {
		t.Error("a value under the cap must pass through unchanged")
	}
	if Cap(long, 0) != long {
		t.Error("max<=0 must disable capping")
	}
}

// Capping before redacting could cut a secret in half and leave the surviving prefix in
// the output, unmatched by any pattern because it is no longer a well-formed token.
func TestStringCappedRedactsFirst(t *testing.T) {
	ResetSecretsForTest()
	t.Cleanup(ResetSecretsForTest)

	const key = "sk-or-v1-abcdefghijklmnopqrstuvwxyz012345"
	// Realistic shape: the key sits inside a command line, surrounded by whitespace, with
	// enough trailing output that the value still needs capping after redaction. The cap
	// is chosen to land INSIDE the key's original span, so capping-before-redacting would
	// leave a well-formed-looking prefix behind that no pattern would then match.
	head := "$ export OPENROUTER_API_KEY_ALT=" // deliberately not a name envAssignment matches on its own
	in := head + key + "\n" + strings.Repeat("build output line\n", 40)
	cutInsideKey := len(head) + 20

	got := StringCapped(in, cutInsideKey)
	if strings.Contains(got, "sk-or-v1-abcdefghij") {
		t.Errorf("a truncated secret prefix survived: %s", got)
	}
	// The mark itself may be clipped by the head cut — what matters is that no part of
	// the KEY survives and that redaction visibly fired.
	if !strings.Contains(got, "[redact") {
		t.Errorf("the key should have been masked: %s", got)
	}
	if !strings.Contains(got, "elided") {
		t.Errorf("the value should still have been capped: %s", got)
	}
}

// The biggest values in the trace are build and test output, where the interesting part
// — the failure, the summary line, the exit status — is at the END. A head-only cap
// reliably discards the one thing the reader opened the log for.
func TestCapKeepsBothEnds(t *testing.T) {
	body := "HEAD-MARKER" + strings.Repeat("filler ", 2000) + "TAIL-MARKER"
	got := Cap(body, 200)

	if !strings.Contains(got, "HEAD-MARKER") {
		t.Errorf("the head was lost: %s", got)
	}
	if !strings.Contains(got, "TAIL-MARKER") {
		t.Errorf("the tail was lost — a failing build's error is at the end: %s", got)
	}
	if len(got) > 400 {
		t.Errorf("the cap did not bound the output: %d bytes", len(got))
	}
}

// Slicing on a raw byte index split a multi-byte character and emitted a lone
// continuation byte, which makes the whole log file invalid UTF-8.
func TestCapNeverSplitsARune(t *testing.T) {
	// Every character is 3 bytes, so almost every byte index is mid-rune.
	body := strings.Repeat("日", 2000)
	for _, max := range []int{100, 101, 102, 103, 250, 999} {
		got := Cap(body, max)
		if !utf8.ValidString(got) {
			t.Errorf("Cap(max=%d) produced invalid UTF-8", max)
		}
	}
}
