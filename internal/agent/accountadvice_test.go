package agent

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
)

// accountadvice_test.go pins what a TURN says when it stops at the account door.
//
// Every one of these codes used to fall through to "Model error: backend: http 402
// subscription_required: …" — a billing problem described as a model problem, which is
// the one description that leads nowhere. The rules below are what make each of them
// actionable, and each is a rule someone could plausibly undo.

// accountAdvicePrefix is the sentinel every account reply must open with. It is
// deliberately failure-SHAPED: a bare "Account:" is an ordinary heading an assistant
// answer could legitimately begin with, and prefix matching would then record a real
// result as a failed turn.
const accountAdvicePrefix = "Account problem:"

// accountCodes is every account verdict a turn can meet.
var accountAdviceCodes = []string{
	backend.CodeAuthRequired,
	backend.CodeAuthTokenExpired,
	backend.CodeAuthTokenInvalid,
	backend.CodeAuthSessionRevoked,
	backend.CodeAuthClientNotAllowed,
	backend.CodeAuthPermissionDenied,
	backend.CodeSubscriptionRequired,
	backend.CodeSubscriptionInactive,
	backend.CodeUsageLimitReached,
	backend.CodeAuthDependencyUnavailable,
	backend.CodeEntitlementUnavailable,
	backend.CodeUsageAccountingUnavailable,
	backend.CodeCredentialUnavailable,
}

// Every account code produces advice, and it is a registered wake sentinel. An
// unregistered prefix would let the supervisor's unattended wake mistake a turn that
// stopped at the account door for a real answer, and record the work it was supervising
// as summarized.
func TestEveryAccountCodeProducesRegisteredAdvice(t *testing.T) {
	for _, code := range accountAdviceCodes {
		msg := accountFailureAdvice(&backend.Error{Code: code})
		if msg == "" {
			t.Errorf("%s produces no advice — it falls through to the generic \"Model error:\"", code)
			continue
		}
		// The EXACT prefix, not merely "some registered prefix". Accepting any of them
		// would let a regression that returned "Model error: …" pass while silently
		// undoing the whole point of this taxonomy.
		if !strings.HasPrefix(msg, accountAdvicePrefix) {
			t.Errorf("%s: advice %q does not start with %q", code, msg, accountAdvicePrefix)
		}
		if !IsWakeFailureReply(msg) {
			t.Errorf("%s: advice %q is not a registered wake-failure reply", code, msg)
		}
	}
}

// NOTHING may point at a cockpit slash command. The CLI is the only desktop
// authentication surface — native Daintree has no sign-in UI to fall back on — so a
// message naming `/login` or `/auth` leaves the reader with no way forward at all.
func TestAccountAdviceNeverNamesACockpitCommand(t *testing.T) {
	for _, code := range accountAdviceCodes {
		msg := accountFailureAdvice(&backend.Error{Code: code})
		for _, banned := range []string{"/login", "/auth", "/signin"} {
			if strings.Contains(msg, banned) {
				t.Errorf("%s names %q, which is not a command that exists: %q", code, banned, msg)
			}
		}
	}
}

// The three groups need three different next steps, and collapsing any two of them is
// how a user ends up in a loop that cannot terminate.
func TestAccountAdviceGivesEachGroupItsOwnRemedy(t *testing.T) {
	cases := []struct {
		code        string
		wantAny     []string
		bannedParts []string
	}{
		// Identity that a sign-in fixes.
		{backend.CodeAuthRequired, []string{"auth login"}, []string{"plan", "billing"}},
		{backend.CodeAuthTokenExpired, []string{"auth login"}, []string{"plan", "billing"}},
		{backend.CodeAuthSessionRevoked, []string{"auth login"}, []string{"plan", "billing"}},
		// Identity that a sign-in does NOT fix. Another credential is refused
		// identically, so offering one opens a loop with no exit.
		{backend.CodeAuthClientNotAllowed, []string{"backend"}, []string{"auth login"}},
		{backend.CodeAuthPermissionDenied, []string{"administers"}, []string{"auth login"}},
		// Plan. The sign-in is fine, and the two are not interchangeable: telling
		// someone whose payment failed to choose a plan is how they pay twice.
		{backend.CodeSubscriptionRequired, []string{"no plan", "sign-in is fine"}, []string{"auth login", "billing"}},
		// "buying" is not banned here: the sentence steers AWAY from it ("rather than
		// buying again"), which is the whole point. What must be absent is the
		// missing-plan remedy, which would send someone with a lapsed subscription to
		// purchase a second one.
		{backend.CodeSubscriptionInactive, []string{"not currently active", "billing", "rather than buying again"}, []string{"auth login", "Choose a plan", "no plan"}},
		{backend.CodeUsageLimitReached, []string{"usage limit"}, []string{"auth login"}},
		// Dependency. Never a verdict — and specifically never "not subscribed".
		{backend.CodeAuthDependencyUnavailable, []string{"could not be checked", "unaffected"}, []string{"auth login", "no plan", "not subscribed"}},
		{backend.CodeEntitlementUnavailable, []string{"could not be checked", "unaffected"}, []string{"auth login", "no plan", "not subscribed"}},
		{backend.CodeUsageAccountingUnavailable, []string{"could not be checked", "unaffected"}, []string{"auth login", "no plan", "not subscribed"}},
		// Local. No request was made, so this is not the backend rejecting anything —
		// and sending someone to sign in when the keychain is locked walks them into a
		// browser flow that fails at the same write.
		{backend.CodeCredentialUnavailable, []string{"keychain", "auth status"}, []string{"auth login", "no plan"}},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			msg := accountFailureAdvice(&backend.Error{Code: tc.code})
			for _, want := range tc.wantAny {
				if !strings.Contains(msg, want) {
					t.Errorf("missing %q in %q", want, msg)
				}
			}
			for _, banned := range tc.bannedParts {
				if strings.Contains(msg, banned) {
					t.Errorf("contains %q, the wrong remedy here: %q", banned, msg)
				}
			}
		})
	}
}

// Each code gets its OWN sentence. Two codes sharing advice erases the only thing that
// distinguishes them — and every pair here differs in what the reader must do next.
func TestAccountAdviceIsDistinctPerCode(t *testing.T) {
	seen := map[string]string{}
	for _, code := range accountAdviceCodes {
		msg := accountFailureAdvice(&backend.Error{Code: code})
		// Two groups deliberately share a sentence, and both share it because the
		// READER'S NEXT STEP is identical — which is the property this test is really
		// about. The three dependency codes differ only in which service is down, which
		// is the backend's business; and an expired token and an invalid one have both
		// already failed the one automatic refresh by the time a turn sees them, leaving
		// the same single move.
		switch code {
		case backend.CodeEntitlementUnavailable, backend.CodeUsageAccountingUnavailable,
			backend.CodeAuthTokenInvalid:
			continue
		}
		if prev, dup := seen[msg]; dup {
			t.Errorf("%s and %s produce identical advice", prev, code)
		}
		seen[msg] = code
	}
}

// A code outside the account taxonomy returns "" so the caller keeps its own handling.
// Answering for an unknown code would let a genuinely new backend condition masquerade
// as one we understand.
func TestAccountAdviceIgnoresEverythingElse(t *testing.T) {
	for _, code := range []string{
		"", "internal_error", "stream_interrupted",
		backend.CodeUpstreamUnavailable,
		backend.CodeProviderInvalidAPIKey,
		// A genuine rate limit keeps the rate-limit branch above this one, which
		// carries the health badge that clears on the next good usage report.
		backend.CodeAccountRateLimited,
	} {
		if msg := accountFailureAdvice(&backend.Error{Code: code}); msg != "" {
			t.Errorf("%q produced account advice %q, want none", code, msg)
		}
	}
}

// The account and upstream taxonomies must stay disjoint. An overlap would make the
// dispatch order in classifyBackendError load-bearing in a way nothing declares.
func TestAccountAndUpstreamAdviceDoNotOverlap(t *testing.T) {
	for _, code := range accountAdviceCodes {
		if msg := upstreamFailureAdvice(&backend.Error{Code: code}); msg != "" {
			t.Errorf("%s is answered by BOTH taxonomies: upstream says %q", code, msg)
		}
	}
}

// The sentinel must not swallow an ordinary answer. Prefix matching is cheap and blunt,
// so the prefix itself has to be failure-shaped — an assistant reply that opens with the
// word "Account" as a heading is entirely plausible, and misreading one as a failed turn
// loses the work the supervisor was summarizing.
func TestAnOrdinaryAnswerBeginningWithAccountIsNotAFailure(t *testing.T) {
	for _, reply := range []string{
		"Account: the migration finished cleanly across all three worktrees.",
		"Accounts are reconciled — 4 of 4 terminals idle.",
		"Account problems were discussed but nothing is blocked.",
	} {
		if IsWakeFailureReply(reply) {
			t.Errorf("a real answer was classified as a failed turn: %q", reply)
		}
	}
}

// The advice must actually REACH a user. Every test above calls the helper directly, so
// deleting or misordering the dispatch in classifyBackendError would leave them all
// green while a turn went back to saying "Model error: backend: http 402 …".
func TestAccountAdviceReachesTheTurnReply(t *testing.T) {
	for _, code := range accountAdviceCodes {
		t.Run(code, func(t *testing.T) {
			s, sink := sessionForClassify(t)
			got := s.classifyBackendError(&backend.Error{Code: code, Message: "no", HTTPStatus: 402})
			if !strings.HasPrefix(got, accountAdvicePrefix) {
				t.Fatalf("turn reply = %q, want the account advice", got)
			}
			if got != accountFailureAdvice(&backend.Error{Code: code}) {
				t.Errorf("the turn reply is not the advice:\n got %q\nwant %q",
					got, accountFailureAdvice(&backend.Error{Code: code}))
			}
			if !sink.sawFailure() {
				t.Error("the turn was not marked failed")
			}
		})
	}
}

// A recognised account code decides on its own, whatever status it arrives with. The
// status branches sit above this one, so a subscription verdict carrying a contradictory
// 429 used to render as "Model rate-limited" and one carrying 426 as a version mismatch
// — both pointing the reader somewhere no billing problem is. A backend bug, a proxy, or
// a future status change all produce exactly this.
func TestARecognisedAccountCodeOutranksAContradictoryStatus(t *testing.T) {
	for _, status := range []int{429, 426, 401, 500, 0} {
		s, _ := sessionForClassify(t)
		got := s.classifyBackendError(&backend.Error{
			Code: backend.CodeSubscriptionInactive, HTTPStatus: status,
			Type: "rate_limit_error", Message: "no",
		})
		if !strings.HasPrefix(got, accountAdvicePrefix) {
			t.Errorf("status %d produced %q, want the billing advice", status, got)
		}
	}
}

// Mid-stream, an SSE error carries its code with HTTPStatus 0 and Stream true. The
// advice has to survive that, or the whole taxonomy is unreachable for any failure that
// happens after the 200 is committed — which is most of them.
func TestAccountAdviceSurvivesAMidStreamError(t *testing.T) {
	s, _ := sessionForClassify(t)
	got := s.classifyBackendError(&backend.Error{
		Code: backend.CodeSubscriptionRequired, Stream: true, Message: "no",
	})
	if !strings.HasPrefix(got, accountAdvicePrefix) {
		t.Fatalf("mid-stream reply = %q, want the account advice", got)
	}
}

// account_rate_limited stays with the rate-limit branch, which carries the health badge
// that clears on the next good usage report. It is a genuine transient throttle, not an
// exhausted plan — usage_limit_reached is the exhausted one, and it must NOT land here.
func TestTheTwo429sAreRoutedDifferently(t *testing.T) {
	s, _ := sessionForClassify(t)
	throttled := s.classifyBackendError(&backend.Error{Code: backend.CodeAccountRateLimited, HTTPStatus: 429})
	if !strings.HasPrefix(throttled, "Model rate-limited:") {
		t.Errorf("account_rate_limited = %q, want the rate-limit reply", throttled)
	}

	s2, _ := sessionForClassify(t)
	exhausted := s2.classifyBackendError(&backend.Error{Code: backend.CodeUsageLimitReached, HTTPStatus: 429})
	if !strings.HasPrefix(exhausted, accountAdvicePrefix) {
		t.Errorf("usage_limit_reached = %q, want the account advice", exhausted)
	}
	if strings.Contains(exhausted, "rate-limited") {
		t.Errorf("an exhausted plan was described as a rate limit: %q", exhausted)
	}
}

// classifySink records just enough to prove a turn was reported as failed.
type classifySink struct {
	NoopEventSink
	phases []domain.RunPhase
	errs   []string
}

func (c *classifySink) Phase(p domain.RunPhase) { c.phases = append(c.phases, p) }
func (c *classifySink) Error(m string)          { c.errs = append(c.errs, m) }

func (c *classifySink) sawFailure() bool {
	for _, p := range c.phases {
		if p == domain.PhaseFailed {
			return true
		}
	}
	return false
}

// sessionForClassify builds the minimum Session needed to exercise classifyBackendError.
func sessionForClassify(t *testing.T) (*Session, *classifySink) {
	t.Helper()
	sink := &classifySink{}
	deps := baseDeps(nil, nil)
	deps.Events = sink
	return NewSession(deps), sink
}
