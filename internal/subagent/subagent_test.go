package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// --- fakes -----------------------------------------------------------------

// fakeBackend replays a scripted sequence of rounds and records every request it
// was sent, so a test can assert on the WIRE (profile, session isolation, tool
// choice) as well as on the outcome.
type fakeBackend struct {
	mu       sync.Mutex
	rounds   []roundScript
	next     int
	requests []backend.RespondRequest
	// block, when set, holds the rounds named in blockRounds until the channel
	// closes (or the round's context expires) — for the cancel and deadline tests.
	// Named explicitly rather than "all" because the two cases need opposite
	// shapes: the deadline test must let the WRAP-UP round through to produce its
	// partial report, while the wrap-up-cancel test must hold that same round open
	// so the cancel has something to interrupt.
	block       chan struct{}
	blockRounds map[int]bool
}

type roundScript struct {
	content string
	calls   []backend.ToolCall
	err     error
	usage   backend.Usage
	cost    *float64
	state   string
}

func (f *fakeBackend) RespondStream(ctx context.Context, req backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	i := f.next
	f.next++
	blocker := f.block
	f.mu.Unlock()

	if blocker != nil && (f.blockRounds == nil || f.blockRounds[i]) {
		select {
		case <-blocker:
		case <-ctx.Done():
			return backend.RespondResult{}, ctx.Err()
		}
	}
	if i >= len(f.rounds) {
		return backend.RespondResult{}, errors.New("fakeBackend: no script left for round " + itoa(i))
	}
	r := f.rounds[i]
	if r.err != nil {
		return backend.RespondResult{}, r.err
	}
	if cb.OnMeta != nil {
		state := r.state
		if state == "" {
			state = "state-" + itoa(i)
		}
		cb.OnMeta(backend.StreamMeta{State: state, Model: "fake-model"})
	}
	res := backend.RespondResult{
		Message: backend.RespondMessage{Role: "assistant", Content: r.content, ToolCalls: r.calls},
		Usage:   r.usage,
	}
	if r.cost != nil {
		res.Cost = &backend.TurnCost{Total: *r.cost}
	}
	return res, nil
}

func (f *fakeBackend) snapshot() []backend.RespondRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]backend.RespondRequest(nil), f.requests...)
}

func itoa(i int) string { return string(rune('0' + i)) }

// fakeTools offers a fixed inventory and returns scripted results.
type fakeTools struct {
	names   []string
	results map[string]domain.ToolResult
	mu      sync.Mutex
	calls   []string
}

func newFakeTools(names ...string) *fakeTools {
	return &fakeTools{names: names, results: map[string]domain.ToolResult{}}
}

func (f *fakeTools) Tools() ([]models.ChatTool, error) {
	out := make([]models.ChatTool, 0, len(f.names))
	for _, n := range f.names {
		out = append(out, models.ChatTool{
			Type: "function",
			Function: models.ChatToolFunc{
				Name:        wire(n),
				Description: "fake " + n,
				Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
			},
		})
	}
	return out, nil
}

func (f *fakeTools) ResolveWireName(w string) string {
	for _, n := range f.names {
		if wire(n) == w {
			return n
		}
	}
	return ""
}

func (f *fakeTools) Dispatch(_ context.Context, name, _ string) domain.ToolResult {
	f.mu.Lock()
	f.calls = append(f.calls, name)
	f.mu.Unlock()
	if r, ok := f.results[name]; ok {
		return r
	}
	return domain.Ok("did "+name, map[string]any{"tool": name})
}

func (f *fakeTools) dispatched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func wire(dotted string) string { return strings.ReplaceAll(dotted, ".", "__") }

type memSink struct {
	mu       sync.Mutex
	contents []string
}

func (m *memSink) Put(c string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.contents = append(m.contents, c)
	return "artifact_test" + itoa(len(m.contents)-1)
}

func (m *memSink) last() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.contents) == 0 {
		return ""
	}
	return m.contents[len(m.contents)-1]
}

func toolCall(id, dotted, args string) backend.ToolCall {
	return backend.ToolCall{
		ID:       id,
		Type:     "function",
		Function: backend.FunctionCall{Name: wire(dotted), Arguments: args},
	}
}

// newRunner wires a runner over the fakes, with the transcript sink attached.
func newRunner(t *testing.T, be *fakeBackend, ft *fakeTools, sink TranscriptSink) *Runner {
	t.Helper()
	return New(Deps{Backend: be, Tools: ft, Transcript: sink})
}

// --- tests -----------------------------------------------------------------

func TestRun_CompletesAndReportsOnlyTheFinalMessage(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{calls: []backend.ToolCall{toolCall("c1", "fs.search", `{"query":"terrain"}`)}},
		{calls: []backend.ToolCall{toolCall("c2", "fs.read", `{"path":"a.go"}`)}},
		{content: "internal/terrain/mesh.go:214 — the flicker is the chunk-border normal.",
			usage: backend.Usage{PromptTokens: 900, CompletionTokens: 40}},
	}}
	ft := newFakeTools("fs.search", "fs.read")
	sink := &memSink{}

	rep := newRunner(t, be, ft, sink).Run(context.Background(), Brief{Task: "find the flicker"}, nil)

	if rep.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed (note %q)", rep.Status, rep.Note)
	}
	if rep.Partial {
		t.Error("a completed run must not be marked partial")
	}
	if !strings.Contains(rep.Text, "mesh.go:214") {
		t.Errorf("report = %q, want the final message", rep.Text)
	}
	// The whole point: the report is the FINAL message only — no tool output.
	if strings.Contains(rep.Text, "did fs.search") {
		t.Error("the report leaked tool output into the caller's context")
	}
	if rep.Rounds != 3 || rep.ToolCalls != 2 {
		t.Errorf("rounds=%d toolCalls=%d, want 3/2", rep.Rounds, rep.ToolCalls)
	}
	if got := ft.dispatched(); len(got) != 2 || got[0] != "fs.search" || got[1] != "fs.read" {
		t.Errorf("dispatched = %v, want [fs.search fs.read]", got)
	}
	if rep.TranscriptID == "" {
		t.Error("no transcript id — the run left no receipt")
	}
	if !strings.HasPrefix(rep.ID, domain.PrefixSubagent) {
		t.Errorf("id = %q, want a %s… handle", rep.ID, domain.PrefixSubagent)
	}
}

// The transcript is the receipt: it must hold what the report deliberately omits.
func TestRun_TranscriptHoldsTheToolOutputTheReportDrops(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{calls: []backend.ToolCall{toolCall("c1", "fs.search", `{"query":"needle"}`)}},
		{content: "found it"},
	}}
	ft := newFakeTools("fs.search")
	ft.results["fs.search"] = domain.Ok("3 hits", map[string]any{"hits": []string{"alpha", "beta"}})
	sink := &memSink{}

	rep := newRunner(t, be, ft, sink).Run(context.Background(), Brief{Task: "find the needle", Deliverable: "the path"}, nil)

	tr := sink.last()
	for _, want := range []string{rep.ID, "find the needle", "the path", "fs__search", "alpha", "## Report", "found it"} {
		if !strings.Contains(tr, want) {
			t.Errorf("transcript missing %q\n---\n%s", want, tr)
		}
	}
}

// A sub-agent must be isolated on the wire: its own session id, the subagent
// profile, and no leakage of the caller's session.
func TestRun_SendsSubagentProfileAndItsOwnSession(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{{content: "done"}}}
	rep := newRunner(t, be, newFakeTools("fs.read"), nil).Run(context.Background(), Brief{Task: "t"}, nil)

	reqs := be.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("requests = %d, want 1", len(reqs))
	}
	if reqs[0].Profile != backend.ProfileSubagent {
		t.Errorf("profile = %q, want %q", reqs[0].Profile, backend.ProfileSubagent)
	}
	if reqs[0].Session.ID != rep.ID {
		t.Errorf("session id = %q, want the run id %q", reqs[0].Session.ID, rep.ID)
	}
	if reqs[0].State != nil {
		t.Error("round 0 must send no state token — a fresh sub-agent has no prior state")
	}
	if len(reqs[0].Input.Tools) == 0 {
		t.Error("no tool inventory sent")
	}
}

// The state token from round N must be replayed on round N+1.
func TestRun_ReplaysTheBackendStateToken(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{calls: []backend.ToolCall{toolCall("c1", "fs.read", `{}`)}, state: "tok-A"},
		{content: "done", state: "tok-B"},
	}}
	newRunner(t, be, newFakeTools("fs.read"), nil).Run(context.Background(), Brief{Task: "t"}, nil)

	reqs := be.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if reqs[1].State == nil || *reqs[1].State != "tok-A" {
		t.Errorf("round 1 state = %v, want tok-A", reqs[1].State)
	}
}

// Hitting the round budget must still produce a report — via a final round with
// tools withheld — and must mark it partial.
func TestRun_RoundBudgetForcesAPartialReport(t *testing.T) {
	call := []backend.ToolCall{toolCall("c1", "fs.search", `{}`)}
	be := &fakeBackend{rounds: []roundScript{
		{calls: call}, {calls: call},
		{content: "Partial: I checked internal/ but not cmd/."},
	}}
	ft := newFakeTools("fs.search")

	rep := newRunner(t, be, ft, &memSink{}).Run(context.Background(), Brief{Task: "t", MaxRounds: 2}, nil)

	if rep.Status != StatusExhausted {
		t.Fatalf("status = %q, want exhausted", rep.Status)
	}
	if !rep.Partial {
		t.Error("an exhausted run must be marked partial")
	}
	if !strings.Contains(rep.Text, "Partial:") {
		t.Errorf("report = %q, want the wrap-up round's text", rep.Text)
	}
	if !strings.Contains(rep.Note, "2-round budget") {
		t.Errorf("note = %q, want it to name the budget", rep.Note)
	}
	reqs := be.snapshot()
	if len(reqs) != 3 {
		t.Fatalf("requests = %d, want 3 (2 search rounds + the wrap-up)", len(reqs))
	}
	// The wrap-up round must withhold tools, or the model just searches again.
	if reqs[2].Input.ToolChoice != "none" {
		t.Errorf("wrap-up tool_choice = %v, want \"none\"", reqs[2].Input.ToolChoice)
	}
	if len(reqs[2].Input.Tools) != 0 {
		t.Errorf("wrap-up sent %d tools, want none", len(reqs[2].Input.Tools))
	}
}

// A tool the sub-agent was never offered must fail closed, and the run must carry
// on rather than abort — the model can recover from a refusal.
func TestRun_RefusesAToolOutsideItsInventory(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{calls: []backend.ToolCall{toolCall("c1", "agentTask.spawnForEdits", `{"goal":"fix it"}`)}},
		{content: "That needs an edit, which I cannot do."},
	}}
	ft := newFakeTools("fs.read")

	rep := newRunner(t, be, ft, &memSink{}).Run(context.Background(), Brief{Task: "t"}, nil)

	if rep.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", rep.Status)
	}
	if len(ft.dispatched()) != 0 {
		t.Errorf("dispatched %v — an out-of-inventory tool must never reach the registry", ft.dispatched())
	}
	if rep.FailedCalls != 1 {
		t.Errorf("failedCalls = %d, want 1", rep.FailedCalls)
	}
	// The refusal has to reach the MODEL too, or it will just try again.
	reqs := be.snapshot()
	last := reqs[len(reqs)-1].Input.Messages
	var toolMsg string
	for _, m := range last {
		if m.Role == "tool" {
			toolMsg = string(m.Content)
		}
	}
	if !strings.Contains(toolMsg, "TOOL_NOT_OFFERED") {
		t.Errorf("tool message = %q, want a TOOL_NOT_OFFERED refusal", toolMsg)
	}
}

// A backend failure on the very first round has nothing to salvage.
func TestRun_BackendFailureOnFirstRoundFails(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{{err: errors.New("upstream exploded")}}}

	rep := newRunner(t, be, newFakeTools("fs.read"), &memSink{}).Run(context.Background(), Brief{Task: "t"}, nil)

	if rep.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", rep.Status)
	}
	if !strings.Contains(rep.Note, "upstream exploded") {
		t.Errorf("note = %q, want the underlying error", rep.Note)
	}
}

// A backend failure AFTER some reading has happened is salvaged: one wrap-up round
// on the history it already has beats returning nothing for the money spent.
func TestRun_BackendFailureMidRunSalvagesAReport(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{calls: []backend.ToolCall{toolCall("c1", "fs.read", `{}`)}},
		{err: errors.New("upstream exploded")},
		{content: "I only got as far as reading a.go."},
	}}

	rep := newRunner(t, be, newFakeTools("fs.read"), &memSink{}).Run(context.Background(), Brief{Task: "t"}, nil)

	if rep.Status != StatusExhausted {
		t.Fatalf("status = %q, want exhausted (salvaged)", rep.Status)
	}
	if !strings.Contains(rep.Text, "a.go") {
		t.Errorf("report = %q, want the salvaged wrap-up", rep.Text)
	}
}

// If even the wrap-up round fails, the run falls back to the last prose the
// sub-agent produced rather than returning an empty report.
func TestRun_SalvageFallsBackToLastProseWhenWrapUpAlsoFails(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{content: "Looking at internal/terrain now.", calls: []backend.ToolCall{toolCall("c1", "fs.read", `{}`)}},
		{err: errors.New("boom")},
		{err: errors.New("boom again")},
	}}

	rep := newRunner(t, be, newFakeTools("fs.read"), &memSink{}).Run(context.Background(), Brief{Task: "t"}, nil)

	if rep.Status != StatusExhausted {
		t.Fatalf("status = %q, want exhausted", rep.Status)
	}
	if !strings.Contains(rep.Text, "internal/terrain") {
		t.Errorf("report = %q, want the last prose", rep.Text)
	}
	if !strings.Contains(rep.Note, "last prose") {
		t.Errorf("note = %q must say the text is not a real report", rep.Note)
	}
}

func TestRun_CancelStopsTheRun(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{{content: "never"}}, block: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rep := newRunner(t, be, newFakeTools("fs.read"), &memSink{}).Run(ctx, Brief{Task: "t"}, nil)

	if rep.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled", rep.Status)
	}
}

// The wall-clock deadline is independent of the round budget: one slow round must
// still end in a partial report, not a hang.
func TestRun_DeadlineProducesAPartialReport(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{content: "never"}, // round 0 blocks until the deadline fires
		{content: "Out of time; I found nothing conclusive."},
	}, block: make(chan struct{}), blockRounds: map[int]bool{0: true}}
	r := New(Deps{Backend: be, Tools: newFakeTools("fs.read"), Transcript: &memSink{}, Timeout: 40 * time.Millisecond})

	rep := r.Run(context.Background(), Brief{Task: "t"}, nil)

	if rep.Status != StatusExhausted {
		t.Fatalf("status = %q, want exhausted (note %q)", rep.Status, rep.Note)
	}
	if !strings.Contains(rep.Note, "time budget") {
		t.Errorf("note = %q, want it to name the time budget", rep.Note)
	}
}

// The clamp is the load-bearing context protection: a huge tool result must be
// truncated in the sub-agent's LIVE history while the transcript keeps all of it.
func TestRun_ClampsHugeToolResultsInHistoryButNotInTheTranscript(t *testing.T) {
	huge := strings.Repeat("x", MaxToolResultChars+5_000)
	be := &fakeBackend{rounds: []roundScript{
		{calls: []backend.ToolCall{toolCall("c1", "fs.read", `{}`)}},
		{content: "done"},
	}}
	ft := newFakeTools("fs.read")
	ft.results["fs.read"] = domain.Ok("big", map[string]any{"body": huge})
	sink := &memSink{}

	newRunner(t, be, ft, sink).Run(context.Background(), Brief{Task: "t"}, nil)

	reqs := be.snapshot()
	var toolMsg string
	for _, m := range reqs[len(reqs)-1].Input.Messages {
		if m.Role == "tool" {
			toolMsg = string(m.Content)
		}
	}
	if n := len([]rune(toolMsg)); n > MaxToolResultChars+200 {
		t.Errorf("tool message is %d runes — the clamp did not apply", n)
	}
	if !strings.Contains(toolMsg, "result truncated") {
		t.Error("the clamped result must SAY it was truncated, or the model reads a partial file as complete")
	}
	if n := len(sink.last()); n < MaxToolResultChars {
		t.Errorf("transcript is %d chars — it must keep the full result", n)
	}
}

func TestRun_ClampsTheReportItself(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{{content: strings.Repeat("y", MaxReportChars+2_000)}}}

	rep := newRunner(t, be, newFakeTools("fs.read"), nil).Run(context.Background(), Brief{Task: "t"}, nil)

	if n := len([]rune(rep.Text)); n > MaxReportChars {
		t.Errorf("report is %d runes, over the %d cap", n, MaxReportChars)
	}
}

func TestRun_EmptyTaskFailsWithoutCallingTheBackend(t *testing.T) {
	be := &fakeBackend{}
	rep := newRunner(t, be, newFakeTools("fs.read"), nil).Run(context.Background(), Brief{Task: "   "}, nil)

	if rep.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", rep.Status)
	}
	if len(be.snapshot()) != 0 {
		t.Error("an empty brief must not reach the backend")
	}
}

func TestRun_ProgressIsReportedPerRound(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{calls: []backend.ToolCall{toolCall("c1", "fs.search", `{}`)}},
		{content: "done"},
	}}
	var mu sync.Mutex
	var msgs []string

	newRunner(t, be, newFakeTools("fs.search"), nil).Run(context.Background(), Brief{Task: "t", MaxRounds: 4},
		func(m string) { mu.Lock(); msgs = append(msgs, m); mu.Unlock() })

	joined := strings.Join(msgs, " | ")
	if !strings.Contains(joined, "round 1/4") {
		t.Errorf("progress = %q, want a round counter", joined)
	}
	if !strings.Contains(joined, "fs.search") {
		t.Errorf("progress = %q, want the tool being run", joined)
	}
}

func TestRun_AccumulatesUsageAndCost(t *testing.T) {
	c1, c2 := 0.001, 0.002
	be := &fakeBackend{rounds: []roundScript{
		{calls: []backend.ToolCall{toolCall("c1", "fs.read", `{}`)},
			usage: backend.Usage{PromptTokens: 100, CompletionTokens: 10}, cost: &c1},
		{content: "done", usage: backend.Usage{PromptTokens: 200, CompletionTokens: 20}, cost: &c2},
	}}

	rep := newRunner(t, be, newFakeTools("fs.read"), nil).Run(context.Background(), Brief{Task: "t"}, nil)

	if rep.PromptTokens != 300 || rep.CompletionTokens != 30 {
		t.Errorf("tokens = %d/%d, want 300/30", rep.PromptTokens, rep.CompletionTokens)
	}
	if rep.CostUSD == nil || *rep.CostUSD < 0.0029 || *rep.CostUSD > 0.0031 {
		t.Errorf("cost = %v, want ~0.003", rep.CostUSD)
	}
}

// An unreported cost must stay nil rather than decode as free.
func TestRun_UnreportedCostStaysNil(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{{content: "done"}}}
	rep := newRunner(t, be, newFakeTools("fs.read"), nil).Run(context.Background(), Brief{Task: "t"}, nil)
	if rep.CostUSD != nil {
		t.Errorf("cost = %v, want nil (unknown is not zero)", rep.CostUSD)
	}
}

func TestRun_NoBackendFailsCleanly(t *testing.T) {
	rep := New(Deps{Tools: newFakeTools("fs.read")}).Run(context.Background(), Brief{Task: "t"}, nil)
	if rep.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", rep.Status)
	}
}

func TestRun_NoDispatcherFailsCleanly(t *testing.T) {
	rep := New(Deps{Backend: &fakeBackend{}}).Run(context.Background(), Brief{Task: "t"}, nil)
	if rep.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", rep.Status)
	}
}

// A round that produced neither prose nor a tool call is nudged once rather than
// silently ending the run with an empty report.
func TestRun_EmptyRoundIsNudgedNotAccepted(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{content: ""},
		{content: "here is the answer"},
	}}
	rep := newRunner(t, be, newFakeTools("fs.read"), nil).Run(context.Background(), Brief{Task: "t"}, nil)

	if rep.Status != StatusCompleted || rep.Text != "here is the answer" {
		t.Fatalf("status=%q text=%q, want a completed run with the second round's text", rep.Status, rep.Text)
	}
}

func TestClampRounds(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, DefaultMaxRounds}, {-3, DefaultMaxRounds}, {1, 1}, {7, 7},
		{MaxRoundsCeiling, MaxRoundsCeiling}, {MaxRoundsCeiling + 50, MaxRoundsCeiling},
	} {
		if got := clampRounds(tc.in); got != tc.want {
			t.Errorf("clampRounds(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestStatusPartial(t *testing.T) {
	if StatusCompleted.Partial() {
		t.Error("completed must not be partial")
	}
	for _, s := range []Status{StatusExhausted, StatusFailed, StatusCancelled} {
		if !s.Partial() {
			t.Errorf("%s must be partial", s)
		}
	}
}

// The opening message must carry every part of the brief the sub-agent has no
// other way to learn — it sees nothing else.
func TestBuildOpeningMessage(t *testing.T) {
	got := buildOpeningMessage(Brief{
		Task:        "find the issue",
		Context:     "the user already tried #42",
		Deliverable: "number and URL",
	}, 6)
	for _, want := range []string{"find the issue", "the user already tried #42", "number and URL", "6 rounds"} {
		if !strings.Contains(got, want) {
			t.Errorf("opening message missing %q:\n%s", want, got)
		}
	}
}

func TestCallSummary(t *testing.T) {
	calls := []models.ToolCallRequest{
		{Function: models.ToolCallFunction{Name: "fs__read"}},
		{Function: models.ToolCallFunction{Name: "fs__read"}},
		{Function: models.ToolCallFunction{Name: "fs__search"}},
	}
	got := callSummary(calls)
	if !strings.Contains(got, "fs.read") || !strings.Contains(got, "fs.search") {
		t.Errorf("callSummary = %q, want both dotted names", got)
	}
	if strings.Count(got, "fs.read") != 1 {
		t.Errorf("callSummary = %q, want the duplicate deduped", got)
	}
}

func TestClampRunes_DoesNotSplitMultibyte(t *testing.T) {
	got := clampRunes("héllo wörld", 4)
	if got != "héll" {
		t.Errorf("clampRunes = %q, want %q", got, "héll")
	}
}

// stripReportPreamble exists because of what the FIRST live run produced: a
// report beginning "I have everything needed.\n\n## Report\n\n**Dispatch is
// defined at …**". Both wasted lines land in the caller's context, which is the
// one thing this feature protects.
func TestStripReportPreamble(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"heading only", "## Report\n\nThe answer is X.", "The answer is X."},
		// Deliberately NOT stripped: structurally indistinguishable from a real
		// answer followed by a section heading, and deleting an answer is far worse
		// than leaving a stray line. The prompt is what prevents this shape.
		{"preamble then heading is left alone",
			"I have everything needed.\n\n## Report\n\nThe answer is X.",
			"I have everything needed.\n\n## Report\n\nThe answer is X."},
		{"h3 heading", "### Report\n\nThe answer is X.", "The answer is X."},
		{"bold heading", "**Report**\n\nThe answer is X.", "The answer is X."},
		{"labelled", "Report:\n\nThe answer is X.", "The answer is X."},
		// A report that already leads with the answer must survive untouched —
		// this is the case that must never regress into eating real content.
		{"already clean", "internal/tools/dispatch.go:41 defines Dispatch.", "internal/tools/dispatch.go:41 defines Dispatch."},
		{"answer then a body heading", "The answer is X.\n\n## Report\n\nmore detail",
			"The answer is X.\n\n## Report\n\nmore detail"},
		// A long first line is the answer, never throat-clearing.
		{"long first line then heading",
			strings.Repeat("a", 90) + "\n\n## Report\n\nbody",
			strings.Repeat("a", 90) + "\n\n## Report\n\nbody"},
		{"unrelated heading is kept", "## Findings\n\nThe answer is X.", "## Findings\n\nThe answer is X."},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripReportPreamble(tc.in); got != tc.want {
				t.Errorf("stripReportPreamble(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRun_ReportPreambleIsStrippedEndToEnd(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{content: "## Report\n\ninternal/tools/dispatch.go:41"},
	}}
	rep := newRunner(t, be, newFakeTools("fs.read"), nil).Run(context.Background(), Brief{Task: "t"}, nil)

	if rep.Text != "internal/tools/dispatch.go:41" {
		t.Errorf("report = %q, want the bare answer", rep.Text)
	}
}

// The transcript recorded the final message twice — once as the last round's
// prose, once under "## Report" — which is what the first live run's artifact
// showed. A round with no tool calls must contribute only its header.
func TestRun_TranscriptDoesNotRepeatTheFinalMessage(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{calls: []backend.ToolCall{toolCall("c1", "fs.read", `{}`)}},
		{content: "the one and only answer"},
	}}
	sink := &memSink{}
	newRunner(t, be, newFakeTools("fs.read"), sink).Run(context.Background(), Brief{Task: "t"}, nil)

	if n := strings.Count(sink.last(), "the one and only answer"); n != 1 {
		t.Errorf("the final message appears %d times in the transcript, want 1:\n%s", n, sink.last())
	}
}

// Prose from a round that DID call tools is still recorded — it is the running
// narrative a reader needs to follow the search.
func TestRun_TranscriptKeepsProseFromToolRounds(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{content: "Checking internal/ first.", calls: []backend.ToolCall{toolCall("c1", "fs.read", `{}`)}},
		{content: "done"},
	}}
	sink := &memSink{}
	newRunner(t, be, newFakeTools("fs.read"), sink).Run(context.Background(), Brief{Task: "t"}, nil)

	if !strings.Contains(sink.last(), "Checking internal/ first.") {
		t.Errorf("transcript dropped a tool round's prose:\n%s", sink.last())
	}
}

// The nudge is ONE-SHOT. A model that has gone silent will usually stay silent,
// and re-asking it for the remaining rounds buys nothing — the wrap-up round is a
// better use of the last one. Without the latch this loop merely terminated at the
// round budget, having spent every round on the same question.
func TestRun_SilentRoundsAreNudgedOnceThenWrappedUp(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{content: ""}, // dead round 1 → nudge
		{content: ""}, // dead round 2 → straight to the wrap-up
		{content: "here is what I had"},
	}}
	rep := newRunner(t, be, newFakeTools("fs.read"), &memSink{}).
		Run(context.Background(), Brief{Task: "t", MaxRounds: 10}, nil)

	if rep.Status != StatusExhausted {
		t.Fatalf("status = %q, want exhausted", rep.Status)
	}
	if rep.Text != "here is what I had" {
		t.Errorf("report = %q, want the wrap-up round's text", rep.Text)
	}
	if rep.Rounds != 3 {
		t.Errorf("rounds = %d, want 3 — it must not keep re-asking a silent model", rep.Rounds)
	}
	if !strings.Contains(rep.Note, "stopped producing output") {
		t.Errorf("note = %q, want it to name the cause", rep.Note)
	}
}

// The context bound must see tool-call ARGUMENTS. An assistant turn that only
// calls tools has null content, so sizing by content alone scored it at zero —
// and a search-heavy run is mostly those turns, which is precisely the run
// MaxTranscriptChars exists to bound.
func TestMessageChars_CountsToolCallArguments(t *testing.T) {
	args := `{"query":"` + strings.Repeat("x", 500) + `"}`
	m := models.ChatMessage{
		Role:        "assistant",
		ContentNull: true,
		ToolCalls: []models.ToolCallRequest{
			{Function: models.ToolCallFunction{Name: "fs__search", Arguments: args}},
		},
	}
	if got := messageChars(m); got < 500 {
		t.Errorf("messageChars = %d for a %d-char tool call — arguments are not counted", got, len(args))
	}
}

func TestRun_HistoryBudgetAccountsForToolCallTurns(t *testing.T) {
	bigArgs := `{"query":"` + strings.Repeat("y", 400) + `"}`
	be := &fakeBackend{rounds: []roundScript{
		{calls: []backend.ToolCall{toolCall("c1", "fs.search", bigArgs)}},
		{content: "done"},
	}}
	st := &run{}
	before := st.historyChars
	st.push(agent.BackendAssistantMessage(backend.RespondMessage{
		Role: "assistant", ToolCalls: []backend.ToolCall{toolCall("c1", "fs.search", bigArgs)},
	}))
	if st.historyChars-before < 400 {
		t.Errorf("history grew by %d for a 400-char tool call turn", st.historyChars-before)
	}
	// And the run still completes normally with the new accounting.
	rep := newRunner(t, be, newFakeTools("fs.search"), nil).Run(context.Background(), Brief{Task: "t"}, nil)
	if rep.Status != StatusCompleted {
		t.Fatalf("status = %q, want completed", rep.Status)
	}
}

// --- regressions found in review -------------------------------------------

// The salvage predicate must be "did any round SUCCEED", not "how many attempts
// were made". round() increments st.rounds before returning an error, so the old
// `st.rounds == 0` check was dead code and a first-round failure took the salvage
// path with no research behind it. The old test passed only because the fake ran
// out of script inside that path.
func TestRun_FirstRoundFailureFailsWithoutAttemptingSalvage(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{err: errors.New("upstream exploded")},
		// A wrap-up round IS scripted. If the salvage path were still reachable it
		// would consume this and report "completed"/"exhausted" off no research.
		{content: "I would be a fabricated report"},
	}}

	rep := newRunner(t, be, newFakeTools("fs.read"), &memSink{}).Run(context.Background(), Brief{Task: "t"}, nil)

	if rep.Status != StatusFailed {
		t.Fatalf("status = %q, want failed", rep.Status)
	}
	if strings.Contains(rep.Text, "fabricated") {
		t.Error("it salvaged a report despite never completing a round")
	}
	if n := len(be.snapshot()); n != 1 {
		t.Errorf("made %d backend calls, want 1 — no salvage round should be attempted", n)
	}
}

// A sum missing a contribution is a FLOOR. The codebase rule forbids rendering a
// floor as a total, so an unreported round must make the whole figure unknown —
// in either arrival order.
func TestRun_PartialCostIsNotPublishedAsATotal(t *testing.T) {
	c := 0.01
	for _, tc := range []struct {
		name   string
		rounds []roundScript
	}{
		{"cost then nil", []roundScript{
			{calls: []backend.ToolCall{toolCall("c1", "fs.read", `{}`)}, cost: &c},
			{content: "done"},
		}},
		{"nil then cost", []roundScript{
			{calls: []backend.ToolCall{toolCall("c1", "fs.read", `{}`)}},
			{content: "done", cost: &c},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			be := &fakeBackend{rounds: tc.rounds}
			rep := newRunner(t, be, newFakeTools("fs.read"), nil).Run(context.Background(), Brief{Task: "t"}, nil)
			if rep.CostUSD != nil {
				t.Errorf("cost = %v, want nil — a partial sum must not present as a total", *rep.CostUSD)
			}
		})
	}
}

// The control: when EVERY round reports, the total is published.
func TestRun_CompleteCostIsPublished(t *testing.T) {
	c1, c2 := 0.001, 0.002
	be := &fakeBackend{rounds: []roundScript{
		{calls: []backend.ToolCall{toolCall("c1", "fs.read", `{}`)}, cost: &c1},
		{content: "done", cost: &c2},
	}}
	rep := newRunner(t, be, newFakeTools("fs.read"), nil).Run(context.Background(), Brief{Task: "t"}, nil)
	if rep.CostUSD == nil || *rep.CostUSD < 0.0029 || *rep.CostUSD > 0.0031 {
		t.Fatalf("cost = %v, want ~0.003", rep.CostUSD)
	}
}

// A caller that gives up must stop the run — including during the wrap-up round,
// which deliberately sheds the RUN deadline. That shed used to drop the caller's
// cancel with it, so Escape did nothing for up to 90 seconds in exactly the case
// where the user is most likely to press it.
func TestRun_CallerCancelDuringWrapUpStopsTheRun(t *testing.T) {
	be := &fakeBackend{
		rounds: []roundScript{{content: "never"}, {content: "never either"}},
		block:  make(chan struct{}),
		// Round 0 blocks so the RUN deadline fires; round 1 (the wrap-up) blocks so
		// the cancel below has a live call to interrupt.
		blockRounds: map[int]bool{0: true, 1: true},
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := New(Deps{Backend: be, Tools: newFakeTools("fs.read"), Transcript: &memSink{},
		Timeout: 40 * time.Millisecond})

	done := make(chan Report, 1)
	go func() { done <- r.Run(ctx, Brief{Task: "t"}, nil) }()

	// Let the run deadline fire and the wrap-up round begin, then give up.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case rep := <-done:
		if rep.Status == StatusCompleted {
			t.Fatalf("status = %q, want a stopped run", rep.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the run ignored the caller's cancel during the wrap-up round")
	}
}

// A caller whose OWN deadline expires is not the same as our time budget running
// out: the work is no longer wanted, so it must not continue under a shed deadline.
func TestRun_CallerDeadlineIsCancelledNotExhausted(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{{content: "never"}}, block: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	// The RUN's own budget is generous, so the only expiry is the caller's.
	r := New(Deps{Backend: be, Tools: newFakeTools("fs.read"), Transcript: &memSink{}, Timeout: time.Minute})

	rep := r.Run(ctx, Brief{Task: "t"}, nil)

	if rep.Status != StatusCancelled {
		t.Fatalf("status = %q, want cancelled — the caller gave up", rep.Status)
	}
}

// tool_choice "none" is a request, not a guarantee. A provider that returns tool
// calls anyway must not leave them unmatched in the terminal history.
func TestRun_WrapUpRoundDropsToolCallsItWasToldNotToMake(t *testing.T) {
	be := &fakeBackend{rounds: []roundScript{
		{calls: []backend.ToolCall{toolCall("c1", "fs.read", `{}`)}},
		{content: "here is the partial answer",
			calls: []backend.ToolCall{toolCall("c9", "fs.read", `{}`)}},
	}}
	sink := &memSink{}
	rep := newRunner(t, be, newFakeTools("fs.read"), sink).
		Run(context.Background(), Brief{Task: "t", MaxRounds: 1}, nil)

	if rep.Text != "here is the partial answer" {
		t.Errorf("report = %q", rep.Text)
	}
	if !strings.Contains(sink.last(), "despite tool_choice=none") {
		t.Error("the transcript must record that the provider ignored tool_choice")
	}
}

// MaxTranscriptChars must be a BOUND, not a tripwire noticed one batch too late:
// a single round can request many tools, and eight near-cap results used to be
// appended in full before anything checked.
func TestRun_OneBatchCannotBlowThroughTheTranscriptBudget(t *testing.T) {
	huge := strings.Repeat("z", MaxToolResultChars*2)
	calls := make([]backend.ToolCall, 0, 12)
	for i := 0; i < 12; i++ {
		calls = append(calls, toolCall("c"+string(rune('a'+i)), "fs.read", `{}`))
	}
	be := &fakeBackend{rounds: []roundScript{{calls: calls}, {content: "done"}}}
	ft := newFakeTools("fs.read")
	ft.results["fs.read"] = domain.Ok("big", map[string]any{"body": huge})

	newRunner(t, be, ft, &memSink{}).Run(context.Background(), Brief{Task: "t"}, nil)

	reqs := be.snapshot()
	last := reqs[len(reqs)-1]
	total := 0
	toolMsgs := 0
	for _, m := range last.Input.Messages {
		total += len([]rune(string(m.Content)))
		if m.Role == "tool" {
			toolMsgs++
		}
	}
	// Every call still gets a result — the budget shrinks them, never drops them.
	if toolMsgs != len(calls) {
		t.Errorf("%d tool results for %d calls — a dropped result leaves an unmatched call", toolMsgs, len(calls))
	}
	// And the history stayed within a sane multiple of the budget rather than the
	// ~2x overshoot one unbounded batch used to produce.
	if total > MaxTranscriptChars+MaxToolResultChars {
		t.Errorf("history reached %d runes, over the %d budget by more than one result", total, MaxTranscriptChars)
	}
}
