package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// drain collects every result the channel yields, so a test can assert on the exact
// (line, err) pairs rather than on a lossy string slice.
func drain(ch <-chan lineResult) []lineResult {
	var out []lineResult
	for r := range ch {
		out = append(out, r)
	}
	return out
}

// TestStreamLinesSplitsOnNewlinesAndKeepsEmptyOnes: an empty line is a real submission,
// not an absence, so it must survive the reader and be the caller's to interpret.
//
// Input ending in a newline also yields a trailing ("", io.EOF) sentinel, and that
// sentinel is load-bearing rather than noise: it is how BOTH consumers learn the input
// ended. The REPL's readLine turns it into ok=false, and the multi-turn loop treats it
// as the end of the script. Suppressing it would leave each of them waiting on a closed
// channel for a signal that never came.
func TestStreamLinesSplitsOnNewlinesAndKeepsEmptyOnes(t *testing.T) {
	got := drain(streamLines(context.Background(), strings.NewReader("one\n\ntwo\n"), 0))
	want := []string{"one\n", "\n", "two\n", ""}
	if len(got) != len(want) {
		t.Fatalf("got %d results (%v), want %d", len(got), got, len(want))
	}
	for i, r := range got {
		if r.line != want[i] {
			t.Errorf("result[%d].line = %q, want %q", i, r.line, want[i])
		}
	}
	// The first three are real lines with no error; the last is the EOF sentinel.
	for i := 0; i < 3; i++ {
		if got[i].err != nil {
			t.Errorf("result[%d].err = %v, want nil — a delivered line is not an error", i, got[i].err)
		}
	}
	if !errors.Is(got[3].err, io.EOF) {
		t.Errorf("trailing sentinel err = %v, want io.EOF", got[3].err)
	}
}

// TestStreamLinesDeliversAFinalUnterminatedLine is the one that would silently eat a
// prompt: a prompt file whose last line has no trailing newline arrives as (text, EOF),
// and a reader that dropped it would run every prompt but the last.
func TestStreamLinesDeliversAFinalUnterminatedLine(t *testing.T) {
	got := drain(streamLines(context.Background(), strings.NewReader("first\nlast, no newline"), 0))
	if len(got) != 2 {
		t.Fatalf("got %d results (%v), want 2", len(got), got)
	}
	if got[1].line != "last, no newline" {
		t.Errorf("final line = %q, want the unterminated text", got[1].line)
	}
	if !errors.Is(got[1].err, io.EOF) {
		t.Errorf("final line err = %v, want io.EOF alongside the text", got[1].err)
	}
}

// TestStreamLinesDistinguishesEOFFromAnEmptyLine is the whole reason lineResult carries
// an error beside the text: "the caller submitted nothing" and "there is no more input"
// mean opposite things to the loops that consume this.
func TestStreamLinesDistinguishesEOFFromAnEmptyLine(t *testing.T) {
	got := drain(streamLines(context.Background(), strings.NewReader("\n"), 0))
	if len(got) != 2 {
		t.Fatalf("got %d results (%v), want 2: the empty submission, then the EOF sentinel", len(got), got)
	}
	// The submission: text, no error. A caller must be free to read this as "the user
	// pressed Enter" and keep going.
	if got[0].line != "\n" || got[0].err != nil {
		t.Fatalf("empty submission = (%q, %v), want (\"\\n\", nil) — an empty line is not EOF",
			got[0].line, got[0].err)
	}
	// The end of input: no text, and an error. Identical strings, opposite meanings —
	// which is exactly why lineResult carries both halves.
	if got[1].line != "" || !errors.Is(got[1].err, io.EOF) {
		t.Fatalf("EOF sentinel = (%q, %v), want (\"\", io.EOF)", got[1].line, got[1].err)
	}
}

// TestStreamLinesReleasesTheGoroutineOnCancel pins the cancellation path that actually
// exists: the select on the SEND. A reader goroutine holding a line nobody is receiving
// would otherwise leak for the life of the process, one per abandoned stream.
//
// What this deliberately does NOT claim is that cancelling frees a goroutine blocked in
// the read itself. It cannot: an idle pipe read has no deadline and returns when it
// returns. That is why every consumer selects on ctx.Done() of its own accord (the
// REPL's readLine, runJSONTurns' loop) rather than waiting for this channel to close —
// and why the honest end-to-end proof of the bound is the real-binary timeout test in
// internal/e2e, not anything assertable here.
func TestStreamLinesReleasesTheGoroutineOnCancel(t *testing.T) {
	// A line IS available, so the goroutine gets past the read and parks on the send
	// with no receiver — the state this test is about.
	ctx, cancel := context.WithCancel(context.Background())
	lines := streamLines(ctx, strings.NewReader("a line nobody reads\n"), 0)

	cancel()

	// The goroutine must notice and return, which closes the channel. Draining is safe
	// either way: it may yield the buffered line first if the send won the race.
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for range lines {
		}
	}()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("reader goroutine did not exit after cancellation; it leaks per abandoned stream")
	}
}

// TestStreamLinesBoundsALine: a line has no natural end, so input containing no newline
// at all — a binary file piped in by mistake, or /dev/zero — would otherwise accumulate
// in memory until the process died. A --timeout cannot save it: cancellation frees the
// consumer without interrupting the read already in flight.
func TestStreamLinesBoundsALine(t *testing.T) {
	const limit = 512
	// Well past bufio's default 4096-byte buffer in the unbounded case, to prove the
	// bound is what stops it rather than the buffer size.
	huge := strings.Repeat("x", 20000)

	got := drain(streamLines(context.Background(), strings.NewReader(huge), limit))
	if len(got) == 0 {
		t.Fatal("no result at all; the bound must REPORT, not silently truncate")
	}
	if got[0].err == nil || !strings.Contains(got[0].err.Error(), "larger than") {
		t.Fatalf("result = (%q, %v), want an error naming the bound", got[0].line, got[0].err)
	}
	// A truncated prompt is worse than a rejected one: the run would look successful
	// while the model was asked a different question.
	if got[0].line != "" {
		t.Errorf("line = %q, want empty — an over-long line is refused, never truncated", got[0].line)
	}

	// A line comfortably longer than bufio's internal buffer but WITHIN the bound still
	// arrives whole, so the bound is a bound and not a buffer-size artefact.
	long := strings.Repeat("y", 10000)
	ok := drain(streamLines(context.Background(), strings.NewReader(long+"\n"), 1<<20))
	if len(ok) == 0 || ok[0].line != long+"\n" {
		t.Errorf("a 10000-byte line within the bound did not arrive intact (got %d results)", len(ok))
	}
}

// TestStreamLinesNeverClosesTheReader: os.Stdin belongs to the process, and closing it
// would break every later reader in the same run.
func TestStreamLinesNeverClosesTheReader(t *testing.T) {
	r := &closeSpy{Reader: strings.NewReader("only line\n")}
	drain(streamLines(context.Background(), r, 0))
	if r.closed {
		t.Fatal("streamLines closed the reader; os.Stdin is the process's, not this read's")
	}
}

type closeSpy struct {
	io.Reader
	closed bool
}

func (c *closeSpy) Close() error {
	c.closed = true
	return nil
}
