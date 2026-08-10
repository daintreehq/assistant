package daemon

import (
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// judgeAgentFinished is the shared "is this agent actually done?" confirmation. It
// must be FAIL-CLOSED: only a confident YES reports finished, and an empty tail /
// disconnected MCP / low confidence all report not-finished without (in the empty /
// disconnected cases) ever consulting the model.

func finishedSignals(tail string) WatcherSignals {
	return WatcherSignals{AgentState: "waiting", RuntimeStatus: "running", Tail: tail}
}

func TestJudgeAgentFinished_ConfidentYes(t *testing.T) {
	ctx := ctxFor(newFakeStore(), newFakeQueue(), newFakeMCP(), &progModel{judgeFn: finishedYesJudge})
	_, finished, judged := judgeAgentFinished(ctx, termWatcher("wch", []string{"t1"}), finishedSignals("Investigation complete."))
	if !finished {
		t.Fatal("a confident YES should report finished")
	}
	if !judged {
		t.Fatal("a real tail should have consulted the model (judged=true)")
	}
}

func TestJudgeAgentFinished_ConfidentNo(t *testing.T) {
	ctx := ctxFor(newFakeStore(), newFakeQueue(), newFakeMCP(), &progModel{judgeFn: finishedNoJudge})
	_, finished, judged := judgeAgentFinished(ctx, termWatcher("wch", []string{"t1"}), finishedSignals("still working…"))
	if finished {
		t.Fatal("a confident NO must report not-finished")
	}
	if !judged {
		t.Fatal("a real tail should have consulted the model even for a NO (judged=true)")
	}
}

func TestJudgeAgentFinished_LowConfidenceNotFinished(t *testing.T) {
	low := func(q, _ string) domain.ModelJudgeAnswer {
		// Matched YES but BELOW the confidence floor → must not count as finished.
		return domain.ModelJudgeAnswer{Matched: true, Confidence: 0.4, Reason: "maybe"}
	}
	ctx := ctxFor(newFakeStore(), newFakeQueue(), newFakeMCP(), &progModel{judgeFn: low})
	_, finished, _ := judgeAgentFinished(ctx, termWatcher("wch", []string{"t1"}), finishedSignals("ambiguous output"))
	if finished {
		t.Fatal("a YES below the confidence floor must not report finished")
	}
}

// An empty tail fail-closes WITHOUT a model call AND reports judged=false, so the caller
// (confirmExploreFinished) does not stamp the judge cooldown for a judge that never ran —
// otherwise a blank tail (e.g. a failed deep read this tick) would suppress the next real
// judge for a whole cooldown window and strand a genuinely-finished agent.
func TestJudgeAgentFinished_EmptyTailFailClosedNoModelCall(t *testing.T) {
	mustNotJudge := func(q, _ string) domain.ModelJudgeAnswer {
		t.Error("judge must NOT be consulted when there is no tail evidence")
		return domain.ModelJudgeAnswer{}
	}
	ctx := ctxFor(newFakeStore(), newFakeQueue(), newFakeMCP(), &progModel{judgeFn: mustNotJudge})
	_, finished, judged := judgeAgentFinished(ctx, termWatcher("wch", []string{"t1"}), finishedSignals("   "))
	if finished {
		t.Fatal("empty tail must fail-closed to not-finished")
	}
	if judged {
		t.Fatal("empty tail must report judged=false (no model consulted → don't stamp the cooldown)")
	}
}

func TestJudgeAgentFinished_DisconnectedFailClosed(t *testing.T) {
	mcp := newFakeMCP()
	mcp.connected = false
	mustNotJudge := func(q, _ string) domain.ModelJudgeAnswer {
		t.Error("judge must NOT be consulted when MCP is disconnected")
		return domain.ModelJudgeAnswer{}
	}
	ctx := ctxFor(newFakeStore(), newFakeQueue(), mcp, &progModel{judgeFn: mustNotJudge})
	_, finished, judged := judgeAgentFinished(ctx, termWatcher("wch", []string{"t1"}), finishedSignals("done"))
	if finished {
		t.Fatal("disconnected MCP must fail-closed to not-finished")
	}
	if judged {
		t.Fatal("disconnected MCP must report judged=false")
	}
}
