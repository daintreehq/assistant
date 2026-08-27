package extractionx

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
)

// These are PURE ports of the watcher-engine DSL helpers (daemon/condition.go,
// daemon/outcome.go). They are reproduced here — rather than imported from the
// daemon subsystem — so this package stays independently compilable. Extraction
// rejects modelJudge up front, so the judge-answer map is intentionally absent
// (a modelJudge leaf evaluates to false here, but it never reaches this code).

// signals are the deterministic signals one read folds into for condition eval.
type signals struct {
	AgentState    string
	WaitingReason string // "prompt" | "question" | "approval" | "error" | "" (meaningful only while waiting).
	// The last three are BLOCKED states — see domain.IsBlockingWaitingReason. This used
	// to name only question/prompt, and the two it omitted were both scored as finished.
	RuntimeStatus string // "running" | "exited"
	ExitCode      *int
	Tail          string
	MsSinceOutput int64 // 0 when not finite/observed
}

// evaluateCondition evaluates a WatchCondition against deterministic signals.
// modelJudge → false (extraction rejects it before polling). Mirrors
// daemon.EvaluateCondition exactly for the supported leaves.
func evaluateCondition(cond domain.WatchCondition, s signals) bool {
	switch {
	case cond.StateIs != nil:
		return s.AgentState == string(*cond.StateIs)
	case cond.RuntimeStatusIs != nil:
		return s.RuntimeStatus == *cond.RuntimeStatusIs
	case cond.Contains != nil:
		return strings.Contains(s.Tail, *cond.Contains)
	case cond.Regex != nil:
		re, err := regexp.Compile(*cond.Regex)
		if err != nil {
			return false
		}
		return re.MatchString(s.Tail)
	case cond.NoOutputForMs != nil:
		// Only fire when we actually know how long the terminal has been quiet.
		return s.MsSinceOutput >= *cond.NoOutputForMs
	case cond.ModelJudge != nil:
		return false
	case len(cond.All) > 0:
		for i := range cond.All {
			if !evaluateCondition(cond.All[i], s) {
				return false
			}
		}
		return true
	case len(cond.Any) > 0:
		for i := range cond.Any {
			if evaluateCondition(cond.Any[i], s) {
				return true
			}
		}
		return false
	case cond.Not != nil:
		return !evaluateCondition(*cond.Not, s)
	}
	return false
}

// collectModelJudges returns every distinct modelJudge question across a
// (possibly composite) condition, in first-seen order, deduplicated. Used only to
// REJECT extraction waits that carry a modelJudge.
func collectModelJudges(c *domain.WatchCondition) []string {
	seen := make(map[string]bool)
	var order []string
	var walk func(c *domain.WatchCondition)
	walk = func(c *domain.WatchCondition) {
		if c == nil {
			return
		}
		switch {
		case c.ModelJudge != nil:
			if !seen[*c.ModelJudge] {
				seen[*c.ModelJudge] = true
				order = append(order, *c.ModelJudge)
			}
		case len(c.All) > 0:
			for i := range c.All {
				walk(&c.All[i])
			}
		case len(c.Any) > 0:
			for i := range c.Any {
				walk(&c.Any[i])
			}
		case c.Not != nil:
			walk(c.Not)
		}
	}
	walk(c)
	return order
}

// hashTail is a stable 32-bit hash of a terminal tail, base36 (h=(h<<5)-h+c with
// int32 truncation, then the unsigned 32-bit value in base36). Bit-exactness
// matters — a divergence would mis-track output.
func hashTail(s string) string {
	var h int32
	for _, c := range s {
		h = (h << 5) - h + int32(c)
	}
	return strconv.FormatUint(uint64(uint32(h)), 36)
}

// terminalState is the per-terminal output-tracking memory for noOutputForMs and
// the settle latch.
type terminalState struct {
	outHash string
	outAt   int64
	// seenWorking latches once this terminal's agent has been observed in the
	// "working" state. It is the working→waiting settle gate (mirrors daemon's
	// SeenWorking): a freshly spawned agent parks at "waiting" before it picks up the
	// prompt, so a "waiting" reading is only a real settle once we have first seen it
	// work. readSignals sets it; nextOutputState preserves it across poll iterations.
	seenWorking bool
}

// nextOutputState advances a terminal's output-tracking state. When the tail
// changed since last check, outAt becomes now; otherwise it is preserved so the
// time since the last NEW output keeps growing. The seenWorking latch is carried
// forward (monotonic — once true, stays true). Returns the new state and elapsed
// ms. Pure. Mirrors daemon.NextOutputState.
func nextOutputState(prev *terminalState, tail string, now int64) (terminalState, int64) {
	outHash := hashTail(tail)
	changed := prev == nil || prev.outHash != outHash
	outAt := now
	if !changed {
		outAt = prev.outAt
		if outAt == 0 {
			outAt = now
		}
	}
	st := terminalState{outHash: outHash, outAt: outAt, seenWorking: prev != nil && prev.seenWorking}
	ms := now - outAt
	if ms < 0 {
		ms = 0
	}
	return st, ms
}

// runtimeFromAgentState maps Daintree's agentState onto the coarse runtimeStatus
// the DSL exposes ("" → "", "exited" → "exited", else "running").
func runtimeFromAgentState(agentState string) string {
	if agentState == "" {
		return ""
	}
	if agentState == "exited" {
		return "exited"
	}
	return "running"
}
