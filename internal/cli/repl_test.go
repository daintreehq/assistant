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
