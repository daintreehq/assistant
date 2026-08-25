package backend

import "strings"

// verify.go answers one question: can this deployment actually fund a turn?
//
// It is about the PROVIDER credential behind the backend, never the caller's account —
// two different questions that `doctor` reports as two different rows. The CLI never asks
// anyone for a provider key: the backend holds its own. `/v1/daintree/auth/verify`
// answers for whichever key the request WOULD spend, the backend's own on every normal
// install, and it is the one probe that can say "this deployment can actually run a turn"
// before a turn is spent finding out. `doctor` is its only caller.

// The stable machine-readable outcomes `/v1/daintree/auth/verify` answers with. The
// backend composes `detail` from these (`_REASON_DETAIL` in its daintree_auth.py) and
// its own comment says the CLI is to branch on the reason "rather than reimplementing
// the arithmetic" — which is exactly what these are for. Detail is prose and may be
// reworded; a reason may not.
//
// An UNRECOGNISED reason is not an error. A newer backend can name a condition this
// build has no copy for, and the honest rendering is to repeat the reason it gave
// rather than to pick the nearest one we know and assert it.
const (
	ReasonOK               = "ok"
	ReasonProviderRejected = "provider_rejected"
	ReasonCreditsExhausted = "credits_exhausted"
)

// IsUsable reports whether the account behind a VALID key can actually fund a turn.
//
// The backend answers this directly now (`usable`, with a stable `reason`), and its
// answer is the one to trust: it judges conservatively — only a positively reported
// zero-or-negative balance counts as exhausted, so an unlimited or pay-as-you-go key,
// which reports no limit at all, stays usable.
//
// The LimitRemaining fallback is for a backend that predates the field. It cannot simply
// be deleted in favour of the new one: `usable` is a pointer precisely because absent
// must not decode as false, and treating "not reported" as "unusable" would warn every
// user of an older deployment that their working key has no credit.
func (v KeyVerification) IsUsable() bool {
	if v.Usable != nil {
		return *v.Usable
	}
	// A reason that NAMES a failure settles it even with no `usable` flag and no
	// balance. Without this the fallback below reads "nothing was reported" and answers
	// yes — so `{"valid":true,"reason":"credits_exhausted"}` would pass doctor green
	// while its own machine-readable half said the account is spent. Fail-open on a
	// field we just declared stable is the wrong default; the flag is a pointer to keep
	// ABSENCE from meaning false, not to let a stated failure be ignored.
	if v.Reason != "" && v.Reason != ReasonOK {
		return false
	}
	return v.LimitRemaining == nil || *v.LimitRemaining > 0
}

// ScrubKey removes every occurrence of a secret from text destined for a human.
//
// A backend we do not control can echo the Authorization header into an error body,
// and that text reaches the attached session sheet and the 0600 debug log. The attached session renders
// on the NORMAL screen buffer, so a leaked key would persist in the host's scrollback
// long after the session. Cheap insurance at the one boundary where untrusted text
// meets a known secret — and it costs nothing on the normal path, where there is no
// caller key for a backend to echo in the first place.
func ScrubKey(text, key string) string {
	key = strings.TrimSpace(key)
	if key == "" || !strings.Contains(text, key) {
		return text
	}
	return strings.ReplaceAll(text, key, "«redacted key»")
}
