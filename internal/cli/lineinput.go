package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
)

// lineResult keeps a read error ALONGSIDE the line rather than collapsing the two.
// That is the whole reason this shape exists: a reader that returned only a string
// could not tell "the caller submitted an empty line" from "stdin closed", and the
// two mean opposite things — one is a turn to skip, the other is the end of input.
type lineResult struct {
	line string
	err  error
}

// streamLines reads r line by line on its own goroutine and publishes each result on
// the returned channel, closing it when the reader is exhausted.
//
// The goroutine exists so a BLOCKED read cannot pin the caller: a line read from a
// terminal or an idle pipe has no deadline, so SIGTERM, a --timeout, or a cancelled
// parent would otherwise be unable to release an idle consumer. Cancellation releases
// the CONSUMER; the read itself may well stay blocked in the kernel until the process
// exits, which is the honest limit of doing this without a raw-mode reader.
//
// limit caps a single line in bytes; 0 means unbounded. It matters because a line has
// no natural end: input with no newline in it — a binary file piped in by mistake, or
// /dev/zero — accumulates in memory until the process dies, and a --timeout cannot save
// it, since cancellation frees the consumer without interrupting the read already in
// flight. A refusal naming the bound is the same trade --prompt-file makes at the same
// size: a truncated prompt is worse than a rejected one, because the run then looks
// successful while the model was asked a different question.
//
// r is never closed — os.Stdin belongs to the process, not to this read.
func streamLines(ctx context.Context, r io.Reader, limit int64) <-chan lineResult {
	lines := make(chan lineResult)
	reader := bufio.NewReader(r)
	go func() {
		defer close(lines)
		for {
			line, err := readBoundedLine(reader, limit)
			// The line is published even when err is non-nil: a final line with no
			// trailing newline arrives as (text, io.EOF), and dropping it would
			// silently discard the last prompt in a file that lacks one.
			select {
			case lines <- lineResult{line: line, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return lines
}

// readBoundedLine is bufio.Reader.ReadString('\n') with a size bound.
//
// It cannot BE ReadString, because that method's whole failure mode here is that it
// grows without asking. ReadSlice reports ErrBufferFull once its fixed buffer fills
// without finding the delimiter, which is the only hook a bounded accumulator needs:
// append, check, continue. With limit 0 the check never fires and the behaviour — the
// returned text INCLUDING the newline, and the error that accompanies a final
// unterminated line — is byte-identical to ReadString, which is what lets the classic
// REPL keep using this unchanged.
func readBoundedLine(reader *bufio.Reader, limit int64) (string, error) {
	var buf []byte
	for {
		// ReadSlice's result aliases the reader's internal buffer and is invalidated by
		// the next read, so it must be copied before looping — append does that.
		chunk, err := reader.ReadSlice('\n')
		buf = append(buf, chunk...)
		if limit > 0 && int64(len(buf)) > limit {
			return "", fmt.Errorf("--multi-turn: a prompt line is larger than the %d-byte limit", limit)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return string(buf), err
	}
}
