package daemon

import (
	"sync"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// lastOutputAtLabel humanizes msSinceOutput as "<floor(ms/1000)>s ago", or "" when
// it was unobserved this tick.
func lastOutputAtLabel(msSinceOutput *int64) string {
	if msSinceOutput == nil {
		return ""
	}
	secs := *msSinceOutput / 1000
	return itoa(secs) + "s ago"
}

// classifyWithModel runs the terminal-state classification. On any error it
// returns the documented fallback (unknown / 0.3) so a model blip never aborts the
// check.
func classifyWithModel(ctx *CheckContext, rec domain.WatcherRecord, signals WatcherSignals, previous string) domain.WatcherVerdict {
	verdict, err := ctx.Model.Classify(ctx.Ctx, ClassifyInput{
		Tier:          rec.ModelTier,
		Goal:          rec.Goal,
		AgentState:    signals.AgentState,
		RuntimeStatus: signals.RuntimeStatus,
		LastOutputAt:  lastOutputAtLabel(signals.MsSinceOutput),
		Previous:      previous,
		Tail:          signals.Tail,
	})
	if err != nil {
		return domain.WatcherVerdict{
			Classification:    domain.ClassUnknown,
			Confidence:        0.3,
			Summary:           "Could not classify terminal output.",
			Evidence:          []string{},
			RecommendedAction: domain.ActionNone,
		}
	}
	if verdict.Evidence == nil {
		verdict.Evidence = []string{}
	}
	if verdict.RecommendedAction == "" {
		verdict.RecommendedAction = domain.ActionNone
	}
	return verdict
}

// runModelJudges answers each modelJudge question independently against the
// terminal's current tail/state, returning a map keyed by the exact question
// string. Questions run in PARALLEL so total latency is bounded by the slowest
// single call (not the sum). A failed individual call degrades to an unmatched,
// zero-confidence answer — one bad question never aborts the map.
func runModelJudges(ctx *CheckContext, questions []string, rec domain.WatcherRecord, signals WatcherSignals) map[string]domain.ModelJudgeAnswer {
	out := make(map[string]domain.ModelJudgeAnswer, len(questions))
	if len(questions) == 0 {
		return out
	}
	lastOutputAt := lastOutputAtLabel(signals.MsSinceOutput)

	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, q := range questions {
		wg.Add(1)
		go func(question string) {
			defer wg.Done()
			answer, err := ctx.Model.Judge(ctx.Ctx, JudgeInput{
				Tier:          rec.ModelTier,
				Question:      question,
				Goal:          rec.Goal,
				AgentState:    signals.AgentState,
				RuntimeStatus: signals.RuntimeStatus,
				WaitingReason: signals.WaitingReason,
				LastOutputAt:  lastOutputAt,
				Tail:          signals.Tail,
			})
			if err != nil {
				answer = domain.ModelJudgeAnswer{Reason: "Could not evaluate the question.", Confidence: 0, Matched: false}
			}
			mu.Lock()
			out[question] = answer
			mu.Unlock()
		}(q)
	}
	wg.Wait()
	return out
}

// itoa is a tiny int64→string helper avoiding a strconv import churn at call sites.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
