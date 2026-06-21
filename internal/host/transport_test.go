package host

import (
	"bufio"
	"bytes"
	"context"
	"io"
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
