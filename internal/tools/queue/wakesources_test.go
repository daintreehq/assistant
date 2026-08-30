package queue

import (
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// The wake filter and this package's refusal list must agree — checked against the REAL
// functions, not against a copy of either.
//
// queue.publish keeps its own list because importing the agent package from the handler
// would be a dependency cycle, and two lists are two chances to drift. Drifting OPEN is
// a security hole: a source that becomes actionable in agent.IsActionableWake while
// queue.publish still permits it hands the model a way to manufacture paid turns out of
// its own text. A test that compared two hand-written maps would pass straight through
// that, so this one derives the actionable set by CALLING IsActionableWake and checks it
// by CALLING isWakeActionableSource.
func TestEveryActionableSourceIsRefusedHere(t *testing.T) {
	all := []domain.EventSource{
		domain.SourceTimer, domain.SourceTerminalWatcher, domain.SourceWorktreeWatcher,
		domain.SourcePRWatcher, domain.SourceWorkflow, domain.SourceModelWorker,
		domain.SourceAsyncTool, domain.SourceSystem, domain.SourceUser,
	}
	actionable := 0
	for _, src := range all {
		// The richest target any source could carry, so a source that is actionable
		// only WITH a target still counts.
		e := domain.QueueEvent{
			Source:  src,
			Summary: "x",
			Target: &domain.EventTarget{
				TerminalID: "term-1", TimerID: "tmr_1", TimerMessage: true, TimerOccurrence: 1,
			},
		}
		if !agent.IsActionableWake(e) {
			continue
		}
		actionable++
		if !isWakeActionableSource(src) {
			t.Errorf("source %q can start an autonomous turn but queue.publish still permits it", src)
		}
	}
	if actionable == 0 {
		t.Fatal("no source is actionable — the filter is inert and this test proves nothing")
	}
}
