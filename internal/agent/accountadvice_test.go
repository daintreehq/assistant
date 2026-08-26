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

// identityAdviceCodes are the three verdicts a sign-in actually fixes: no credential was
// sent, the stored one was refused, or the session was ended elsewhere. These are the
// ONLY codes that may offer a sign-in at all.
var identityAdviceCodes = []string{
	backend.CodeAuthRequired,
	backend.CodeAuthTokenExpired,
	backend.CodeAuthTokenInvalid,
	backend.CodeAuthSessionRevoked,
}

// noLoginCodes are the failures where a sign-in is not the remedy and offering one is
// actively harmful — it either loops with no exit (the two 403s), walks the reader into a
// browser flow that fails at the same local write (the keychain), or answers a question
// nobody asked (the three dependency outages, where the credential was never in doubt).
//
// account_contract_invalid belongs here too and has no branch at all: it is a LOCAL
// decode failure, and answering it with an account remedy would let a malformed body read
// as a verdict about the reader's plan.
var noLoginCodes = []string{
	backend.CodeAuthClientNotAllowed,
	backend.CodeAuthPermissionDenied,
	backend.CodeAccountContractInvalid,
	backend.CodeAuthDependencyUnavailable,
	backend.CodeEntitlementUnavailable,
	backend.CodeUsageAccountingUnavailable,
	backend.CodeCredentialUnavailable,
	// Not a sign-in problem either: the plan is exhausted, the identity is fine.
	backend.CodeUsageLimitReached,
}

// testAccountLinks is what the seam hands back once discovery has SUCCEEDED against a
// configured deployment: the two origin-pinned browser destinations projected out of a
// validated manifest (auth.Status.WithManifest). Advice may render these and may render
// nothing else — it never assembles a URL from the backend hostname or lifts one out of
// the error body.
var testAccountLinks = AccountLinks{
	Account:   "https://staging.daintree.org/account",
	Subscribe: "https://staging.daintree.org/subscribe",
}

// Every account code produces advice, and it is a registered wake sentinel. An
// unregistered prefix would let the supervisor's unattended wake mistake a turn that
// stopped at the account door for a real answer, and record the work it was supervising
// as summarized.
//
// Run against BOTH link states, because the degraded one is not a rare edge: a deployment
// with no identity provider never yields links at all, and every reply must still stand
// on its own there.
func TestEveryAccountCodeProducesRegisteredAdvice(t *testing.T) {
	for _, links := range []AccountLinks{testAccountLinks, {}} {
		for _, code := range accountAdviceCodes {
			msg := accountFailureAdvice(&backend.Error{Code: code}, links)
			if msg == "" {
				t.Errorf("%s produces no advice — it falls through to the generic \"Model error:\"", code)
				continue
			}
			// The EXACT prefix, not merely "some registered prefix". Accepting any of
			// them would let a regression that returned "Model error: …" pass while
			// silently undoing the whole point of this taxonomy.
			if !strings.HasPrefix(msg, accountAdvicePrefix) {
				t.Errorf("%s: advice %q does not start with %q", code, msg, accountAdvicePrefix)
			}
			if !IsWakeFailureReply(msg) {
				t.Errorf("%s: advice %q is not a registered wake-failure reply", code, msg)
			}
		}
	}
}

// The remedy for an identity failure LEADS with the slash command in this assistant.
//
// `/login` is a registered engine command (internal/commands/registry.go), dispatched for
// the embedded host and the line REPL alike. Native Daintree renders this reply in a panel
// and may be the whole of the reader's desktop, so advice whose only route is a terminal
// command leaves them with nothing to do. The standalone binary is still named — it is the
// same operation — but SECOND, and never as a prerequisite.
func TestIdentityAdviceLeadsWithTheInAssistantCommand(t *testing.T) {
	for _, code := range identityAdviceCodes {
		t.Run(code, func(t *testing.T) {
			msg := accountFailureAdvice(&backend.Error{Code: code}, testAccountLinks)
			slash := strings.Index(msg, "`/login`")
			if slash < 0 {
				t.Fatalf("advice never names `/login`, the one remedy an embedded reader can run: %q", msg)
			}
			standalone := strings.Index(msg, "daintree-assistant auth login")
			if standalone < 0 {
				t.Fatalf("advice drops the standalone equivalent entirely: %q", msg)
			}
			if standalone < slash {
				t.Errorf("the terminal command is named before `/login`, which reads as the required route: %q", msg)
			}
			// Terminal access must not be described as necessary. "in your terminal to
			// sign in" was the old phrasing and is exactly the claim being retired.
			if strings.Contains(msg, "in your terminal to sign in") {
				t.Errorf("advice still requires a terminal: %q", msg)
			}
		})
	}

	// The one code where the reader may hold no account at all — nothing was sent, so
	// nothing is known about them — has to say the browser flow CREATES one too, or a
	// user without an account reads "sign in" as a door they cannot open.
	first := accountFailureAdvice(&backend.Error{Code: backend.CodeAuthRequired}, testAccountLinks)
	if !strings.Contains(first, "create an account") {
		t.Errorf("auth_required does not say the browser flow can create an account: %q", first)
	}
}

// Nothing may name a command that does not exist. `/login`, `/logout` and `/account` are
// registered; `/auth` and `/signin` never were, and a message naming one leaves the reader
// with no way forward at all — the failure the old no-slash-commands rule was really about.
//
// Run with ZERO links so a URL path can never satisfy or trip the substring check.
func TestAccountAdviceOnlyNamesCommandsThatExist(t *testing.T) {
	for _, code := range accountAdviceCodes {
		msg := accountFailureAdvice(&backend.Error{Code: code}, AccountLinks{})
		for _, banned := range []string{"/auth ", "`/auth`", "/signin", "/sign-in"} {
			if strings.Contains(msg, banned) {
				t.Errorf("%s names %q, which is not a command that exists: %q", code, banned, msg)
			}
		}
	}
}

// A sign-in is offered for the identity codes and NOWHERE else. Each of these fails for a
// reason a fresh credential cannot touch, and offering one is how a reader ends up in a
// login/checkout loop that cannot terminate.
func TestLoginIsNeverOfferedWhereItCannotHelp(t *testing.T) {
	for _, links := range []AccountLinks{testAccountLinks, {}} {
		for _, code := range noLoginCodes {
			msg := accountFailureAdvice(&backend.Error{Code: code}, links)
			for _, banned := range []string{"/login", "auth login", "/signin", "sign in", "Sign in"} {
				if strings.Contains(msg, banned) {
					t.Errorf("%s offers %q, which cannot fix it: %q", code, banned, msg)
				}
			}
		}
	}

	// A local decode failure has no account remedy at all: it must keep falling through
	// to the caller's own handling rather than being answered as a plan verdict.
	if msg := accountFailureAdvice(&backend.Error{Code: backend.CodeAccountContractInvalid}, testAccountLinks); msg != "" {
		t.Errorf("account_contract_invalid produced account advice %q, want none", msg)
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
		// Identity that a sign-in fixes. `/login` first, the terminal command beside it.
		{backend.CodeAuthRequired, []string{"`/login`", "auth login"}, []string{"plan is the problem", "billing"}},
		{backend.CodeAuthTokenExpired, []string{"`/login`", "auth login"}, []string{"plan is the problem", "billing"}},
		{backend.CodeAuthSessionRevoked, []string{"`/login`", "auth login"}, []string{"plan is the problem", "billing"}},
		// Identity that a sign-in does NOT fix. Another credential is refused
		// identically, so offering one opens a loop with no exit.
		{backend.CodeAuthClientNotAllowed, []string{"backend"}, []string{"`/login`", "auth login"}},
		{backend.CodeAuthPermissionDenied, []string{"administers"}, []string{"`/login`", "auth login"}},
		// Plan. The sign-in is fine — so neither of these offers one — and the two are
		// not interchangeable: telling someone whose payment failed to choose a plan is
		// how they pay twice. `/account` is the surface that reports what they hold.
		{backend.CodeSubscriptionRequired, []string{"no plan", "sign-in is fine", "`/account`"}, []string{"`/login`", "auth login", "billing"}},
		// "buying" is not banned here: the sentence steers AWAY from it ("rather than
		// buying again"), which is the whole point. What must be absent is the
		// missing-plan remedy, which would send someone with a lapsed subscription to
		// purchase a second one.
		{backend.CodeSubscriptionInactive, []string{"not currently active", "billing", "rather than buying again", "`/account`"}, []string{"`/login`", "auth login", "choose a plan", "Choose a plan", "no plan"}},
		{backend.CodeUsageLimitReached, []string{"usage limit"}, []string{"`/login`", "auth login"}},
		// Dependency. Never a verdict — and specifically never "not subscribed".
		{backend.CodeAuthDependencyUnavailable, []string{"could not be checked", "unaffected"}, []string{"`/login`", "auth login", "no plan", "not subscribed"}},
		{backend.CodeEntitlementUnavailable, []string{"could not be checked", "unaffected"}, []string{"`/login`", "auth login", "no plan", "not subscribed"}},
		{backend.CodeUsageAccountingUnavailable, []string{"could not be checked", "unaffected"}, []string{"`/login`", "auth login", "no plan", "not subscribed"}},
		// Local. No request was made, so this is not the backend rejecting anything —
		// and sending someone to sign in when nothing here can hold a credential walks
		// them into a browser flow that fails at the same write.
		//
		// "keychain" is BANNED rather than required, which is the reverse of what this
		// row used to say. The code has two causes now — a locked keychain, and an
		// account layer that could not be built at all — and naming either one sends
		// half of these readers to check something that was never the problem. The reply
		// routes to the surfaces that CAN tell them apart instead.
		{backend.CodeCredentialUnavailable, []string{"`/account`", "doctor", "never rejected"}, []string{"`/login`", "auth login", "no plan", "keychain"}},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			msg := accountFailureAdvice(&backend.Error{Code: tc.code}, testAccountLinks)
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

// The validated seam is the ONLY source of a URL, and each code gets the link that matches
// its remedy. A lapsed subscription pointed at a checkout is how someone pays twice, so
// subscription_inactive must reach the account page and never the plans page.
func TestAdviceRendersOnlyTheValidatedLinkForItsRemedy(t *testing.T) {
	const subscribeLine = "Create an account or choose a plan: https://staging.daintree.org/subscribe"

	// auth_required is the ONE identity code that offers it. Nothing was sent, so nothing
	// is known about the reader and they may hold no account at all.
	signedOut := accountFailureAdvice(&backend.Error{Code: backend.CodeAuthRequired}, testAccountLinks)
	if !strings.Contains(signedOut, subscribeLine) {
		t.Errorf("auth_required does not offer the validated create-or-choose page: %q", signedOut)
	}

	// The other three prove the opposite. An expired, invalid or revoked credential is a
	// credential that EXISTED, so its holder has an account and a create-or-choose-a-plan
	// page invites a duplicate — the same reason subscription_required says "Choose a
	// plan". It also matters beyond wording: subscribe_url is contracted as a subscribe
	// destination only, so today's redirect from it into sign-in is a property of that
	// site and not of the wire contract, and must not be leaned on as a login route.
	for _, code := range []string{
		backend.CodeAuthTokenExpired,
		backend.CodeAuthTokenInvalid,
		backend.CodeAuthSessionRevoked,
	} {
		msg := accountFailureAdvice(&backend.Error{Code: code}, testAccountLinks)
		if strings.Contains(msg, testAccountLinks.Subscribe) {
			t.Errorf("%s offers a subscribe link to a reader who demonstrably has an account: %q", code, msg)
		}
		if !strings.Contains(msg, "`/login`") {
			t.Errorf("%s dropped the sign-in remedy along with the link: %q", code, msg)
		}
	}

	// The same destination, a different label: this reader already HAS an account, so
	// "Create an account" would invite a duplicate. `/account` uses the same wording for
	// the same state.
	required := accountFailureAdvice(&backend.Error{Code: backend.CodeSubscriptionRequired}, testAccountLinks)
	if !strings.Contains(required, "Choose a plan: "+testAccountLinks.Subscribe) {
		t.Errorf("subscription_required does not offer the validated plans page: %q", required)
	}
	if strings.Contains(required, "Create an account") {
		t.Errorf("subscription_required invites a duplicate account: %q", required)
	}

	inactive := accountFailureAdvice(&backend.Error{Code: backend.CodeSubscriptionInactive}, testAccountLinks)
	if !strings.Contains(inactive, testAccountLinks.Account) {
		t.Errorf("subscription_inactive does not offer the validated account page: %q", inactive)
	}
	if strings.Contains(inactive, testAccountLinks.Subscribe) {
		t.Errorf("subscription_inactive offers a second checkout: %q", inactive)
	}

	// The mixed state is the one that would catch a fallback: the account page is
	// missing and a checkout URL is sitting right there. A lapsed plan renders NO link
	// rather than the wrong one — paying twice is worse than having nothing to click.
	halfLinked := accountFailureAdvice(&backend.Error{Code: backend.CodeSubscriptionInactive},
		AccountLinks{Subscribe: testAccountLinks.Subscribe})
	if strings.Contains(halfLinked, testAccountLinks.Subscribe) {
		t.Errorf("subscription_inactive fell back to the checkout page: %q", halfLinked)
	}
	if strings.Contains(halfLinked, "http") {
		t.Errorf("subscription_inactive rendered a link it does not have: %q", halfLinked)
	}

	// A validated manifest keeps the scheme's original case, so an upper-case one has to
	// render. Rejecting it would silently degrade a perfectly good link to no link.
	shouty := accountFailureAdvice(&backend.Error{Code: backend.CodeAuthRequired},
		AccountLinks{Subscribe: "HTTPS://staging.daintree.org/subscribe"})
	if !strings.Contains(shouty, "HTTPS://staging.daintree.org/subscribe") {
		t.Errorf("an upper-case scheme was dropped: %q", shouty)
	}

	// The account page belongs to the lapsed-plan case alone. Anywhere else it is either
	// the wrong remedy or an unexplained extra destination.
	for _, code := range accountAdviceCodes {
		if code == backend.CodeSubscriptionInactive {
			continue
		}
		if msg := accountFailureAdvice(&backend.Error{Code: code}, testAccountLinks); strings.Contains(msg, testAccountLinks.Account) {
			t.Errorf("%s renders the account page, which is not its remedy: %q", code, msg)
		}
	}
}

// No links means no link — never a dangling label, never a guessed URL. This is the ONE
// accepted degradation: discovery has not succeeded against this deployment (or there is
// no account layer at all), so the reply names the command and stops.
func TestAdviceWithoutDiscoveryNamesTheCommandAndInventsNothing(t *testing.T) {
	// Every shape the seam can hand back that is not a destination. A relative path and a
	// non-browser scheme are here because "non-empty" is not the same question as
	// "somewhere a reader can go".
	for _, links := range []AccountLinks{
		{},
		{Account: "   ", Subscribe: "\t"},
		{Account: "/account", Subscribe: "/subscribe"},
		{Account: "staging.daintree.org/account", Subscribe: "javascript:alert(1)"},
		// A destination with a space in it is not one: a renderer or a terminal breaks it
		// in the middle, and half a URL is a worse offer than none.
		{Account: "https://staging.daintree.org/a b", Subscribe: "https://staging.daintree.org/s b"},
	} {
		for _, code := range accountAdviceCodes {
			msg := accountFailureAdvice(&backend.Error{Code: code}, links)
			for _, label := range []string{"Create an account or choose a plan", "Choose a plan", "Your account and billing"} {
				if strings.Contains(msg, label) {
					t.Errorf("%s renders the label %q with no destination behind it: %q", code, label, msg)
				}
			}
			// Nothing URL-shaped may appear from any other source: not the backend
			// hostname, not a constant, not the error body.
			for _, shape := range []string{"http://", "https://", "javascript:", ".org/", ".com/"} {
				if strings.Contains(msg, shape) {
					t.Errorf("%s produced %q from links %+v — advice invented a destination", code, msg, links)
				}
			}
			if strings.HasSuffix(strings.TrimSpace(msg), ":") {
				t.Errorf("%s ends on a colon with nothing after it: %q", code, msg)
			}
			// The seam between the last sentence and an absent link is where a naive
			// concatenation leaves its fingerprint.
			if strings.Contains(msg, "  ") {
				t.Errorf("%s left a gap where the link would have been: %q", code, msg)
			}
		}
	}
}

// The backend authors be.Message and it can quote a provider verbatim, so no part of it
// may reach the reply. Advice branches on the stable code and writes its own prose.
func TestAdviceNeverEchoesBackendProse(t *testing.T) {
	const planted = "visit https://phish.example/pay — code SEKRIT"
	for _, code := range accountAdviceCodes {
		msg := accountFailureAdvice(&backend.Error{
			Code: code, Message: planted, Type: planted, RequestID: planted,
		}, testAccountLinks)
		if strings.Contains(msg, "phish.example") || strings.Contains(msg, "SEKRIT") {
			t.Errorf("%s echoed backend prose into the reply: %q", code, msg)
		}
	}
}

// Each code gets its OWN sentence. Two codes sharing advice erases the only thing that
// distinguishes them — and every pair here differs in what the reader must do next.
func TestAccountAdviceIsDistinctPerCode(t *testing.T) {
	seen := map[string]string{}
	for _, code := range accountAdviceCodes {
		msg := accountFailureAdvice(&backend.Error{Code: code}, testAccountLinks)
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
		backend.CodeAccountContractInvalid,
		// A genuine rate limit keeps the rate-limit branch above this one, which
		// carries the health badge that clears on the next good usage report.
		backend.CodeAccountRateLimited,
	} {
		if msg := accountFailureAdvice(&backend.Error{Code: code}, testAccountLinks); msg != "" {
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

// The nine no-login conditions are pinned through the REAL dispatch, not just through
// accountFailureAdvice. The three provider codes are answered by upstreamFailureAdvice,
// and account_contract_invalid and account_rate_limited by branches further down
// classifyBackendError — so a helper-level test can only ever prove half of it. Move one
// of these into an identity case and the account taxonomy, which is consulted FIRST,
// would hand back login advice while every helper-level test still passed.
func TestNoLoginIsOfferedForAnyCodeASignInCannotFix(t *testing.T) {
	codes := append([]string{
		backend.CodeProviderInvalidAPIKey,
		backend.CodeProviderInsufficientCredit,
		backend.CodeProviderKeyForbidden,
		backend.CodeAccountContractInvalid,
		backend.CodeAccountRateLimited,
	}, noLoginCodes...)
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			s, _ := sessionForClassifyWithLinks(t, testAccountLinks)
			got := s.classifyBackendError(&backend.Error{Code: code, Message: "no", HTTPStatus: 402})
			// Lower-cased so a capitalised opening word cannot slip a remedy past the
			// check, and phrased broadly: the rule is "no sign-in", not "not this
			// exact string". ("Signing in again produces the same result" survives —
			// it denies the remedy rather than offering it, and "signing" is why the
			// bare word is not on the list.)
			lower := strings.ToLower(got)
			for _, banned := range []string{"/login", "auth login", "/signin", "sign in", "log in", "authenticate again"} {
				if strings.Contains(lower, banned) {
					t.Errorf("the turn reply offers %q for a failure a sign-in cannot fix: %q", banned, got)
				}
			}
		})
	}
}

// The advice must actually REACH a user. Every test above calls the helper directly, so
// deleting or misordering the dispatch in classifyBackendError would leave them all
// green while a turn went back to saying "Model error: backend: http 402 …".
func TestAccountAdviceReachesTheTurnReply(t *testing.T) {
	for _, code := range accountAdviceCodes {
		t.Run(code, func(t *testing.T) {
			s, sink := sessionForClassifyWithLinks(t, testAccountLinks)
			got := s.classifyBackendError(&backend.Error{Code: code, Message: "no", HTTPStatus: 402})
			if !strings.HasPrefix(got, accountAdvicePrefix) {
				t.Fatalf("turn reply = %q, want the account advice", got)
			}
			if got != accountFailureAdvice(&backend.Error{Code: code}, testAccountLinks) {
				t.Errorf("the turn reply is not the advice:\n got %q\nwant %q",
					got, accountFailureAdvice(&backend.Error{Code: code}, testAccountLinks))
			}
			if !sink.sawFailure() {
				t.Error("the turn was not marked failed")
			}
		})
	}
}

// SessionDeps.AccountLinks is optional and nil on every path with no account layer — a
// caller-key install, a process whose auth layer could not be built, and most tests. Nil
// must behave EXACTLY as an undiscovered deployment does: the command, no link, no panic.
func TestATurnWithNoLinkProviderStillAdvisesAndDoesNotPanic(t *testing.T) {
	s, _ := sessionForClassify(t) // deps.AccountLinks is nil
	got := s.classifyBackendError(&backend.Error{Code: backend.CodeAuthRequired, Message: "no", HTTPStatus: 401})
	if !strings.Contains(got, "`/login`") {
		t.Errorf("a session with no link provider lost the remedy: %q", got)
	}
	if strings.Contains(got, "http") {
		t.Errorf("a session with no link provider invented a destination: %q", got)
	}
	if got != accountFailureAdvice(&backend.Error{Code: backend.CodeAuthRequired}, AccountLinks{}) {
		t.Errorf("nil provider does not match the zero-links reply: %q", got)
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

// sessionForClassify builds the minimum Session needed to exercise classifyBackendError,
// with NO link provider — the default everywhere outside a wired App.
func sessionForClassify(t *testing.T) (*Session, *classifySink) {
	t.Helper()
	sink := &classifySink{}
	deps := baseDeps(nil, nil)
	deps.Events = sink
	return NewSession(deps), sink
}

// sessionForClassifyWithLinks is the same session with discovery having succeeded, so the
// turn-level assertions see what a configured deployment actually renders.
func sessionForClassifyWithLinks(t *testing.T, links AccountLinks) (*Session, *classifySink) {
	t.Helper()
	sink := &classifySink{}
	deps := baseDeps(nil, nil)
	deps.Events = sink
	deps.AccountLinks = func() AccountLinks { return links }
	return NewSession(deps), sink
}
