package extractionx

import (
	"context"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// routeRouter records which model surface was used (Chat vs JSON) so we can assert
// format routing.
type routeRouter struct {
	chatCalled bool
	jsonCalled bool
	chatRes    ChatResult
	jsonRes    any
}

func (r *routeRouter) Chat(_ context.Context, _ domain.ModelTier, _ []ChatMessage, _ int) (ChatResult, error) {
	r.chatCalled = true
	return r.chatRes, nil
}
func (r *routeRouter) JSON(_ context.Context, _ domain.ModelTier, _ []ChatMessage, _ int) (any, error) {
	r.jsonCalled = true
	return r.jsonRes, nil
}

// format=text routes through router.Chat (never JSON) and reports text.
func TestRunExtractTextViaChat(t *testing.T) {
	r := &routeRouter{chatRes: ChatResult{Content: "  extracted  "}}
	core := &extractCore{terminalIDs: []string{"t1"}, instruction: "get it", format: "text", maxTokens: 100}
	res, err := runExtract(context.Background(), Deps{Router: r}, core, "tail")
	if err != nil {
		t.Fatal(err)
	}
	if !r.chatCalled || r.jsonCalled {
		t.Fatalf("text must use Chat only: chat=%v json=%v", r.chatCalled, r.jsonCalled)
	}
	if res.text != "extracted" {
		t.Fatalf("text trimmed: %q", res.text)
	}
}

// format=json routes through router.JSON (never Chat) and carries the parsed value.
func TestRunExtractJSONViaJSON(t *testing.T) {
	r := &routeRouter{jsonRes: map[string]any{"k": "v"}}
	core := &extractCore{terminalIDs: []string{"t1"}, instruction: "get it", format: "json", jsonSchema: "{}", maxTokens: 100}
	res, err := runExtract(context.Background(), Deps{Router: r}, core, "tail")
	if err != nil {
		t.Fatal(err)
	}
	if r.chatCalled || !r.jsonCalled {
		t.Fatalf("json must use JSON only: chat=%v json=%v", r.chatCalled, r.jsonCalled)
	}
	if m, _ := res.json.(map[string]any); m["k"] != "v" {
		t.Fatalf("json value not carried: %v", res.json)
	}
}

// text extraction flags truncation on a length finishReason.
func TestRunExtractTextFlagsTruncation(t *testing.T) {
	r := &routeRouter{chatRes: ChatResult{Content: "partial", FinishReason: "length"}}
	core := &extractCore{terminalIDs: []string{"t1"}, instruction: "x", format: "text", maxTokens: 10}
	res, _ := runExtract(context.Background(), Deps{Router: r}, core, "tail")
	if !res.truncated {
		t.Fatal("length finishReason should flag truncation")
	}
}

// fakeReader serves a scripted status map + deep-read outputs, recording which
// terminals required a deep ReadOutput.
type fakeReader struct {
	statuses   StatusReadResult
	deepOut    map[string]string
	deepReads  []string
	deepFailed map[string]bool
}

func (r *fakeReader) Connected() bool { return true }
func (r *fakeReader) ReadStatuses(_ context.Context, _ []string, _ bool) StatusReadResult {
	return r.statuses
}
func (r *fakeReader) ReadOutput(_ context.Context, id string, _ int) OutputReadResult {
	r.deepReads = append(r.deepReads, id)
	if r.deepFailed[id] {
		return OutputReadResult{OK: false}
	}
	return OutputReadResult{OK: true, Value: r.deepOut[id]}
}

func strp(s string) *string { return &s }

// runtimeStatus is "exited" only when ALL terminals exited (a single running one
// keeps the aggregate running).
func TestReadSignalsAllExitedGating(t *testing.T) {
	// Two terminals, both exited → aggregate exited.
	bothExited := &fakeReader{statuses: StatusReadResult{OK: true, ByID: map[string]TerminalStatusEntry{
		"a": {AgentState: "exited", RecentOutput: strp("done a")},
		"b": {AgentState: "exited", RecentOutput: strp("done b")},
	}}}
	r := readSignals(context.Background(), Deps{Reader: bothExited}, []string{"a", "b"}, 12000,
		map[string]*terminalState{}, 1000)
	if r.signals.RuntimeStatus != "exited" || !r.finished {
		t.Fatalf("both exited should be exited+finished, got %q finished=%v", r.signals.RuntimeStatus, r.finished)
	}

	// One still running → aggregate running, not finished.
	oneRunning := &fakeReader{statuses: StatusReadResult{OK: true, ByID: map[string]TerminalStatusEntry{
		"a": {AgentState: "exited", RecentOutput: strp("done a")},
		"b": {AgentState: "working", RecentOutput: strp("busy b")},
	}}}
	r = readSignals(context.Background(), Deps{Reader: oneRunning}, []string{"a", "b"}, 12000,
		map[string]*terminalState{}, 1000)
	if r.signals.RuntimeStatus == "exited" || r.finished {
		t.Fatalf("one running should NOT be exited/finished, got %q finished=%v", r.signals.RuntimeStatus, r.finished)
	}
}

// The inline recentOutput is used when it already covers the requested window;
// a deep ReadOutput is the fallback when it does not.
func TestReadSignalsInlineVsDeepFallback(t *testing.T) {
	// recentOutput longer than tailBytes → inline tail used, no deep read.
	inline := &fakeReader{statuses: StatusReadResult{OK: true, ByID: map[string]TerminalStatusEntry{
		"a": {AgentState: "working", RecentOutput: strp("0123456789")},
	}}}
	r := readSignals(context.Background(), Deps{Reader: inline}, []string{"a"}, 4,
		map[string]*terminalState{}, 1000)
	if len(inline.deepReads) != 0 {
		t.Fatalf("inline coverage should skip deep read, did %v", inline.deepReads)
	}
	if r.signals.Tail != "6789" {
		t.Fatalf("inline tail not capped to last runes: %q", r.signals.Tail)
	}

	// recentOutput shorter than tailBytes → deep getOutput fallback drives the tail.
	deep := &fakeReader{
		statuses: StatusReadResult{OK: true, ByID: map[string]TerminalStatusEntry{
			"a": {AgentState: "working", RecentOutput: strp("hi")},
		}},
		deepOut: map[string]string{"a": "full deep scrollback"},
	}
	r = readSignals(context.Background(), Deps{Reader: deep}, []string{"a"}, 4,
		map[string]*terminalState{}, 1000)
	if len(deep.deepReads) != 1 || deep.deepReads[0] != "a" {
		t.Fatalf("short inline should trigger a deep read, did %v", deep.deepReads)
	}
	if r.signals.Tail != "full deep scrollback" {
		t.Fatalf("deep tail not used: %q", r.signals.Tail)
	}
}
