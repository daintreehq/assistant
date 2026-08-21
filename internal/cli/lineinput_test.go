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
	got := drain(streamLines(context.Background(), strings.NewReader("one\n\ntwo\n")))
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
	got := drain(streamLines(context.Background(), strings.NewReader("first\nlast, no newline")))
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
	got := drain(streamLines(context.Background(), strings.NewReader("\n")))
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

// TestStreamLinesReleasesTheConsumerOnCancel: an idle read has no deadline, so
// cancellation is the only way a --timeout or a SIGTERM can free a waiting caller.
func TestStreamLinesReleasesTheConsumerOnCancel(t *testing.T) {
	// A reader that never returns, standing in for an idle pipe or a terminal.
	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	lines := streamLines(ctx, pr)

	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		select {
		case <-ctx.Done():
		case <-lines:
		}
	}()

	cancel()
	select {
	case <-consumed:
	case <-time.After(2 * time.Second):
		t.Fatal("consumer was not released by cancellation")
	}
}

// TestStreamLinesNeverClosesTheReader: os.Stdin belongs to the process, and closing it
// would break every later reader in the same run.
func TestStreamLinesNeverClosesTheReader(t *testing.T) {
	r := &closeSpy{Reader: strings.NewReader("only line\n")}
	drain(streamLines(context.Background(), r))
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
