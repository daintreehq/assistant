package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/backend/accountfixture"
)

// accountcontract_test.go drives the version-1 account contract from the SHARED canonical
// bodies in internal/backend/accountfixture, rather than from JSON written inline here.
//
// The distinction is the whole reason the fixtures were extracted. This package and the
// backend each used to pass against their own hand-written examples of the same response,
// which is how the CLI came to demand `checked_at` on a body the server never puts it on:
// both suites were green, against documents that did not match. Anything asserted here is
// asserted about the same bytes internal/auth, internal/cli and internal/commands decode.

// serveBody returns a client whose account endpoint answers with exactly these bytes.
func serveBody(t *testing.T, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AccountStatusPath {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return NewClient(ClientConfig{BaseURL: srv.URL})
}

// The response the whole re-audit turned on: identity established, entitlement never
// looked up. It has no checked_at because there was nothing to timestamp, and a decoder
// that demands one turns a correct backend answer into "could not verify".
func TestTheIdentityOnlyResponseIsValid(t *testing.T) {
	got, err := serveBody(t, accountfixture.String(accountfixture.Unverified)).Account(context.Background())
	if err != nil {
		t.Fatalf("the canonical identity-only response was refused: %v", err)
	}
	if got.Access != AccessUnverified {
		t.Fatalf("access = %q, want %q", got.Access, AccessUnverified)
	}
	if got.SubjectHash != accountfixture.SubjectHash {
		t.Errorf("subject hash = %q, want %q", got.SubjectHash, accountfixture.SubjectHash)
	}
	// Zero, not the epoch and not "now". A caller renders a time only when it has one
	// (auth/status.go guards on IsZero), so a fabricated instant here would surface as a
	// "plan checked" line for a check that never happened.
	if !got.CheckedAtTime.IsZero() {
		t.Errorf("CheckedAtTime = %v, want the zero time — nothing was checked", got.CheckedAtTime)
	}
	if got.CheckedAt != "" {
		t.Errorf("CheckedAt = %q, want empty", got.CheckedAt)
	}
	// Neither granted nor refused. Both readings are wrong and both are damaging: one
	// hands out paid access, the other sends a paying customer to a checkout page.
	if got.Granted() {
		t.Error("an unverified response reported itself as granted")
	}
	if got.Stale() {
		t.Error("an unverified response reported a staleness it never established")
	}
	if got.PlanID != "" || got.EntitlementSource != "" || got.SubscriptionStatus != "" {
		t.Errorf("entitlement fields leaked into an unverified response: %+v", got)
	}
}

// Every well-formed fixture decodes, and each one's checked/unchecked halves agree with
// its verdict. Ranging over the list rather than naming cases means a fixture added for
// another package's benefit is proved decodable here too.
func TestEveryWellFormedFixtureDecodes(t *testing.T) {
	for _, name := range accountfixture.WellFormed() {
		t.Run(name, func(t *testing.T) {
			got, err := serveBody(t, accountfixture.String(name)).Account(context.Background())
			if err != nil {
				t.Fatalf("refused a well-formed body: %v", err)
			}
			if got.SubjectHash != accountfixture.SubjectHash {
				t.Errorf("subject hash = %q, want %q", got.SubjectHash, accountfixture.SubjectHash)
			}
			if got.Access == AccessUnverified {
				if !got.CheckedAtTime.IsZero() {
					t.Errorf("unverified carried a check time %v", got.CheckedAtTime)
				}
				return
			}
			// A completed lookup always happened at a time, and that time is what tells a
			// user how much to trust the plan line above it.
			if got.CheckedAtTime.IsZero() {
				t.Errorf("%s carried no check time", got.Access)
			}
			if _, off := got.CheckedAtTime.Zone(); got.CheckedAt != "" && !strings.ContainsAny(got.CheckedAt, "Zz+") && off == 0 {
				t.Errorf("checked_at %q parsed without an offset", got.CheckedAt)
			}
		})
	}
}

// Every adversarial body is refused, and refused as a LOCAL contract error.
//
// The code matters more than the refusal. CodeAccountContractInvalid sits deliberately
// outside accountCodes, so it cannot reach AuthRemedy, cannot clear a credential and
// cannot be rendered as a billing verdict. A malformed body is a statement about the
// BACKEND; the one thing it must never become is "you are signed out" or "you are not
// subscribed".
func TestEveryMalformedFixtureIsALocalContractError(t *testing.T) {
	for _, name := range accountfixture.Malformed() {
		t.Run(name, func(t *testing.T) {
			got, err := serveBody(t, accountfixture.String(name)).Account(context.Background())
			if err == nil {
				t.Fatalf("accepted a malformed body: %+v", got)
			}
			be, ok := err.(*Error)
			if !ok {
				t.Fatalf("error is %T, want *backend.Error", err)
			}
			if be.Code != CodeAccountContractInvalid {
				t.Fatalf("code = %q, want %q", be.Code, CodeAccountContractInvalid)
			}
			// The reason is OUR words about which rule failed. A backend that echoed a
			// bearer into a field would otherwise have it quoted back through the error,
			// into the debug log and into a support bundle.
			if strings.Contains(be.Message, accountfixture.Subject) {
				t.Error("the contract error quoted the offending value")
			}
			// Nothing is returned alongside a refusal. A half-decoded status is a status
			// a caller could render.
			if got != (AccountStatus{}) {
				t.Errorf("a refused body still returned data: %+v", got)
			}
		})
	}
}

// The two fixture lists must account for every file on disk. A fixture added to testdata/
// and left out of both lists is a case nothing exercises — the same silent-gap failure the
// package exists to prevent, one level up.
func TestEveryFixtureOnDiskIsClassified(t *testing.T) {
	listed := map[string]bool{}
	for _, n := range accountfixture.WellFormed() {
		listed[n] = true
	}
	for _, n := range accountfixture.Malformed() {
		if listed[n] {
			t.Errorf("%s is listed as both well-formed and malformed", n)
		}
		listed[n] = true
	}
	for _, n := range accountfixture.Names() {
		if !listed[n] {
			t.Errorf("testdata/%s.json is in neither WellFormed() nor Malformed() — nothing exercises it", n)
		}
	}
}

// entitlement_stale is decoded through a POINTER so that absent and false stay different
// facts. This is the assertion that would fail if someone "simplified" it back to a bool:
// the identity-only rule below it becomes unenforceable, because `entitlement_stale: false`
// — the sentence "we checked, and the answer is current" — would decode identically to a
// response that said nothing at all.
func TestStalenessDistinguishesAbsentFromFalse(t *testing.T) {
	var absent AccountStatus
	if err := json.Unmarshal(accountfixture.Body(accountfixture.Unverified), &absent); err != nil {
		t.Fatal(err)
	}
	if absent.EntitlementStale != nil {
		t.Error("an omitted entitlement_stale decoded as present")
	}

	var explicit AccountStatus
	if err := json.Unmarshal(accountfixture.Body(accountfixture.GrantedStandard), &explicit); err != nil {
		t.Fatal(err)
	}
	if explicit.EntitlementStale == nil {
		t.Fatal("an explicit entitlement_stale:false decoded as absent")
	}
	if *explicit.EntitlementStale {
		t.Error("entitlement_stale:false decoded as true")
	}
	// Both read as "not stale" through the accessor, which is what every caller uses.
	if absent.Stale() || explicit.Stale() {
		t.Error("Stale() reported staleness for a body that claimed none")
	}
}

// A stale cached grant is the one combination in which entitlement_stale may be true, and
// it must survive intact: "we believe you are subscribed, as of some hours ago" is a
// materially different claim from a fresh one, and the user is shown the difference.
func TestAStaleCachedGrantKeepsBothItsPlanAndItsDoubt(t *testing.T) {
	got, err := serveBody(t, accountfixture.String(accountfixture.GrantedStaleCache)).Account(context.Background())
	if err != nil {
		t.Fatalf("refused a stale cached grant: %v", err)
	}
	if !got.Granted() {
		t.Error("a stale cached grant did not report as granted — an aged cache is still an answer")
	}
	if !got.Stale() {
		t.Error("a stale cached grant did not report as stale")
	}
	if got.EntitlementSource != EntitlementSourceCache {
		t.Errorf("source = %q, want %q", got.EntitlementSource, EntitlementSourceCache)
	}
	if got.CheckedAtTime.IsZero() {
		t.Error("a stale grant carried no check time — the time is what makes the doubt legible")
	}
}

// A checked verdict must name its authority AND say whether its answer is fresh — on all
// three, not only on `granted`.
//
// This is the inverse of what an earlier reading of the contract allowed, and the reason
// for the change is the freshness half rather than the source half. Stale() reports an
// ABSENT entitlement_stale as false, so a body that never made a freshness claim would be
// rendered as "we checked, and this is current". The backend types both fields as
// non-optional and refuses a website answer missing either, so no healthy deployment can
// produce one of these.
func TestACheckedVerdictMustNameItsAuthorityAndItsFreshness(t *testing.T) {
	const base = `{"version":1,"subject_hash":"` + accountfixture.SubjectHash + `","access":"subscription_required"`
	cases := map[string]string{
		"no source and no freshness": base + `,"checked_at":"2026-08-25T09:14:00Z"}`,
		"no source":                  base + `,"entitlement_stale":false,"checked_at":"2026-08-25T09:14:00Z"}`,
		"no freshness":               base + `,"entitlement_source":"polar","checked_at":"2026-08-25T09:14:00Z"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := serveBody(t, body).Account(context.Background())
			if err == nil {
				t.Fatalf("accepted a checked verdict with an incomplete entitlement block: %+v", got)
			}
			be, ok := err.(*Error)
			if !ok || be.Code != CodeAccountContractInvalid {
				t.Fatalf("err = %v, want a local %s", err, CodeAccountContractInvalid)
			}
		})
	}
}

// The email bound is measured in BYTES here and in CODE POINTS at the backend, so the
// local ceiling has to be the widest a server-valid string can be rather than the same
// number. A 320-character address of two-byte runes is exactly what a 320-byte cap would
// have refused — as a CONTRACT failure, which reads to the user as "we could not verify
// your account" over their own email address.
func TestALongUnicodeEmailIsNotAContractFailure(t *testing.T) {
	body := `{"version":1,"subject_hash":"` + accountfixture.SubjectHash +
		`","access":"unverified","email":"` + strings.Repeat("é", 320) + `"}`
	if _, err := serveBody(t, body).Account(context.Background()); err != nil {
		t.Fatalf("a 320-character email was refused for being %d bytes: %v", 320*2, err)
	}
}

// The bound on subscription_status is deliberately WIDER than the backend's own 64, and
// this pins the asymmetry as intentional. A reader that accepts more than the writer can
// emit never refuses a legitimate response; one that matched exactly would turn a future
// server-side widening into "could not verify your plan" on every older build.
func TestSubscriptionStatusAcceptsMoreThanTheBackendCanSend(t *testing.T) {
	const backendCap = 64
	if maxAccountSubscriptionStatusBytes <= backendCap {
		t.Fatalf("the local bound (%d) is not wider than the backend's (%d) — an older build would "+
			"start refusing responses the moment the server widened this field",
			maxAccountSubscriptionStatusBytes, backendCap)
	}
	body := `{"version":1,"subject_hash":"` + accountfixture.SubjectHash +
		`","access":"subscription_inactive","subscription_status":"` + strings.Repeat("p", backendCap+1) +
		`","entitlement_source":"polar","entitlement_stale":false,"checked_at":"2026-08-25T09:14:00Z"}`
	if _, err := serveBody(t, body).Account(context.Background()); err != nil {
		t.Fatalf("refused a status longer than the backend's cap but inside ours: %v", err)
	}
}

// A check time in the future or long past is DATA, not a contract violation: clocks drift,
// and refusing over skew would convert a working session into "could not verify". The
// contract's job is shape; freshness is a judgement made where it is rendered.
func TestClockSkewInTheCheckTimeIsNotAContractFailure(t *testing.T) {
	for _, at := range []time.Time{
		time.Now().Add(2 * time.Hour),
		time.Now().Add(-90 * 24 * time.Hour),
	} {
		body := `{"version":1,"subject_hash":"` + accountfixture.SubjectHash +
			`","access":"granted","plan_id":"pro","entitlement_source":"polar","entitlement_stale":false,` +
			`"checked_at":"` + at.Format(time.RFC3339) + `"}`
		got, err := serveBody(t, body).Account(context.Background())
		if err != nil {
			t.Fatalf("checked_at %v was refused as malformed: %v", at, err)
		}
		if !got.Granted() {
			t.Errorf("checked_at %v changed the verdict", at)
		}
	}
}
