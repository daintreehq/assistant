package agent

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// --- fakes ---

// fakeRunEventStore records inserted rows, ordered by insertion (mirrors
// db.listRunEvents which returns them seq-ordered within a run).
type fakeRunEventStore struct {
	rows []domain.RunEventRecord
}

func (f *fakeRunEventStore) InsertRunEvent(rec domain.RunEventRecord) (domain.RunEventRecord, error) {
	f.rows = append(f.rows, rec)
	return rec, nil
}

func (f *fakeRunEventStore) forRun(runID string) []domain.RunEventRecord {
	var out []domain.RunEventRecord
	for _, r := range f.rows {
		if r.RunID == runID {
			out = append(out, r)
		}
	}
	return out
}

type brokenRunEventStore struct{}

func (brokenRunEventStore) InsertRunEvent(domain.RunEventRecord) (domain.RunEventRecord, error) {
	return domain.RunEventRecord{}, errors.New("disk full")
}

func typesOf(rows []domain.RunEventRecord) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Type
	}
	return out
}

func seqsOf(rows []domain.RunEventRecord) []int {
	out := make([]int, len(rows))
	for i, r := range rows {
		out[i] = r.Seq
	}
	return out
}

// --- RunEventSink ---

func TestRunEventSinkNoOpWhenNoRunActive(t *testing.T) {
	store := &fakeRunEventStore{}
	ref := &RunIDRef{} // no current run
	sink := NewRunEventSink(store, ref)
	sink.AssistantStart()
	sink.AssistantToken("hi")
	sink.AssistantEnd("hi", "")
	if len(store.rows) != 0 {
		t.Fatalf("wrote %d rows with no active run, want 0", len(store.rows))
	}
}

func TestRunEventSinkTypedSeqOrderedRows(t *testing.T) {
	store := &fakeRunEventStore{}
	ref := &RunIDRef{}
	ref.Set("run_1")
	sink := NewRunEventSink(store, ref)
	sink.AssistantStart()
	sink.ToolCall(ToolCallEvent{ID: "c1", Name: "fs.read", Args: `{"path":"a"}`})
	sink.ToolResult(ToolResultEvent{ID: "c1", Name: "fs.read", Result: domain.Ok("read a", nil)})
	sink.AssistantEnd("done", "")

	rows := store.forRun("run_1")
	wantTypes := []string{"assistant:start", "tool:call", "tool:result", "assistant:end"}
	if got := typesOf(rows); !equalStrings(got, wantTypes) {
		t.Fatalf("types = %v want %v", got, wantTypes)
	}
	if got := seqsOf(rows); !equalInts(got, []int{0, 1, 2, 3}) {
		t.Fatalf("seqs = %v want [0 1 2 3]", got)
	}
	// tool:call carries the parsed args.
	var call map[string]any
	mustUnmarshal(t, *rows[1].Payload, &call)
	if call["id"] != "c1" || call["name"] != "fs.read" {
		t.Fatalf("tool:call payload = %v", call)
	}
	if args, _ := call["args"].(map[string]any); args["path"] != "a" {
		t.Fatalf("tool:call args = %v", call["args"])
	}
	// tool:result carries the flattened ok/summary.
	var result map[string]any
	mustUnmarshal(t, *rows[2].Payload, &result)
	if result["ok"] != true || result["summary"] != "read a" {
		t.Fatalf("tool:result payload = %v", result)
	}
	// assistant:end carries content.
	var end map[string]any
	mustUnmarshal(t, *rows[3].Payload, &end)
	if end["content"] != "done" {
		t.Fatalf("assistant:end payload = %v", end)
	}
}

func TestRunEventSinkTurnPromptIsFirstRow(t *testing.T) {
	store := &fakeRunEventStore{}
	ref := &RunIDRef{}
	ref.Set("run_1")
	sink := NewRunEventSink(store, ref)
	sink.TurnPrompt("explain the build failure")
	sink.AssistantStart()

	rows := store.forRun("run_1")
	if got := typesOf(rows); !equalStrings(got, []string{"turn:prompt", "assistant:start"}) {
		t.Fatalf("types = %v want [turn:prompt assistant:start]", got)
	}
	if rows[0].Seq != 0 {
		t.Fatalf("turn:prompt must be seq 0, got %d", rows[0].Seq)
	}
	var p map[string]any
	mustUnmarshal(t, *rows[0].Payload, &p)
	if p["prompt"] != "explain the build failure" {
		t.Fatalf("turn:prompt payload = %v", p)
	}
}

func TestRunEventSinkDropsFinalRoundTokens(t *testing.T) {
	store := &fakeRunEventStore{}
	ref := &RunIDRef{}
	ref.Set("run_1")
	sink := NewRunEventSink(store, ref)
	sink.AssistantStart()
	sink.AssistantToken("Hel")
	sink.AssistantToken("lo")
	sink.AssistantEnd("Hello", "")
	// assistant:end is authoritative — buffered tokens dropped, no content row.
	want := []string{"assistant:start", "assistant:end"}
	if got := typesOf(store.forRun("run_1")); !equalStrings(got, want) {
		t.Fatalf("types = %v want %v", got, want)
	}
}

func TestRunEventSinkFlushesIntermediateProse(t *testing.T) {
	store := &fakeRunEventStore{}
	ref := &RunIDRef{}
	ref.Set("run_1")
	sink := NewRunEventSink(store, ref)
	// Round 1: prose, then a tool call (no assistant:end fires).
	sink.AssistantStart()
	sink.AssistantToken("Let me ")
	sink.AssistantToken("check.")
	sink.ToolCall(ToolCallEvent{ID: "c1", Name: "fs.read", Args: ""})
	sink.ToolResult(ToolResultEvent{ID: "c1", Name: "fs.read", Result: domain.Ok("ok", nil)})
	// Round 2: the final answer.
	sink.AssistantStart()
	sink.AssistantEnd("done", "")

	rows := store.forRun("run_1")
	want := []string{"assistant:start", "assistant:content", "tool:call", "tool:result", "assistant:start", "assistant:end"}
	if got := typesOf(rows); !equalStrings(got, want) {
		t.Fatalf("types = %v want %v", got, want)
	}
	var content map[string]any
	mustUnmarshal(t, *rows[1].Payload, &content)
	if content["content"] != "Let me check." {
		t.Fatalf("intermediate prose = %v", content)
	}
}

func TestRunEventSinkToolResultCarriesAuditID(t *testing.T) {
	store := &fakeRunEventStore{}
	ref := &RunIDRef{}
	ref.Set("run_1")
	sink := NewRunEventSink(store, ref)
	res := domain.Ok("ok", nil)
	res.AuditID = "aud_1234"
	sink.ToolResult(ToolResultEvent{ID: "c1", Name: "fs.read", Result: res})
	var payload map[string]any
	mustUnmarshal(t, *store.forRun("run_1")[0].Payload, &payload)
	if payload["auditId"] != "aud_1234" {
		t.Fatalf("auditId = %v want aud_1234", payload["auditId"])
	}
}

func TestRunEventSinkCapsOversizedPayload(t *testing.T) {
	store := &fakeRunEventStore{}
	ref := &RunIDRef{}
	ref.Set("run_1")
	sink := NewRunEventSink(store, ref)
	sink.AssistantEnd(strings.Repeat("x", 100_000), "")
	var payload map[string]any
	mustUnmarshal(t, *store.forRun("run_1")[0].Payload, &payload)
	if payload["truncated"] != true {
		t.Fatalf("expected truncated:true, got %v", payload)
	}
	if bytes, _ := payload["bytes"].(float64); bytes <= 100_000 {
		t.Fatalf("bytes = %v want > 100000", payload["bytes"])
	}
	if _, ok := payload["preview"].(string); !ok {
		t.Fatal("expected a string preview")
	}
}

func TestRunEventSinkResetsSeqOnRunIDChange(t *testing.T) {
	store := &fakeRunEventStore{}
	ref := &RunIDRef{}
	ref.Set("run_1")
	sink := NewRunEventSink(store, ref)
	sink.AssistantStart()
	sink.AssistantEnd("a", "")
	ref.Set("run_2")
	sink.AssistantStart()
	sink.AssistantEnd("b", "")
	if got := seqsOf(store.forRun("run_1")); !equalInts(got, []int{0, 1}) {
		t.Fatalf("run_1 seqs = %v want [0 1]", got)
	}
	if got := seqsOf(store.forRun("run_2")); !equalInts(got, []int{0, 1}) {
		t.Fatalf("run_2 seqs = %v want [0 1]", got)
	}
}

func TestRunEventSinkSwallowsDBWriteFailure(t *testing.T) {
	ref := &RunIDRef{}
	ref.Set("run_1")
	sink := NewRunEventSink(brokenRunEventStore{}, ref)
	// Must not panic despite every insert throwing.
	sink.AssistantStart()
	sink.AssistantEnd("done", "")
}

func TestRunEventSinkUnserializableFallback(t *testing.T) {
	// A payload that can't be JSON-encoded (a channel value) must still serialize to
	// a stable {error:"unserializable"} stub rather than panicking or returning "".
	out := serializeRunPayload(map[string]any{"bad": make(chan int)})
	var payload map[string]any
	mustUnmarshal(t, out, &payload)
	if payload["error"] != "unserializable" {
		t.Fatalf("fallback payload = %v want {error:unserializable}", payload)
	}
}

// --- MultiSink isolation ---

// throwingSink panics on every method to prove MultiSink isolates a misbehaving
// sink so the healthy one still receives every event.
type throwingSink struct{}

func (throwingSink) Phase(domain.RunPhase)       { panic("boom") }
func (throwingSink) AssistantStart()             { panic("boom") }
func (throwingSink) AssistantToken(string)       { panic("boom") }
func (throwingSink) AssistantEnd(string, string) { panic("boom") }
func (throwingSink) AssistantCancelled(string)   { panic("boom") }
func (throwingSink) ToolBatch([]BatchedToolCall) { panic("boom") }
func (throwingSink) ToolState(string, ToolState) { panic("boom") }
func (throwingSink) ToolProgress(string, string) { panic("boom") }
func (throwingSink) ToolCall(ToolCallEvent)      { panic("boom") }
func (throwingSink) ToolResult(ToolResultEvent)  { panic("boom") }
func (throwingSink) Error(string)                { panic("boom") }
func (throwingSink) Info(string)                 { panic("boom") }
func (throwingSink) Usage(UsageEvent)            { panic("boom") }
func (throwingSink) TurnPrompt(string)           { panic("boom") }

// recordingSink captures a flat string log of received events.
type recordingSink struct {
	NoopEventSink
	log []string
}

func (r *recordingSink) TurnPrompt(p string)      { r.log = append(r.log, "prompt:"+p) }
func (r *recordingSink) AssistantStart()          { r.log = append(r.log, "start") }
func (r *recordingSink) AssistantEnd(c, _ string) { r.log = append(r.log, "end:"+c) }
func (r *recordingSink) Info(m string)            { r.log = append(r.log, "info:"+m) }

func TestMultiSinkDeliversWhenOneThrows(t *testing.T) {
	healthy := &recordingSink{}
	// Throwing sink FIRST so its panic can't short-circuit the healthy one.
	fan := NewMultiSink(throwingSink{}, healthy)
	fan.TurnPrompt("p")
	fan.AssistantStart()
	fan.AssistantEnd("hi", "")
	fan.Info("note")
	want := []string{"prompt:p", "start", "end:hi", "info:note"}
	if !equalStrings(healthy.log, want) {
		t.Fatalf("healthy log = %v want %v", healthy.log, want)
	}
}

// --- helpers ---

func mustUnmarshal(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("payload not valid JSON: %v\n%s", err, s)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
