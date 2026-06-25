package domain

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
	RepeatFailureWarn         = 2
	RepeatFailureAbort        = 3
	ControlMessageCount       = 3
	MaxToolResultChars        = 8000
	TruncationPreviewChars    = 1500
	TruncationSummaryChars    = 500
	MaxStoredArtifacts        = 64
	AutoCompactTokenThreshold = 60000
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
	// a no-model lossy head-truncation. Set well above AutoCompactTokenThreshold (the
	// soft, model-summarized threshold) yet far below LargeContextWindowTokens so the
	// large-model turn still has ample headroom — the fallback bounds growth long
	// before the context window is at risk.
	AutoCompactHardTruncationThreshold = 200_000

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
	// is shed from the head (oldest first) until it fits. Set to ~1/3 of the soft threshold
	// to leave ample headroom for the controls and the summary note.
	AutoCompactVerbatimTailTokenBudget = 20_000

	// LargeContextWindowTokens is the main (large) model's context window, used as the
	// denominator for the cockpit's CTX% gauge — "% of the model's context in use", NOT
	// "% toward auto-compaction". The large tier is sized to a ~1M-token window; the gauge
	// must reflect that (a small conversation reads ~1%, not 13% of the 60K compact threshold).
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
