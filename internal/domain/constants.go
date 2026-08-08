package domain

import "errors"

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
}{
	Success:     0,
	Error:       1,
	Cancelled:   2,
	ToolFailure: 3,
}

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
	// ControlMessageCount is 0: the CLI no longer holds any client-side control
	// prefix. The backend owns the system prompt, developer instructions, and skill
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
	// "% toward auto-compaction". (The cockpit previously rendered a live CTX% gauge off this
	// denominator; that gauge was removed as noise — context is managed for the operator.)
	// The large tier is sized to a ~1M-token window.
	LargeContextWindowTokens = 1_000_000

	// MainPromptCacheKey is the DeepSeek prompt_cache_key. Plain, UNVERSIONED:
	// it only groups requests onto a cache node, never a version.
	MainPromptCacheKey = "daintree-main"

	CancelledReply = "Turn cancelled"
	ClearMarker    = "[conversation cleared — context reset to initial state]"
)

// ErrLoginRequested is the cross-surface sentinel a session surface (the cockpit
// runner or the classic REPL) returns when the user asked to re-run the login
// flow (/login). The interactive launcher catches it, runs the blocking prompt
// once the surface has released the terminal, rebuilds the App with the fresh
// credentials, and restarts the surface. Lives in domain because internal/ui and
// internal/cli must both reference it without importing each other.
var ErrLoginRequested = errors.New("login requested")

// ungrantableTools can never be covered by an automation grant — keyed by
// internal dotted tool name. Granting the grant tools themselves would let an
// automation mint its own authority; granting daintree.call (the raw, unbounded
// MCP escape hatch, RiskSystem) would let a watcher/timer reach ANY Daintree MCP
// method unattended, bypassing the per-method typed-wrapper gating that is the
// whole point of the wrappers. Both grant.create (the minting path) and the
// denial-event recommender consult this so neither ever offers an impossible
// grant.
var ungrantableTools = map[string]bool{
	"grant.create":  true,
	"grant.revoke":  true,
	"daintree.call": true,
}

// IsUngrantableTool reports whether a tool can never be covered by an automation
// grant (see ungrantableTools for the rationale). The single source of truth so
// grant.create's validation and the blocked-event grant recommendation stay in
// lockstep.
func IsUngrantableTool(name string) bool { return ungrantableTools[name] }

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
