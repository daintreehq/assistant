package extractionx

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Finding 1: the extract tools advertise required fields that strict decoding alone
// didn't enforce. Validate() must reject an empty/oversized terminalIds list and an
// empty-string id. Output shape is no longer a per-call arg — `format` is gone from
// every extract tool, so it is now rejected as an UNKNOWN FIELD by strict decode
// (the model picks terminal.extract for text or terminal.extract.json for structured
// instead of toggling a format enum).
func TestExtractRejectsRequiredAndEnumGaps(t *testing.T) {
	tool := newExtractTool(Deps{})
	for name, bad := range map[string]string{
		"missing terminalIds": `{"instruction":"x"}`,
		"empty terminalIds":   `{"terminalIds":[]}`,
		"empty-string id":     `{"terminalIds":[""]}`,
		"blank id":            `{"terminalIds":["  "]}`,
		"too many ids":        `{"terminalIds":["1","2","3","4","5","6","7","8","9","10","11","12","13","14","15","16","17"]}`,
		"format unknown now":  `{"terminalIds":["t"],"format":"json"}`,
		"jsonSchema unknown":  `{"terminalIds":["t"],"jsonSchema":"{}"}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("%s should be rejected: %s", name, bad)
		}
	}
	// A valid gate-only call (no instruction) still decodes — extract allows it.
	if _, err := tool.Decode(json.RawMessage(`{"terminalIds":["t"]}`)); err != nil {
		t.Errorf("valid extract args should decode: %v", err)
	}
	// And a plain text extract with an instruction.
	if _, err := tool.Decode(json.RawMessage(`{"terminalIds":["t"],"instruction":"the vote"}`)); err != nil {
		t.Errorf("valid text extract args should decode: %v", err)
	}
}

// terminal.extract.json structurally requires BOTH instruction and jsonSchema, so a
// schema-less call can't even decode (the old runtime "jsonSchema is required when
// format is 'json'" footgun is now impossible to reach).
func TestExtractJSONRequiresInstructionAndSchema(t *testing.T) {
	tool := newExtractJSONTool(Deps{})
	for name, bad := range map[string]string{
		"missing both":        `{"terminalIds":["t"]}`,
		"missing schema":      `{"terminalIds":["t"],"instruction":"the votes"}`,
		"blank schema":        `{"terminalIds":["t"],"instruction":"the votes","jsonSchema":"  "}`,
		"missing instruction": `{"terminalIds":["t"],"jsonSchema":"{}"}`,
		"blank instruction":   `{"terminalIds":["t"],"instruction":"  ","jsonSchema":"{}"}`,
		"missing terminalIds": `{"instruction":"x","jsonSchema":"{}"}`,
		// There is no `format` arg on the json tool either — the tool IS the shape, so a
		// stray format key must be rejected as unknown, not silently tolerated.
		"stray format key": `{"terminalIds":["t"],"instruction":"x","jsonSchema":"{}","format":"json"}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("json %s should be rejected: %s", name, bad)
		}
	}
	if _, err := tool.Decode(json.RawMessage(`{"terminalIds":["t"],"instruction":"each player's vote","jsonSchema":"{\"votes\":[]}"}`)); err != nil {
		t.Errorf("valid json extract args should decode: %v", err)
	}
}

func TestExtractAsyncRequiresInstruction(t *testing.T) {
	tool := newExtractAsyncTool(Deps{})
	for name, bad := range map[string]string{
		"missing instruction": `{"terminalIds":["t"]}`,
		"empty instruction":   `{"terminalIds":["t"],"instruction":""}`,
		"blank instruction":   `{"terminalIds":["t"],"instruction":"   "}`,
		"missing terminalIds": `{"instruction":"x"}`,
		"format unknown now":  `{"terminalIds":["t"],"instruction":"x","format":"json"}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("async %s should be rejected: %s", name, bad)
		}
	}
	if _, err := tool.Decode(json.RawMessage(`{"terminalIds":["t"],"instruction":"pull the answer"}`)); err != nil {
		t.Errorf("valid async args should decode: %v", err)
	}
}

// --- Finding 4 supports: cancellation + publish-failure ---

// stubReader is a TerminalReader that always reports "running" so a wait condition
// never settles — letting us test that a cancelled context stops the poll loop.
type stubReader struct{}

func (stubReader) Connected() bool                                  { return true }
func (stubReader) ListTerminals(_ context.Context) ([]string, bool) { return nil, false }
func (stubReader) ReadStatuses(_ context.Context, ids []string, _ bool) StatusReadResult {
	byID := make(map[string]TerminalStatusEntry, len(ids))
	empty := ""
	for _, id := range ids {
		byID[id] = TerminalStatusEntry{AgentState: "working", RecentOutput: &empty}
	}
	return StatusReadResult{OK: true, ByID: byID}
}
func (stubReader) ReadOutput(_ context.Context, _ string, _ int) OutputReadResult {
	return OutputReadResult{OK: true, Value: "still going"}
}

// failingQueue.Publish always errors so we exercise the publish-failure path.
type failingQueue struct{ called int }

func (q *failingQueue) Publish(_ context.Context, _ domain.QueuePublishArgs) (domain.QueueEvent, error) {
	q.called++
	return domain.QueueEvent{}, errors.New("queue offline")
}

// Finding 4: a detached async extraction must (a) stop when its context is
// cancelled (it inherits the app-scoped ctx, not the turn's), and (b) survive a
// Publish that returns an error — the goroutine must not panic, and the failure
// must be surfaced (attempted), not swallowed silently. Here the app-scoped ctx is
// already cancelled, so the poll returns immediately with matched=false and the
// "wait timed out" event is published (and fails) without panicking.
func TestRunAsyncExtractionCancelAndPublishFailure(t *testing.T) {
	q := &failingQueue{}
	deps := Deps{Reader: stubReader{}, Queue: q}
	wait := settledWait()
	base := resolvedBase{
		terminalIDs:    []string{"t"},
		wait:           &wait,
		pollIntervalMs: 10,
		maxAttempts:    5,
		tailBytes:      100,
		maxTokens:      100,
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // simulate an app shutdown / already-cancelled app-scoped ctx

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Must not panic even though Publish errors.
		runAsyncExtraction(ctx, deps, base, extractAsyncArgs{Instruction: "x"}, "req-1")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runAsyncExtraction did not return promptly on a cancelled ctx")
	}
	if q.called == 0 {
		t.Fatal("expected a publish attempt (so the failure is surfaced, not silently dropped)")
	}
}

// A nil queue must not panic the background goroutine.
func TestRunAsyncExtractionNilQueueSafe(t *testing.T) {
	deps := Deps{Reader: stubReader{}, Queue: nil}
	wait := settledWait()
	base := resolvedBase{
		terminalIDs: []string{"t"}, wait: &wait,
		pollIntervalMs: 10, maxAttempts: 2, tailBytes: 100, maxTokens: 100,
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runAsyncExtraction(ctx, deps, base, extractAsyncArgs{Instruction: "x"}, "req-nil")
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runAsyncExtraction with a nil queue hung")
	}
}

// asyncDeadline must derive a sane, bounded deadline from the poll knobs.
func TestAsyncDeadlineBounds(t *testing.T) {
	small := asyncDeadline(resolvedBase{maxAttempts: 1, pollIntervalMs: 1})
	if small < 120*time.Second {
		t.Errorf("deadline should respect the floor, got %s", small)
	}
	big := asyncDeadline(resolvedBase{maxAttempts: 120, pollIntervalMs: 60_000})
	// 120 * 60000 * 2 + 60000 = 14_460_000 ms.
	if big <= small {
		t.Errorf("a large poll budget should yield a larger deadline: small=%s big=%s", small, big)
	}
}
