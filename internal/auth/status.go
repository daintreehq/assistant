package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"time"
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
	// LastVerifiedAt is when the backend last confirmed this session.
	LastVerifiedAt *time.Time `json:"lastVerifiedAt,omitempty"`
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
	m.mu.Lock()
	state, access, lastErr, tier := m.state, m.access, m.lastErr, m.tier
	m.mu.Unlock()

	s := Status{
		State:         state,
		Authenticated: state.SignedIn(),
		BackendURL:    m.backendURL,
		StorageTier:   tier,
		AuthRevision:  m.revision.Current().String(),
	}
	if !access.ExpiresAt.IsZero() {
		t := access.ExpiresAt
		s.AccessExpiresAt = &t
	}
	if lastErr != nil {
		// The CODE only. A message can quote a provider's error_description, which is
		// exactly the text this package refuses to repeat elsewhere.
		if c := CodeOf(lastErr); c != "" {
			s.LastErrorCode = c
		} else {
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
