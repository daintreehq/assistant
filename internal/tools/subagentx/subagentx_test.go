package subagentx

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/assistant/internal/subagent"
	"github.com/daintreehq/assistant/internal/tools"
)

// fakeRunner records the brief it was handed and replays a canned report, so the
// tests can assert on the ARGUMENT TRANSLATION (which is this package's actual
// job) without a backend.
type fakeRunner struct {
	mu       sync.Mutex
	got      subagent.Brief
	report   subagent.Report
	progress []string
	// emit, when set, is pushed through the progress callback before returning.
	emit []string
}

func (f *fakeRunner) Run(_ context.Context, b subagent.Brief, p subagent.Progress) subagent.Report {
	f.mu.Lock()
	f.got = b
	f.mu.Unlock()
	for _, m := range f.emit {
		if p != nil {
			p(m)
		}
	}
	return f.report
}

func theTool(t *testing.T, deps Deps) tools.Tool {
	t.Helper()
	all := Tools(deps)
	if len(all) != 1 {
		t.Fatalf("Tools() returned %d tools, want 1", len(all))
	}
	return all[0]
}

// decode runs the tool's STRICT decoder, which is where argument validation
// actually lives — calling the handler directly would skip it.
func decode(t *testing.T, tool tools.Tool, raw string) (json.RawMessage, error) {
	t.Helper()
	return tool.Decode(json.RawMessage(raw))
}

func TestTool_Shape(t *testing.T) {
	tool := theTool(t, Deps{})
	if tool.Name != "subagent.run" {
		t.Errorf("name = %q", tool.Name)
	}
	// Read risk is the whole safety story: a sub-agent is offered read-only tools,
	// so the delegation itself performs only read-risk work and needs no
	// confirmation. If this ever changes, the tier policy and the approval matrix
	// both need revisiting — hence the assertion.
	if tool.Risk != "read" {
		t.Errorf("risk = %q, want read", tool.Risk)
	}
	if !tool.Parallelizable {
		t.Error("subagent.run must be Parallelizable — fan-out is the point")
	}
}

func TestValidate_RejectsAnEmptyTask(t *testing.T) {
	tool := theTool(t, Deps{})
	for _, raw := range []string{`{}`, `{"task":""}`, `{"task":"   "}`} {
		if _, err := decode(t, tool, raw); err == nil {
			t.Errorf("decode(%s) accepted an empty task", raw)
		}
	}
}

func TestValidate_RejectsAnOversizedBrief(t *testing.T) {
	tool := theTool(t, Deps{})
	big, _ := json.Marshal(map[string]string{"task": strings.Repeat("x", maxTaskRunes+1)})
	_, err := decode(t, tool, string(big))
	if err == nil {
		t.Fatal("decode accepted an over-long task")
	}
	// The message has to say what to do instead, or the model just retries the same.
	if !strings.Contains(err.Error(), "context") {
		t.Errorf("error = %q, want it to steer toward the context field", err)
	}
}

func TestValidate_RejectsMaxRoundsOutOfRange(t *testing.T) {
	tool := theTool(t, Deps{})
	for _, raw := range []string{`{"task":"t","maxRounds":0}`, `{"task":"t","maxRounds":99}`} {
		if _, err := decode(t, tool, raw); err == nil {
			t.Errorf("decode(%s) accepted an out-of-range maxRounds", raw)
		}
	}
	if _, err := decode(t, tool, `{"task":"t","maxRounds":6}`); err != nil {
		t.Errorf("decode rejected a valid maxRounds: %v", err)
	}
}

func TestValidate_RejectsUnknownFields(t *testing.T) {
	tool := theTool(t, Deps{})
	// The strict decoder is what turns a model's invented argument into a
	// correctable INVALID_ARGS instead of a silently ignored instruction.
	if _, err := decode(t, tool, `{"task":"t","prompt":"do the thing"}`); err == nil {
		t.Error("decode accepted an unknown field")
	}
}

func TestHandle_TranslatesTheBriefAndReturnsTheReport(t *testing.T) {
	fr := &fakeRunner{report: subagent.Report{
		ID: "sub_abc", Status: subagent.StatusCompleted,
		Text: "issue #4021 — terrain flicker", Rounds: 4, ToolCalls: 9,
		TranscriptID: "artifact_xyz", DurationMS: 12_400,
	}}
	tool := theTool(t, Deps{Runner: fr})

	raw, err := decode(t, tool, `{"task":"find the flicker issue","context":"user tried #42","deliverable":"number and title","maxRounds":6}`)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := tool.Handle(context.Background(), raw, &tools.ToolContext{})

	if !res.Ok {
		t.Fatalf("result not ok: %+v", res.Error)
	}
	if fr.got.Task != "find the flicker issue" || fr.got.Context != "user tried #42" ||
		fr.got.Deliverable != "number and title" || fr.got.MaxRounds != 6 {
		t.Errorf("brief = %+v, want every field forwarded", fr.got)
	}
	rep, ok := res.Result.(subagent.Report)
	if !ok {
		t.Fatalf("result payload is %T, want subagent.Report", res.Result)
	}
	if rep.Text != "issue #4021 — terrain flicker" || rep.TranscriptID != "artifact_xyz" {
		t.Errorf("report = %+v, want the runner's report verbatim", rep)
	}
	if !strings.Contains(res.Summary, "4 rounds") || !strings.Contains(res.Summary, "9 tool calls") {
		t.Errorf("summary = %q, want the counters", res.Summary)
	}
}

// A partial report that reads as complete is the most damaging thing this tool can
// return, so the summary must say so loudly.
func TestHandle_ExhaustedRunIsMarkedPartial(t *testing.T) {
	fr := &fakeRunner{report: subagent.Report{
		Status: subagent.StatusExhausted, Partial: true,
		Text: "only checked internal/", Rounds: 10, ToolCalls: 22,
	}}
	tool := theTool(t, Deps{Runner: fr})
	raw, _ := decode(t, tool, `{"task":"t"}`)

	res := tool.Handle(context.Background(), raw, &tools.ToolContext{})

	if !res.Ok {
		t.Fatal("an exhausted run still produced a finding — it must not be a tool failure")
	}
	if !strings.Contains(res.Summary, "PARTIAL") {
		t.Errorf("summary = %q, want it to shout PARTIAL", res.Summary)
	}
}

func TestHandle_FailedRunIsARecoverableToolFailure(t *testing.T) {
	fr := &fakeRunner{report: subagent.Report{Status: subagent.StatusFailed, Note: "the backend call failed"}}
	tool := theTool(t, Deps{Runner: fr})
	raw, _ := decode(t, tool, `{"task":"t"}`)

	res := tool.Handle(context.Background(), raw, &tools.ToolContext{})

	if res.Ok {
		t.Fatal("a failed run must be a tool failure")
	}
	if res.Error.Code != codeFailed {
		t.Errorf("code = %q, want %q", res.Error.Code, codeFailed)
	}
	// Recoverable: "the delegate could not run" is never a reason to abandon the
	// question — the caller can retry or do it itself.
	if !res.Error.Recoverable {
		t.Error("a failed delegation must stay recoverable")
	}
	if !strings.Contains(res.Error.Message, "this thread") {
		t.Errorf("message = %q, want it to name the fallback", res.Error.Message)
	}
}

func TestHandle_NoRunnerFailsCleanly(t *testing.T) {
	tool := theTool(t, Deps{})
	raw, _ := decode(t, tool, `{"task":"t"}`)

	res := tool.Handle(context.Background(), raw, &tools.ToolContext{})

	if res.Ok || res.Error.Code != codeUnavailable {
		t.Fatalf("want %s, got %+v", codeUnavailable, res)
	}
}

// Progress must reach the registry's beat, or the cockpit shows one frozen row for
// a run that can last a minute.
func TestHandle_ForwardsProgressToTheToolContext(t *testing.T) {
	fr := &fakeRunner{
		report: subagent.Report{Status: subagent.StatusCompleted, Text: "ok"},
		emit:   []string{"round 1/10", "round 2/10 · fs.search"},
	}
	tool := theTool(t, Deps{Runner: fr})
	raw, _ := decode(t, tool, `{"task":"t"}`)

	var got []string
	tctx := &tools.ToolContext{ReportProgress: func(p tools.ToolProgress) { got = append(got, p.Message) }}
	tool.Handle(context.Background(), raw, tctx)

	if len(got) != 2 || got[0] != "round 1/10" || !strings.Contains(got[1], "fs.search") {
		t.Errorf("progress = %v, want both beats forwarded", got)
	}
}

// A ToolContext with no progress hook (a non-interactive actor, or a test) must
// not panic the run.
func TestHandle_NilProgressHookIsSafe(t *testing.T) {
	fr := &fakeRunner{
		report: subagent.Report{Status: subagent.StatusCompleted, Text: "ok"},
		emit:   []string{"round 1/10"},
	}
	tool := theTool(t, Deps{Runner: fr})
	raw, _ := decode(t, tool, `{"task":"t"}`)

	if res := tool.Handle(context.Background(), raw, nil); !res.Ok {
		t.Fatalf("nil ToolContext broke the call: %+v", res.Error)
	}
}

func TestSummarize(t *testing.T) {
	for _, tc := range []struct {
		name string
		rep  subagent.Report
		want []string
	}{
		{"completed", subagent.Report{Status: subagent.StatusCompleted, Rounds: 1, ToolCalls: 1},
			[]string{"Reported back", "1 round,", "1 tool call"}},
		// No "Sub-agent" prefix — the activity row renders the verb, and repeating
		// it here produced "Sub-agent  Sub-agent reported back".
		{"no verb stutter", subagent.Report{Status: subagent.StatusCompleted}, nil},
		{"failed calls", subagent.Report{Status: subagent.StatusCompleted, Rounds: 3, ToolCalls: 5, FailedCalls: 2},
			[]string{"(2 failed)"}},
		{"cancelled", subagent.Report{Status: subagent.StatusCancelled}, []string{"Cancelled"}},
		// PARTIAL leads so it survives the row renderer's truncation.
		{"partial leads", subagent.Report{Status: subagent.StatusExhausted, Rounds: 11}, []string{"PARTIAL"}},
		{"duration", subagent.Report{Status: subagent.StatusCompleted, DurationMS: 2500}, []string{"2.5s"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := summarize(tc.rep)
			if strings.Contains(got, "Sub-agent") {
				t.Errorf("summary stutters against the row verb: %q", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("summarize = %q, want %q in it", got, w)
				}
			}
		})
	}
}
