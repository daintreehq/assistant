package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
)

// status.go is the redacted account view every surface renders: the `auth status`
// command, the `--json` event stream, and Daintree's settings panel.
//
// The rule is absolute and it is why this type exists at all rather than callers
// reaching into the Manager: NOTHING here is a credential. No access token, no refresh
// token, no authorization URL. There is deliberately no field that could carry one and
// no method that returns one, so a future caller cannot render a token by accident —
// the type simply has nowhere to put it.
//
// The one identifier that is useful for support is the subject HASH, never the raw user
// id: it is stable enough to correlate two reports and useless for anything else.

// Status is the redacted account snapshot.
type Status struct {
	// State is the typed local state every consumer branches on.
	State State `json:"state"`
	// Authenticated is the coarse answer, derived from State so the two cannot disagree.
	Authenticated bool `json:"authenticated"`
	// Environment and BackendURL say WHICH deployment this account belongs to. Both are
	// non-secret configuration, and both matter: the same person can hold a staging and
	// a production session, and a status that did not name the environment could not
	// tell them apart.
	Environment string `json:"environment,omitempty"`
	BackendURL  string `json:"backendUrl,omitempty"`
	// Configured and AuthRequired describe the DEPLOYMENT, not the credential, and they
	// are the pair a native consumer needs to tell three situations apart that State
	// alone renders identically:
	//
	//   - accounts not configured here, anonymous requests served (configured false)
	//   - accounts configured but optional (configured true, authRequired false)
	//   - accounts required (both true)
	//
	// POINTERS because there is a fourth answer — "we could not ask" — and a bare false
	// would decode as the first one. A consumer that rendered an unreachable backend as
	// "this deployment has no accounts" would tell someone their sign-in is unnecessary
	// during an outage. Absent means unknown; branch accordingly.
	Configured   *bool `json:"configured,omitempty"`
	AuthRequired *bool `json:"authRequired,omitempty"`
	// Email is display-only and may be absent. It is never a database join key and is
	// never persisted — it comes from the backend session endpoint each time.
	Email string `json:"email,omitempty"`
	// SubjectHash is a stable, non-reversible correlation id for support. The raw
	// subject is deliberately not carried.
	SubjectHash string `json:"subjectHash,omitempty"`
	// Plan and EntitlementSource describe the billing verdict, when one is known.
	Plan               string `json:"planId,omitempty"`
	EntitlementSource  string `json:"entitlementSource,omitempty"`
	EntitlementStale   bool   `json:"entitlementStale,omitempty"`
	UsageRemainingText string `json:"usageRemaining,omitempty"`
	// AccessExpiresAt is when the CURRENT access token lapses. It is a time, never the
	// token: the useful question is "how long until this rotates", and rendering it as a
	// relative duration answers that without disclosing anything.
	AccessExpiresAt *time.Time `json:"accessExpiresAt,omitempty"`
	// SessionMaxAgeSeconds is the provider's session policy, for the "you will be asked
	// to sign in again in N days" line.
	SessionMaxAgeSeconds int `json:"sessionMaxAgeSeconds,omitempty"`
	// StorageTier says where the credential actually lives. TierMemory MUST be surfaced:
	// the session works and then disappears on exit.
	StorageTier StorageTier `json:"storageTier"`
	// LastVerifiedAt is when the backend last confirmed this SESSION — any protected
	// request succeeding counts, so it answers "is this login still good".
	LastVerifiedAt *time.Time `json:"lastVerifiedAt,omitempty"`
	// EntitlementCheckedAt is when the billing answer itself was established, as the
	// BACKEND reported it.
	//
	// Separate from LastVerifiedAt because the two drift apart in the direction that
	// misleads. Any successful protected call moves LastVerifiedAt, so a session
	// confirmed a second ago can sit beside a plan that was last looked up an hour ago
	// — and rendering only the newer of the two would present a stale entitlement as
	// freshly checked. That is precisely the claim a person needs to be able to
	// distrust, which is also why EntitlementStale exists.
	EntitlementCheckedAt *time.Time `json:"entitlementCheckedAt,omitempty"`
	// LastErrorCode is the stable code of the most recent failure, never its message —
	// a message can quote a provider, and a code cannot.
	LastErrorCode string `json:"lastErrorCode,omitempty"`
	// Links are the safe, validated account and subscribe URLs.
	Links StatusLinks `json:"links,omitempty"`
	// AuthRevision is the shared marker, for correlating what a daemon believes with
	// what a terminal believes.
	AuthRevision string `json:"authRevision,omitempty"`
}

// StatusLinks are the browser destinations a caller may open. They come from the
// validated manifest, so they are already pinned to a Daintree origin.
type StatusLinks struct {
	Account   string `json:"account,omitempty"`
	Subscribe string `json:"subscribe,omitempty"`
}

// backendCodeOf returns the stable ACCOUNT code carried by a backend error, or "".
//
// The two taxonomies are deliberately disjoint (see internal/backend/account.go), so a
// caller can branch on either without ambiguity — but status has to read both, because
// it reports whatever last went wrong and that can come from either side.
func backendCodeOf(err error) string {
	var be *backend.Error
	if errors.As(err, &be) && be != nil {
		return be.Code
	}
	return ""
}

// sanitizeURLForDisplay strips userinfo from a URL before it can be rendered.
//
// It lives HERE, at the point Status is built, rather than at each surface that prints
// one. The type's whole premise is that nothing in it is a credential, and the backend
// URL is operator-supplied: nothing upstream rejects userinfo in an https:// endpoint, so
// `DAINTREE_BACKEND_URL=https://user:secret@example.test` would otherwise put `secret`
// into a struct that goes to stdout, to the NDJSON event stream, and into a support
// bundle. Sanitizing at each caller was how `auth login --json` came to emit it while
// `auth status --json` did not.
func sanitizeURLForDisplay(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("«redacted»")
	return u.String()
}

// SubjectHash derives the support correlation id from a subject.
//
// Truncated to 16 hex characters: long enough that two accounts will not collide in any
// realistic support queue, short enough to read aloud, and one-way so it cannot be
// turned back into the identifier it came from.
func SubjectHash(subject string) string {
	if subject == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("daintree-assistant-subject:" + subject))
	return hex.EncodeToString(sum[:8])
}

// Status builds the redacted snapshot from what this process currently knows.
//
// It performs NO I/O. A status call must be answerable while the network is down, while
// the keychain is locked, and while another process holds the credential lock —
// precisely the situations in which someone asks what is going on.
func (m *Manager) Status() Status {
	// One read of the shared marker, used for BOTH the reported revision and the
	// snapshot's staleness check. Reading it twice could report one marker while
	// validating against another, and it happens outside the lock because it is a file
	// read (see accountSnapshotLocked).
	marker := m.revision.Current()

	m.mu.Lock()
	state, access, lastErr, tier := m.state, m.access, m.lastErr, m.tier
	verified := m.lastVerifiedAt
	snap, hasSnap := m.accountSnapshotLocked(marker)
	m.mu.Unlock()

	s := Status{
		State:         state,
		Authenticated: state.SignedIn(),
		BackendURL:    sanitizeURLForDisplay(m.backendURL),
		StorageTier:   tier,
		AuthRevision:  marker.String(),
	}
	if !access.ExpiresAt.IsZero() {
		t := access.ExpiresAt
		s.AccessExpiresAt = &t
	}
	if verified != nil {
		t := *verified
		s.LastVerifiedAt = &t
	}
	// The account fields the backend supplied. They are populated ONLY from a snapshot
	// belonging to the current identity — accountSnapshotLocked enforces that — so a
	// logout cannot leave the previous account's email on a status line, and neither can
	// a verdict that arrived for a session this process has moved on from.
	//
	// A RETAINED snapshot is deliberately still rendered when a later check could not
	// reach the backend. Blanking the plan on an outage would tell someone their
	// subscription had gone away because their wifi did; the honest reading is "this is
	// what we last knew, and here is when" — which is what LastVerifiedAt is for.
	if hasSnap {
		s.Email = snap.email
		s.SubjectHash = snap.subjectHash
		s.Plan = snap.planID
		s.EntitlementSource = snap.entitlementSource
		s.EntitlementStale = snap.entitlementStale
		if !snap.checkedAt.IsZero() {
			t := snap.checkedAt
			s.EntitlementCheckedAt = &t
		}
	}
	if lastErr != nil {
		// The CODE only. A message can quote a provider's error_description, which is
		// exactly the text this package refuses to repeat elsewhere.
		//
		// BOTH taxonomies are consulted. lastErr is now frequently a *backend.Error —
		// the account verdicts reach here through ApplyBackendVerdict — and CodeOf only
		// understands this package's own errors, so reading it alone rendered every
		// backend verdict as the literal string "unknown". A stable code the backend
		// already defined, replaced by the one word that carries no information.
		switch {
		case CodeOf(lastErr) != "":
			s.LastErrorCode = CodeOf(lastErr)
		case backendCodeOf(lastErr) != "":
			s.LastErrorCode = backendCodeOf(lastErr)
		default:
			s.LastErrorCode = "unknown"
		}
	}
	return s
}

// WithManifest fills in the environment and links from a validated manifest.
func (s Status) WithManifest(m *Manifest) Status {
	if m == nil {
		return s
	}
	s.Environment = m.Environment
	s.Links = StatusLinks{Account: m.AccountURL, Subscribe: m.SubscribeURL}
	s.SessionMaxAgeSeconds = m.SessionPolicy.SessionMaxAgeSeconds
	configured, required := m.Configured == nil || *m.Configured, m.Required
	s.Configured, s.AuthRequired = &configured, &required
	return s
}

// WithAvailability fills in what the deployment says about accounts.
//
// Separate from WithManifest because it is the only one that can be called at all when
// there is no valid manifest — which is exactly the unconfigured case, the one this pair
// of fields exists to describe. A caller applies whichever it has; on a normal
// deployment both run and agree.
//
// An availability that is not Known writes NOTHING. Leaving both fields absent is the
// honest answer for a backend we could not reach, and stamping false would say the
// deployment has no accounts on the strength of a network failure.
func (s Status) WithAvailability(a Availability) Status {
	if !a.Known {
		return s
	}
	configured, required := a.Configured, a.Required
	s.Configured, s.AuthRequired = &configured, &required
	// The environment is filled in only if nothing has already set it. A validated
	// manifest is the better source — it has been through every check — and on a normal
	// deployment WithManifest runs first; overwriting there would let the pre-validation
	// copy win for no reason. This is the fallback for the unconfigured case, where
	// there is no validated manifest at all.
	if s.Environment == "" {
		s.Environment = a.Environment
	}
	// A KNOWN "no accounts here" overrides whatever the local credential state says, and
	// this is the single rule that makes the forbidden rendering unreachable rather than
	// merely unlikely.
	//
	// The state is derived from the credential STORE, which knows nothing about the
	// deployment. So a machine that signed in while accounts existed, on a deployment
	// that has since turned them off, reports `signed_in_unverified` and
	// `authenticated:true` beside `configured:false` — and if the store entry is missing
	// it reports `signed_out` and sends the user to a login no endpoint will answer, and
	// if the store is locked it reports an outage. Three different wrong answers from
	// three different local accidents, none of which the deployment cares about.
	//
	// Overriding here catches all three at the one point every surface reads.
	if !configured {
		s.State = StateAccountsUnavailable
		s.Authenticated = false
		// The account fields go with it. They can only have come from a snapshot taken
		// while this deployment DID have accounts, and an email beside "this backend has
		// no accounts" is not a partial truth — it is two statements that cannot both
		// hold, on the one line someone reads to find out what is going on.
		// EVERY session-derived field goes, not only the account ones. A block that says
		// "this backend has no accounts" beside "verified 10 minutes ago", "session
		// renews in 58m" or a leftover entitlement error code is not partially right —
		// each of those describes a session that, by the deployment's own answer, does
		// not exist. They can only be left over from when the deployment did have
		// accounts, or from a different endpoint entirely.
		s.Email, s.SubjectHash, s.Plan = "", "", ""
		s.EntitlementSource, s.EntitlementStale = "", false
		s.EntitlementCheckedAt = nil
		s.AccessExpiresAt, s.LastVerifiedAt = nil, nil
		s.LastErrorCode = ""
		s.SessionMaxAgeSeconds = 0
		s.Links = StatusLinks{}
	}
	return s
}

// AccessExpiresIn returns how long the current access token has left, or zero when
// there is no token or no known expiry. Callers render this as a relative duration.
func (s Status) AccessExpiresIn(now time.Time) time.Duration {
	if s.AccessExpiresAt == nil {
		return 0
	}
	d := s.AccessExpiresAt.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}
