package domain

// Schema constants (schemas.ts §2.7).
const (
	// VerificationEvidencePrefix marks an evidence string that carries a
	// serialized VerificationResult.
	VerificationEvidencePrefix = "verification:"

	// JSONOutputSchemaVersion is the one-shot --json line schema version. Plain
	// monotonic int; bump only on a breaking line-shape change.
	JSONOutputSchemaVersion = 1
)

// OneShotExitCode is the one-shot exit-code mapping (schemas.ts ONE_SHOT_EXIT_CODE).
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

// Agent-loop magic constants (_contracts.md §6, agent/loop.ts).
const (
	MaxToolIterations         = 12
	RepeatFailureWarn         = 2
	RepeatFailureAbort        = 3
	ControlMessageCount       = 3
	MaxToolResultChars        = 8000
	TruncationPreviewChars    = 1500
	TruncationSummaryChars    = 500
	MaxStoredArtifacts        = 64
	AutoCompactTokenThreshold = 60000
	CharsPerToken             = 4

	// LargeContextWindowTokens is the main (large) model's context window, used as the
	// denominator for the cockpit's CTX% gauge — "% of the model's context in use", NOT
	// "% toward auto-compaction". glm-5p2 carries a ~1M-token window; the gauge must reflect
	// that (a small conversation reads ~1%, not 13% of the 60K compact threshold).
	LargeContextWindowTokens = 1_000_000

	// MainPromptCacheKey is the Fireworks prompt_cache_key. Plain, UNVERSIONED:
	// it only groups requests onto a cache node, never a version.
	MainPromptCacheKey = "daintree-main"

	CancelledReply = "Turn cancelled"
	ClearMarker    = "[conversation cleared — context reset to initial state]"
)
