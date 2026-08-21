package cli

import (
	"bufio"
	"context"
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
// r is never closed — os.Stdin belongs to the process, not to this read.
func streamLines(ctx context.Context, r io.Reader) <-chan lineResult {
	lines := make(chan lineResult)
	reader := bufio.NewReader(r)
	go func() {
		defer close(lines)
		for {
			line, err := reader.ReadString('\n')
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
