package host

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sync"
)

// maxFrameBytes caps a single inbound line and a single outbound event frame.
// A frame larger than this is a protocol violation (or a hostile peer); we refuse
// it rather than buffer unbounded. 4 MiB comfortably exceeds any legitimate
// prompt/command while bounding memory.
const maxFrameBytes = 4 << 20 // 4 MiB

// outQueueDepth bounds the serialized writer queue. Events are tiny and the
// writer drains continuously, so a deep queue absorbs any burst (approval:decided
// + turn:end + tool:settled fired back-to-back) without a producer ever blocking
// on a slow stdout. If the queue ever fills (stdout wedged), an enqueue is dropped
// rather than parking the command loop — a wedged stdout already means the parent
// is gone and onSendFail will tear down.
const outQueueDepth = 1024

// inbound is one decoded inbound line plus a read error sentinel. err != nil
// signals EOF / read failure / oversize — the command loop treats it as a
// parent-exit and tears down.
type inbound struct {
	line []byte
	err  error
}

// transport owns stdio NDJSON framing. Reads: a line-reader goroutine over stdin
// feeding a channel, cancelable via Close()/context so a non-os.Exit shutdown does
// not leak it. Writes: a SINGLE dedicated writer goroutine drains a queue of
// pre-encoded frames, so events NEVER interleave (each is one whole line, flushed
// before the next) AND no producer (command loop, bridge, worker) ever blocks on a
// slow/blocked stdout — the enqueue is non-blocking. Diagnostics go to a separate
// stderr writer; protocol JSON is never written to stderr.
//
// onSendFail is invoked (once) when a stdout write fails: a broken stdout means
// the parent is gone, so the host cancels + tears down.
type transport struct {
	in  io.Reader
	out io.Writer
	err io.Writer

	// outQ carries pre-encoded NDJSON frames (without the trailing newline) to the
	// single writer goroutine. NEVER closed (producers send to it concurrently); the
	// writer stops on t.closed, draining the buffer first.
	outQ chan []byte
	// emu guards err writes (diag) so a producer-side diagnostic can't interleave
	// with a writer-goroutine diagnostic.
	emu sync.Mutex

	// closeOnce makes Close idempotent; closed signals the reader to stop and gates
	// the outQ producers so a post-Close enqueue is a no-op (never a send on a
	// closed channel panic).
	closeOnce sync.Once
	closed    chan struct{}

	// writeMu serializes the actual stdout write+flush so the teardown sendSync path
	// (a direct write of the final host:shutdown frame) can never interleave with the
	// writer goroutine still draining outQ — both write t.out. It also guards
	// sendFailed, which both goroutines read and set.
	writeMu    sync.Mutex
	sendFailed bool // guarded by writeMu (writerLoop + the teardown sendSync path)
	failOnce   sync.Once
	onSendFail func(error)
}

func newTransport(in io.Reader, out, errw io.Writer) *transport {
	return &transport{
		in:     in,
		out:    out,
		err:    errw,
		outQ:   make(chan []byte, outQueueDepth),
		closed: make(chan struct{}),
	}
}

// start launches the serialized writer goroutine. Must be called once before any
// send(); Run starts it as part of bootstrapping the loop.
func (t *transport) start() {
	go t.writerLoop()
}

// writerLoop is the single stdout owner. It drains outQ in order, writing each
// frame as one NDJSON line and flushing immediately, so frames never interleave
// and the last event before exit is delivered. The first write failure trips
// onSendFail (parent gone) and the loop keeps draining (cheaply discarding) until
// Close so producers never block.
//
// It exits on t.closed, NOT on a closed outQ: outQ is deliberately never closed (a
// producer may be mid-enqueue on another goroutine, and closing it would risk a
// send-on-closed panic in send()). On t.closed it drains whatever is buffered for a
// best-effort final delivery, then returns.
func (t *transport) writerLoop() {
	for {
		select {
		case frame := <-t.outQ:
			t.writeFrame(frame)
		case <-t.closed:
			for {
				select {
				case frame := <-t.outQ:
					t.writeFrame(frame)
				default:
					return
				}
			}
		}
	}
}

// writeFrame writes one NDJSON frame under writeMu — serialized against the
// teardown sendSync direct-write so the two never interleave on t.out — and trips
// onSendFail once on the first write error.
func (t *transport) writeFrame(frame []byte) {
	t.writeMu.Lock()
	if t.sendFailed {
		t.writeMu.Unlock()
		return
	}
	if _, werr := t.out.Write(append(frame, '\n')); werr != nil {
		t.sendFailed = true
		t.writeMu.Unlock()
		t.failOnce.Do(func() {
			if t.onSendFail != nil {
				// Off-goroutine: the hook only signals cancellation; it must not
				// re-enter send (which would deadlock on this single writer).
				go t.onSendFail(werr)
			}
		})
		return
	}
	// bufio.Writer needs an explicit Flush(); an *os.File write is already
	// unbuffered. Detect Flush() first (bufio), fall back to Sync() (file-level
	// best-effort). Either error is best-effort — the frame already left Write.
	flushWriter(t.out)
	t.writeMu.Unlock()
}

// flushWriter flushes a buffered writer (bufio.Writer.Flush) or syncs a file
// (os.File.Sync). bufio.Writer implements Flush() but NOT Sync(), so a Sync-only
// check would silently leave a bufio-wrapped writer unflushed per event.
func flushWriter(w io.Writer) {
	if f, ok := w.(interface{ Flush() error }); ok {
		_ = f.Flush()
		return
	}
	if f, ok := w.(interface{ Sync() error }); ok {
		_ = f.Sync()
	}
}

// commands starts the stdin reader goroutine and returns a channel of inbound
// lines. The reader uses a bufio.Scanner with a raised buffer; an oversize line or
// any read error is delivered as a terminal inbound{err} then the channel closes.
// The reader unblocks on Close(): once closed it stops forwarding (a blocked Scan
// on a live pipe can't be force-aborted, but the select drops the line and exits),
// so no goroutine leaks on a non-os.Exit teardown.
func (t *transport) commands() <-chan inbound {
	ch := make(chan inbound)
	go func() {
		defer close(ch)
		scanner := bufio.NewScanner(t.in)
		scanner.Buffer(make([]byte, 0, 64*1024), maxFrameBytes)
		for scanner.Scan() {
			// Copy: Scanner reuses its buffer between Scan() calls.
			b := scanner.Bytes()
			line := make([]byte, len(b))
			copy(line, b)
			select {
			case ch <- inbound{line: line}:
			case <-t.closed:
				return
			}
		}
		var term inbound
		if err := scanner.Err(); err != nil {
			term = inbound{err: err}
		} else {
			// Clean EOF: the parent closed stdin → treat as a terminal signal.
			term = inbound{err: io.EOF}
		}
		select {
		case ch <- term:
		case <-t.closed:
		}
	}()
	return ch
}

// send marshals + enqueues one event as a single NDJSON frame on the serialized
// writer queue. The enqueue is NON-BLOCKING: a producer (the command loop
// resolving an approval, a worker goroutine, the bridge) never blocks on a slow or
// wedged stdout — that is the structural property that keeps decide/interrupt from
// stalling the loop. A marshal error or oversize frame is dropped to stderr (never
// stdout). A full queue / closed transport drops the frame.
func (t *transport) send(sessionID string, ev HostEvent) {
	data, err := ev.encode(sessionID)
	if err != nil {
		t.diag(fmt.Sprintf("host: failed to encode event: %v", err))
		return
	}
	if len(data)+1 > maxFrameBytes {
		t.diag(fmt.Sprintf("host: refusing oversize outbound frame (%d bytes)", len(data)))
		return
	}
	select {
	case <-t.closed:
		// Transport closed (teardown in progress): drop. The teardown path emits its
		// own final frames synchronously before Close.
		return
	default:
	}
	select {
	case t.outQ <- data:
	case <-t.closed:
	default:
		// Queue full: stdout is wedged. Dropping avoids parking the loop; onSendFail
		// (tripped on the wedged write) tears down.
		t.diag("host: dropping outbound frame (writer queue full)")
	}
}

// sendSync writes one event directly, bypassing the queue and blocking until the
// write completes. Used ONLY by the teardown path for the final host:shutdown so
// the reason reaches Daintree even though Close() will immediately stop the writer
// goroutine (a queued frame could be lost in the close race). Safe to call from
// the single teardown goroutine.
func (t *transport) sendSync(sessionID string, ev HostEvent) {
	data, err := ev.encode(sessionID)
	if err != nil {
		t.diag(fmt.Sprintf("host: failed to encode event: %v", err))
		return
	}
	if len(data)+1 > maxFrameBytes {
		t.diag(fmt.Sprintf("host: refusing oversize outbound frame (%d bytes)", len(data)))
		return
	}
	// Take writeMu so this direct write can't interleave with a writerLoop frame
	// still draining from outQ (teardown calls sendSync before Close()).
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if t.sendFailed {
		return
	}
	if _, werr := t.out.Write(append(data, '\n')); werr != nil {
		t.sendFailed = true
		return
	}
	flushWriter(t.out)
}

// Close signals the reader to stop and shuts the writer queue. Idempotent. After
// Close, send() is a no-op (drops). The teardown path calls Close after its final
// synchronous shutdown frame.
//
// If the input is an io.Closer (an os.Pipe / io.PipeReader / *os.File), Close also
// closes it so a reader goroutine blocked in Read/Scan unblocks immediately — that
// is what actually prevents the goroutine leak on a non-os.Exit shutdown (closing
// t.closed alone can't interrupt a blocked Read; only closing the source can).
func (t *transport) Close() {
	t.closeOnce.Do(func() {
		close(t.closed)
		// outQ is deliberately NOT closed: a producer (send) may be mid-enqueue on
		// another goroutine, and closing it would risk a send-on-closed-channel panic.
		// writerLoop drains the buffer and exits on t.closed instead.
		if c, ok := t.in.(io.Closer); ok {
			_ = c.Close()
		}
	})
}

// diag writes a human-readable diagnostic line to stderr. Never stdout.
func (t *transport) diag(msg string) {
	if t.err == nil {
		return
	}
	t.emu.Lock()
	defer t.emu.Unlock()
	_, _ = io.WriteString(t.err, msg+"\n")
}

// contextCloser cancels Close when ctx is done — used so a parent-context cancel
// (non-os.Exit shutdown) reliably unblocks the reader goroutine.
func (t *transport) closeOnContext(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			t.Close()
		case <-t.closed:
		}
	}()
}
