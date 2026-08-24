package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// endlessRouter never stops calling tools. It is the shape the convergence guard
// exists for: a model that keeps producing prose plus another SUCCESSFUL batch every
// round, so no failure breaker can ever fire and the loop has nothing else to stop it.
// `novel` decides whether each round asks for something new (a different path) or
// re-issues the same call, which is the difference between the budget and the stall
// signal. It has no queued script and no terminal answer at all: if the loop does not
// converge on its own, the test hangs — which is exactly the defect, so a hang here is
// a legitimate (if blunt) failure signal.
type endlessRouter struct {
	novel  bool
	rounds int
	// content is the prose each round streams; "" models a round that emits a tool
	// batch and nothing else, which is what makes a closing round come back silent.
	content string
	// onRound, when set, runs before each round's result is returned — the seam for
	// driving a mid-turn injection.
	onRound func(round int)
}

func (r *endlessRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	i := r.rounds
	r.rounds++
	if r.onRound != nil {
		r.onRound(i)
	}
	path := `{"path":"same"}`
	if r.novel {
		path = `{"path":"f` + itoa(i) + `"}`
	}
	return models.ChatResult{
		Content:   r.content,
		ToolCalls: []models.ToolCallRequest{toolCall("c"+itoa(i), "fs__read", path)},
	}, nil
}

func (r *endlessRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "summary"}, nil
}
func (r *endlessRouter) ModelFor(domain.ModelTier) string { return "m" }

// closingRouter behaves like endlessRouter until the round that arrives with
// tool_choice "none", where it returns the report the guard asked for. It proves the
// closing round is a real model round whose answer reaches the user — not a canned
// string the engine substitutes.
type closingRouter struct {
	endlessRouter
	report string
}

// The turn loop talks to the backend, not the router, so tool_choice is observed
// through the recording backend below; this router keys off the same signal by
// spotting the closing brief the guard pushes into history.
func (r *closingRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	for _, m := range opts.Messages {
		if strings.Contains(m.StringContent, "No further tools will run this turn") {
			r.rounds++
			return models.ChatResult{Content: r.report}, nil
		}
	}
	return r.endlessRouter.Stream(ctx, tier, opts, onToken)
}

// changingTools answers every call differently — the shape of a legitimate poll of a
// mutable read (watcher.list, agentTask.status), where identical arguments genuinely
// return new data each time.
type changingTools struct{ calls int }

func (t *changingTools) OpenAITools(filter []string) ([]models.ChatTool, error) { return nil, nil }
func (t *changingTools) ResolveWireName(w string) string {
	return strings.ReplaceAll(w, "__", ".")
}
func (t *changingTools) Dispatch(ctx context.Context, name, args string, turn TurnContext) domain.ToolResult {
	t.calls++
	return domain.Ok("poll "+itoa(t.calls), map[string]any{"seq": t.calls})
}

// toolChoices returns the wire tool_choice of every recorded round, in order.
func toolChoices(be *recordingBackend) []string {
	reqs := be.requests()
	out := make([]string, 0, len(reqs))
	for _, req := range reqs {
		s, _ := req.Input.ToolChoice.(string)
		out = append(out, s)
	}
	return out
}

// assertClosedOnBudget checks the wire shape every budget close must have: the turn ran
// exactly one round past the budget, and only that last round forbade tools.
func assertClosedOnBudget(t *testing.T, be *recordingBackend, rounds int) {
	t.Helper()
	if rounds != domain.TurnRoundBudget+1 {
		t.Fatalf("turn ran %d rounds, want %d (the budget plus its closing round)", rounds, domain.TurnRoundBudget+1)
	}
	choices := toolChoices(be)
	for i, c := range choices[:len(choices)-1] {
		if c != "auto" {
			t.Fatalf("round %d sent tool_choice %q, want auto", i, c)
		}
	}
	if last := choices[len(choices)-1]; last != "none" {
		t.Fatalf("closing round sent tool_choice %q, want none", last)
	}
}

// The reported defect: an open-ended prompt where the model keeps making NEW,
// successful tool calls forever. Nothing fails, so neither circuit breaker fires; the
// model never returns a tool-call-free round, so the loop's only exit is never taken.
// The round budget must close the turn.
func TestConvergence_UnboundedNovelRoundsHitTheBudget(t *testing.T) {
	r := &endlessRouter{novel: true, content: "This is a big, complex job and I will do whatever it takes."}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	sink := &captureSink{}
	deps.Events = sink
	s := NewSession(deps)

	reply, err := s.Send(context.Background(), "do a huge round of performance work", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertClosedOnBudget(t, be, r.rounds)
	// The user gets a stated outcome, not silence.
	if reply == "" || sink.endContent != reply {
		t.Fatalf("reply = %q / AssistantEnd = %q; the turn must seal with visible content", reply, sink.endContent)
	}
	// The human is told the turn was closed rather than answered.
	if len(sink.warnings) == 0 {
		t.Fatal("closing a turn without an answer must surface a warning")
	}
}

// A model that re-issues the SAME call and gets the SAME answer learns nothing and must
// be caught long before the round budget — the cheap signal the failure breakers cannot
// see, because every one of these calls succeeds.
func TestConvergence_RepeatedSuccessfulCallsStallEarly(t *testing.T) {
	r := &endlessRouter{novel: false, content: "Still a big, complex job."}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if r.rounds >= domain.TurnRoundBudget {
		t.Fatalf("turn ran %d rounds; a turn repeating one call must stall out well before the budget", r.rounds)
	}
	if choices := toolChoices(be); choices[len(choices)-1] != "none" {
		t.Fatalf("closing round sent tool_choice %q, want none", choices[len(choices)-1])
	}
	// The model was nudged before it was closed: it gets a chance to correct itself.
	nudged := false
	for _, m := range s.Messages() {
		if m.Role == "user" && strings.Contains(m.StringContent, "told you nothing new") {
			nudged = true
		}
	}
	if !nudged {
		t.Fatal("a stalling turn must be nudged before it is closed")
	}
}

// The same call repeated with a CHANGING result is a legitimate poll of a mutable read,
// not repetition. It must survive the stall signal — the round budget, not the stall
// counter, is what eventually bounds it.
func TestConvergence_PollingAMutableReadIsProgress(t *testing.T) {
	r := &endlessRouter{novel: false, content: "watching"} // identical call every round
	deps, be := recordingDeps(r, &changingTools{})
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "watch the agents", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	assertClosedOnBudget(t, be, r.rounds)
}

// The closing round is a real model round: whatever it produces is what the user sees.
// This is the difference between "the engine gave up" and "the assistant reported".
func TestConvergence_ClosingRoundAnswerReachesTheUser(t *testing.T) {
	const report = "I spawned three Claude terminals on perf worktrees; watchers are armed."
	r := &closingRouter{endlessRouter: endlessRouter{content: "working on it"}, report: report}
	s := NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != report {
		t.Fatalf("reply = %q, want the closing round's own report", reply)
	}
	if IsWakeFailureReply(reply) {
		t.Fatal("a turn that closed WITH a report is an answer, not a non-result")
	}
}

// A closing round that produces nothing at all still has to say something, and that
// something is a non-result a wake reactor must not mistake for a real summary.
func TestConvergence_SilentClosingRoundFallsBackToANonResult(t *testing.T) {
	r := &endlessRouter{novel: false}
	s := NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !isStalledReply(reply) {
		t.Fatalf("reply = %q, want the deterministic stalled fallback", reply)
	}
	if !IsWakeFailureReply(reply) {
		t.Fatal("a turn that closed with no report at all is a non-result")
	}
}

// The stalled-turn sentinel is matched WHOLE. A genuine convergence report that happens
// to open with the same words is an ANSWER, and classifying it as a wake failure would
// retry and requeue work the assistant actually did.
func TestIsStalledReply_RejectsALookalikeAnswer(t *testing.T) {
	if real := stalledReply(33); !isStalledReply(real) || !IsWakeFailureReply(real) {
		t.Fatalf("the engine's own fallback must classify as a non-result: %q", real)
	}
	lookalike := "Stopped after 12 rounds of benchmarking. Here is what I set running and the plan from here."
	if isStalledReply(lookalike) {
		t.Fatalf("a real report was matched as the sentinel: %q", lookalike)
	}
	if IsWakeFailureReply(lookalike) {
		t.Fatal("a real report that opens like the sentinel must not be a wake failure")
	}
}

// The closing round drops any batch the backend emitted despite tool_choice "none":
// nothing runs, and the transcript must not carry an assistant tool_call with no
// matching tool reply (that shape is unreplayable — DeepSeek 400s on it).
func TestConvergence_ClosingRoundDropsToolCallsAndKeepsTranscriptReplayable(t *testing.T) {
	tools := &fakeTools{result: domain.Ok("ok", nil)}
	r := &endlessRouter{novel: false, content: "still going"}
	s := NewSession(baseDeps(r, tools))

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	pending := 0
	for _, m := range s.Messages() {
		switch {
		case m.Role == "assistant":
			pending += len(m.ToolCalls)
		case m.Role == "tool":
			pending--
		}
	}
	if pending != 0 {
		t.Fatalf("%d assistant tool_call(s) have no matching tool reply", pending)
	}
	// The closing round's batch was dropped, not dispatched: one call per non-closing
	// round is all the runner ever saw.
	if tools.dispatched != r.rounds-1 {
		t.Fatalf("dispatched %d calls over %d rounds; the closing round must dispatch nothing", tools.dispatched, r.rounds)
	}
}

// THE GUARANTEE: nothing a mid-turn injection can do extends a turn past its budget.
// InjectPrompt is reachable from the MCP server, so this is not only about the human at
// the keyboard — a caller injecting before every close would otherwise own the loop
// forever, which is the defect this guard exists to remove.
func TestConvergence_InjectionCannotOutrunTheRoundBudget(t *testing.T) {
	var s *Session
	r := &endlessRouter{novel: true, content: "carrying on"}
	r.onRound = func(int) { s.InjectPrompt("and also do this") }
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	s = NewSession(deps)

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	assertClosedOnBudget(t, be, r.rounds)
}

// A fresh instruction clears the repetition tally, and the cleared tracker has to be
// fully armed again — able to nudge AND to close. A closing latch left standing through
// reset would silently disable the guard for the rest of the turn.
func TestTurnStall_ResetRearmsNudgeAndClose(t *testing.T) {
	st := newTurnStall()
	for i := 0; i < domain.TurnStallAbort+1; i++ {
		st.observe([]string{"same"}) // first is novel, the rest are barren
	}
	if act := st.step(1); act.step != convergenceClose {
		t.Fatalf("step = %v, want close after %d barren rounds", act.step, domain.TurnStallAbort)
	}

	st.reset()
	if act := st.step(2); act.step != convergenceContinue {
		t.Fatalf("step = %v straight after reset, want continue", act.step)
	}
	for i := 0; i < domain.TurnStallWarn+1; i++ {
		st.observe([]string{"other"})
	}
	if act := st.step(3); act.step != convergenceNudge {
		t.Fatalf("step = %v, want nudge — a reset tracker must be able to warn again", act.step)
	}
	for i := 0; i < domain.TurnStallAbort; i++ {
		st.observe([]string{"other"})
	}
	if act := st.step(4); act.step != convergenceClose {
		t.Fatalf("step = %v, want close — a reset tracker must be able to close again", act.step)
	}
}

// The budget half of the tracker is NOT rewound by reset: an injection restarts the
// repetition tally, never the turn's round ceiling.
func TestTurnStall_ResetDoesNotRewindTheBudget(t *testing.T) {
	st := newTurnStall()
	st.reset()
	if act := st.step(domain.TurnRoundBudget + 1); act.step != convergenceClose {
		t.Fatalf("step = %v past the budget after a reset, want close", act.step)
	}
	if !st.budgetClosed {
		t.Fatal("a close at the round ceiling must record itself as a BUDGET close")
	}
}

// A round that produces at least one result the turn has not seen is progress, and
// clears the barren run even if the rest of the batch is repetition.
func TestTurnStall_OneNewResultClearsTheBarrenRun(t *testing.T) {
	st := newTurnStall()
	for i := 0; i < domain.TurnStallAbort+1; i++ {
		st.observe([]string{"a"})
	}
	st.observe([]string{"a", "b"}) // b is new
	if st.barren != 0 {
		t.Fatalf("barren = %d after a novel round, want 0", st.barren)
	}
	if act := st.step(1); act.step != convergenceContinue {
		t.Fatalf("step = %v, want continue — the turn is making progress", act.step)
	}
}

// Signatures are canonical in the arguments and sensitive to the RESULT: the same call
// with its keys reordered is the same call, but the same call answered differently is
// not. Together those are what make the novelty signal honest.
func TestCallSignature_CanonicalArgsButResultSensitive(t *testing.T) {
	a := callSignature("fs.read", `{"path":"x","depth":2}`, "d1")
	b := callSignature("fs.read", `{"depth":2,"path":"x"}`, "d1")
	if a != b {
		t.Fatalf("signatures differ on key order:\n%s\n%s", a, b)
	}
	if c := callSignature("fs.read", `{"path":"x","depth":2}`, "d2"); c == a {
		t.Fatal("a call answered differently must not share a signature with its earlier run")
	}
}

// roundSignatures reads each call's reply back off the transcript, so an identical call
// answered differently in two rounds produces two different signatures.
func TestRoundSignatures_FoldInTheSettledResult(t *testing.T) {
	s := NewSession(baseDeps(&fakeRouter{}, &fakeTools{}))
	calls := []models.ToolCallRequest{toolCall("c1", "fs__read", `{"path":"x"}`)}

	s.pushMessage(models.ChatMessage{Role: "tool", ToolCallID: "c1", Name: "fs.read", StringContent: "first answer"})
	first := s.roundSignatures(calls)
	s.pushMessage(models.ChatMessage{Role: "tool", ToolCallID: "c1", Name: "fs.read", StringContent: "second answer"})
	second := s.roundSignatures(calls)

	if first[0] == second[0] {
		t.Fatal("the same call with a changed result must produce a new signature")
	}
}

// Each escalation is nudge-then-close in its own right. A turn that repeats itself
// early, is nudged for it, then corrects and runs long must still be warned that the
// round budget is approaching — one shared latch swallowed the second warning.
func TestTurnStall_StallAndBudgetWarnIndependently(t *testing.T) {
	st := newTurnStall()
	for i := 0; i < domain.TurnStallWarn+1; i++ {
		st.observe([]string{"same"})
	}
	if act := st.step(1); act.step != convergenceNudge || !strings.Contains(act.instruction, "told you nothing new") {
		t.Fatalf("step = %v / %q, want the repetition nudge", act.step, act.instruction)
	}
	// The model corrects itself and does genuinely new work from here.
	st.observe([]string{"new"})
	if act := st.step(domain.TurnRoundWarn); act.step != convergenceNudge ||
		!strings.Contains(act.instruction, "closed automatically at") {
		t.Fatalf("step = %v / %q, want the budget nudge — the two warnings must latch separately", act.step, act.instruction)
	}
}

// The close latch does not re-fire, and the tools-off brief is what the closing round
// actually carries.
func TestTurnStall_ClosesOnceOnly(t *testing.T) {
	st := newTurnStall()
	closes := 0
	for round := 1; round <= domain.TurnRoundBudget+4; round++ {
		if act := st.step(round); act.step == convergenceClose {
			closes++
			if !strings.Contains(act.instruction, "No further tools will run this turn") {
				t.Fatalf("close instruction = %q, want the tools-off brief", act.instruction)
			}
			if act.warning == "" {
				t.Fatal("a close must carry a human-facing warning")
			}
		}
		st.observe([]string{"f" + itoa(round)}) // always novel: only the budget can fire
	}
	if closes != 1 {
		t.Fatalf("closes = %d, want exactly 1 (the latch must not re-fire)", closes)
	}
}

// A cancelled turn is not a stalled turn: the guard must never convert a user's Escape
// into a convergence report.
func TestConvergence_CancelStillWinsOverTheGuard(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &endlessRouter{novel: false}
	s := NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))
	cancel()

	reply, err := s.Send(ctx, "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != domain.CancelledReply {
		t.Fatalf("reply = %q, want %q", reply, domain.CancelledReply)
	}
}
