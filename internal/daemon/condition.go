package daemon

import (
	"regexp"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
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

// triState is the three-valued result of a condition evaluation. A modelJudge leaf
// whose answer is MISSING (the model wasn't consulted this tick — e.g. MCP was
// disconnected) is genuinely unknown, distinct from a present-but-not-matched
// answer (false). Negation and the all/any combinators propagate unknown so a
// not:{modelJudge} of an unanswered judge stays unknown rather than flipping to a
// confident true.
type triState int8

const (
	triFalse triState = iota
	triTrue
	triUnknown
)

// EvaluateCondition reports whether a WatchCondition DEFINITELY holds against the
// deterministic signals. It fails closed: an unknown result (a missing model-judge
// answer, possibly under negation) is NOT a match, so an alert/stop never fires on
// an unproven condition. modelJudge answers are precomputed in runModelJudges and
// threaded in keyed by the exact question string.
func EvaluateCondition(cond domain.WatchCondition, s WatcherSignals, judges map[string]domain.ModelJudgeAnswer) bool {
	return evalConditionTri(cond, s, judges) == triTrue
}

// evalConditionTri is the three-valued core. Deterministic leaves are always
// known (true/false); only a missing modelJudge answer yields unknown.
func evalConditionTri(cond domain.WatchCondition, s WatcherSignals, judges map[string]domain.ModelJudgeAnswer) triState {
	switch {
	case cond.StateIs != nil:
		return boolTri(s.AgentState == string(*cond.StateIs))
	case cond.RuntimeStatusIs != nil:
		return boolTri(s.RuntimeStatus == *cond.RuntimeStatusIs)
	case cond.Contains != nil:
		return boolTri(strings.Contains(s.Tail, *cond.Contains))
	case cond.Regex != nil:
		// Invalid regex caught → false. Conditions are validated at creation, but
		// a Go RE2 difference (no backreferences/lookahead) could still fail to
		// compile a legacy pattern; fail closed rather than panic.
		re, err := regexp.Compile(*cond.Regex)
		if err != nil {
			return triFalse
		}
		return boolTri(re.MatchString(s.Tail))
	case cond.NoOutputForMs != nil:
		// Only fire when we actually know how long the terminal has been quiet.
		return boolTri(s.MsSinceOutput != nil && *s.MsSinceOutput >= *cond.NoOutputForMs)
	case cond.ModelJudge != nil:
		r, ok := judges[*cond.ModelJudge]
		if !ok {
			// The judge wasn't answered this tick — unknown, NOT false.
			return triUnknown
		}
		return boolTri(r.Matched && r.Confidence >= judgeConfidenceFloor)
	case len(cond.All) > 0:
		// false if any child is false; unknown if any child is unknown (and none
		// false); else true.
		result := triTrue
		for i := range cond.All {
			switch evalConditionTri(cond.All[i], s, judges) {
			case triFalse:
				return triFalse
			case triUnknown:
				result = triUnknown
			}
		}
		return result
	case len(cond.Any) > 0:
		// true if any child is true; unknown if any child is unknown (and none
		// true); else false.
		result := triFalse
		for i := range cond.Any {
			switch evalConditionTri(cond.Any[i], s, judges) {
			case triTrue:
				return triTrue
			case triUnknown:
				result = triUnknown
			}
		}
		return result
	case cond.Not != nil:
		switch evalConditionTri(*cond.Not, s, judges) {
		case triTrue:
			return triFalse
		case triFalse:
			return triTrue
		default:
			return triUnknown
		}
	}
	return triFalse
}

func boolTri(b bool) triState {
	if b {
		return triTrue
	}
	return triFalse
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
