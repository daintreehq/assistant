package backend

import (
	"errors"
	"strings"
	"testing"
)

// testSecret is the sentinel every redaction assertion below looks for. It is a fixture
// string, not a credential of any kind — the point is only that a value embedded in a
// rejected URL must not come back out in the error.
const testSecret = "supersecret"

// The endpoints this binary ships with have to survive their own validator, byte for
// byte. A default that normalized to something else would mean every fresh install ran
// against a URL nobody wrote down.
func TestNormalizeBaseURLLeavesTheCompiledEndpointsAlone(t *testing.T) {
	for _, in := range []string{DefaultBaseURL, LocalBaseURL} {
		got, err := NormalizeBaseURL(in, false)
		if err != nil {
			t.Fatalf("NormalizeBaseURL(%q) rejected a compiled-in endpoint: %v", in, err)
		}
		if got != in {
			t.Errorf("NormalizeBaseURL(%q) = %q — a compiled default must be its own canonical form", in, got)
		}
	}
}

// The shapes that must keep working. Loopback over plaintext is the local development
// loop: there is no network to intercept.
func TestNormalizeBaseURLAcceptsWhatItShould(t *testing.T) {
	for _, in := range []string{
		"https://backend.example",
		"https://backend.example:8443",
		"https://backend.example/proxy-prefix",
		"http://127.0.0.1:8473",
		"http://[::1]:8473",
		"http://localhost:8473",
		"http://localhost.:8473",  // a trailing DNS root dot is the same machine
		"HTTPS://backend.example", // people paste what a mail client capitalized
	} {
		if _, err := NormalizeBaseURL(in, false); err != nil {
			t.Errorf("NormalizeBaseURL(%q) rejected a valid endpoint: %v", in, err)
		}
	}
}

// The spellings that actually drift have to collapse onto one string, or the comparisons
// that decide "am I already on this endpoint?" answer no to an endpoint they are already
// on. Scope is deliberately narrow and worth stating: scheme case, outer whitespace and
// literal trailing path slashes. Host CASE, a default port written out (`:443`), an IDNA
// host against its punycode, and one IPv6 spelling against another are all left exactly
// as typed — rewriting the authority is not this function's business, and each of those
// would be a canonicalizer quietly deciding it knows better than the person who typed it.
func TestNormalizeBaseURLCollapsesTheSpellingsThatDrift(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"https://backend.example", "https://backend.example"},
		{"https://backend.example/", "https://backend.example"},
		{"https://backend.example///", "https://backend.example"},
		{"  https://backend.example/  ", "https://backend.example"},
		{"https://backend.example/prefix/", "https://backend.example/prefix"},
		{"HTTPS://backend.example/", "https://backend.example"},
	} {
		got, err := NormalizeBaseURL(tc.in, false)
		if err != nil {
			t.Errorf("NormalizeBaseURL(%q) error = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A slash that arrived as %2F is part of a path SEGMENT, not a separator. Trimming it
// would rename the endpoint rather than tidy it, which is the one way a canonicalizer
// can quietly change where a request goes.
func TestNormalizeBaseURLDoesNotTrimAnEncodedSlash(t *testing.T) {
	got, err := NormalizeBaseURL("https://backend.example/a%2F", false)
	if err != nil {
		t.Fatalf("NormalizeBaseURL: %v", err)
	}
	if got != "https://backend.example/a%2F" {
		t.Errorf("NormalizeBaseURL collapsed an encoded slash: %q", got)
	}
}

// Every shape the interactive `/backend` command has always refused, now refused at
// every door. Each is something that fails silently or dangerously if it is let past —
// see NormalizeBaseURL's doc comment for which failure belongs to which.
func TestNormalizeBaseURLRejectsWhatCannotBeAnEndpoint(t *testing.T) {
	for name, in := range map[string]string{
		"empty":              "",
		"whitespace only":    "   ",
		"userinfo":           "https://user:" + testSecret + "@backend.example",
		"userinfo no pass":   "https://user@backend.example",
		"query":              "https://backend.example?token=" + testSecret,
		"force query":        "https://backend.example?",
		"fragment":           "https://backend.example#frag",
		"no host":            "https://",
		"no host with path":  "https:///v1",
		"schemeless":         "127.0.0.1:8473",
		"bare hostname":      "localhost",
		"unsupported scheme": "ftp://backend.example",
		"file scheme":        "file:///etc/passwd",
		"remote plaintext":   "http://backend.example",
		"control character":  "https://backend.example\x1b[2J",
		"newline":            "https://backend.example\nhttps://evil.example",
		"interior space":     "https://backend.example /v1",
		"backslash":          "https://backend.example\\@evil.example",
		"unclosed ipv6":      "https://[::1",
		"non numeric port":   "https://backend.example:notaport",
		"too long":           "https://" + strings.Repeat("a", MaxBaseURLLength) + ".example",
		// net/url parses each of these happily; only a check of our own catches them,
		// and without one they boot fine and then fail every turn against an
		// endpoint that was never dialable.
		"two ports":            "https://backend.example:443:443",
		"port out of range":    "https://backend.example:65536",
		"zero port":            "https://backend.example:0",
		"empty port":           "https://backend.example:",
		"percent encoded host": "https://%FF.example",
		"bidi override":        "https://ab\u202ecd.example",
	} {
		if got, err := NormalizeBaseURL(in, false); err == nil {
			t.Errorf("%s: NormalizeBaseURL(%q) = %q, want an error", name, in, got)
		}
	}
}

// THE redaction rule, and the parse-error path is where it was actually broken:
// url.Error embeds the whole raw input in its message, so `invalid backend URL %q` put a
// password on the terminal and into the startup log. Every rejection is checked, because
// a rule with one exception is not a rule.
func TestNormalizeBaseURLNeverEchoesASecret(t *testing.T) {
	for name, in := range map[string]string{
		// These four fail INSIDE url.Parse — the live leak.
		"parse: bad port":      "https://user:" + testSecret + "@backend.example:notaport",
		"parse: unclosed ipv6": "https://user:" + testSecret + "@[::1",
		"parse: bad escape":    "https://user:" + testSecret + "%zz@backend.example",
		"parse: bad host char": "https://user:" + testSecret + "@back|end.example",
		// …and these are rejected by our own checks, which must hold the same line.
		"userinfo":           "https://user:" + testSecret + "@backend.example",
		"query token":        "https://backend.example?token=" + testSecret,
		"fragment":           "https://backend.example#" + testSecret,
		"control character":  "https://user:" + testSecret + "@backend.example\x00",
		"unsupported scheme": "ftp://user:" + testSecret + "@backend.example",
		"too long":           "https://user:" + testSecret + "@" + strings.Repeat("a", MaxBaseURLLength) + ".example",
	} {
		_, err := NormalizeBaseURL(in, false)
		if err == nil {
			t.Errorf("%s: expected a rejection", name)
			continue
		}
		if strings.Contains(err.Error(), testSecret) {
			t.Errorf("%s: the rejection echoed the secret back: %v", name, err)
		}
	}
	// ValidatePlaintextRemote is the other public door onto the same parser and had the
	// same leak.
	if err := ValidatePlaintextRemote("https://user:"+testSecret+"@[::1", false); err == nil {
		t.Error("ValidatePlaintextRemote accepted an unparseable URL")
	} else if strings.Contains(err.Error(), testSecret) {
		t.Errorf("ValidatePlaintextRemote echoed the secret back: %v", err)
	}
}

// The plaintext rule is the ONE rejection a caller has to be able to tell from the rest:
// config.LoadConfig reports it as a different diagnostic, and app.ResolveBackendTarget
// substitutes its own remedy because `/backend` has no escape hatch to offer.
func TestNormalizeBaseURLTypesThePlaintextRefusal(t *testing.T) {
	_, err := NormalizeBaseURL("http://backend.example:8080", false)
	var plaintext *PlaintextRemoteError
	if !errors.As(err, &plaintext) {
		t.Fatalf("a plaintext remote endpoint must fail with *PlaintextRemoteError, got %v", err)
	}
	if plaintext.Host != "backend.example:8080" {
		t.Errorf("PlaintextRemoteError.Host = %q, want the host:port that was refused", plaintext.Host)
	}
	// A malformed endpoint must NOT be reported as the security refusal — that is how a
	// repair job gets rendered as "your endpoint is insecure".
	if _, err := NormalizeBaseURL("https://user@backend.example", false); errors.As(err, &plaintext) {
		t.Error("a shape rejection was typed as the plaintext refusal")
	}
}

// allowInsecure is a PARAMETER because the answer differs by caller and must keep
// differing: startup honours --allow-insecure-backend, `/backend` passes false.
func TestNormalizeBaseURLInsecureOverrideIsScopedToPlaintext(t *testing.T) {
	got, err := NormalizeBaseURL("http://backend.example/", true)
	if err != nil {
		t.Fatalf("the escape hatch should authorize a plaintext remote endpoint: %v", err)
	}
	if got != "http://backend.example" {
		t.Errorf("NormalizeBaseURL = %q, want the canonical form", got)
	}
	// It authorizes PLAINTEXT and nothing else. A blanket "trust me" here would let the
	// insecure flag smuggle a credential-bearing endpoint past every other check.
	for _, in := range []string{
		"http://user:" + testSecret + "@backend.example",
		"http://backend.example?token=" + testSecret,
		"ftp://backend.example",
	} {
		if _, err := NormalizeBaseURL(in, true); err == nil {
			t.Errorf("allowInsecure widened past the plaintext rule for %q", in)
		}
	}
}
