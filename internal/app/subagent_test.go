package app

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/safety"
	"github.com/daintreehq/assistant/internal/tools"
)

// TestSubagentInventoryIsReadOnly is the load-bearing safety test for delegation.
//
// A sub-agent runs unattended: nobody sees its rounds, nobody can approve
// anything, and it can be fanned out several at a time. The entire argument for
// that being safe is that its inventory contains ONLY read-risk tools. This test
// is what holds that argument up as the registry grows — a new mutating family
// added tomorrow inherits the filter, and a mistake in the filter fails here
// rather than in production.
func TestSubagentInventoryIsReadOnly(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	names := a.subagentToolNames()
	if len(names) == 0 {
		t.Fatal("the sub-agent was offered no tools at all")
	}

	for _, n := range names {
		tool := a.Registry.Get(n)
		if tool == nil {
			t.Errorf("%s is offered but not registered", n)
			continue
		}
		if tool.Risk != domain.RiskRead {
			t.Errorf("%s has risk %q — a sub-agent must be offered read-risk tools ONLY", n, tool.Risk)
		}
	}
}

// The inventory has to be genuinely useful, or delegation solves nothing: the
// three things a sub-agent is dispatched to do are search files, read them, and
// look at forge issues.
func TestSubagentInventoryCoversTheDelegationUseCases(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	have := map[string]bool{}
	for _, n := range a.subagentToolNames() {
		have[n] = true
	}
	for _, want := range []string{"fs.search", "fs.read", "fs.list", "forge.listIssues", "forge.getIssue"} {
		if !have[want] {
			t.Errorf("%s is missing from the sub-agent inventory — it is one of the cases this exists for", want)
		}
	}
}

// Every denylist entry must still be REGISTERED and still be READ-RISK. Without
// this, an exclusion outlives the tool it names (a stale entry nobody notices) or
// silently becomes redundant when a tool's risk changes — and either way the list
// stops describing a decision anyone made.
func TestSubagentDenylistEntriesAreLiveAndOtherwiseAdmissible(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	for name := range subagentToolDenylist {
		tool := a.Registry.Get(name)
		if tool == nil {
			t.Errorf("denylist names %q, which is not registered — remove the stale entry", name)
			continue
		}
		if tool.Risk != domain.RiskRead {
			t.Errorf("denylist names %q, but its risk is %q — the read filter already excludes it, so the entry is redundant", name, tool.Risk)
		}
	}
}

func TestSubagentInventoryExcludesTheDenylist(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	for _, n := range a.subagentToolNames() {
		if _, denied := subagentToolDenylist[n]; denied {
			t.Errorf("%s is denylisted but was offered anyway", n)
		}
	}
}

// Recursion is excluded by name, and the reason is worth pinning separately: a
// sub-agent that can delegate turns a bounded round budget into an unbounded tree.
func TestSubagentCannotDelegateFurther(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	for _, n := range a.subagentToolNames() {
		if n == "subagent.run" {
			t.Fatal("subagent.run is in the sub-agent's own inventory — sub-agents must not recurse")
		}
	}
}

// The dispatcher must fail CLOSED on a mutating call. This is the second line of
// defence behind the inventory filter: even if a mutating tool somehow reached
// dispatch, the non-interactive actor has no approval surface, so the registry's
// grant-or-blocked branch must refuse it rather than prompt a human who is not
// there.
func TestSubagentDispatchRefusesAMutatingTool(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	// Pick a real registered mutating tool rather than inventing a name, so the
	// call travels the whole dispatch path.
	var mutating string
	for _, tool := range a.Registry.List() {
		if tool != nil && tool.Risk != domain.RiskRead && tool.Risk != domain.RiskLocal && tool.Risk != domain.RiskUI {
			mutating = tool.Name
			break
		}
	}
	if mutating == "" {
		t.Skip("no mutating tool registered in this configuration")
	}

	res := subagentDispatcher{app: a}.Dispatch(context.Background(), mutating, `{}`)

	if res.Ok {
		t.Fatalf("%s SUCCEEDED under the sub-agent actor — a sub-agent must never mutate", mutating)
	}
}

// The PRIMARY gate is the ActiveToolNames allowlist, not the confirmation branch,
// and this test exists because that is easy to get backwards.
//
// safety.AlwaysConfirm covers terminal/project/external/git/system only. A LOCAL-
// or UI-risk tool needs no confirmation at all, so dispatch's non-interactive
// grant-or-blocked branch never sees it — it would simply RUN under any actor.
// The only thing standing between a sub-agent and a local-risk mutation is that
// the tool is absent from ActiveToolNames. If a future change ever drops that
// assignment in subagentDispatcher.Dispatch, every other test here still passes
// and this one fails.
func TestSubagentDispatchRefusesALocalRiskToolViaTheAllowlist(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	var local string
	for _, tool := range a.Registry.List() {
		if tool != nil && tool.Risk == domain.RiskLocal {
			local = tool.Name
			break
		}
	}
	if local == "" {
		t.Skip("no local-risk tool registered in this configuration")
	}
	// Precondition: this tool genuinely bypasses the confirmation branch, so the
	// allowlist is the only thing that can stop it.
	if safety.AlwaysConfirm(domain.RiskLocal) {
		t.Fatal("RiskLocal now always-confirms; this test's premise needs revisiting")
	}

	res := subagentDispatcher{app: a}.Dispatch(context.Background(), local, `{}`)

	if res.Ok {
		t.Fatalf("%s (local risk) RAN under the sub-agent actor — the ActiveToolNames allowlist is not being applied", local)
	}
}

// A sub-agent must never be able to consume an automation grant. tryGrant bails on
// an empty ActorID before it queries, which is why the dispatcher passes "" — a
// grant minted for a watcher or an unattended wake must not become a sub-agent's
// authority to mutate.
func TestSubagentDispatchCarriesNoActorIDAndSoCannotConsumeAGrant(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	tctx := a.buildContext(domain.ActorSubagent, "")
	if tctx.ActorID != "" {
		t.Fatalf("ActorID = %q, want empty — a non-empty id would let a sub-agent consume grants", tctx.ActorID)
	}
	if tctx.AskChoice != nil {
		t.Error("AskChoice must be nil for a sub-agent — nobody is there to answer")
	}
	if ok, _ := tctx.Confirm(context.Background(), tools.ConfirmRequest{}); ok {
		t.Error("Confirm approved for a sub-agent actor — it must never approve")
	}
}

// The projection the sub-agent is actually shown must match the names it is
// allowed to call, or the model sees a tool the dispatcher will then refuse.
func TestSubagentProjectionMatchesTheInventory(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	specs, err := subagentDispatcher{app: a}.Tools()
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(specs) != len(a.subagentToolNames()) {
		t.Fatalf("projected %d tools for an inventory of %d", len(specs), len(a.subagentToolNames()))
	}
	d := subagentDispatcher{app: a}
	for _, sp := range specs {
		internal := d.ResolveWireName(sp.Function.Name)
		if internal == "" {
			t.Errorf("projected %q does not resolve back to an internal name", sp.Function.Name)
			continue
		}
		if tool := a.Registry.Get(internal); tool == nil || tool.Risk != domain.RiskRead {
			t.Errorf("projected %q resolves to %q, which is not read-risk", sp.Function.Name, internal)
		}
	}
}

// The sub-agent inventory must be materially SMALLER than the main one — that
// reduction is a real part of what makes a delegated round cheap, and a filter
// that quietly stopped filtering would still pass every test above.
func TestSubagentInventoryIsSmallerThanTheMainOne(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	sub := len(a.subagentToolNames())
	all := len(a.Registry.List())
	if sub >= all {
		t.Fatalf("sub-agent inventory is %d of %d tools — the filter is not filtering", sub, all)
	}
	t.Logf("sub-agent inventory: %d of %d registered tools", sub, all)
}

func TestSubagentRunIsRegisteredAndWired(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	tool := a.Registry.Get("subagent.run")
	if tool == nil {
		t.Fatal("subagent.run is not registered")
	}
	if tool.Risk != domain.RiskRead {
		t.Errorf("risk = %q, want read", tool.Risk)
	}
	if !strings.Contains(tool.Description, "read-only") {
		t.Error("the description must state the read-only constraint — it is the model's only warning")
	}
}

// A run outside a live session must degrade to "no transcript", never panic. The
// tool builder runs before a.Session exists, so this path is reachable.
func TestSubagentTranscriptSinkWithoutASessionIsSafe(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	sink := subagentTranscriptSink{app: &App{}}
	if id := sink.Put("anything"); id != "" {
		t.Errorf("Put returned %q for a session-less app, want \"\"", id)
	}
	// And with a real session it must produce a readable artifact id.
	if id := (subagentTranscriptSink{app: a}).Put("hello transcript"); id == "" {
		t.Error("Put returned no id for a live session")
	} else if got, ok := a.Session.Artifacts().Get(id); !ok || got != "hello transcript" {
		t.Errorf("artifact %q = %q (found %v), want the stored transcript", id, got, ok)
	}
}
