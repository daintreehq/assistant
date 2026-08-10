package extractionx

import (
	"context"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// routeRouter records which extraction task was used (ExtractText vs ExtractJSON)
// so we can assert format routing. textRes/textTruncated script the text task;
// jsonRes scripts the json task. judgeFn scripts the settle-gate finished
// confirmation; judgeCalls counts how often it was consulted (so a test can assert
// the judge was NOT called on a non-settle path).
type routeRouter struct {
	textCalled    bool
	jsonCalled    bool
	textRes       string
	textTruncated bool
	jsonRes       any
	judgeFn       func(JudgeInput) domain.ModelJudgeAnswer
	judgeCalls    int
}

func (r *routeRouter) ExtractText(_ context.Context, _ string, _ []string, _ string) (string, bool, error) {
	r.textCalled = true
	return r.textRes, r.textTruncated, nil
}
func (r *routeRouter) ExtractJSON(_ context.Context, _ string, _ []string, _ string, _ map[string]any) (any, error) {
	r.jsonCalled = true
	return r.jsonRes, nil
}
func (r *routeRouter) Verdict(_ context.Context, _ string, _ string) (bool, string, error) {
	return false, "", nil
}
func (r *routeRouter) Judge(_ context.Context, in JudgeInput) (domain.ModelJudgeAnswer, error) {
	r.judgeCalls++
	if r.judgeFn != nil {
		return r.judgeFn(in), nil
	}
	return domain.ModelJudgeAnswer{}, nil
}

// format=text routes through ExtractText (never ExtractJSON) and reports text.
func TestRunExtractTextViaExtractText(t *testing.T) {
	r := &routeRouter{textRes: "  extracted  "}
	core := &extractCore{terminalIDs: []string{"t1"}, instruction: "get it", format: "text", maxTokens: 100}
	res, err := runExtract(context.Background(), Deps{Router: r}, core, "tail")
	if err != nil {
		t.Fatal(err)
	}
	if !r.textCalled || r.jsonCalled {
		t.Fatalf("text must use ExtractText only: text=%v json=%v", r.textCalled, r.jsonCalled)
	}
	if res.text != "extracted" {
		t.Fatalf("text trimmed: %q", res.text)
	}
}

// format=json routes through ExtractJSON (never ExtractText) and carries the parsed value.
func TestRunExtractJSONViaExtractJSON(t *testing.T) {
	r := &routeRouter{jsonRes: map[string]any{"k": "v"}}
	core := &extractCore{terminalIDs: []string{"t1"}, instruction: "get it", format: "json", jsonSchema: "{}", maxTokens: 100}
	res, err := runExtract(context.Background(), Deps{Router: r}, core, "tail")
	if err != nil {
		t.Fatal(err)
	}
	if r.textCalled || !r.jsonCalled {
		t.Fatalf("json must use ExtractJSON only: text=%v json=%v", r.textCalled, r.jsonCalled)
	}
	if m, _ := res.json.(map[string]any); m["k"] != "v" {
		t.Fatalf("json value not carried: %v", res.json)
	}
}

// text extraction propagates the backend's truncated flag.
func TestRunExtractTextFlagsTruncation(t *testing.T) {
	r := &routeRouter{textRes: "partial", textTruncated: true}
	core := &extractCore{terminalIDs: []string{"t1"}, instruction: "x", format: "text", maxTokens: 10}
	res, _ := runExtract(context.Background(), Deps{Router: r}, core, "tail")
	if !res.truncated {
		t.Fatal("a truncated extract should flag truncation")
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

func (r *fakeReader) Connected() bool                                  { return true }
func (r *fakeReader) ListTerminals(_ context.Context) ([]string, bool) { return nil, false }
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

// A per-entry "terminal not found" (Daintree's shape for a dropped id — the batch
// never omits it) must read as exited, with no deep-read fallback for the dead id:
// treating it as a present-but-blank entry would poll forever.
func TestReadSignalsNotFoundEntryIsGone(t *testing.T) {
	reader := &fakeReader{statuses: StatusReadResult{OK: true, ByID: map[string]TerminalStatusEntry{
		"a": {NotFound: true},
		"b": {AgentState: "exited", RecentOutput: strp("done b")},
	}}}
	r := readSignals(context.Background(), Deps{Reader: reader}, []string{"a", "b"}, 12000,
		map[string]*terminalState{}, 1000)
	if r.signals.RuntimeStatus != "exited" || !r.finished {
		t.Fatalf("a dropped terminal plus an exited one should aggregate exited+finished, got %q finished=%v",
			r.signals.RuntimeStatus, r.finished)
	}
	for _, id := range reader.deepReads {
		if id == "a" {
			t.Fatalf("a gone terminal must not be deep-read, reads=%v", reader.deepReads)
		}
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

	// recentOutput LONG ENOUGH to cover the window but WHITESPACE-ONLY (a bottom-padded
	// TUI's screen grab, e.g. Codex) must NOT short-circuit the deep read — otherwise the
	// blank inline is used and a finished agent reads as empty and never settles. The deep
	// getOutput fallback must drive the tail instead.
	blankLong := &fakeReader{
		statuses: StatusReadResult{OK: true, ByID: map[string]TerminalStatusEntry{
			"a": {AgentState: "waiting", WaitingReason: "prompt", RecentOutput: strp("\r\n\r\n\r\n")},
		}},
		deepOut: map[string]string{"a": "the real finished answer"},
	}
	r = readSignals(context.Background(), Deps{Reader: blankLong}, []string{"a"}, 4,
		map[string]*terminalState{}, 1000)
	if len(blankLong.deepReads) != 1 || blankLong.deepReads[0] != "a" {
		t.Fatalf("blank-but-long inline should fall through to a deep read, did %v", blankLong.deepReads)
	}
	if r.signals.Tail != "the real finished answer" {
		t.Fatalf("deep tail should drive when the inline is blank, got %q", r.signals.Tail)
	}
}
