package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// repl_test.go locks the REPL interrupt contract (runCancellable) and the console
// phase-liveness cues — the two behaviors that keep the line REPL responsive
// without killing the process or sitting mute through model latency.

func TestRunCancellable_PropagatesNormalError(t *testing.T) {
	want := errors.New("boom")
	got := runCancellable(context.Background(), make(chan os.Signal), func(context.Context) error {
		return want
	})
	if got != want {
		t.Fatalf("normal completion error = %v, want %v", got, want)
	}
}

func TestClassicREPL_ParentCancellationExits(t *testing.T) {
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	overrides, err := overridesFromOptions(Options{
		Offline: boolPtr(true),
		Project: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Create(app.CreateOptions{Overrides: overrides})
	if err != nil {
		t.Fatal(err)
	}

	stdin, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() {
		os.Stdin = originalStdin
		_ = writer.Close()
		_ = stdin.Close()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if code := startRepl(ctx, a); code != domain.OneShotExitCode.Cancelled {
		t.Fatalf("cancelled line REPL exit = %d, want %d", code, domain.OneShotExitCode.Cancelled)
	}
}

func TestRunCancellable_SignalCancelsTheUnit(t *testing.T) {
	sig := make(chan os.Signal, 1)
	started := make(chan struct{})
	var sawCancel bool
	go func() {
		<-started
		sig <- os.Interrupt
	}()
	got := runCancellable(context.Background(), sig, func(ctx context.Context) error {
		close(started)
		<-ctx.Done() // block until the interrupt cancels us
		sawCancel = true
		return ctx.Err()
	})
	if got != nil {
		t.Fatalf("a signal-cancelled unit must return nil (not an error), got %v", got)
	}
	if !sawCancel {
		t.Fatal("the unit's context was never cancelled by the signal")
	}
}

func newSink(tty bool) (*consoleSink, *bytes.Buffer) {
	var buf bytes.Buffer
	r := render.New(&buf)
	return &consoleSink{r: r, diagnostics: r, tty: tty}, &buf
}

// Backend runbook selection writes NOTHING to the console transcript, matching the attached session.
// It is prompt-assembly machinery, not a step in the operator's narrative; the run log /
// --json stream / debug trace keep the signal. Pinned so the cue is not reintroduced by
// reflex.
func TestConsoleSinkRunbookLoadedIsSilent(t *testing.T) {
	s, buf := newSink(false)
	s.RunbookLoaded([]string{"Orchestrate agents"})
	s.RunbookLoaded([]string{"Orchestrate agents", "Plain worktree"})
	if got := buf.String(); got != "" {
		t.Fatalf("runbook loads must print nothing, got %q", got)
	}
}

// …and a load arriving MID-ANSWER must not break the streamed answer in two. The old
// visible cue called closeAnswer() before printing, which was right for it and wrong for
// a silent event: keeping that call would end the open paragraph and start a second one
// with no visible cause. So the answer must read as one continuous block.
// The committed per-round decision is silent for the same reason, and more so: it fires
// on EVERY round, so even a muted cue would print a line per round for prompt-assembly
// machinery the operator cannot act on.
func TestConsoleSinkRunbookDecisionIsSilent(t *testing.T) {
	s, buf := newSink(false)
	s.AssistantToken("before ")
	s.RunbookDecision(agent.RunbookDecisionEvent{
		Active:   []agent.RunbookRef{{ID: "orchestrate", Title: "Orchestrate agents"}},
		Selector: agent.RunbookSelectorOutcome{Ran: true, Degraded: true},
	})
	if !s.answerOpen {
		t.Fatal("a silent runbook decision closed the open answer")
	}
	s.AssistantToken("after")
	s.AssistantEnd("", "")

	got := buf.String()
	if strings.Contains(got, "Orchestrate agents") || strings.Contains(got, "degraded") {
		t.Fatalf("runbook decisions must print nothing, got %q", got)
	}
	if !strings.Contains(got, "before after") {
		t.Fatalf("the answer was split by a mid-stream runbook decision: %q", got)
	}
}

func TestConsoleSinkRunbookLoadedDoesNotSplitAnswer(t *testing.T) {
	s, buf := newSink(false)
	s.AssistantToken("before ")
	s.RunbookLoaded([]string{"Orchestrate agents"})
	if !s.answerOpen {
		t.Fatal("a silent runbook load closed the open answer")
	}
	s.AssistantToken("after")
	s.AssistantEnd("", "")

	got := buf.String()
	if !strings.Contains(got, "before after") {
		t.Fatalf("the answer was split by a mid-stream runbook load: %q", got)
	}
}

func TestOneShotConsoleSinkSeparatesDiagnosticsAndTracksCancellation(t *testing.T) {
	var stdout, stderr bytes.Buffer
	s := newOneShotConsoleSink(render.New(&stdout), render.New(&stderr))

	s.Error("backend unavailable")
	if !s.Failed() || !strings.Contains(stderr.String(), "backend unavailable") {
		t.Fatalf("error was not tracked on stderr: failed=%v stderr=%q", s.Failed(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("failed one-shot wrote to stdout: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	s = newOneShotConsoleSink(render.New(&stdout), render.New(&stderr))
	s.AssistantCancelled("")
	if !s.Cancelled() || !strings.Contains(stderr.String(), "Turn cancelled") {
		t.Fatalf("cancellation was not tracked on stderr: cancelled=%v stderr=%q", s.Cancelled(), stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("cancelled one-shot with no content wrote to stdout: %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	s = newOneShotConsoleSink(render.New(&stdout), render.New(&stderr))
	s.AssistantStart()
	s.AssistantToken("hello")
	s.AssistantEnd("hello", "")
	if got := stdout.String(); got != "\nhello\n" {
		t.Fatalf("successful streamed answer formatting = %q", got)
	}

	stdout.Reset()
	stderr.Reset()
	s = newOneShotConsoleSink(render.New(&stdout), render.New(&stderr))
	s.AssistantToken("partial")
	s.Error("stream failed")
	if got := stdout.String(); got != "\npartial\n" {
		t.Fatalf("failed partial answer was not cleanly terminated: %q", got)
	}
	if !strings.Contains(stderr.String(), "stream failed") {
		t.Fatalf("partial-stream error missing from stderr: %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	s = newOneShotConsoleSink(render.New(&stdout), render.New(&stderr))
	s.AssistantToken("before")
	s.ToolCall(agent.ToolCallEvent{Name: "memory.list", Args: `{}`})
	s.ToolResult(agent.ToolResultEvent{Result: domain.Ok("listed", nil)})
	s.AssistantToken("after")
	s.AssistantEnd("beforeafter", "")
	got := stdout.String()
	if strings.Count(got, "before") != 1 || strings.Count(got, "after") != 1 || !strings.Contains(got, "memory.list") {
		t.Fatalf("multi-round console output lost or duplicated content: %q", got)
	}
}

// The classic-REPL confirm handler must mirror the attached session's friction off the safety
// gate's verdict: git/system (NeedsTypedConfirm) demand the typed phrase, everything
// else keeps the single-key [y/N]. This is the divergence issue #210 fixes — the same
// action must be equally hard to approve on either surface.
func TestBuildConfirmFunc(t *testing.T) {
	cases := []struct {
		name      string
		typed     bool
		answer    string
		wantApprv bool
	}{
		{"typed phrase approves", true, "confirm", true},
		{"typed wrong phrase declines", true, "nope", false},
		{"typed bare y does NOT approve", true, "y", false},
		{"typed phrase trimmed+casefolded approves", true, "  CONFIRM  ", true},
		{"typed empty declines", true, "", false},
		{"single-key y approves", false, "y", true},
		{"single-key yes approves", false, "yes", true},
		{"single-key n declines", false, "n", false},
		{"single-key empty declines (safe default)", false, "", false},
		{"single-key confirm word does NOT approve", false, "confirm", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			r := render.New(&buf)
			ask := func(string) string { return c.answer }
			fn := buildConfirmFunc(r, ask)
			got, err := fn(context.Background(), tools.ConfirmRequest{
				ToolName:          "git.push",
				Risk:              domain.RiskGit,
				Summary:           "push branch to origin",
				NeedsTypedConfirm: c.typed,
			})
			if err != nil {
				t.Fatalf("confirm returned error: %v", err)
			}
			if got != c.wantApprv {
				t.Fatalf("approve = %v, want %v (typed=%v answer=%q)", got, c.wantApprv, c.typed, c.answer)
			}
		})
	}
}

// A typed-confirm prompt must actually ASK for the phrase (not the bare [y/N]), so the
// human sees the heightened friction.
func TestBuildConfirmFunc_TypedPromptText(t *testing.T) {
	var buf bytes.Buffer
	r := render.New(&buf)
	var prompted string
	ask := func(p string) string { prompted = p; return "confirm" }
	fn := buildConfirmFunc(r, ask)
	_, _ = fn(context.Background(), tools.ConfirmRequest{
		ToolName: "daintree.call", Risk: domain.RiskSystem, NeedsTypedConfirm: true,
	})
	if !strings.Contains(prompted, replConfirmPhrase) {
		t.Fatalf("typed-confirm prompt should ask for the phrase %q, got %q", replConfirmPhrase, prompted)
	}
	if strings.Contains(prompted, "[y/N]") {
		t.Fatalf("typed-confirm must not offer the single-key [y/N] prompt, got %q", prompted)
	}
}

// The classic-REPL multiple-choice handler must accept a letter or 1-based number, take
// the default on an empty line, re-prompt on an invalid entry, and — critically — CANCEL
// on EOF (askLine ok=false) rather than silently answering with the default.
func TestBuildAskChoiceFunc(t *testing.T) {
	var buf bytes.Buffer
	r := render.New(&buf)
	req := tools.AskChoiceRequest{
		Question: "Which env?",
		Options: []tools.ChoiceOption{
			{Label: "A", Text: "Local"}, {Label: "B", Text: "Staging"}, {Label: "C", Text: "Production"},
		},
		Default: 2,
	}

	t.Run("letter answers", func(t *testing.T) {
		fn := buildAskChoiceFunc(r, func(string) (string, bool) { return "b", true })
		ans, err := fn(context.Background(), req)
		if err != nil || ans.Index != 1 || ans.Label != "B" {
			t.Fatalf("b should answer option B (ans=%+v err=%v)", ans, err)
		}
	})
	t.Run("number answers", func(t *testing.T) {
		fn := buildAskChoiceFunc(r, func(string) (string, bool) { return "3", true })
		ans, err := fn(context.Background(), req)
		if err != nil || ans.Index != 2 {
			t.Fatalf("3 should answer option C (ans=%+v err=%v)", ans, err)
		}
	})
	t.Run("empty takes default", func(t *testing.T) {
		fn := buildAskChoiceFunc(r, func(string) (string, bool) { return "", true })
		ans, err := fn(context.Background(), req)
		if err != nil || ans.Index != 2 {
			t.Fatalf("empty should take the default (index 2), got %+v err=%v", ans, err)
		}
	})
	t.Run("EOF cancels", func(t *testing.T) {
		fn := buildAskChoiceFunc(r, func(string) (string, bool) { return "", false })
		_, err := fn(context.Background(), req)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("EOF should cancel (context.Canceled), got %v", err)
		}
	})
	t.Run("invalid then valid re-prompts", func(t *testing.T) {
		calls := 0
		fn := buildAskChoiceFunc(r, func(string) (string, bool) {
			calls++
			if calls == 1 {
				return "z", true // out of range → re-prompt
			}
			return "a", true
		})
		ans, err := fn(context.Background(), req)
		if err != nil || ans.Index != 0 {
			t.Fatalf("should re-prompt then accept 'a' (ans=%+v err=%v)", ans, err)
		}
		if calls != 2 {
			t.Fatalf("expected 2 prompts, got %d", calls)
		}
	})
}

func TestConsoleSinkPhase_TTYShowsSilentWork(t *testing.T) {
	s, buf := newSink(true)

	s.Phase(domain.PhaseAnalyzing)
	if !strings.Contains(buf.String(), "analyzing request") {
		t.Fatalf("Analyzing phase produced no cue:\n%q", buf.String())
	}
	// A repeat of the SAME phase must not re-print (dedup on transitions).
	before := buf.Len()
	s.Phase(domain.PhaseAnalyzing)
	if buf.Len() != before {
		t.Fatalf("repeated Analyzing phase printed again:\n%q", buf.String())
	}
	s.Phase(domain.PhaseIntegrating)
	if !strings.Contains(buf.String(), "integrating results") {
		t.Fatalf("Integrating phase produced no cue:\n%q", buf.String())
	}
	// A self-evident phase (Generating streams its own text) prints nothing.
	before = buf.Len()
	s.Phase(domain.PhaseGenerating)
	if buf.Len() != before {
		t.Fatalf("Generating phase should be silent on the console:\n%q", buf.String())
	}
}

func TestConsoleSinkPhase_NonTTYStaysQuiet(t *testing.T) {
	s, buf := newSink(false)
	s.Phase(domain.PhaseAnalyzing)
	s.Phase(domain.PhaseIntegrating)
	if buf.Len() != 0 {
		t.Fatalf("phase cues must be suppressed on non-TTY (piped) output, got:\n%q", buf.String())
	}
}
