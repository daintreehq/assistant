package daemon

import (
	"regexp"
	"strconv"
	"unicode/utf16"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// MEANINGFUL holds the classifications worth interrupting the user about.
var meaningful = map[domain.WatcherClassification]bool{
	domain.ClassWaitingForInput:     true,
	domain.ClassPermissionPrompt:    true,
	domain.ClassCommandFailed:       true,
	domain.ClassTestsFailed:         true,
	domain.ClassTestsPassed:         true,
	domain.ClassMergeConflict:       true,
	domain.ClassCompletedSuccess:    true,
	domain.ClassCompletedUnverified: true,
	domain.ClassCompletedUnknown:    true,
	domain.ClassTerminalExited:      true,
	domain.ClassRateLimited:         true,
}

// TERMINAL_CLASS holds the classifications that mean the watcher's job is done →
// stop. completed_unverified is deliberately NOT here (the agent reported
// completion but the worktree is dirty/unverified — keep polling until a clean
// completed_success or the user resolves it).
var terminalClass = map[domain.WatcherClassification]bool{
	domain.ClassCompletedSuccess: true,
	domain.ClassTerminalExited:   true,
}

// severityMap maps each classification to its base severity.
var severityMap = map[domain.WatcherClassification]domain.Severity{
	domain.ClassNoChange:            domain.SeverityDebug,
	domain.ClassStillWorking:        domain.SeverityDebug,
	domain.ClassWaitingForInput:     domain.SeverityAttention,
	domain.ClassPermissionPrompt:    domain.SeverityAttention,
	domain.ClassCommandFailed:       domain.SeverityError,
	domain.ClassTestsFailed:         domain.SeverityError,
	domain.ClassTestsPassed:         domain.SeverityDone,
	domain.ClassMergeConflict:       domain.SeverityBlocked,
	domain.ClassCompletedSuccess:    domain.SeverityDone,
	domain.ClassCompletedUnverified: domain.SeverityAttention,
	domain.ClassCompletedUnknown:    domain.SeverityInfo,
	domain.ClassTerminalExited:      domain.SeverityUrgent,
	domain.ClassRateLimited:         domain.SeverityAttention,
	domain.ClassNeedsLargeModel:     domain.SeverityAttention,
	domain.ClassUnknown:             domain.SeverityInfo,
}

// StopReason explains why a check stopped the watcher.
type StopReason string

const (
	StopNone         StopReason = ""
	StopConditionMet StopReason = "condition_met"
	StopTimeout      StopReason = "timeout"
	StopTerminal     StopReason = "terminal"
)

// CheckOutcome is one terminal's resolved check result.
type CheckOutcome struct {
	Classification domain.WatcherClassification
	Confidence     float64
	Summary        string
	Evidence       []string
	EpistemicKind  domain.EpistemicKind
	ShouldPublish  bool
	Severity       domain.Severity
	Stop           bool
	StopReason     StopReason
}

// DecideArgs are the inputs to DecideOutcome.
type DecideArgs struct {
	Classification domain.WatcherClassification
	Confidence     float64
	Summary        string
	Evidence       []string
	Previous       string
	Signals        WatcherSignals
	StopWhen       *domain.WatchCondition
	AlertWhen      *domain.WatchCondition
	JudgeResults   map[string]domain.ModelJudgeAnswer
	TimedOut       bool
	// UsedModel is true when the small model was consulted to reach this
	// classification — the one signal that disambiguates a model-inferred verdict
	// from a deterministic one for epistemicKind.
	UsedModel bool
}

// DecideOutcome decides publish/stop/severity from a classification + conditions.
// Pure — unit-testable without any MCP/model.
func DecideOutcome(a DecideArgs) CheckOutcome {
	sig := a.Signals
	sig.Classification = a.Classification
	sig.Confidence = a.Confidence

	// Defensive validation at the eval entry: JSON-loaded conditions are validated
	// in UnmarshalJSON, but a directly-constructed WatchCondition (a test, or a
	// future in-code caller) bypasses that guard. A degenerate condition (no
	// variant key, an uncompilable regex, a non-positive noOutputForMs) would
	// otherwise silently mis-evaluate or, for the regex leaf, fail closed deep in
	// EvaluateCondition. Treat an invalid condition as "does not match" — fail
	// closed without panicking, mirroring the decode-time rejection.
	alertMatched := a.AlertWhen != nil && a.AlertWhen.Validate() == nil &&
		EvaluateCondition(*a.AlertWhen, sig, a.JudgeResults)
	stopMatched := a.StopWhen != nil && a.StopWhen.Validate() == nil &&
		EvaluateCondition(*a.StopWhen, sig, a.JudgeResults)
	changed := string(a.Classification) != a.Previous
	isTerminal := terminalClass[a.Classification]

	shouldPublish := a.TimedOut ||
		alertMatched ||
		stopMatched ||
		(changed && meaningful[a.Classification]) ||
		a.Classification == domain.ClassNeedsLargeModel

	stop := stopMatched || isTerminal || a.TimedOut
	var reason StopReason
	switch {
	case stopMatched:
		reason = StopConditionMet
	case a.TimedOut:
		reason = StopTimeout
	case isTerminal:
		reason = StopTerminal
	}

	// timedOut forces attention; otherwise base severity from the map.
	severity := domain.SeverityAttention
	if !a.TimedOut {
		severity = severityMap[a.Classification]
	}
	// An explicitly matched alert/stop condition is always worth at least
	// "attention" — never bury it below the surfacing threshold. "done" counts as
	// below (a clean completion that matched an explicit stop/alert must surface).
	if (alertMatched || stopMatched) &&
		(severity == domain.SeverityDebug || severity == domain.SeverityInfo || severity == domain.SeverityDone) {
		severity = domain.SeverityAttention
	}

	return CheckOutcome{
		Classification: a.Classification,
		Confidence:     a.Confidence,
		Summary:        a.Summary,
		Evidence:       a.Evidence,
		EpistemicKind:  domain.ClassificationEpistemicKind(a.Classification, a.UsedModel),
		ShouldPublish:  shouldPublish,
		Severity:       severity,
		Stop:           stop,
		StopReason:     reason,
	}
}

// moreUrgent reports whether a's severity outranks b's (for headline selection).
// Uses domain.RankOf which mirrors the SEVERITY_WEIGHT table.
func moreUrgent(a, b CheckOutcome) bool {
	return domain.RankOf(a.Severity) > domain.RankOf(b.Severity)
}

// hashTail is a stable 32-bit hash of a terminal tail, base36 — persisted in
// outHash and compared across ticks. The algorithm is h=(h<<5)-h+c with int32
// truncation per UTF-16 code unit, then the unsigned 32-bit value in base36.
// Bit-exactness matters: outHash is persisted across processes and compared every
// tick, so a divergence between a watcher's prior hash and the current one would
// re-classify on every check.
//
// The hash feeds UTF-16 code units — a non-BMP rune (e.g. an emoji in the
// terminal tail) is two surrogate-pair units. Ranging a Go string yields whole
// runes, which would feed a single >0xFFFF code point and diverge. Encode to
// UTF-16 first so each code unit is hashed one-for-one.
func hashTail(s string) string {
	var h int32
	for _, u := range utf16.Encode([]rune(s)) {
		h = (h << 5) - h + int32(u)
	}
	return strconv.FormatUint(uint64(uint32(h)), 36)
}

// TerminalState is per-terminal memory persisted in the watcher's optionsJson.
type TerminalState struct {
	Prev    string `json:"prev,omitempty"`
	OutHash string `json:"outHash,omitempty"`
	OutAt   int64  `json:"outAt,omitempty"`
	Seen    bool   `json:"seen,omitempty"`
	// SeenWorking latches once this terminal's agent has actually been observed in
	// the "working" state. It is the working→waiting completion gate: a freshly
	// spawned agent parks at "waiting" (at its prompt, before the injected prompt is
	// submitted) for several seconds, so a "waiting" reading is only a real
	// end-of-turn once we have first seen it work. Without this latch the first tick
	// (~3s, inside the spawn grace) misreads the pre-start prompt as "completed".
	SeenWorking     bool   `json:"seenWorking,omitempty"`
	ReadFailures    int    `json:"readFailures,omitempty"`
	LastClassifyKey string `json:"lastClassifyKey,omitempty"`
	// Subscribed records that this terminal's agent-state resource (ResourceURI,
	// built from AgentID) is subscribed in the CURRENT session, so the check skips
	// re-subscribing every tick. AgentID/ResourceURI are persisted so a reconnect
	// can re-issue the (session-scoped) subscription without a fresh getStatus read.
	AgentID     string `json:"agentId,omitempty"`
	ResourceURI string `json:"resourceUri,omitempty"`
	Subscribed  bool   `json:"subscribed,omitempty"`
}

// NextOutputState advances a terminal's output-tracking state. When the tail
// changed since the last check, outAt becomes now; otherwise it is preserved so
// the time since the last *new* output keeps growing. Returns the new state and
// the elapsed ms. Pure.
func NextOutputState(prev *TerminalState, tail string, now int64) (TerminalState, int64) {
	outHash := hashTail(tail)
	changed := prev == nil || prev.OutHash != outHash
	outAt := now
	if !changed {
		outAt = prev.OutAt
		if outAt == 0 {
			outAt = now
		}
	}
	st := TerminalState{OutHash: outHash, OutAt: outAt}
	if prev != nil {
		st.Prev = prev.Prev
	}
	ms := now - outAt
	if ms < 0 {
		ms = 0
	}
	return st, ms
}

// rateLimitSignature matches recent terminal output that signals a provider rate
// limit. The (?:...) lookaround-free constructs used here are all supported by
// RE2, so this compiles natively. Case-insensitive.
var rateLimitSignature = regexp.MustCompile(`(?i)\b(?:429|529)\b|too many requests|rate[ _-]?limit(?:ed|ing|s)?|quota (?:exceeded|exhausted)|insufficient[_ ]quota|retry[ -]?after\b|resource (?:exhausted|temporarily unavailable)|server is temporarily limiting|\boverloaded\b|you(?:'ve| have) hit your limit|exceed (?:your|the) .{0,40}rate limit`)

// detectRateLimitSignature reports whether a bounded recent tail slice shows a
// provider throttling the agent. Pass a bounded RECENT slice (rateLimitTailWindow).
func detectRateLimitSignature(text string) bool {
	if text == "" {
		return false
	}
	return rateLimitSignature.MatchString(text)
}
