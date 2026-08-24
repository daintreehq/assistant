package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// lockedBuffer is a goroutine-safe bytes.Buffer for capturing the writer-goroutine
// output from the test goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// flushCounter wraps a writer and counts Flush() calls — proving the transport
// calls Flush() (bufio's contract), not Sync(), on a bufio-style writer.
type flushCounter struct {
	w       io.Writer
	mu      sync.Mutex
	flushes int
}

func (f *flushCounter) Write(p []byte) (int, error) { return f.w.Write(p) }
func (f *flushCounter) Flush() error {
	f.mu.Lock()
	f.flushes++
	f.mu.Unlock()
	// Delegate to the underlying writer's Flush (the wrapped bufio.Writer) so the
	// buffered bytes actually reach the backing buffer — proving end-to-end that the
	// transport's per-event Flush() drives data out.
	if bf, ok := f.w.(interface{ Flush() error }); ok {
		return bf.Flush()
	}
	return nil
}
func (f *flushCounter) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushes
}

// Fix 4: a bufio-wrapped writer implements Flush(), not Sync(). The transport must
// Flush() per event or the buffered bytes never reach the OS.
func TestTransportFlushesBufioWriter(t *testing.T) {
	var underlying lockedBuffer
	bw := bufio.NewWriter(&underlying)
	fc := &flushCounter{w: bw} // exposes Flush(), wrapping the bufio.Writer

	tr := newTransport(strings.NewReader(""), fc, io.Discard)
	tr.start()
	defer tr.Close()

	tr.send("s", EvError{Code: "x", Message: "hello"})

	deadline := time.After(2 * time.Second)
	for {
		if fc.count() > 0 && strings.Contains(underlying.String(), `"hello"`) {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("event not flushed: flushes=%d out=%q", fc.count(), underlying.String())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// Confirm a real bufio.Writer (no Flush-wrapper) is also driven to the OS — exercises
// the flushWriter type-switch hitting the bufio.Writer.Flush branch directly.
func TestTransportFlushesRealBufioWriter(t *testing.T) {
	var underlying lockedBuffer
	bw := bufio.NewWriter(&underlying)

	tr := newTransport(strings.NewReader(""), bw, io.Discard)
	tr.start()
	defer tr.Close()
	tr.send("s", EvError{Code: "x", Message: "buffered"})

	deadline := time.After(2 * time.Second)
	for !strings.Contains(underlying.String(), "buffered") {
		select {
		case <-deadline:
			t.Fatalf("bufio.Writer not flushed: %q", underlying.String())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
}

// Regression: closeOnContext must not retire the writer ahead of a teardown that
// hasn't sent its terminal frame yet. In production, a parent-context cancel and
// Run()'s own teardown are triggered by the SAME context — closeOnContext used to
// call the full Close(), closing t.closed and letting an idle writer retire, racing
// the sendSync call teardown makes a moment later. If the writer had already
// retired, the terminal frame would sit in outQ forever undelivered, and sendSync
// would spend its whole budget finding that out — silently losing host:shutdown.
func TestContextCancelDoesNotRaceAwayTheTerminalFrame(t *testing.T) {
	out := &syncBuffer{}
	tr := newTransport(strings.NewReader(""), out, io.Discard)
	tr.start()

	ctx, cancel := context.WithCancel(context.Background())
	tr.closeOnContext(ctx)
	cancel()
	// Give closeOnContext's goroutine a head start — exactly the interleaving that
	// used to let the writer retire before sendSync ever got there.
	time.Sleep(20 * time.Millisecond)

	tr.sendSync("s", EvShutdown{Reason: ShutdownExit})
	tr.Close()

	line := awaitLine(t, out)
	if !strings.Contains(line, "host:shutdown") {
		t.Fatalf("host:shutdown was not delivered after a context cancel raced ahead of it: %q", line)
	}
}

// Fix 6: the stdin reader goroutine must abort on context cancel / Close so a
// non-os.Exit shutdown does not leak it. We use a pipe so the reader is genuinely
// blocked in Scan(), then cancel and assert the inbound channel closes.
func TestTransportReaderCancelableOnContext(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close()
	tr := newTransport(pr, io.Discard, io.Discard)

	ctx, cancel := context.WithCancel(context.Background())
	tr.closeOnContext(ctx)
	ch := tr.commands()

	// Reader is now blocked in Scan() on the live pipe. Cancel → Close → channel closes.
	cancel()

	select {
	case _, ok := <-ch:
		// Either a drained inbound or an immediate close; both are fine — the point is
		// the goroutine does not hang forever.
		_ = ok
	case <-time.After(2 * time.Second):
		t.Fatal("reader goroutine did not unblock on context cancel")
	}
}

// Close must be idempotent and make send() a no-op (no send-on-closed-channel panic).
func TestTransportCloseIdempotentAndSendNoop(t *testing.T) {
	tr := newTransport(strings.NewReader(""), io.Discard, io.Discard)
	tr.start()
	tr.Close()
	tr.Close() // must not panic
	// A post-Close send must not panic on the closed outQ.
	tr.send("s", EvError{Code: "x", Message: "after-close"})
}

// TestTransportConcurrentSendDuringClose hammers send() from many goroutines while
// Close() races, exercising the window where a producer is mid-enqueue as the
// transport shuts down. Because outQ is never closed, no send may panic. Run under
// -race; repeated iterations make the close-vs-send interleaving likely.
func TestTransportConcurrentSendDuringClose(t *testing.T) {
	for iter := 0; iter < 50; iter++ {
		tr := newTransport(strings.NewReader(""), io.Discard, io.Discard)
		tr.start()

		var wg sync.WaitGroup
		for p := 0; p < 8; p++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < 200; i++ {
					// A send racing Close() must never panic on a closed channel.
					tr.send("s", EvError{Code: "x", Message: "race"})
				}
			}()
		}
		// Close concurrently with the producers.
		tr.Close()
		wg.Wait()
	}
}

// Regression for doc finding NH-004: sendStream must never be able to block on
// writer-owned state. Before the fix, sendStream took writeMu merely to read
// sendFailed — and the writer goroutine holds that same mutex across a blocked
// Write — so a wedged consumer stalled sendStream on the MUTEX before its own
// bounded backpressure budget ever started running. sendFailed is now a lock-free
// atomic, so a writer permanently stuck inside Write cannot delay sendStream at all
// when the queue still has room.
// enteringBlockingWriter blocks every Write until release is closed, and closes
// entered the instant the FIRST Write call begins — so a test can synchronize on
// the writer being genuinely inside Write() rather than inferring it from a sleep,
// which could let the old bug (blocking on a mutex the writer hadn't taken yet)
// escape detection.
type enteringBlockingWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *enteringBlockingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

func TestSendStreamNeverBlocksOnAWedgedWriter(t *testing.T) {
	w := &enteringBlockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(w.release)

	tr := newTransport(strings.NewReader(""), w, io.Discard)
	tr.start()
	defer tr.Close()

	tr.send("s", EvError{Code: "x", Message: "first"})
	select {
	case <-w.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the writer never entered Write() on the first frame")
	}

	// Run sendStream on its own goroutine and race it against a timeout, rather
	// than calling it inline and timing the call: a genuine regression here blocks
	// FOREVER (the writer never releases), and an inline call would hang until Go's
	// global test timeout with a confusing failure instead of this test's own
	// message.
	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		tr.sendStream("s", EvTurnToken{TurnID: "t", Chunk: "tok"})
		done <- time.Since(start)
	}()

	select {
	case elapsed := <-done:
		if elapsed > 500*time.Millisecond {
			t.Fatalf("sendStream took %s against a writer blocked in Write; it must not be able "+
				"to block on writer-owned state before its own backpressure budget starts", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sendStream never returned against a writer blocked in Write — it is blocking on writer-owned state")
	}
}

// failingFlusher succeeds every Write but fails Flush — modeling a buffered writer
// that accepts bytes into memory and only fails once they are actually sent
// downstream.
type failingFlusher struct {
	w        io.Writer
	flushErr error
}

func (f *failingFlusher) Write(p []byte) (int, error) { return f.w.Write(p) }
func (f *failingFlusher) Flush() error                { return f.flushErr }

// Regression: a successful write to a REAL OS pipe (production stdout is a pipe to
// the parent process, not a regular file) must never be reported as a failure.
// *os.File implements Sync(), and fsync on a pipe is commonly unsupported (EINVAL
// on Linux) — this must use os.Pipe(), which returns *os.File on both ends, NOT
// io.Pipe(): *io.PipeWriter has no Sync() method at all, so a test built on it
// would pass unconditionally without ever exercising the code path in question.
func TestSuccessfulWriteToAPipeIsNeverAFailure(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer pr.Close()
	// Drain the read side so the writer's Write() call actually completes, and
	// forward every line so this test can positively confirm the frame arrived
	// rather than merely asserting the absence of a failure signal.
	read := make(chan string, 1)
	go func() {
		s := bufio.NewScanner(pr)
		if s.Scan() {
			read <- s.Text()
		}
	}()

	tr := newTransport(strings.NewReader(""), pw, io.Discard)
	failed := make(chan struct{})
	tr.onSendFail = func(error) { close(failed) }
	tr.start()
	defer tr.Close()

	tr.send("s", EvError{Code: "x", Message: "hello"})

	select {
	case line := <-read:
		if !strings.Contains(line, "hello") {
			t.Fatalf("unexpected frame on the pipe: %q", line)
		}
	case <-failed:
		t.Fatal("a successful write to a pipe was reported as a transport failure")
	case <-time.After(2 * time.Second):
		t.Fatal("the frame never arrived on the pipe")
	}
	if tr.sendFailed.Load() {
		t.Error("sendFailed was set after a successful pipe write")
	}
}

// Regression for doc finding NH-006: a flush-only failure must fail the session
// exactly like a write failure. Before the fix, flushWriter discarded the error —
// Write "succeeded", sendFailed stayed false, and onSendFail never ran, so a
// critical frame could vanish downstream with no signal the transport had failed.
func TestFlushFailureFailsTheSession(t *testing.T) {
	ff := &failingFlusher{w: io.Discard, flushErr: errors.New("disk full")}

	var diag bytes.Buffer
	tr := newTransport(strings.NewReader(""), ff, &diag)
	failed := make(chan struct{})
	var once sync.Once
	tr.onSendFail = func(error) { once.Do(func() { close(failed) }) }
	tr.start()
	defer tr.Close()

	tr.send("s", EvError{Code: "x", Message: "hello"})

	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("a flush-only failure did not trip onSendFail; a critical frame can vanish with no signal")
	}
	if !tr.sendFailed.Load() {
		t.Error("sendFailed was not set on a flush-only error")
	}
}

// Regression for doc finding NH-005: shutdown must never lose a frame that was
// already queued ahead of it, and host:shutdown must always be strictly last, even
// with many concurrent producers racing the seal. The old design had a real window
// — writerLoop dequeues a frame from the channel before acquiring writeMu, and a
// concurrent seal could observe writerBusy still false in that gap and finish
// before the dequeued frame was ever written. Routing every frame (including the
// terminal one) through the same single-consumer queue removes that window
// structurally rather than papering over the timing.
//
// Ordering alone is not a strong enough guard here: the historical bug silently
// DISCARDED an already-claimed frame while still leaving shutdown last, so a test
// that only checks ordering would pass against the old bug too. Every producer
// frame carries a unique id; this test records exactly which enqueues the
// transport itself reported as accepted (calling stampAndEnqueue directly, same as
// send() does internally) and then requires every one of them to appear on the
// wire exactly once. Run under -race; repeated iterations make any reintroduced
// race window likely to surface.
func TestConcurrentProducersDuringShutdownPreserveOrderAndLoseNothing(t *testing.T) {
	for iter := 0; iter < 20; iter++ {
		out := &syncBuffer{}
		tr := newTransport(strings.NewReader(""), out, io.Discard)
		tr.start()

		var mu sync.Mutex
		accepted := map[string]bool{}

		var wg sync.WaitGroup
		const producers = 8
		const perProducer = 50
		for p := 0; p < producers; p++ {
			p := p
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := 0; i < perProducer; i++ {
					id := fmt.Sprintf("p%d-%d", p, i)
					if tr.stampAndEnqueue("s", EvTurnToken{TurnID: "t", Chunk: id}) == enqueueOK {
						mu.Lock()
						accepted[id] = true
						mu.Unlock()
					}
				}
			}()
		}
		time.Sleep(time.Millisecond) // let some frames land before the seal
		tr.sendSync("s", EvShutdown{Reason: ShutdownExit})
		wg.Wait()
		tr.Close()

		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		if len(lines) == 0 || lines[0] == "" {
			t.Fatalf("iter %d: no frames written at all", iter)
		}

		seen := map[string]bool{}
		for i, line := range lines {
			var f map[string]any
			if err := json.Unmarshal([]byte(line), &f); err != nil {
				t.Fatalf("iter %d line %d: not valid JSON: %v (%q)", iter, i, err, line)
			}
			isShutdown := f["type"] == "host:shutdown"
			isLast := i == len(lines)-1
			if isShutdown != isLast {
				t.Fatalf("iter %d: host:shutdown must be exactly the last of %d frames (line %d, type %v)",
					iter, len(lines), i, f["type"])
			}
			if id, ok := f["chunk"].(string); ok {
				if seen[id] {
					t.Fatalf("iter %d: frame %q reached the wire more than once", iter, id)
				}
				seen[id] = true
			}
		}
		for id := range accepted {
			if !seen[id] {
				t.Fatalf("iter %d: frame %q was accepted onto the queue but never reached the wire — silently lost", iter, id)
			}
		}
		if len(seen) != len(accepted) {
			t.Fatalf("iter %d: %d frame(s) reached the wire but only %d were ever accepted", iter, len(seen), len(accepted))
		}
	}
}

// Focused, transport-level version of the reportSync-then-teardown ordering
// contract (host_test.go's TestHostBadDescriptorErrorBeforeShutdown covers it
// end-to-end through the whole Host, but a failure there is a three-second Host
// harness timeout rather than a direct assertion on the transport's own two
// entry points). sendPriorityError must not be terminal: the writer has to stay
// alive afterward so the real sendSync can still go through.
func TestSendPriorityErrorPrecedesSendSyncAndDoesNotStopTheWriter(t *testing.T) {
	out := &syncBuffer{}
	tr := newTransport(strings.NewReader(""), out, io.Discard)
	tr.start()

	tr.sendPriorityError("s", EvError{Code: "bad-descriptor", Message: "boom"})
	tr.sendSync("s", EvShutdown{Reason: ShutdownError})
	tr.Close()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want exactly 2 frames (error then shutdown), got %d: %v", len(lines), lines)
	}
	var first, second map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("line 0 not valid JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(lines[1]), &second); err != nil {
		t.Fatalf("line 1 not valid JSON: %v", err)
	}
	if first["type"] != "host:error" || first["code"] != "bad-descriptor" {
		t.Errorf("frame 0 = %v, want the bad-descriptor host:error", first)
	}
	if second["type"] != "host:shutdown" {
		t.Errorf("frame 1 = %v, want host:shutdown — sendPriorityError must not stop the writer", second)
	}
}

// Regression: Close() called WITHOUT ever going through sendSync (most tests, and
// any abrupt non-graceful teardown, do exactly this) must still reliably retire the
// writer goroutine — not just stop producing NEW output, which -race and a plain
// output assertion cannot distinguish from a genuine leak.
func TestCloseWithoutATerminalRequestRetiresTheWriter(t *testing.T) {
	tr := newTransport(strings.NewReader(""), io.Discard, io.Discard)
	tr.start()
	tr.send("s", EvError{Code: "x", Message: "one"}) // a nonempty queue at Close time
	tr.Close()

	select {
	case <-tr.writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the writer goroutine did not retire after Close()")
	}
}

// Regression: a write or flush failure occurring ON THE TERMINAL frame itself must
// still trip onSendFail/sendFailed, resolve sendSync's wait promptly (not spend the
// full budget), and still let the writer goroutine retire.
func TestTerminalFrameWriteFailureStillFailsAndRetires(t *testing.T) {
	w := &failingWriter{err: errors.New("broken pipe")}
	var diag bytes.Buffer
	tr := newTransport(strings.NewReader(""), w, &diag)
	failed := make(chan struct{})
	var once sync.Once
	tr.onSendFail = func(error) { once.Do(func() { close(failed) }) }
	tr.start()

	start := time.Now()
	tr.sendSync("s", EvShutdown{Reason: ShutdownExit})
	if elapsed := time.Since(start); elapsed > sealDrainBudget {
		t.Errorf("sendSync took %s on a terminal write failure; deliver() resolves done immediately on failure, it should not wait out the budget", elapsed)
	}

	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("a write failure on the terminal frame did not trip onSendFail")
	}
	if !tr.sendFailed.Load() {
		t.Error("sendFailed was not set")
	}
	select {
	case <-tr.writerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("the writer goroutine did not retire after a terminal write failure")
	}
}

// failingWriter fails every Write with err.
type failingWriter struct{ err error }

func (w *failingWriter) Write([]byte) (int, error) { return 0, w.err }

// Regression: when a priority (sendSync/sendPriorityError) frame cannot even be
// ENQUEUED within its budget — the same "nothing is draining stdout" condition
// send() already treats as fatal for a critical frame — the transport must mark
// itself failed rather than merely logging a diagnostic. Silently returning let the
// caller (teardown) proceed to exit(0) believing host:shutdown went out when it
// never even reached the queue.
func TestPriorityEnqueueFailureFailsTheSession(t *testing.T) {
	w := &enteringBlockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	defer close(w.release)

	tr := newTransport(strings.NewReader(""), w, io.Discard)
	tr.start()
	defer tr.Close() // the queue never truly drains on its own here; retire the writer explicitly
	failed := make(chan struct{})
	var once sync.Once
	tr.onSendFail = func(error) { once.Do(func() { close(failed) }) }

	// Wedge the writer, then fill the queue past capacity so sendSync's own enqueue
	// has nowhere to go.
	tr.send("s", EvError{Code: "x", Message: "wedge"})
	select {
	case <-w.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the writer never entered Write()")
	}
	for i := 0; i < outQueueDepth+8; i++ {
		tr.send("s", EvTurnToken{TurnID: "t", Chunk: "filler"})
	}

	go tr.sendSync("s", EvShutdown{Reason: ShutdownExit})

	select {
	case <-failed:
	case <-time.After(sealDrainBudget + 2*time.Second):
		t.Fatal("a priority frame that could not be enqueued did not fail the session")
	}
}
