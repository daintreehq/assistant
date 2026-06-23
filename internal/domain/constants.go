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
