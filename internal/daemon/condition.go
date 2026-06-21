package daemon

import (
	"regexp"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// WatcherSignals are the deterministic signals a single check reads for a
// terminal, plus the resolved classification/confidence threaded back into
// DecideOutcome.
type WatcherSignals struct {
	AgentState    string
	RuntimeStatus string // "running" | "exited"
	WaitingReason string // "prompt" | "question"
	// ExitCode is the numeric process exit code; nil until the terminal exited.
	ExitCode *int
	Tail     string
	// MsSinceOutput is nil when not observed this tick (a failed read or a
	// disconnected MCP). nil NEVER trips noOutputForMs — "not observed" is not
	// "silence".
	MsSinceOutput  *int64
	Classification domain.WatcherClassification
	Confidence     float64
}

// EvaluateCondition evaluates a WatchCondition against deterministic signals.
//
// modelJudge leaves are NOT evaluated against the live model here (that would
// make this async/untestable); answers are precomputed in runModelJudges and
// threaded in keyed by the exact question string. A missing judge answer → false;
// not:{modelJudge} of a missing answer flips false→true (accepted wart, not
// three-valued logic).
func EvaluateCondition(cond domain.WatchCondition, s WatcherSignals, judges map[string]domain.ModelJudgeAnswer) bool {
	switch {
	case cond.StateIs != nil:
		return s.AgentState == string(*cond.StateIs)
	case cond.RuntimeStatusIs != nil:
		return s.RuntimeStatus == *cond.RuntimeStatusIs
	case cond.Contains != nil:
		return strings.Contains(s.Tail, *cond.Contains)
	case cond.Regex != nil:
		// Invalid regex caught → false. Conditions are validated at creation, but
		// a Go RE2 difference (no backreferences/lookahead) could still fail to
		// compile a legacy pattern; fail closed rather than panic.
		re, err := regexp.Compile(*cond.Regex)
		if err != nil {
			return false
		}
		return re.MatchString(s.Tail)
	case cond.NoOutputForMs != nil:
		// Only fire when we actually know how long the terminal has been quiet.
		return s.MsSinceOutput != nil && *s.MsSinceOutput >= *cond.NoOutputForMs
	case cond.ModelJudge != nil:
		r, ok := judges[*cond.ModelJudge]
		return ok && r.Matched && r.Confidence >= judgeConfidenceFloor
	case len(cond.All) > 0:
		for i := range cond.All {
			if !EvaluateCondition(cond.All[i], s, judges) {
				return false
			}
		}
		return true
	case len(cond.Any) > 0:
		for i := range cond.Any {
			if EvaluateCondition(cond.Any[i], s, judges) {
				return true
			}
		}
		return false
	case cond.Not != nil:
		return !EvaluateCondition(*cond.Not, s, judges)
	}
	return false
}

// CollectModelJudges returns every distinct modelJudge question across one or
// more (possibly composite) conditions, in first-seen order and deduplicated. A
// question shared by alertWhen and stopWhen costs a single model call.
func CollectModelJudges(conds ...*domain.WatchCondition) []string {
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
	for _, c := range conds {
		walk(c)
	}
	return order
}

// HasTextCondition reports whether any contains/regex leaf is present (recursive).
// Such conditions need the full scrollback window, so the watcher reads the deep
// terminal.getOutput tail rather than the bounded inline recentOutput tail.
func HasTextCondition(cond *domain.WatchCondition) bool {
	if cond == nil {
		return false
	}
	if cond.Contains != nil || cond.Regex != nil {
		return true
	}
	for i := range cond.All {
		if HasTextCondition(&cond.All[i]) {
			return true
		}
	}
	for i := range cond.Any {
		if HasTextCondition(&cond.Any[i]) {
			return true
		}
	}
	if cond.Not != nil {
		return HasTextCondition(cond.Not)
	}
	return false
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
