package backend

import "strings"

// verify.go holds what survives of key verification now that there is no sign-in.
//
// The CLI never asks for a credential: the backend holds its own and serves a request
// with no Authorization header. But `/v1/daintree/auth/verify` did not go away — it
// answers for whichever key the request WOULD spend, the backend's own on every normal
// install — so it is still the one probe that can say "this deployment can actually run
// a turn" before a turn is spent finding out. `doctor` is its caller now.

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
