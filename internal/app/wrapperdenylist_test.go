package app

import (
	"testing"

	"github.com/daintreehq/assistant/internal/tools/mcpwrap"
	"github.com/daintreehq/assistant/internal/tools/mcpx"
)

// A typed wrapper and its daintree.call denylist entry are two halves of ONE guarantee,
// and they live in different packages — mcpwrap registers the wrapper, mcpx decides what
// the raw escape hatch refuses. Nothing but this test holds them together, and each half
// fails silently on its own:
//
//   - A wrapper registered WITHOUT a denylist entry leaves the raw route open, so the
//     model can still reach the action through daintree.call and skip every bound the
//     wrapper declares. That is the bug the wrapper was built to prevent, and it looks
//     exactly like success from the outside.
//   - A denylist entry WITHOUT anywhere to go is worse: the raw route is refused and the
//     model is redirected to nothing, so the action becomes unreachable by any path.
//
// internal/app is where the check belongs because it is the only package that already
// imports both families (see toolfamilies.go / tools.go); neither of them may import the
// other.
//
// rawPathExceptions are the wrapper names deliberately NOT denied, each with the reason.
// An exception list rather than a blanket skip: a new wrapper that forgets its entry
// fails here, which is the regression this guards, while the two honest reasons an
// existing wrapper is absent stay visible and attributable instead of quietly widening
// the rule for everyone.
var rawPathExceptions = map[string]string{
	// RENAME, not a gap. The wrapper is named for what it answers; the raw action behind
	// it has a different name and IS denied, so there is no open bypass here.
	"forge.getChecks": "wraps the differently-named forge.getCIStatus, which is denied",

	// The five pre-existing gaps issue #367 recorded here — forge.listIssues,
	// forge.getIssue, forge.listPRs, worktree.list and worktree.getCurrent — were
	// closed by issue #368, which had to reconcile the same two indexes to decide
	// which raw actions daintree.invoke may classify. Writing the gaps down is what
	// made them cheap to close: they were already named, counted and reasoned about,
	// so the follow-up was a denylist entry each rather than a fresh investigation.
}

func TestEveryMcpwrapWrapperIsDeniedOnTheRawPath(t *testing.T) {
	registered := map[string]bool{}
	for _, tool := range mcpwrap.Tools(mcpwrap.Deps{}) {
		registered[tool.Name] = true
		if mcpx.IsWrappedMCPName(tool.Name) {
			if why, excused := rawPathExceptions[tool.Name]; excused {
				t.Errorf("%s is now denied on the raw path — remove it from rawPathExceptions (it was listed as: %s)", tool.Name, why)
			}
			continue
		}
		if _, excused := rawPathExceptions[tool.Name]; excused {
			continue
		}
		t.Errorf("mcpwrap registers %q but daintree.call does not refuse it — the raw route still bypasses the wrapper's validation. "+
			"Add it to wrappedMCPTools in internal/tools/mcpx/discovery.go, or add it to rawPathExceptions with a reason.", tool.Name)
	}
	// An exception naming a wrapper that no longer exists is stale bookkeeping that
	// would silently excuse a future tool that happened to reuse the name.
	for name := range rawPathExceptions {
		if !registered[name] {
			t.Errorf("rawPathExceptions names %q, which mcpwrap no longer registers — remove it", name)
		}
	}
}

// The reverse direction: a redirect must actually point somewhere, or it blocks the only
// route that worked and names no replacement.
func TestEveryDenylistedNameHasSomewhereToGo(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	for _, raw := range mcpx.WrappedMCPNames() {
		msg := mcpx.WrappedMCPRedirect(raw)
		if msg == "" {
			t.Errorf("daintree.call refuses %q with an empty redirect — the model is blocked with no alternative named", raw)
			continue
		}
		// When a same-named tool exists the redirect is trivially reachable. When it does
		// not, the entry is a rename (terminal.focus wraps panel.focus, forge.getChecks
		// wraps forge.getCIStatus) and the message is the only thing telling the model
		// where to go — so it must be substantial rather than a bare restatement.
		if !a.Registry.Has(raw) && len(msg) < len(raw) {
			t.Errorf("daintree.call refuses %q, no tool of that name is registered, and the redirect %q names no replacement", raw, msg)
		}
	}
}

// Every issue #367 wrapper must be denied — no exceptions. Stated separately from the
// general rule above so this PR's own guarantee cannot be weakened later by someone
// adding a name to rawPathExceptions.
func TestIssue367WrappersAreAllDenied(t *testing.T) {
	for _, name := range []string{
		"project.detectRunners", "project.runCheck", "forge.listIssueComments",
		"agentSessionHistory.list", "browser.getConsoleMessages", "errors.recent",
		"notifications.recent", "worktree.resource.status",
	} {
		if !mcpx.IsWrappedMCPName(name) {
			t.Errorf("%s must be refused by daintree.call — the typed wrapper is the only validated route", name)
		}
		if _, excused := rawPathExceptions[name]; excused {
			t.Errorf("%s must not be in rawPathExceptions — issue #367 wrappers close the raw path unconditionally", name)
		}
	}
}
