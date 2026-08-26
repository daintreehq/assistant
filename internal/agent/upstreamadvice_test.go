package agent

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
)

// Each upstream-failure code must produce its OWN advice. The whole point of the
// backend's taxonomy split is that these five conditions have five different fixes and
// used to arrive as one indistinguishable 502; advice that collapses them again would
// undo the split from this end, and the user-visible symptom is the bad one — a tester
// with an empty balance replacing a key that was never the problem.
func TestUpstreamFailureAdviceIsDistinctPerCode(t *testing.T) {
	codes := []string{
		backend.CodeProviderInvalidAPIKey,
		backend.CodeProviderInsufficientCredit,
		backend.CodeProviderKeyForbidden,
		backend.CodeUpstreamNoCompliantProvider,
		backend.CodeUpstreamUnavailable,
		backend.CodeUpstreamRequestRejected,
		// Both reportable codes, so the table catches the easy mistake of giving them
		// one shared "something went wrong, report it" sentence — which would erase the
		// only thing that distinguishes them: whose fault it was.
		backend.CodeUpstreamProtocolError,
	}
	seen := map[string]string{}
	for _, code := range codes {
		msg := upstreamFailureAdvice(&backend.Error{Code: code})
		if msg == "" {
			t.Errorf("%s produces no advice — it would fall through to the generic \"Model error:\"", code)
			continue
		}
		if prev, dup := seen[msg]; dup {
			t.Errorf("%s and %s produce identical advice", prev, code)
		}
		seen[msg] = code

		// Every reply is a wake sentinel. An unregistered prefix would let the
		// supervisor's unattended wake mistake a failed turn for a real answer and
		// record the work it was supervising as summarized.
		if !IsWakeFailureReply(msg) {
			t.Errorf("%s: advice %q does not start with a registered wake-failure prefix", code, msg)
		}
	}
}

// A code outside the taxonomy must return "" so the caller keeps its generic fallback.
// Returning a plausible-looking string for an unknown code would let a genuinely new
// backend condition masquerade as one we understand.
func TestUpstreamFailureAdviceIgnoresUnknownCodes(t *testing.T) {
	for _, code := range []string{"", "internal_error", "stream_interrupted", "task_output_invalid"} {
		if msg := upstreamFailureAdvice(&backend.Error{Code: code}); msg != "" {
			t.Errorf("%q produced advice %q, want none", code, msg)
		}
	}
}

// The three PROVIDER-credential failures describe a credential the user does not hold.
//
// The backend funds every model call with its own key; the CLI ships none. So none of
// these may say "your API key", offer a place for the user to top up, or point at a
// sign-in — all three did, and the first sent the reader to `/login` to replace a
// credential they have never seen. That command exists today, which does not make it the
// right answer here: signing in again cannot change a key the deployment holds.
func TestProviderCredentialAdviceDoesNotBlameTheUsersKey(t *testing.T) {
	for _, code := range []string{
		backend.CodeProviderInvalidAPIKey,
		backend.CodeProviderInsufficientCredit,
		backend.CodeProviderKeyForbidden,
	} {
		msg := upstreamFailureAdvice(&backend.Error{Code: code})
		// `/login` and `/account` are real engine commands now, which makes naming one
		// here worse rather than better: the reader would run a command that works, watch
		// it report a perfectly healthy account, and be no closer to the deployment
		// credential that actually failed.
		for _, banned := range []string{
			"your API key", "your OpenRouter account", "openrouter.ai/credits",
			"/login", "auth login", "/account",
		} {
			if strings.Contains(msg, banned) {
				t.Errorf("%s says %q, which describes a credential the user does not hold: %q", code, banned, msg)
			}
		}
		// It must still say whose problem it is, or the reader has a diagnosis and
		// nowhere to take it.
		if !strings.Contains(msg, "deployment") {
			t.Errorf("%s does not say the credential belongs to the deployment: %q", code, msg)
		}
	}

	// A credential that is recognised and funded but not permitted for this model still
	// names permissions, which is the one thing that distinguishes it from the other two.
	forbidden := upstreamFailureAdvice(&backend.Error{Code: backend.CodeProviderKeyForbidden})
	if !strings.Contains(forbidden, "permission") {
		t.Errorf("a permissions problem should name permissions, got %q", forbidden)
	}
}

// Our own bugs are the one class where the user has nothing to fix, so the advice must
// say so and carry the correlation id that makes a report useful. The id is the only
// handle on a failure whose detail lives in the server's log.
func TestUpstreamFailureAdviceQuotesTheRequestID(t *testing.T) {
	withID := upstreamFailureAdvice(&backend.Error{
		Code:      backend.CodeUpstreamProtocolError,
		RequestID: "req_01m008ts2sepcj5gfp",
	})
	if !strings.Contains(withID, "req_01m008ts2sepcj5gfp") {
		t.Errorf("advice omits the request id: %q", withID)
	}
	if !strings.Contains(withID, "not your account") {
		t.Errorf("advice should absolve the user's account: %q", withID)
	}

	// The two reportable codes share a next step and have OPPOSITE culprits. Attributing
	// a provider's malformed reply to a Daintree bug sends someone hunting through our
	// code for something that is not there.
	rejected := upstreamFailureAdvice(&backend.Error{Code: backend.CodeUpstreamRequestRejected})
	if !strings.Contains(rejected, "Daintree bug") {
		t.Errorf("a request Daintree built and the provider rejected is our bug: %q", rejected)
	}
	if strings.Contains(withID, "Daintree bug") {
		t.Errorf("an unparseable provider reply must not be blamed on Daintree: %q", withID)
	}

	// No id available: still coherent advice, with no dangling "request id " fragment.
	withoutID := upstreamFailureAdvice(&backend.Error{Code: backend.CodeUpstreamProtocolError})
	if withoutID == "" {
		t.Fatal("advice disappeared when no request id was present")
	}
	if strings.Contains(withoutID, "request id") {
		t.Errorf("advice mentions a request id it does not have: %q", withoutID)
	}
}
