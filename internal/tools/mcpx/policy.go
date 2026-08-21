package mcpx

import (
	"sort"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/safety"
)

/* --------------------------- target policy catalog ------------------------ */

// The catalog answers ONE question for a raw Daintree MCP action name: what would
// this action's policy be if we had bothered to write a typed wrapper for it?
//
// Without an answer there is only `daintree.call`, which applies the SAME policy —
// system risk, typed confirmation, ungrantable — to `terminal.list` and to
// `worktree.delete`. That is not caution, it is a missing distinction: it makes a
// harmless read cost a typed-phrase approval, which trains the operator to approve
// system-tier prompts by reflex, and it makes a genuinely dangerous action no more
// alarming than a listing.
//
// Three properties are load-bearing:
//
//   - It is LOCAL and explicit. Daintree's ListTools returns only
//     {name, description, inputSchema} — no danger, no tier, no visibility — and MCP
//     tool ANNOTATIONS are non-normative hints the spec itself says a client must
//     not trust for authorization. So the classification cannot be read off the
//     wire; it has to be a reviewed statement in this repo, which is what
//     localTargetPolicies is.
//   - It FAILS CLOSED. A name with no entry has unknown policy, and an unknown
//     policy is not "probably a read" — it is refused by the target-aware invoker
//     and left to `daintree.call`'s system/typed-confirm path. Every future Daintree
//     action is therefore born unclassified and harmless-by-default, and the only
//     way to widen the surface is a reviewed edit here.
//   - It is a SEAM, not a hard-coded table. TargetPolicySource is where a
//     host-supplied manifest (daintreehq/daintree#11910) plugs in when it exists.
//     It is absent today, so Resolve simply never consults it — a capability gate
//     that costs nothing and needs no version negotiation.
type TargetPolicy struct {
	// Action is the raw MCP action name this policy describes.
	Action string
	// Risk is the class dispatch will gate the invocation at.
	Risk domain.RiskClass
	// Danger mirrors Daintree's own vocabulary ("safe" | "confirm" | "dangerous").
	// Reported to the model for context; the gate is Risk, never this.
	Danger string
	// Summary is one short line on what the action does, used when the live
	// catalog's own description is missing.
	Summary string
	// Known distinguishes a real classification from the zero value. A
	// TargetPolicy{} is UNKNOWN, not "read" — the zero RiskClass would otherwise
	// read as a permissive default.
	Known bool
	// Source names where the classification came from ("local" | "host"), so a
	// discovery result can say whether it is repo-reviewed or host-supplied.
	Source string
}

// RequiredTier is the least tier permitted to run this action, derived from the
// same matrix the dispatch gate uses.
func (p TargetPolicy) RequiredTier() domain.Tier { return safety.MinimumTierFor(p.Risk) }

// Confirms reports whether invoking this action would require an approval (or a
// scoped grant for a non-interactive actor).
func (p TargetPolicy) Confirms() bool { return safety.AlwaysConfirm(p.Risk) }

// TargetPolicySource is the capability-gated seam for host-supplied action policy.
// Daintree does not expose a machine-readable action manifest yet
// (daintreehq/daintree#11910); until it does, App wires nil and the local catalog
// is the only source.
//
// A host source may only ever be consulted for actions the local catalog does NOT
// classify, and it can only ADD classifications — see Resolve. That ordering is
// deliberate: the host is the thing being driven, so letting it re-classify an
// action this repo already reviewed would make the safety policy something the
// supervised process gets to choose.
type TargetPolicySource interface {
	// Lookup returns the host's policy for one action, or ok=false when the host
	// does not classify it. Implementations must never block on the network for
	// long — this runs inside tool dispatch.
	Lookup(action string) (TargetPolicy, bool)
}

// neverDynamic can NEVER be dynamically invoked, whatever any source says.
//
// The prose in localTargetPolicies explains why each one is excluded, but prose is
// not enforcement: an absent entry is only "unclassified", and the host seam below
// classifies unclassified names. Without this list, a Daintree build could hand
// itself `worktree.delete` — and the supervised process choosing the policy it is
// supervised under is the one arrangement this whole file exists to prevent.
//
// A name here is not merely unclassified; it is refused, permanently, and stays
// reachable only through daintree.call's system-tier typed confirmation, which no
// automation grant can satisfy.
var neverDynamic = map[string]bool{
	"git.commit":        true, // rewrites/publishes history
	"git.push":          true,
	"worktree.delete":   true, // Daintree's third always-confirm dangerous action
	"terminal.kill":     true, // PERMANENT delete; internal/agent/session.go keeps it behind daintree.call
	"agentSettings.get": true, // discloses preset env config, provider auth tokens included
}

// neverDynamicNormalized is neverDynamic re-keyed through normalizeMCPName, the
// same case-folding/whitespace-stripping the daintree.call denylist matches on.
//
// The exact-match map alone is only as strong as the spelling a source happens to
// use. localTargetPolicies is exact-keyed too, so today a case variant simply falls
// through to "unknown" and is refused — but that is an accident of there being no
// host source wired, not a property of the exclusion. The moment a host classifies
// "Terminal.Kill" or "Git.Commit", the exact lookup misses, the AlwaysConfirm clamp
// happily admits terminal/git risk, and a case-insensitive Daintree dispatcher runs
// the excluded action under an approvable, grantable policy. A hard exclusion that
// a different capitalisation walks around is not a hard exclusion.
var neverDynamicNormalized = func() map[string]bool {
	m := make(map[string]bool, len(neverDynamic))
	for k := range neverDynamic {
		m[normalizeMCPName(k)] = true
	}
	return m
}()

// ResolveTargetPolicy classifies one raw action name: hard exclusions first, then
// the reviewed local catalog, then the host, then unknown.
func ResolveTargetPolicy(src TargetPolicySource, action string) TargetPolicy {
	// Both spellings: the exact name as given, and the normalized form, so a
	// case/whitespace variant cannot route around the exclusion (see
	// neverDynamicNormalized).
	if neverDynamic[action] || neverDynamicNormalized[normalizeMCPName(action)] {
		return TargetPolicy{Action: action}
	}
	if p, ok := localTargetPolicies[action]; ok {
		p.Action, p.Known, p.Source = action, true, "local"
		return p
	}
	if src != nil {
		if p, ok := src.Lookup(action); ok && p.Risk.IsValid() && safety.AlwaysConfirm(p.Risk) {
			// A host classification is accepted ONLY if it still requires a human (or a
			// target-scoped grant). The host may tell us an action needs MORE care than
			// we assumed; it may not tell us an action is safe.
			//
			// The asymmetry is the whole point. A no-confirm class is an assertion that
			// nobody needs to see this call, and today that assertion would arrive over
			// an unauthenticated channel from the process being driven — so a host that
			// was compromised, misconfigured, or simply newer than this build could turn
			// every unknown action into a silent read. Nothing is lost by the clamp: the
			// actions worth running unattended are the ones reviewed HERE, and lifting it
			// is a deliberate change to make when daintree#11910 ships a manifest whose
			// provenance can actually be checked.
			p.Action, p.Known, p.Source = action, true, "host"
			return p
		}
	}
	return TargetPolicy{Action: action}
}

// localTargetPolicies is the reviewed classification of the Daintree MCP actions
// this repo is willing to invoke dynamically. Keys are EXACT raw action names as
// Daintree advertises them (no normalization — a case variant is a different
// string and must not resolve, or the normalization would become an unreviewed
// widening).
//
// The classifications come from docs/DAINTREE_MCP.md's tier/danger breakdown and
// from the risk classes the existing typed wrappers already assign to the same
// underlying actions, so a dynamically-invoked action is gated exactly as its
// wrapper would gate it. Actions that HAVE a typed wrapper are deliberately absent:
// the invoker redirects to the wrapper rather than classifying a second route to
// the same call.
//
// Adding an entry is the only way to widen the dynamic surface, and it is a
// reviewed change. Do not add one because an action "looks harmless" — read what
// Daintree does with it first.
var localTargetPolicies = map[string]TargetPolicy{
	// --- workbench-tier reads. No confirmation; these are the whole reason the
	// target-aware path exists, since daintree.call charges a typed system-tier
	// approval for each of them today.
	"actions.getContext":  {Risk: domain.RiskRead, Danger: "safe", Summary: "Active project / worktree / focused-terminal snapshot."},
	"actions.list":        {Risk: domain.RiskRead, Danger: "safe", Summary: "List Daintree's action registry."},
	"actions.search":      {Risk: domain.RiskRead, Danger: "safe", Summary: "Search Daintree's action registry."},
	"actions.getSchema":   {Risk: domain.RiskRead, Danger: "safe", Summary: "Daintree's own schema lookup for one action."},
	"project.getCurrent":  {Risk: domain.RiskRead, Danger: "safe", Summary: "The currently bound project."},
	"agent.listAvailable": {Risk: domain.RiskRead, Danger: "safe", Summary: "Agent presets this Daintree can launch."},
	"agent.listToolbar":   {Risk: domain.RiskRead, Danger: "safe", Summary: "Toolbar agent entries."},
	"agent.getState":      {Risk: domain.RiskRead, Danger: "safe", Summary: "Live single-agent state snapshot, keyed by AGENT id (not terminal id)."},
	"cliAvailability.get": {Risk: domain.RiskRead, Danger: "safe", Summary: "Which agent CLIs are installed."},
	"terminal.list":       {Risk: domain.RiskRead, Danger: "safe", Summary: "Open terminals and their agent state."},
	"terminal.getStatus":  {Risk: domain.RiskRead, Danger: "safe", Summary: "One terminal's status, optionally with recent output."},
	// --- action-tier mutations. These confirm (or need a target-scoped grant), and
	// each is gated at the class the equivalent typed wrapper uses.
	"terminal.new":           {Risk: domain.RiskTerminal, Danger: "confirm", Summary: "Open a new terminal."},
	"terminal.inject":        {Risk: domain.RiskTerminal, Danger: "confirm", Summary: "Inject text into a terminal without submitting it."},
	"terminal.waitUntilIdle": {Risk: domain.RiskTerminal, Danger: "confirm", Summary: "Block until a terminal goes idle."},

	// EVERY entry above is a name this repo can point at in docs/DAINTREE_MCP.md.
	// That rule is the catalog's own integrity check, not pedantry: classifying a
	// name Daintree does not currently expose does not sit inert — it PRE-AUTHORIZES
	// it, so the day some future host build ships an action under that spelling it
	// is born already classified instead of born unknown. A guessed name is a
	// standing grant to a stranger.
	//
	// NOTE ON WHAT IS ABSENT, and why none of it is an oversight:
	//
	//   - git.commit / git.push / worktree.delete are Daintree's three
	//     always-confirm dangerous actions, and terminal.kill is a PERMANENT delete
	//     that internal/agent/session.go deliberately keeps behind daintree.call. All
	//     four stay unclassified, so the escape hatch's system-tier typed confirmation
	//     — which no automation grant can ever satisfy — remains their only route.
	//     Classifying terminal.kill as terminal-risk would have made it single-key
	//     approvable, remembered by an auto-approve session, and reachable unattended
	//     through a target-scoped grant. That is three downgrades for one entry.
	//   - Every action with a typed wrapper is absent by construction — see
	//     wrappedMCPTools and getLocalWrapperNames. The invoker redirects to the
	//     wrapper rather than classifying a second route to the same call. That
	//     includes all four forge READS (listIssues, listPRs, getIssue, getPR), whose
	//     wrappers live in internal/tools/mcpwrap — a package neither of this file's
	//     two indexes can see, which is why the real guard is the cross-package test
	//     in internal/app (TestClassifiedMCPActionsDoNotCollideWithLocalTools). It
	//     caught exactly this: three of them were classified here before it existed.
	//     agentSessionHistory.list and worktree.resource.status were classified here
	//     too until issue #367 gave each a typed mcpwrap wrapper of the same name;
	//     they came straight back out. That is the index working: a wrapper is the
	//     better route (it validates arguments and reports honestly), so a second
	//     classified route to the same call would only be a way around it.
	//   - agentSettings.get is workbench-tier and "safe" by Daintree's own reckoning,
	//     and it is still absent, because tier is not the only axis. Its result
	//     carries preset ENVIRONMENT CONFIGURATION — observed returning provider auth
	//     tokens in the clear — so classifying it read/no-confirm would let the model
	//     pull credentials into the conversation with nobody asked. Today that costs a
	//     typed system-tier approval, and the human seeing that prompt is the entire
	//     control. "Read-only" bounds what an action WRITES; it says nothing about
	//     what it discloses.
	//   - Forge WRITES (forge.addIssueComment, forge.commentOnPR,
	//     forge.requestReviewers, …) are absent for now. They are external-risk
	//     always-confirm, so classifying them would be defensible, but each one
	//     publishes to a place other people read and none of them is on the path this
	//     issue exists to unblock. They can be added later, deliberately.
}

/* ---------------------------- preferred wrapper --------------------------- */

// preferredWrapperFor reports the typed local tool that governs a raw MCP action,
// or "" when none does.
//
// It consults BOTH indexes because they answer different questions and have
// historically disagreed. wrappedMCPTools is raw-call REDIRECT POLICY (which raw
// names daintree.call refuses, and the prose it refuses them with);
// getLocalWrapperNames is the family's own registration (which local tools exist
// under this exact name). A wrapper can exist without a denylist entry —
// copyTree.generate did for a while — and treating either index as the whole truth
// is how a raw route around a typed wrapper stays open.
func preferredWrapperFor(action string) string {
	if hint, ok := denylistLookup[normalizeMCPName(action)]; ok {
		// The denylist value is model-facing prose ("terminal.focus", or a longer
		// sentence); the leading token is the tool name.
		return hint
	}
	if getLocalWrapperNames()[action] {
		return action + " (typed wrapper — call it directly with its own declared parameters)"
	}
	return ""
}

// preferredWrapperToolName reduces a denylist hint to just its leading tool name,
// for the machine-readable `preferredTool` field. The hints are prose that starts
// with the tool name, so the first whitespace-delimited token is the name; a hint
// listing alternatives ("terminal.summarize (…), terminal.read (…)") reduces to the
// first, which is the one it recommends.
func preferredWrapperToolName(action string) string {
	hint := preferredWrapperFor(action)
	if hint == "" {
		return ""
	}
	// Guard the index rather than assume the hint is well-formed. policyBlock calls
	// this for EVERY catalog row on every tool.search / daintree.listTools, so an
	// empty or whitespace-only denylist value — a plausible typo in a hand-kept map —
	// would turn routine discovery into a recovered TOOL_THREW panic. An unnamed
	// preferred tool is simply no preferred tool.
	fields := strings.Fields(hint)
	if len(fields) == 0 {
		return ""
	}
	return strings.TrimRight(fields[0], ",")
}

// ClassifiedActionNames lists every locally-classified action, sorted. Used by the
// invoker's failure prose so a policy-unknown name can be answered with "here is
// what IS dynamically invocable" instead of a dead end, and EXPORTED so a test in
// the wiring package can cross-check the list against the whole registry — the
// typed-wrapper indexes inside this package only see this package's own tools, and
// the wrappers that matter most (recipe, workflow, worktree, forge) live in
// internal/tools/mcpwrap.
func ClassifiedActionNames() []string {
	out := make([]string, 0, len(localTargetPolicies))
	for name := range localTargetPolicies {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
