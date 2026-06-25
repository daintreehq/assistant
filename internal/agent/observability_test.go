package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// TestRecordToolFailureBucketsByName proves the session-cumulative tally accumulates
// per tool name across calls and is independent per name.
func TestRecordToolFailureBucketsByName(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	if got := s.recordToolFailure("terminal.read"); got != 1 {
		t.Fatalf("first terminal.read failure count = %d, want 1", got)
	}
	if got := s.recordToolFailure("terminal.read"); got != 2 {
		t.Fatalf("second terminal.read failure count = %d, want 2", got)
	}
	if got := s.recordToolFailure("fs.read"); got != 1 {
		t.Fatalf("first fs.read failure count = %d, want 1 (buckets are per-name)", got)
	}
	counts := s.ToolFailureCounts()
	if counts["terminal.read"] != 2 || counts["fs.read"] != 1 {
		t.Fatalf("ToolFailureCounts = %v, want terminal.read:2 fs.read:1", counts)
	}
}

// TestToolFailureCountsZeroValue proves the getter is safe on a fresh session (the
// map is lazy-init'd in recordToolFailure, nil until the first failure).
func TestToolFailureCountsZeroValue(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	if counts := s.ToolFailureCounts(); len(counts) != 0 {
		t.Fatalf("fresh session ToolFailureCounts = %v, want empty", counts)
	}
}

// TestToolFailureCountsReturnsCopy proves the getter hands back an independent copy:
// mutating it must not corrupt the session's internal tally.
func TestToolFailureCountsReturnsCopy(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.recordToolFailure("fs.read")
	snapshot := s.ToolFailureCounts()
	snapshot["fs.read"] = 999
	snapshot["injected"] = 7
	again := s.ToolFailureCounts()
	if again["fs.read"] != 1 {
		t.Fatalf("internal fs.read tally = %d, want 1 (caller mutation leaked back)", again["fs.read"])
	}
	if _, ok := again["injected"]; ok {
		t.Fatal("a key added to the returned copy leaked into the session map")
	}
}

// TestCompactionDepthCounts proves the depth getter tracks each compaction and resets
// on /clear (the compaction chain is destroyed).
func TestCompactionDepthCounts(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	if got := s.CompactionDepth(); got != 0 {
		t.Fatalf("fresh session depth = %d, want 0", got)
	}
	s.Compact("first")
	s.Compact("second")
	if got := s.CompactionDepth(); got != 2 {
		t.Fatalf("depth after two compactions = %d, want 2", got)
	}
	s.Clear()
	if got := s.CompactionDepth(); got != 0 {
		t.Fatalf("depth after /clear = %d, want 0 (chain destroyed)", got)
	}
}

// TestCompactionNotePrefixEmbedsDepth proves the prefix tags the depth while keeping
// the "checkpoint" framing the rendered note and tests key on (issue #256).
func TestCompactionNotePrefixEmbedsDepth(t *testing.T) {
	p := compactionNotePrefix(3)
	if !strings.Contains(p, "checkpoint") {
		t.Fatalf("prefix %q lost the 'checkpoint' framing", p)
	}
	if !strings.Contains(p, "depth 3") {
		t.Fatalf("prefix %q does not embed the depth", p)
	}
}

// --- end-to-end wiring (through Send) ---

// obsRecordingSink captures the observability fields off the live event stream so the
// wiring tests assert runToolBatch/emitUsage actually stamp them (not just that the
// durable sink serializes a value it is handed).
type obsRecordingSink struct {
	NoopEventSink
	failureCounts []int // ToolResultEvent.FailureCount per result, in order
	usageDepths   []int // UsageEvent.CompactionDepth per round, in order
}

func (s *obsRecordingSink) ToolResult(ev ToolResultEvent) {
	s.failureCounts = append(s.failureCounts, ev.FailureCount)
}
func (s *obsRecordingSink) Usage(ev UsageEvent) {
	s.usageDepths = append(s.usageDepths, ev.CompactionDepth)
}

// TestToolFailureCountWiredThroughSend proves runToolBatch stamps the cumulative
// per-tool failure count onto each failing ToolResultEvent AND feeds the session
// tally. Two failing rounds with DIFFERENT args avoid the identical-call breaker.
func TestToolFailureCountWiredThroughSend(t *testing.T) {
	sink := &obsRecordingSink{}
	tools := &fakeTools{result: domain.Fail("BOOM", "broke")}
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c1", "fs__read", `{"path":"a"}`)}},
		{ToolCalls: []models.ToolCallRequest{toolCall("c2", "fs__read", `{"path":"b"}`)}},
		{Content: "done"},
	}}
	deps := baseDeps(r, tools)
	deps.Events = sink
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if !equalInts(sink.failureCounts, []int{1, 2}) {
		t.Fatalf("ToolResult FailureCounts = %v, want [1 2]", sink.failureCounts)
	}
	if got := s.ToolFailureCounts()["fs.read"]; got != 2 {
		t.Fatalf("session fs.read tally = %d, want 2", got)
	}
}

// cancelingFailTool cancels the turn ctx from inside dispatch (a user hitting Escape
// while the tool ran) and returns a failing result — the exact shape that must NOT
// inflate the failure tally.
type cancelingFailTool struct {
	cancel     context.CancelFunc
	dispatched int
}

func (t *cancelingFailTool) OpenAITools([]string) ([]models.ChatTool, error) { return nil, nil }
func (t *cancelingFailTool) ResolveWireName(w string) string {
	return strings.ReplaceAll(w, "__", ".")
}
func (t *cancelingFailTool) Dispatch(ctx context.Context, name, args string, turn TurnContext) domain.ToolResult {
	t.dispatched++
	t.cancel()
	return domain.Fail("BOOM", "broke")
}

// TestCancelledToolResultNotCounted proves a failed result produced while the turn is
// being cancelled is excluded from the tally — a user abort is not a tool failure.
func TestCancelledToolResultNotCounted(t *testing.T) {
	tools := &cancelingFailTool{}
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c1", "fs__read", `{}`)}},
		{Content: "done"},
	}}
	s := NewSession(baseDeps(r, tools))
	ctx, cancel := context.WithCancel(context.Background())
	tools.cancel = cancel
	defer cancel()
	if _, err := s.Send(ctx, "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if counts := s.ToolFailureCounts(); len(counts) != 0 {
		t.Fatalf("a cancelled tool result must not be counted, got %v", counts)
	}
}

// TestUsageEventCarriesSessionCompactionDepth proves emitUsage reads the live session
// depth onto the per-round UsageEvent (the value is wired, not just serializable).
func TestUsageEventCarriesSessionCompactionDepth(t *testing.T) {
	sink := &obsRecordingSink{}
	r := &fakeRouter{results: []models.ChatResult{{Content: "done"}}}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.Events = sink
	s := NewSession(deps)
	s.Compact("prior work") // depth → 1
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if n := len(sink.usageDepths); n == 0 || sink.usageDepths[n-1] != 1 {
		t.Fatalf("UsageEvent CompactionDepth = %v, want last = 1", sink.usageDepths)
	}
}
