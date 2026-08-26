// Package accountfixture holds the canonical GET /v1/daintree/account response bodies,
// as literal JSON, shared by every package that renders or reasons about an account.
//
// It exists because the same contract is exercised from four places that used to write
// their own examples: the decoder's own tests (internal/backend), the manager that folds
// a response into local state (internal/auth), the `auth status --refresh` and login
// surfaces (internal/cli), and the embedded /account card (internal/commands). Four
// hand-written copies of a wire body drift, and the way they drift is the expensive one —
// each package's suite goes on passing against the shape ITS fixture describes while the
// real backend sends something none of them agree with. That is precisely how this CLI
// came to require `checked_at` on a response the backend never puts it on: both sides
// were green against their own fixtures.
//
// So the bodies live on disk, once, as the bytes a backend would actually send, and every
// test decodes the same file. A fixture that stops matching the server is a failure in
// every package at once, which is the point.
//
// It is ORDINARY code rather than a _test.go file because Go test files are not importable
// across packages, and go:embed is what lets one copy reach all four. Nothing outside a
// test imports it, so it is never linked into the shipped binary.
//
// Adding a case: drop the JSON in testdata/ and name it below. The well-formed set is
// what a healthy deployment returns; the Malformed set is the adversarial half, and every
// one of those must be refused as a LOCAL contract error — never as an account verdict,
// because a body we cannot parse is a statement about the backend and must never be
// rendered to a user as "you are not subscribed".
package accountfixture

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed testdata/*.json
var files embed.FS

// The well-formed bodies. Between them they cover every verdict the contract defines and
// both of its documents — the identity-only response that carries no entitlement fields
// at all, and the four shapes a completed lookup produces.
const (
	// Unverified is the identity-only response: the caller is authenticated and
	// entitlement was never looked up. It carries NO checked_at, which is the whole
	// point of it — see the package comment.
	Unverified = "unverified"
	// GrantedStandard and GrantedPro are fresh live grants, one per plan.
	GrantedStandard = "granted_standard"
	GrantedPro      = "granted_pro"
	// GrantedStaleCache is a grant served from an aged cache: the only combination in
	// which entitlement_stale may be true.
	GrantedStaleCache = "granted_stale_cache"
	// SubscriptionRequired is a good login with no plan behind it; SubscriptionInactive
	// is a good login whose plan has lapsed. They are deliberately separate because the
	// remedies differ — one is a checkout, the other is a billing page, and sending a
	// lapsed customer to buy a second subscription is how people pay twice.
	SubscriptionRequired = "subscription_required"
	SubscriptionInactive = "subscription_inactive"
)

// The adversarial bodies. Each violates exactly ONE rule, so a test that expects a
// refusal cannot pass for the wrong reason.
const (
	// The three ways an identity-only response can claim a lookup it never made. The
	// stale-false case is the subtle one and the reason entitlement_stale is decoded
	// through a pointer: as a plain bool it is indistinguishable from omission.
	BadUnverifiedCarriesCheckedAt  = "bad_unverified_carries_checked_at"
	BadUnverifiedCarriesStaleFalse = "bad_unverified_carries_stale_false"
	BadUnverifiedCarriesPlan       = "bad_unverified_carries_plan"
	// The support correlation id, absent and mis-cased. This endpoint is protected, so
	// there is always a subject to hash and neither can be a real response.
	BadSubjectHashAbsent    = "bad_subject_hash_absent"
	BadSubjectHashUppercase = "bad_subject_hash_uppercase"
	// Paid access to something unnamed.
	BadGrantedWithoutPlan = "bad_granted_without_plan"
	// A completed lookup with no time on it, and one whose time has no offset — an
	// instant the reader would have to guess a zone for.
	BadCheckedWithoutCheckedAt = "bad_checked_without_checked_at"
	BadCheckedAtNaive          = "bad_checked_at_naive"
	// A live answer calling itself stale.
	BadStaleWithoutCache = "bad_stale_without_cache"
	// A completed lookup that names no authority, and one that will not say whether its
	// answer is fresh. The second is the subtle one: absent staleness reads as "not
	// stale" through the accessor, so a body that never made the claim would be rendered
	// as though it had.
	BadCheckedWithoutSource = "bad_checked_without_source"
	BadCheckedWithoutStale  = "bad_checked_without_stale"
	// Year one parses cleanly and is Go's zero time, which every consumer reads as "no
	// check happened" — a completed lookup that silently loses its own timestamp.
	BadCheckedAtYearOne = "bad_checked_at_year_one"
	// The remaining two ways an identity-only response can carry a lookup's results.
	BadUnverifiedCarriesSource             = "bad_unverified_carries_source"
	BadUnverifiedCarriesSubscriptionStatus = "bad_unverified_carries_subscription_status"
	// An explicit null is not a plan. It decodes to the empty string, so this is the
	// same refusal as an absent plan_id — asserted separately because a reader may
	// reasonably wonder whether null is treated as a value.
	BadGrantedPlanNull = "bad_granted_plan_null"
	// An explicit null subject hash, for the same reason.
	BadSubjectHashNull = "bad_subject_hash_null"
	// A version this build has never seen, and a verdict outside the closed set.
	BadVersion       = "bad_version"
	BadAccessUnknown = "bad_access_unknown"
)

// WellFormed lists every body a correct backend may send. Iterate it rather than naming
// cases one at a time: a fixture added here is then automatically asserted to decode by
// whichever suites range over it, so a new contract shape cannot be added without every
// consumer proving it can read one.
func WellFormed() []string {
	return []string{
		Unverified,
		GrantedStandard,
		GrantedPro,
		GrantedStaleCache,
		SubscriptionRequired,
		SubscriptionInactive,
	}
}

// Malformed lists every body that must be refused as a local contract error.
func Malformed() []string {
	return []string{
		BadUnverifiedCarriesCheckedAt,
		BadUnverifiedCarriesStaleFalse,
		BadUnverifiedCarriesPlan,
		BadSubjectHashAbsent,
		BadSubjectHashUppercase,
		BadGrantedWithoutPlan,
		BadCheckedWithoutCheckedAt,
		BadCheckedAtNaive,
		BadStaleWithoutCache,
		BadCheckedWithoutSource,
		BadCheckedWithoutStale,
		BadCheckedAtYearOne,
		BadUnverifiedCarriesSource,
		BadUnverifiedCarriesSubscriptionStatus,
		BadGrantedPlanNull,
		BadSubjectHashNull,
		BadVersion,
		BadAccessUnknown,
	}
}

// SubjectHash is the subject_hash every fixture carries.
//
// It is the canonical cross-project test vector: the same literal appears in the backend
// and website suites, so a fixture here that disagreed with it would be caught by the
// hash test in internal/auth rather than by three services failing to correlate a support
// report months later.
const SubjectHash = "b4c864ea44cbb4a1"

// Subject is the raw subject SubjectHash is derived from. Present so a test can prove
// the derivation rather than restate its output.
const Subject = "8f14e45f-ea8b-4c1d-9b2a-0000feedface"

// Body returns the fixture's bytes, exactly as a backend would send them.
//
// It PANICS on an unknown name rather than returning an error. The only caller is a test
// naming a constant from this package, so an unknown name is a typo in the test itself,
// and making every call site handle an impossible error would bury the real assertions.
func Body(name string) []byte {
	b, err := files.ReadFile("testdata/" + name + ".json")
	if err != nil {
		panic(fmt.Sprintf("accountfixture: no such fixture %q (have: %s)", name, strings.Join(all(), ", ")))
	}
	return b
}

// String is Body as a string, for the many call sites that write it to an httptest
// response.
func String(name string) string { return string(Body(name)) }

func all() []string {
	entries, err := files.ReadDir("testdata")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	sort.Strings(names)
	return names
}

// Names returns every fixture on disk, sorted.
//
// Exported so a test can assert that WellFormed() and Malformed() between them account
// for ALL of them. Without that, a fixture added to testdata/ and forgotten in the lists
// above is a case nothing exercises — which is the same failure this package exists to
// prevent, one level up.
func Names() []string { return all() }
