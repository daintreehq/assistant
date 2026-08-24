package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
)

// pkce.go generates the per-attempt secrets for Authorization Code with PKCE.
//
// PKCE is what makes a public client safe without a client secret. The authorization
// code travels back through the system browser and a loopback HTTP hop — both places a
// second local process could observe it — and PKCE binds the code to the process that
// started the flow: only whoever holds the verifier can redeem it. `state` is a
// separate defence against a different attack (an unsolicited or cross-flow callback),
// and the two are deliberately not the same value.
//
// Everything here is memory-only and lives for one login attempt. Nothing in this file
// is ever written to disk, logged, or printed.

const (
	// stateBytes is the entropy behind `state`. The spec's floor is 32 bytes; there is
	// no reason to go under it and no cost to meeting it exactly.
	stateBytes = 32
	// verifierBytes yields 86 base64url characters, comfortably inside RFC 7636's
	// 43..128 range. Chosen over the 32-byte minimum because the verifier is the value
	// that actually stops a stolen code being redeemed, and the extra 32 bytes cost
	// nothing.
	verifierBytes = 64
)

// b64 is the unpadded base64url alphabet RFC 7636 requires. Padding is not merely
// optional here — a "=" would have to be percent-encoded in the query string, and
// providers differ on whether they compare before or after decoding.
var b64 = base64.RawURLEncoding

// randomString returns n cryptographically random bytes as unpadded base64url.
//
// A failure is fatal to the attempt and must never be papered over: continuing with a
// weak or partially-filled state would produce a login that LOOKS protected. crypto/rand
// does not return short reads, so any error here is a broken system, not a retry.
func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", wrapError(CodeExchangeFailed, "could not generate secure random data", err)
	}
	return b64.EncodeToString(buf), nil
}

// newState returns a fresh anti-forgery state value.
func newState() (string, error) { return randomString(stateBytes) }

// newVerifier returns a fresh PKCE code verifier.
func newVerifier() (string, error) { return randomString(verifierBytes) }

// challengeS256 derives the code challenge from a verifier.
//
// S256 only. The "plain" method is still in the RFC and is worthless for this client:
// it sends the verifier itself as the challenge, so anyone who can see the
// authorization request can redeem the code, which is the entire attack PKCE exists to
// stop. There is deliberately no parameter to choose the method.
func challengeS256(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return b64.EncodeToString(sum[:])
}

// sameState compares two state values in constant time.
//
// Constant time because a byte-by-byte comparison against an attacker-supplied callback
// leaks the expected value one position at a time, and the listener will happily accept
// as many attempts as the attacker sends. subtle.ConstantTimeCompare already returns 0
// for differing lengths, so length is not separately branched on.
func sameState(want, got string) bool {
	if want == "" || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(got)) == 1
}

// pkceAttempt is the memory-only secret set for exactly one login attempt.
type pkceAttempt struct {
	State     string
	Verifier  string
	Challenge string
}

// newPKCEAttempt generates a complete attempt.
func newPKCEAttempt() (pkceAttempt, error) {
	state, err := newState()
	if err != nil {
		return pkceAttempt{}, err
	}
	verifier, err := newVerifier()
	if err != nil {
		return pkceAttempt{}, err
	}
	return pkceAttempt{State: state, Verifier: verifier, Challenge: challengeS256(verifier)}, nil
}

// String deliberately renders no secret. An attempt struct is exactly the sort of value
// that ends up in a %v during debugging, and both fields it holds are live credentials
// for the duration of the flow.
func (p pkceAttempt) String() string {
	return fmt.Sprintf("pkceAttempt{state:%d bytes, verifier:%d bytes, method:S256}", len(p.State), len(p.Verifier))
}
