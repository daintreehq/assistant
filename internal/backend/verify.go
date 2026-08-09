package backend

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// CheckSignIn is the shared sign-in verification both entry points run — the startup
// flow (internal/cli) and the cockpit's `/login` (internal/app) — so the two can't
// diverge on what "verified" means.
//
// It answers two DIFFERENT questions, in order, because they fail differently:
//
//  1. Is this a Daintree backend we can talk to, and is the header well-formed?
//     (`/v1/daintree/capabilities` — a hard gate. A wrong URL or a mangled key stops here.)
//  2. Does the provider actually accept this key? (`/v1/daintree/auth/verify` — the only
//     check that can catch a key that is well-formed but wrong, revoked, or unfunded.)
//
// The second is a hard gate too WHEN THE BACKEND SUPPORTS IT. Older backends — including
// the currently deployed one — have no verify endpoint, and refusing to sign in against
// them would be a self-inflicted outage, so an unsupported endpoint downgrades to a
// warning rather than a failure. `warning` is non-empty exactly in that case.
//
// A provider we could not REACH is likewise a warning, never a rejection: "we could
// not check" must not be reported as "your key is bad", which would send the user
// hunting for a problem they do not have. Cancellation and timeout are the exception —
// they are hard failures, because neither is evidence about the key and neither is
// consent to persist an unverified one.
//
// A recognised key with no credit left also warns: it is a real, fixable state, and
// refusing the sign-in would leave no way to configure the CLI while topping up.
func CheckSignIn(ctx context.Context, c *Client) (verification KeyVerification, warning string, err error) {
	if _, err := c.Capabilities(ctx); err != nil {
		return KeyVerification{}, "", err
	}

	v, err := c.VerifyKey(ctx)
	switch {
	// Cancellation and timeout are checked BEFORE the soft cases. Neither is evidence
	// about the key: the user pressed Ctrl-C, or we gave up waiting. Treating them as
	// "provider unreachable" meant a verify endpoint that merely hung ended up
	// persisting and swapping in a completely unverified sign-in.
	case ctx.Err() != nil, errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return KeyVerification{}, "", fmt.Errorf("key verification did not complete: %w", err)
	case errors.Is(err, ErrVerifyUnsupported):
		return KeyVerification{}, "this backend can't check whether the key actually works — a bad key will surface on your first message", nil
	case err != nil:
		var berr *Error
		if errors.As(err, &berr) && berr.IsAuth() {
			// Our own door rejecting the header at this point means the key is
			// structurally broken; that IS a definite verdict.
			return KeyVerification{}, "", fmt.Errorf("the backend rejected the key: %s", berr.Message)
		}
		return KeyVerification{}, "could not confirm the key with the provider (" + err.Error() + ")", nil
	case !v.Valid:
		return v, "", fmt.Errorf("%w%s", ErrKeyRejected, verdictDetail(v))
	// A key the provider recognises but that has nothing left to spend fails every turn
	// just as surely as a wrong one. Warn rather than reject: "recognised but empty" is
	// a real, fixable state, and refusing the sign-in would leave no way to configure
	// the CLI while topping the account up.
	case v.LimitRemaining != nil && *v.LimitRemaining <= 0:
		return v, "the provider accepted this key but reports no credit remaining — turns will fail until the account is topped up", nil
	}
	return v, "", nil
}

// ScrubKey removes every occurrence of a secret from text destined for a human.
//
// A backend we do not control can echo the Authorization header into an error body,
// and that text reaches the cockpit sheet and the 0600 debug log. The cockpit renders
// on the NORMAL screen buffer, so a leaked key would persist in the host's scrollback
// long after the session. Cheap insurance at the one boundary where untrusted text
// meets a known secret.
func ScrubKey(text, key string) string {
	key = strings.TrimSpace(key)
	if key == "" || !strings.Contains(text, key) {
		return text
	}
	return strings.ReplaceAll(text, key, "«redacted key»")
}

// ErrKeyRejected is the definite verdict: the upstream provider does not accept this
// key. A sentinel rather than a bare message so both sign-in surfaces can render it as
// the actionable thing it is ("this key doesn't work") instead of burying it inside a
// generic "could not verify <url>" wrapper, which reads like a connectivity problem.
var ErrKeyRejected = errors.New("the provider rejected this API key")

// verdictDetail appends the backend's own wording when it adds anything beyond the
// sentinel's, so a provider-specific reason (expired, over quota) is not thrown away.
func verdictDetail(v KeyVerification) string {
	d := strings.TrimSpace(v.Detail)
	if d == "" || strings.EqualFold(strings.TrimRight(d, "."), ErrKeyRejected.Error()) ||
		strings.EqualFold(d, "The provider rejected this API key.") {
		return ""
	}
	return " — " + d
}
