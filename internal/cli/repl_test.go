package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/cli/render"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// repl_test.go locks the REPL interrupt contract (runCancellable) and the console
// phase-liveness cues — the two behaviors that keep the classic REPL responsive
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
	return &consoleSink{r: render.New(&buf), tty: tty}, &buf
}

// The classic-REPL confirm handler must mirror the cockpit's friction off the safety
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
				ToolName:          "git.snapshotDelete",
				Risk:              domain.RiskGit,
				Summary:           "discard uncommitted work",
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
