package domain

import (
	"strings"
	"time"
)

// Schema constants.
const (
	// VerificationEvidencePrefix marks an evidence string that carries a
	// serialized VerificationResult.
	VerificationEvidencePrefix = "verification:"

	// JSONOutputSchemaVersion is the one-shot --json line schema version. Plain
	// monotonic int; bump only on a breaking line-shape change.
	JSONOutputSchemaVersion = 1
)

// OneShotExitCode is the one-shot exit-code mapping.
// Code 3 (ToolFailure) is RESERVED and never emitted today — the loop has no
// terminal tool-failure signal — but kept stable so a future change can adopt it
// without renumbering.
var OneShotExitCode = struct {
	Success     int
	Error       int
	Cancelled   int
	ToolFailure int
	HardTimeout int
}{
	Success:     0,
	Error:       1,
	Cancelled:   2,
	ToolFailure: 3,
	HardTimeout: 4,
}

// HardTimeoutGrace is how long the cooperative --timeout gets to unwind before the
// process is killed outright.
//
// --timeout cancels a context, and a context only bounds code that watches it. A syscall
// already in flight, a tool that ignores cancellation, or a wedged pipe read is not
// preempted by one — so for a CI runner, whose entire purpose is to finish
// deterministically, a cooperative deadline is not a bound at all. The watchdog is the
// second stage: cancel at the deadline, and if the process is STILL alive this much
// later, exit with HardTimeout rather than hang the job.
//
// The grace is generous because the normal path must never reach it: a run that
// cancels cleanly has to flush its terminal result, release the project lease, and close
// the store, and killing it mid-flush would trade a hung job for a corrupted one.
const HardTimeoutGrace = 30 * time.Second

// Agent-loop magic constants.
const (
	RepeatFailureWarn  = 2
	RepeatFailureAbort = 3
	// CoarseRepeatFailureAbort is the threshold for the COARSE failure breaker, which
	// keys on tool name + error code with pagination fields stripped (NOT the exact
	// args) and counts ONLY UNRECOVERABLE errors. It catches a futile loop the
	// exact-args breaker misses — a model that keeps calling the same tool, getting the
	// same unrecoverable error, but VARYING the arguments each time (e.g. paging a
	// pruned artifact with offset 0, 3500, 7000…). Higher than RepeatFailureAbort
	// because coarse matching is less precise, but far below the dozens such a loop
	// produces. Both breakers now trip MID-batch, so a single huge batch aborts after a
	// handful of calls rather than dispatching them all first.
	CoarseRepeatFailureAbort = 6
	// TurnStallWarn / TurnStallAbort bound CONSECUTIVE model rounds that issue no
	// tool call this turn has not already made. The failure breakers above only ever
	// count FAILURES, so a model that keeps calling tools successfully — re-listing
	// the same directory, re-reading the same file, re-planning out loud between each
	// — never trips them and the turn runs forever. A round that asks for nothing new
	// cannot have learned anything new, so a short run of them is the cheapest honest
	// "no progress" signal available to the loop. Warn nudges; abort closes the turn
	// with a forced report (see turnStall.step). Deliberately small: a legitimate
	// round almost always calls something new, and a foreground poll loop is already
	// bounded by the per-turn wait budget.
	TurnStallWarn  = 2
	TurnStallAbort = 4
	// TurnRoundWarn / TurnRoundBudget bound the TOTAL model rounds one turn may run.
	// This is the backstop the stall counter cannot provide: a model that keeps
	// issuing genuinely new calls (reading a different file every round) while never
	// converging is novel on every round and would otherwise loop without limit.
	//
	// The budget bounds a TURN, not the work. Hitting it does not kill the session or
	// discard anything already set running — the loop spends its last round asking the
	// model to report its plan and state (tools off), and the user can simply say
	// "continue", which starts a fresh turn with a fresh budget. That is why the
	// ceiling can sit well below what a very long autonomous workflow might want:
	// the cost of being wrong is one extra user message, while the cost of no ceiling
	// at all is an unbounded spend that never answers.
	TurnRoundWarn   = 24
	TurnRoundBudget = 32
	// ControlMessageCount is 0: the CLI no longer holds any client-side control
	// prefix. The backend owns the system prompt, developer instructions, and runbook
	// bodies; the CLI's visible conversation begins at index 0 with only
	// user/assistant/tool messages. The constant is retained (rather than deleted) so
	// the history-slicing call sites stay self-documenting — "everything after the
	// control prefix" is now "everything", and the arithmetic ([:0], [0:], >= 0)
	// degenerates cleanly.
	ControlMessageCount    = 0
	MaxToolResultChars     = 8000
	TruncationPreviewChars = 1500
	TruncationSummaryChars = 500
	MaxStoredArtifacts     = 64
	// AutoCompactTokenThreshold is the SOFT trigger: once the (large-tier) context is
	// estimated past this, a round runs the lossless pre-sweep and then a small-model
	// checkpoint summary. Sized against the ~1M-token large window (LargeContextWindowTokens),
	// NOT a fraction of it — DeepSeek prefix-caches the stable head (~99% cache hit on a long
	// run), so a large-but-cached context is nearly free to carry and compacting at a small
	// fraction of the window threw away detail for no real saving. Set high so most turns
	// never compact; the lossless rungs (runPreSweep) carry the cheap wins, and this only
	// fires when the conversation is genuinely large. Must stay BELOW
	// AutoCompactHardTruncationThreshold (the emergency, model-free ceiling).
	AutoCompactTokenThreshold = 500_000
	CharsPerToken             = 4

	// DistillTranscriptMaxRunes caps the transcript fed to the compaction-distillation
	// model (both the auto-compact path in agent.Session and the manual /compact path
	// in the commands layer) — the small model needs only enough recent context to
	// extract durable facts, not the full summary input. Shares the value 8000 with
	// MaxToolResultChars by coincidence; the two are semantically distinct (that one
	// caps a single tool result, this one caps the distillation model's input), so they
	// stay separate constants.
	DistillTranscriptMaxRunes = 8000

	// AutoCompactFailureThreshold is the number of CONSECUTIVE small-model summary
	// failures that must accumulate before the auto-compact fallback (lossy
	// truncation) kicks in. A single transient 429/outage must not destroy history;
	// only a sustained outage that has let the conversation balloon does.
	AutoCompactFailureThreshold = 3

	// AutoCompactHardTruncationThreshold is the SECONDARY (hard) token ceiling that,
	// combined with AutoCompactFailureThreshold consecutive summary failures, triggers
	// a no-model lossy head-truncation. Set above AutoCompactTokenThreshold (the soft,
	// model-summarized threshold) yet below LargeContextWindowTokens so the large-model
	// turn keeps headroom — the fallback only bounds growth when the soft, model-driven
	// path has been DOWN for a sustained stretch, long before the 1M window is at risk.
	AutoCompactHardTruncationThreshold = 800_000

	// AutoCompactHardTruncationKeepMessages is how many of the most-recent working
	// messages the lossy fallback retains (the oldest are dropped first). Recency is
	// what matters most for agent continuity; the head is what normal compaction would
	// have summarized away anyway. Truncation sheds further if the retained tail still
	// exceeds the hard threshold.
	AutoCompactHardTruncationKeepMessages = 16

	// AutoCompactVerbatimTailMessages is how many of the most-recent working messages the
	// HEALTHY (model-summarized) auto-compact path keeps verbatim after the summary note,
	// instead of collapsing to controls + summary only. A model summary captures the gist
	// but rounds off the exact, load-bearing references a mid-task orchestrator still needs
	// (terminal/run/watcher/workflow IDs, the branch it is on, an open grant); retaining the
	// last few raw turns keeps those intact. Same recency rationale — and the same value —
	// as AutoCompactHardTruncationKeepMessages.
	AutoCompactVerbatimTailMessages = 16

	// AutoCompactVerbatimTailTokenBudget caps the verbatim tail's size so the rebuilt
	// history (controls + summary note + tail) lands comfortably back under
	// AutoCompactTokenThreshold and does not immediately re-trip the gate. A tail over this
	// is shed from the head (oldest first) until it fits. Kept well under the soft threshold
	// so the controls and summary note still leave the rebuilt history with ample headroom.
	AutoCompactVerbatimTailTokenBudget = 20_000

	// LargeContextWindowTokens is the main (large) model's context window. It is reported
	// on each UsageEvent and persisted to the durable run-event log for /explain; it is NOT
	// "% toward auto-compaction". (The attached session previously rendered a live CTX% gauge off this
	// denominator; that gauge was removed as noise — context is managed for the operator.)
	// The large tier is sized to a ~1M-token window.
	LargeContextWindowTokens = 1_000_000

	// MainPromptCacheKey is the DeepSeek prompt_cache_key. Plain, UNVERSIONED:
	// it only groups requests onto a cache node, never a version.
	MainPromptCacheKey = "daintree-main"

	CancelledReply = "Turn cancelled"
	ClearMarker    = "[conversation cleared — context reset to initial state]"
)

// ungrantableTools can never be covered by an automation grant — keyed by
// internal dotted tool name. Granting the grant tools themselves would let an
// automation mint its own authority; granting daintree.call (the raw, unbounded
// MCP escape hatch, RiskSystem) would let a watcher/timer reach ANY Daintree MCP
// method unattended, bypassing the per-method typed-wrapper gating that is the
// whole point of the wrappers. Both grant.create (the minting path) and the
// denial-event recommender consult this so neither ever offers an impossible
// grant.
var ungrantableTools = map[string]bool{
	"grant.create":    true,
	"grant.revoke":    true,
	"daintree.call":   true,
	"daintree.invoke": true,
}

// IsUngrantableTool reports whether a tool can never be covered by an automation
// grant (see ungrantableTools for the rationale). The single source of truth so
// grant.create's validation and the blocked-event grant recommendation stay in
// lockstep.
//
// daintree.invoke is listed under its BARE name only. A grant naming the generic
// invoker authorizes nothing, which is the issue's hard requirement — but a grant
// naming one resolved target ("daintree.invoke:terminal.new", see
// DynamicTargetName) is a different, bounded identity and IS grantable. Splitting
// the two on the name is what lets a watcher be authorized for exactly one MCP
// action without handing it the whole escape hatch.
func IsUngrantableTool(name string) bool { return ungrantableTools[name] }

// DynamicInvokePrefix marks a tool identity that a target-aware invoker resolved
// from its ARGUMENTS rather than from its registration. The composite name
// "daintree.invoke:terminal.new" is the identity dispatch confirms, matches grants
// against, and writes to the audit row — so one string carries BOTH facts the
// audit trail has to preserve: which MCP action ran, and that it ran through
// dynamic invocation rather than a dedicated wrapper. No schema column was added
// for this; audit_log.toolName already holds a tool identity and this IS one.
const DynamicInvokePrefix = "daintree.invoke:"

// DynamicTargetName builds the composite identity for one dynamically-invoked MCP
// action. Callers pass the RAW action name exactly as it will be forwarded, so the
// grant a human approves and the call that is made can never name different
// actions.
func DynamicTargetName(action string) string { return DynamicInvokePrefix + action }

// IsDynamicTargetName reports whether a tool identity was resolved per-call by a
// target-aware invoker. Load-bearing in storage.grantAuthorizes: such an identity
// may only ever be authorized by an EXPLICIT name match, never by the risk-class
// half of the grant union rule.
func IsDynamicTargetName(name string) bool { return strings.HasPrefix(name, DynamicInvokePrefix) }

// DynamicTargetAction returns the raw MCP action inside a composite dynamic
// identity ("" when name is not one).
func DynamicTargetAction(name string) string {
	if !IsDynamicTargetName(name) {
		return ""
	}
	return name[len(DynamicInvokePrefix):]
}

// Watcher lifetime defaults.
const (
	// WatcherDefaultLifetimeMS is the lifetime ceiling stamped onto a watcher
	// created without an explicit stopAfterMs. Without it a watcher polls forever:
	// the timeout check is gated on stopAfterMs != nil, and completed_unverified is
	// a non-terminal state by design, so nothing else stops the loop. 24 h is
	// generous — a forgotten terminal or PR watcher can't run away — and an explicit
	// stopAfterMs always overrides it. Lives in domain (not daemon/cadence.go) so the
	// storage layer, the single watcher-insert chokepoint, can apply it. No overflow
	// risk: epoch-ms (~1.77e12) + 8.64e7 stays far inside int64.
	WatcherDefaultLifetimeMS int64 = 86_400_000 // 24 h
)
