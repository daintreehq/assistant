package auth

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
)

// RFC 7636's own worked example. Pinning it proves the challenge derivation is the one
// providers implement, not merely self-consistent with our own hashing.
func TestChallengeMatchesTheRFC7636Vector(t *testing.T) {
	const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"
	const want = "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"
	if got := challengeS256(verifier); got != want {
		t.Fatalf("challengeS256 = %q, want %q", got, want)
	}
}

func TestTheVerifierIsInsideTheLegalLengthRange(t *testing.T) {
	v, err := newVerifier()
	if err != nil {
		t.Fatalf("newVerifier: %v", err)
	}
	if len(v) < 43 || len(v) > 128 {
		t.Fatalf("verifier is %d characters; RFC 7636 requires 43..128", len(v))
	}
}

// A "=" would have to be percent-encoded in the query string, and providers differ on
// whether they compare before or after decoding.
func TestSecretsUseUnpaddedBase64URL(t *testing.T) {
	for name, gen := range map[string]func() (string, error){"state": newState, "verifier": newVerifier} {
		v, err := gen()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if strings.ContainsAny(v, "=+/") {
			t.Errorf("%s %q is not unpadded base64url", name, v)
		}
	}
	c := challengeS256("anything")
	if strings.ContainsAny(c, "=+/") {
		t.Errorf("challenge %q is not unpadded base64url", c)
	}
	// And it really is the SHA-256 digest, not something else of the right shape.
	sum := sha256.Sum256([]byte("anything"))
	if c != base64.RawURLEncoding.EncodeToString(sum[:]) {
		t.Error("the challenge is not base64url(sha256(verifier))")
	}
}

func TestStateMeetsTheEntropyFloor(t *testing.T) {
	s, err := newState()
	if err != nil {
		t.Fatalf("newState: %v", err)
	}
	// 32 bytes -> 43 unpadded base64url characters.
	if len(s) < 43 {
		t.Fatalf("state is %d characters, which is under 32 bytes of entropy", len(s))
	}
}

// Two attempts must never share a secret; a repeat would let one flow's callback settle
// another's.
func TestEveryAttemptGetsFreshSecrets(t *testing.T) {
	seen := make(map[string]bool, 128)
	for i := 0; i < 64; i++ {
		a, err := newPKCEAttempt()
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if a.State == a.Verifier {
			t.Fatal("state and verifier are the same value — they defend against different attacks")
		}
		if a.Challenge != challengeS256(a.Verifier) {
			t.Fatal("the challenge does not match the verifier")
		}
		for _, v := range []string{a.State, a.Verifier} {
			if seen[v] {
				t.Fatalf("secret %q repeated across attempts", v)
			}
			seen[v] = true
		}
	}
}

func TestStateComparisonRejectsEmptyAndMismatched(t *testing.T) {
	if sameState("", "") {
		t.Error("two empty states compared equal — an absent state must never match")
	}
	if sameState("abc", "") || sameState("", "abc") {
		t.Error("an empty state matched a non-empty one")
	}
	if sameState("abc", "abcd") || sameState("abcd", "abc") {
		t.Error("states of different lengths compared equal")
	}
	if !sameState("abc", "abc") {
		t.Error("identical states did not compare equal")
	}
}

// An attempt struct is exactly the sort of value that ends up in a %v while debugging,
// and both fields it holds are live credentials for the duration of the flow.
func TestAnAttemptNeverPrintsItsSecrets(t *testing.T) {
	a, err := newPKCEAttempt()
	if err != nil {
		t.Fatalf("newPKCEAttempt: %v", err)
	}
	rendered := a.String()
	if strings.Contains(rendered, a.State) || strings.Contains(rendered, a.Verifier) {
		t.Fatalf("String() leaked a secret: %q", rendered)
	}
}
