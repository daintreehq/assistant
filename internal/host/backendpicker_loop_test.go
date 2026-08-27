package host

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// askingApp is a fake App whose command handler ASKS, the way `/backend` with no
// argument now does: it puts a multiple-choice question to the bridge and blocks on the
// answer before returning a result.
//
// Embeds the interface rather than implementing all of it: anything this path does not
// use is nil and panics loudly if it is ever reached, which is a better failure than a
// silent no-op pretending to be behaviour.
type askingApp struct {
	App
	bridge   *Bridge
	asked    chan struct{}
	answered chan int
}

// Mirrors what the registry says about `/backend`: slow in its bare form only.
//
// Restated rather than imported — internal/commands reaches internal/app, which reaches
// back here, so importing it from a host test is a cycle. The two halves are covered
// separately and both are real: the registry's classification is asserted in
// internal/commands (TestSignInIsSlowAndOrdinaryCommandsAreNot), and what the LOOP does
// with that classification is asserted here.
func (a *askingApp) IsSlowCommand(line string) bool {
	return strings.TrimSpace(line) == "/backend"
}

// The bare picker owns the session for as long as its sheet is open, which is what
// makes the host refuse prompts and other commands meanwhile rather than admitting them
// and letting the reservation refuse them a moment later.
func (a *askingApp) IsExclusiveCommand(line string) bool {
	return strings.TrimSpace(line) == "/backend"
}

func (a *askingApp) RunCommandWithProgress(ctx context.Context, line string, _ func(string)) CommandOutcome {
	if strings.TrimSpace(line) != "/backend" {
		return CommandOutcome{Text: "not the picker"}
	}
	close(a.asked)
	ans, err := a.bridge.AskChoice(ctx, AskChoiceRequest{
		Local:    true,
		Question: "Which backend should answer?",
		Options: []AskChoiceOption{
			{Label: "A", Text: "official"},
			{Label: "B", Text: "local"},
		},
		Default: 1,
	})
	if err != nil {
		return CommandOutcome{Text: "Nothing changed."}
	}
	a.answered <- ans.Index
	return CommandOutcome{Text: "Backend is now " + ans.Text + "."}
}

func (a *askingApp) RunCommand(ctx context.Context, line string) CommandOutcome {
	return a.RunCommandWithProgress(ctx, line, nil)
}

func (a *askingApp) McpStatus() (bool, *int, string) { return false, nil, "" }

// The whole `/backend` path through the REAL host loop: a command that asks, a question
// frame the host can render, an answer delivered as a command on the very loop the
// command is parked on, and a result that reflects the choice.
//
// This is the test that proves the deadlock is actually avoided. Running the command
// inline would park the single-threaded loop on an answer only that loop can deliver,
// so the assertions below would hang rather than fail — which is why the dispatch is
// exercised end to end here rather than only asserted as a flag in the registry.
func TestBackendPickerAsksAndAnswersThroughTheLoop(t *testing.T) {
	out := &lockedBuffer{}
	tr := newTransport(strings.NewReader(""), out, &lockedBuffer{})
	tr.start()

	// `ready` because handleCommand refuses everything before boot finishes — this test
	// is about a booted host's dispatch, not about its startup gate.
	h := &Host{tr: tr, sessionID: "ses_pick", ready: true}
	h.runCtx, h.runCancel = context.WithCancel(context.Background())
	t.Cleanup(h.runCancel)

	h.bridge = NewBridge(BridgeOptions{SessionID: "ses_pick", Post: h.post})
	app := &askingApp{bridge: h.bridge, asked: make(chan struct{}), answered: make(chan int, 1)}
	h.app = app

	// Routed the way a real command is: through handleCommand, which asks the App
	// whether the line is slow. What it asks is the registry in production and the fake
	// above here (internal/commands is an import cycle from a host test); what it DOES
	// with the answer — worker dispatch, a loop that stays live, an answer delivered
	// back to the parked command — is what this test is for, and is real.
	h.handleCommand(HostCommand{Type: CmdCommand, CommandLine: "/backend"})

	select {
	case <-app.asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the picker never ran — /backend was not dispatched to a worker")
	}

	qid := waitForFrame(t, out, "question:requested", "questionId")
	// A LOCAL question belongs to no turn, so the frame must not name one.
	if frame := findFrame(t, out, "question:requested"); frame["turnId"] != nil {
		t.Errorf("a command's question carried turnId %v", frame["turnId"])
	}

	// THE POINT: the loop is still alive while the command is parked on the answer. If
	// it were not, this command would never be serviced and the test would time out.
	h.handleCommand(HostCommand{Type: CmdQuestionAnswer, QuestionID: qid, ChoiceIndex: 0})

	select {
	case idx := <-app.answered:
		if idx != 0 {
			t.Fatalf("the picker was told index %d, want 0", idx)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer never reached the parked command")
	}

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "Backend is now official") {
		if time.Now().After(deadline) {
			t.Fatalf("no command:result reflecting the choice:\n%s", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	// And the sheet was CLOSED on the way. Without this the frame that tells the host to
	// take the sheet down could go missing entirely and this test would still pass off
	// the command result — leaving a real panel with a question on screen that has
	// already been answered and can never be answered again.
	answered := findFrame(t, out, "question:answered")
	if answered["questionId"] != qid {
		t.Errorf("question:answered names %v, not the question that was asked (%s)", answered["questionId"], qid)
	}
	if answered["cancelled"] != false {
		t.Errorf("an answered question was reported cancelled: %v", answered["cancelled"])
	}
	if answered["choiceIndex"] != float64(0) {
		t.Errorf("question:answered carries index %v, not the one that was chosen", answered["choiceIndex"])
	}
	// …and it closed BEFORE the result, or a host that re-enables input on the result
	// would do so while its own sheet was still up.
	if idxOf(t, out, "question:answered") > idxOf(t, out, "command:result") {
		t.Error("the command result was published before the question was closed")
	}
}

// idxOf is the position of the first frame of `kind` in the emitted stream.
func idxOf(t *testing.T, out *lockedBuffer, kind string) int {
	t.Helper()
	for i, line := range strings.Split(out.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["type"] == kind {
			return i
		}
	}
	t.Fatalf("no %s frame:\n%s", kind, out.String())
	return -1
}

// waitForFrame waits for a frame of `kind` and returns one of its string fields.
func waitForFrame(t *testing.T, out *lockedBuffer, kind, field string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, line := range strings.Split(out.String(), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(line), &m) != nil || m["type"] != kind {
				continue
			}
			if v, _ := m[field].(string); v != "" {
				return v
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %s frame carrying %s:\n%s", kind, field, out.String())
	return ""
}

func findFrame(t *testing.T, out *lockedBuffer, kind string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(out.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["type"] == kind {
			return m
		}
	}
	t.Fatalf("no %s frame:\n%s", kind, out.String())
	return nil
}

// While the picker owns the session, prompts and other commands are refused AT
// ADMISSION — and the things that can settle the sheet are not.
//
// Refused rather than admitted-and-failed, because an admitted prompt has already
// started a turn and already been echoed: failing it inside the runtime reads as the
// engine breaking, not as the session being busy. `/clear` is the sharpest case — it is
// not slow, so it ran inline past cmdBusy, and a host renders a cleared conversation by
// wiping its live state, sheet included: the picker's command was left parked on a
// question nobody could now see or answer, with its reservation held.
func TestAnExclusiveCommandRefusesPromptsAndCommandsButNotAnswers(t *testing.T) {
	out := &lockedBuffer{}
	tr := newTransport(strings.NewReader(""), out, &lockedBuffer{})
	tr.start()

	h := &Host{tr: tr, sessionID: "ses_excl", ready: true}
	h.runCtx, h.runCancel = context.WithCancel(context.Background())
	t.Cleanup(h.runCancel)
	h.bridge = NewBridge(BridgeOptions{SessionID: "ses_excl", Post: h.post})
	app := &askingApp{bridge: h.bridge, asked: make(chan struct{}), answered: make(chan int, 1)}
	h.app = app

	h.handleCommand(HostCommand{Type: CmdCommand, CommandLine: "/backend"})
	select {
	case <-app.asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the picker never ran")
	}
	qid := waitForFrame(t, out, "question:requested", "questionId")

	h.handleCommand(HostCommand{Type: CmdPrompt, Text: "hello"})
	h.handleCommand(HostCommand{Type: CmdCommand, CommandLine: "/clear"})

	// TWO refusals, counted. One shared string would pass with the prompt admitted and
	// only `/clear` refused — which is the half that loses a message.
	//
	// POLLED, because frames reach the sink through the transport's writer goroutine, so
	// reading once races the report rather than testing it.
	refused := time.Now().Add(5 * time.Second)
	for countFrames(out, "command-busy") < 2 {
		if time.Now().After(refused) {
			t.Fatalf("expected a refusal for BOTH the prompt and the command, got %d:\n%s",
				countFrames(out, "command-busy"), out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	// And nothing ran. A turn would have started for the prompt; `/clear` would have
	// erased the sheet the picker is still parked on.
	if strings.Contains(out.String(), `"type":"turn:start"`) {
		t.Error("the prompt started a turn while a picker held the session")
	}
	if strings.Contains(out.String(), "conversationCleared") {
		t.Error("/clear ran while a picker held the session — it would have erased the sheet")
	}

	// …and the answer still lands, or the sheet could never be settled at all.
	h.handleCommand(HostCommand{Type: CmdQuestionAnswer, QuestionID: qid, ChoiceIndex: 0})
	select {
	case <-app.answered:
	case <-time.After(5 * time.Second):
		t.Fatal("the answer was refused by the gate meant to protect the question")
	}

	// And once the command finishes, the session is open again.
	deadline := time.Now().Add(5 * time.Second)
	for h.exclusiveHeld() {
		if time.Now().After(deadline) {
			t.Fatal("the session stayed exclusive after the command finished")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A prompt the host has ALREADY ACCEPTED owns the session, even though Session.inFlight
// is not claimed until its worker reaches Send.
//
// Without this, a bare `/backend` arriving in that gap takes the reservation and the
// accepted prompt's Send is refused — the user's message swallowed by a command they
// typed afterwards. `h.busy` is set synchronously on the loop at admission, which is
// what makes it a usable gate; the reservation is not, which is why it cannot be.
func TestAnExclusiveCommandIsRefusedWhileAPromptIsAlreadyAdmitted(t *testing.T) {
	out := &lockedBuffer{}
	tr := newTransport(strings.NewReader(""), out, &lockedBuffer{})
	tr.start()

	h := &Host{tr: tr, sessionID: "ses_busy", ready: true}
	h.runCtx, h.runCancel = context.WithCancel(context.Background())
	t.Cleanup(h.runCancel)
	h.bridge = NewBridge(BridgeOptions{SessionID: "ses_busy", Post: h.post})
	app := &askingApp{bridge: h.bridge, asked: make(chan struct{}), answered: make(chan int, 1)}
	h.app = app

	// A turn is admitted but its worker has not reached Send — exactly the gap.
	h.turnMu.Lock()
	h.busy = true
	h.turnMu.Unlock()

	h.handleCommand(HostCommand{Type: CmdCommand, CommandLine: "/backend"})

	select {
	case <-app.asked:
		t.Fatal("a picker opened while a prompt was already admitted; that prompt would be refused")
	case <-time.After(200 * time.Millisecond):
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "A turn is running") {
		if time.Now().After(deadline) {
			t.Fatalf("the refusal was never reported:\n%s", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if h.exclusiveHeld() {
		t.Error("a refused command left the session marked exclusive")
	}
}

// PROMPT admission is open before the result says the command is over.
//
// The result frame is what a host uses to decide it can accept input again — Daintree
// keeps its composer disabled from the moment a local question is answered until that
// frame arrives. Publishing it while this command still owned the session meant the
// composer came back live a beat early, and a prompt submitted in that beat was refused
// by the gate: accepted by the transport, cleared from the draft, never seen by the
// model.
//
// The COMMAND gate stays up through this window on purpose (a command admitted here
// would post its own result first), so what is sampled is specifically the prompt half.
//
// Sampled at McpStatus, which the host calls on the SAME goroutine immediately after
// posting the result. Release-before-post reads open there; release-in-the-worker's-
// defer — the bug — reads blocked. That makes this an ordering assertion rather than a
// timing one.
func TestTheExclusiveGateIsReleasedBeforeTheResultIsPublished(t *testing.T) {
	out := &lockedBuffer{}
	tr := newTransport(strings.NewReader(""), out, &lockedBuffer{})
	tr.start()

	h := &Host{tr: tr, sessionID: "ses_order", ready: true}
	h.runCtx, h.runCancel = context.WithCancel(context.Background())
	t.Cleanup(h.runCancel)
	h.bridge = NewBridge(BridgeOptions{SessionID: "ses_order", Post: h.post})

	gateAfterResult := make(chan bool, 1)
	h.app = &gateProbeApp{host: h, seen: gateAfterResult}

	h.handleCommand(HostCommand{Type: CmdCommand, CommandLine: "/backend"})

	select {
	case blocked := <-gateAfterResult:
		if blocked {
			t.Fatal("the result was published while prompts were still refused — one sent on the strength of it would be lost")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the command never published a result")
	}
}

// gateProbeApp samples the PROMPT gate at the first thing the host does AFTER publishing
// a command result.
type gateProbeApp struct {
	App
	host *Host
	seen chan bool
}

func (a *gateProbeApp) IsSlowCommand(string) bool      { return true }
func (a *gateProbeApp) IsExclusiveCommand(string) bool { return true }

func (a *gateProbeApp) McpStatus() (bool, *int, string) {
	promptsBlocked, _ := a.host.exclusiveGates()
	select {
	case a.seen <- promptsBlocked:
	default:
	}
	return false, nil, ""
}

func (a *gateProbeApp) RunCommandWithProgress(context.Context, string, func(string)) CommandOutcome {
	return CommandOutcome{Text: "done"}
}

func (a *gateProbeApp) RunCommand(ctx context.Context, line string) CommandOutcome {
	return a.RunCommandWithProgress(ctx, line, nil)
}

// countFrames counts emitted frames carrying `code` (a host:error code) or that type.
func countFrames(out *lockedBuffer, code string) int {
	n := 0
	for _, line := range strings.Split(out.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		// Matched on the structured CODE, not the prose. The wording of a refusal is
		// microcopy and will be reworded; that it is the command-busy refusal is the
		// contract.
		if m["code"] == code {
			n++
		}
	}
	return n
}

// Prompts are let back in EARLIER than commands, and that gap is deliberate.
//
// Prompts must be admissible by the time `command:result` lands, because that frame is
// what a host uses to re-enable its composer. Commands must NOT be: one admitted in the
// same window posts its own result first, so the host renders the older command's
// outcome last and shows a state that is no longer live.
func TestPromptsReopenBeforeCommandsDoWhenACommandReleasesTheSession(t *testing.T) {
	h := &Host{sessionID: "ses_gates", ready: true}
	h.runCtx, h.runCancel = context.WithCancel(context.Background())
	t.Cleanup(h.runCancel)

	h.turnMu.Lock()
	h.cmdBusy, h.cmdExclusive, h.cmdPromptsReleased = true, true, false
	h.turnMu.Unlock()

	if p, c := h.exclusiveGates(); !p || !c {
		t.Fatalf("while the command owns the session both must be blocked (prompts=%v commands=%v)", p, c)
	}

	// The seam before the result is posted.
	h.releaseExclusive()
	p, c := h.exclusiveGates()
	if p {
		t.Error("prompts are still blocked at the moment the result is about to be published")
	}
	if !c {
		t.Error("commands reopened with prompts — one admitted now reports before this command does")
	}

	// The worker's unwind, after the result is out.
	h.turnMu.Lock()
	h.cmdExclusive, h.cmdPromptsReleased = false, false
	h.turnMu.Unlock()
	if p, c := h.exclusiveGates(); p || c {
		t.Errorf("the session stayed gated after the command finished (prompts=%v commands=%v)", p, c)
	}
}
