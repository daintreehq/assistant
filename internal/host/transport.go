package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// maxFrameBytes caps a single inbound line and a single outbound event frame.
// A frame larger than this is a protocol violation (or a hostile peer); we refuse
// it rather than buffer unbounded. 4 MiB comfortably exceeds any legitimate
// prompt/command while bounding memory.
const maxFrameBytes = 4 << 20 // 4 MiB

// outQueueDepth bounds the serialized writer queue. Events are tiny and the writer
// drains continuously, so a deep queue absorbs any burst without a producer waiting.
const outQueueDepth = 4096

// streamHighWater is the backpressure threshold for STREAM events (tokens, tool
// progress) — the high-volume, low-priority traffic. A stream producer that finds
// this many frames already queued waits for the writer to catch up.
//
// Protocol v2 dropped a frame instead of waiting, which was the right trade for a
// TUI parent (a wedged stdout means the parent is gone) and the WRONG one for a
// rendered transcript: there is no sequence number to notice the hole with, and
// `turn:end` carried no authoritative text to repair it from, so one dropped token
// silently corrupted the conversation forever. v3 applies backpressure instead —
// the agent sink is reading an upstream SSE stream, so making it wait simply
// throttles that read, which is exactly the correct response to a slow consumer.
//
// CONTROL events (approval:decided, turn:end, host:error, host:shutdown, the
// command loop's own acks) never wait and never drop: they take the whole queue
// depth. That preserves the structural property v2 was protecting — decide and
// interrupt can never stall behind a token burst.
const streamHighWater = 2048

// inbound is one decoded inbound line plus a read error sentinel. err != nil
// signals EOF / read failure / oversize — the command loop treats it as a
// parent-exit and tears down.
type inbound struct {
	line []byte
	err  error
}

// writeRequest is one frame handed to the writer goroutine. terminal marks the
// final host:shutdown frame: the writer writes it (or gives up trying, if the
// transport has already failed) and then stops looping — nothing enqueued after a
// terminal request is ever read again, so nothing can follow it onto the wire. done,
// when set, is closed once the writer has resolved the request (written or
// abandoned), so a caller (sendSync) can wait for that outcome under its own bound
// instead of polling internal writer state.
type writeRequest struct {
	frame    []byte
	terminal bool
	done     chan struct{}
}

// transport owns stdio NDJSON framing. Reads: a line-reader goroutine over stdin
// feeding a channel, cancelable via Close()/context so a non-os.Exit shutdown does
// not leak it. Writes: a SINGLE dedicated writer goroutine is the ONLY goroutine
// that ever reads outQ or touches t.out — there is no second writer and therefore
// no lock needed to serialize against one. Producers (command loop, bridge,
// worker) never block on a slow/blocked stdout: enqueue is non-blocking (send) or
// bounded (sendStream, sendSync). Diagnostics go to a separate stderr writer;
// protocol JSON is never written to stderr.
//
// onSendFail is invoked (once) when a stdout write fails, or when a critical frame
// cannot even be queued: a broken/wedged stdout means the parent is gone, so the
// host cancels + tears down.
type transport struct {
	in  io.Reader
	out io.Writer
	err io.Writer

	// outQ carries write requests to the single writer goroutine, in the exact
	// order producers' enqueues complete. NEVER closed (producers send to it
	// concurrently); the writer stops reading it once it processes a terminal
	// request, or on t.closed if no terminal request ever arrives.
	outQ chan writeRequest
	// emu guards err writes (diag) so a producer-side diagnostic can't interleave
	// with a writer-goroutine diagnostic.
	emu sync.Mutex

	// closeOnce makes Close idempotent; closed signals the reader (and, absent a
	// terminal write request, the writer) to stop, and gates outQ producers so a
	// post-Close enqueue is a no-op (never a send on a closed channel panic).
	closeOnce sync.Once
	closed    chan struct{}
	// writerDone is closed once writerLoop returns, by whichever path — a
	// terminal request or draining out on t.closed. Nothing in production reads it
	// (the process exits regardless); it exists so a test can observe that the
	// writer goroutine actually retired instead of merely asserting on output bytes.
	writerDone chan struct{}

	// sendFailed latches once a write or flush has failed. Lock-free: it is read
	// by producers (sendStream's backpressure loop) that must never be able to
	// block on writer-owned state, which is exactly the bug a mutex-guarded bool
	// caused here before — a producer taking a lock the writer held across a
	// blocked Write() waited on that write, not on its own bounded budget.
	sendFailed atomic.Bool
	// sealed latches once teardown (sendSync) has begun, so a producer can refuse a
	// new enqueue immediately rather than adding traffic behind the terminal frame
	// that is about to be requested. It is a courtesy for producers, not what makes
	// the terminal frame actually final — the writer stopping after it does that.
	sealed atomic.Bool

	failOnce   sync.Once
	onSendFail func(error)
	// wedged is closed when a CRITICAL frame could not even be queued (the queue is
	// full and nothing is draining it), which for a consumer that has simply
	// stopped reading produces no write error to observe. See failSend.
	wedged     chan struct{}
	wedgedOnce sync.Once

	// seqMu covers BOTH the seq counter and the encode that stamps it, so the numbers
	// a consumer sees are strictly increasing in emission order. Stamping under one
	// lock is what lets Daintree treat a skipped number as proof of a lost frame — the
	// v3 replacement for v2's silent drops. Held only across an in-memory encode; the
	// enqueue happens after it is released, so a slow stdout never serializes producers.
	seqMu sync.Mutex
	seq   uint64

	// streamStalled latches once a stream producer has waited out the whole
	// backpressure budget, and clears as soon as the queue drains below the high-water
	// mark. While latched, stream frames are shed immediately instead of each paying
	// the budget again — otherwise one unresponsive consumer costs (frames × budget)
	// of cumulative delay, which is a hang wearing a bounded wait's clothes.
	stallMu       sync.Mutex
	streamStalled bool
}

// stampAndEnqueue assigns the next sequence number, encodes the frame, and puts it
// on the writer queue — ALL under one lock.
//
// The three steps are atomic together because splitting them reorders the stream. If
// seq is assigned during encode and the enqueue happens after the lock is released,
// two producers can interleave: A takes N, is descheduled, B takes N+1 and enqueues
// first. The consumer then sees N+1 before N, reports a lost frame that was never
// lost, and applies semantic events out of order. Holding the lock across the enqueue
// is cheap because the enqueue is non-blocking — a stream producer does its waiting
// BEFORE calling this, never while holding the lock.
//
// Returns false when the frame could not be queued (closed, or a full queue), having
// still consumed its sequence number: a frame that does not arrive must leave a
// visible hole rather than vanishing.
// enqueueResult says WHY a frame did not go out, because the three reasons call for
// three different responses and one boolean could not tell them apart — a 4 MiB notice
// was being reported as "the parent stopped reading" and cancelling a healthy session.
type enqueueResult int

const (
	enqueueOK enqueueResult = iota
	// enqueueClosed: teardown has begun (sealed) or already finished (closed). The
	// terminal frame goes out through sendSync; nothing else may follow it.
	enqueueClosed
	// enqueueUndeliverable: the queue is full. Nothing is draining it, which for a
	// consumer that has simply stopped reading produces no write error to observe.
	enqueueUndeliverable
	// enqueueUnencodable: this frame could not be built, or could not be made to fit.
	// That is a producer bug, not a statement about the consumer.
	enqueueUnencodable
)

func (t *transport) stampAndEnqueue(sessionID string, ev HostEvent) enqueueResult {
	t.seqMu.Lock()
	defer t.seqMu.Unlock()

	t.seq++
	data, err := encodeOrShrink(ev, sessionID, t.seq, t.diag)
	if err != nil {
		t.diag(fmt.Sprintf("host: failed to encode event: %v", err))
		return enqueueUnencodable
	}
	if data == nil {
		return enqueueUnencodable
	}
	select {
	case <-t.closed:
		return enqueueClosed
	default:
	}
	if t.sealed.Load() {
		return enqueueClosed
	}
	select {
	case t.outQ <- writeRequest{frame: data}:
		return enqueueOK
	default:
		return enqueueUndeliverable
	}
}

// encodeOrShrink encodes an event, shrinking its payload if the frame would exceed the
// cap. It returns nil data (and no error) only when the frame cannot be made to fit.
//
// THE CASE THIS EXISTS FOR is turn:end. A refused frame is simply not sent, and turn:end
// is the frame that says the turn is over — so a very long answer used to end with the
// host showing a turn that runs forever over a conversation that finished, with only a
// stderr line nobody was reading to say why. Cutting the answer is a visible, recoverable
// loss; never delivering the terminal frame is neither.
//
// An event with no way to shrink is still refused, which is correct: nothing else on this
// wire carries an unbounded field, so an oversize one is a bug in the producer rather
// than a large legitimate payload.
func encodeOrShrink(ev HostEvent, sessionID string, seq uint64, diag func(string)) ([]byte, error) {
	data, err := encodeSeq(ev, sessionID, seq)
	if err != nil {
		return nil, err
	}
	if len(data)+1 <= maxFrameBytes {
		return data, nil
	}
	sh, ok := ev.(shrinkable)
	if !ok {
		diag(fmt.Sprintf("host: refusing oversize outbound frame (%d bytes)", len(data)))
		return nil, nil
	}
	return shrinkToFit(sh, sessionID, seq, len(data), diag)
}

// shrinkToFit searches for the largest payload whose ENCODED frame fits the cap.
//
// A search rather than arithmetic, because the cap applies to encoded bytes while the
// budget can only be expressed in raw ones, and the ratio between them is not knowable in
// advance: a megabyte of ASCII encodes to about a megabyte, while a megabyte of control
// characters encodes to six. Computing a raw budget from the encoded overshoot therefore
// got it wrong in both directions — cutting an ASCII answer in half when it exceeded the
// cap only by its envelope, and still overflowing on heavily-escaped content, which put
// the terminal frame back to not being sent at all.
//
// Bounded at a handful of encodes: each halves the range, so a 4 MiB frame settles in
// well under twenty iterations, and this only ever runs on the exceptional oversize path.
func shrinkToFit(sh shrinkable, sessionID string, seq uint64, encodedLen int, diag func(string)) ([]byte, error) {
	// Start below the cap in RAW terms — encoding only ever grows a payload, never
	// shrinks it, so no fitting payload can be larger than the cap itself.
	lo, hi := 0, maxFrameBytes
	var best []byte
	for i := 0; i < 24 && lo <= hi; i++ {
		mid := (lo + hi) / 2
		candidate, did := sh.shrink(mid)
		if !did {
			// Nothing left to cut at this budget: the payload already fits it, so the
			// overflow is the envelope rather than the payload. Nothing larger can help.
			hi = mid - 1
			continue
		}
		encoded, err := encodeSeq(candidate, sessionID, seq)
		if err != nil {
			return nil, err
		}
		if len(encoded)+1 <= maxFrameBytes {
			best = encoded
			lo = mid + 1
			continue
		}
		hi = mid - 1
	}
	if best != nil {
		diag(fmt.Sprintf("host: outbound frame was %d bytes, past the %d cap; its content was cut to fit",
			encodedLen, maxFrameBytes))
		return best, nil
	}
	// LAST RESORT: emit the frame with no payload at all. It is a worse answer than a
	// truncated one and a far better answer than silence — turn:end is what tells the
	// host the turn is over, and a host that never receives it shows a turn running
	// forever over a conversation that finished.
	stripped, did := sh.shrink(0)
	if did {
		if encoded, err := encodeSeq(stripped, sessionID, seq); err == nil && len(encoded)+1 <= maxFrameBytes {
			diag(fmt.Sprintf("host: outbound frame was %d bytes and could not be cut to fit; "+
				"emitting it without content so the terminal frame still arrives", encodedLen))
			return encoded, nil
		}
	}
	diag(fmt.Sprintf("host: could not bring an oversize frame under the cap (%d bytes)", encodedLen))
	return nil, nil
}

// errControlFrameUndeliverable is why the session fails when a control frame cannot be
// queued: the consumer has stopped reading, which produces no write error to observe.
var errControlFrameUndeliverable = errors.New("a control frame could not be delivered; the parent stopped reading stdout")

// burnSeq consumes a sequence number without emitting anything, so a deliberately
// shed frame leaves a DETECTABLE hole. Shedding silently would be v2's behaviour,
// which is the thing v3 exists to stop.
func (t *transport) burnSeq() {
	t.seqMu.Lock()
	t.seq++
	t.seqMu.Unlock()
}

func newTransport(in io.Reader, out, errw io.Writer) *transport {
	return &transport{
		in:         in,
		out:        out,
		err:        errw,
		outQ:       make(chan writeRequest, outQueueDepth),
		closed:     make(chan struct{}),
		wedged:     make(chan struct{}),
		writerDone: make(chan struct{}),
	}
}

// start launches the serialized writer goroutine. Must be called once before any
// send(); Run starts it as part of bootstrapping the loop.
func (t *transport) start() {
	go t.writerLoop()
}

// writerLoop is the single stdout owner — the ONLY goroutine that ever reads outQ
// or writes t.out. It drains outQ in order, writing each frame as one NDJSON line
// and flushing immediately, so frames never interleave.
//
// It stops in exactly two ways: after processing a request marked terminal (the
// host:shutdown frame — nothing enqueued after it is ever read, which is what makes
// it structurally the last frame on the wire, not a race against a second writer),
// or on t.closed when no terminal request ever arrives (Close() called directly, as
// most tests and any non-graceful teardown do) — draining whatever is already
// buffered for a best-effort final delivery first.
//
// Because this goroutine is the sole consumer AND the sole writer, there is no
// dequeue-then-acquire-a-lock gap for a concurrent teardown to race: a frame is
// either still in the channel (and will be written before anything enqueued after
// it, including a later terminal request) or it has already been handed to
// t.out.Write — there is no third state in which it has been claimed but might
// still be silently dropped.
func (t *transport) writerLoop() {
	defer close(t.writerDone)
	for {
		select {
		case req := <-t.outQ:
			if t.deliver(req) {
				return
			}
		case <-t.closed:
			for {
				select {
				case req := <-t.outQ:
					if t.deliver(req) {
						return
					}
				default:
					return
				}
			}
		}
	}
}

// deliver writes one frame (unless the transport has already failed) and resolves
// the request's done channel, if any. It reports whether the writer must stop
// looping — true for a terminal request, regardless of whether the write itself
// succeeded, because nothing may ever be read after it either way.
func (t *transport) deliver(req writeRequest) (stop bool) {
	defer func() {
		if req.done != nil {
			close(req.done)
		}
	}()
	if !t.sendFailed.Load() {
		if _, werr := t.out.Write(append(req.frame, '\n')); werr != nil {
			t.fail(werr)
		} else if ferr := flushWriter(t.out); ferr != nil {
			// A buffered writer can accept bytes into memory and only fail once they are
			// actually sent downstream. Treating that as success let a critical frame
			// disappear with sendFailed still false and onSendFail never called.
			t.fail(ferr)
		}
	}
	return req.terminal
}

// fail latches the first write/flush failure and trips onSendFail exactly once.
func (t *transport) fail(err error) {
	t.sendFailed.Store(true)
	t.failOnce.Do(func() {
		if t.onSendFail != nil {
			// Off-goroutine: the hook only signals cancellation; it must not re-enter
			// send (which would deadlock on this single writer).
			go t.onSendFail(err)
		}
	})
}

// flushWriter flushes a BUFFERED writer (bufio.Writer.Flush), returning its error —
// a buffered writer can accept a Write into memory and only fail once the bytes are
// actually sent downstream, so a Flush error is exactly as fatal as a Write error.
//
// It deliberately does NOT call Sync() on a plain *os.File. Write on an unbuffered
// file/pipe already hands the bytes to the kernel with no in-process buffering, so
// there is nothing left to flush; fsync additionally asks for durable-storage
// persistence, a question this protocol never asks (production stdout is a live
// pipe to the parent process, not something either side reads back from disk), and
// is often simply unsupported for a pipe (EINVAL on Linux, similarly on Darwin).
// Calling it unconditionally would either fail every healthy session on its very
// first frame (if treated as fatal, the original form of this bug) or spam a
// diagnostic on literally every frame forever (if merely logged, since a pipe never
// stops erroring) — so it is not called at all.
func flushWriter(w io.Writer) error {
	if f, ok := w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
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

// send enqueues one CONTROL event. Non-blocking: the command loop resolving an
// approval, or an interrupt, must never stall behind a token burst — that is the
// structural property protocol v2 was protecting and v3 keeps. Stream producers stop
// at streamHighWater, leaving the rest of the queue depth as control headroom.
func (t *transport) send(sessionID string, ev HostEvent) {
	switch t.stampAndEnqueue(sessionID, ev) {
	case enqueueOK, enqueueClosed:
		// Delivered, or teardown is running/finished and the final frame goes out
		// through sendSync.
		return
	case enqueueUnencodable:
		// The frame could not be built or could not be made to fit. That is a producer
		// bug, and it says nothing about whether the consumer is alive — reporting it as
		// "the parent stopped reading" would cancel a healthy session over one bad
		// event. The sequence number was consumed, so the hole stays visible.
		t.diag("host: PROTOCOL GAP — an outbound frame could not be encoded within the cap and was dropped")
		return
	}

	// The queue is full: nothing is draining stdout.
	//
	// The documentation said control frames never drop. That was not implementable as
	// written — with a finite queue and an unread pipe, something has to give — but the
	// honest version is stronger than what this did, which was to log a line nobody
	// reads and carry on with a hole where a turn outcome or an approval request used to
	// be. A consumer that stopped reading produces no write error, so nothing else
	// notices: the session looks alive while every decision frame vanishes.
	//
	// So: a critical frame is never SILENTLY discarded — if one cannot be delivered, the
	// session fails. Telemetry is not worth a session, though, so only the frames a host
	// needs in order to make progress trigger that. See criticalFrame.
	if !criticalFrame(ev) {
		// The sequence number was already consumed by stampAndEnqueue, so the drop
		// leaves a detectable hole without anything further being needed here.
		t.diag("host: stream congested; dropped a non-critical frame (the sequence gap makes it visible)")
		return
	}
	t.diag("host: PROTOCOL GAP — a critical frame could not be delivered (writer queue full); " +
		"treating the parent as gone and tearing down")
	t.failSend(errControlFrameUndeliverable)
}

// criticalFrame reports whether losing this event would leave the host unable to make
// PROGRESS, as opposed to merely less informed.
//
// That distinction is the whole reason not every undeliverable frame ends the session. A
// host that misses a usage update renders a slightly stale meter. A host that misses
// turn:end waits forever on a turn that is over; one that misses approval:requested waits
// forever on a decision it was never asked for; one that misses command:result never
// learns whether `/clear` actually happened and can go on rendering a transcript the
// engine no longer has. Only this kind is worth ending a session over, and treating them
// alike meant a burst of optional telemetry against a briefly slow consumer could kill a
// healthy run.
func criticalFrame(ev HostEvent) bool {
	switch ev.(type) {
	case EvTurnEnd, EvApprovalRequested, EvApprovalDecided,
		EvQuestionRequested, EvQuestionAnswered, EvShutdown, EvError, EvReady,
		EvCommandResult:
		return true
	}
	return false
}

// failSend trips the parent-gone hook exactly once, from a producer rather than from a
// write error. Used when a control frame cannot be delivered — see send.
//
// It sets sendFailed too, same as fail(): a queue that never had room, or a
// priority frame the writer never confirmed within its budget, is exactly as dead
// as a write error from the perspective of anyone checking whether the transport
// is still good — most importantly a caller that just called sendSync and wants a
// SYNCHRONOUS answer to "did that actually go out" without waiting on the async
// onSendFail hook, which may not have run yet.
//
// It shares failOnce with the write-failure path (see fail), so a wedged consumer that
// later also produces a write error does not cancel twice. The hook runs on its own
// goroutine for the same reason fail's does: it only signals cancellation, and
// re-entering send from here would deadlock against the caller that is still inside it.
func (t *transport) failSend(reason error) {
	t.sendFailed.Store(true)
	t.wedgedOnce.Do(func() { close(t.wedged) })
	t.failOnce.Do(func() {
		if t.onSendFail != nil {
			go t.onSendFail(reason)
		}
	})
}

// isWedged reports whether the transport has given up on the consumer. Lock-free by
// design — see failSend.
func (t *transport) isWedged() bool {
	select {
	case <-t.wedged:
		return true
	default:
		return false
	}
}

// sendStream enqueues a high-volume STREAM frame, WAITING when the queue is above
// streamHighWater rather than dropping it. See streamHighWater for why v3 waits
// where v2 dropped. It returns as soon as the frame is queued, the transport
// closes, or the writer has already failed (parent gone) — never spins, and never
// takes a lock the writer goroutine might be holding across a blocked Write: the
// only writer-owned state it reads (sendFailed) is lock-free by construction, so a
// wedged stdout cannot stall this check before the backpressure budget even starts.
func (t *transport) sendStream(sessionID string, ev HostEvent) {
	// The WAIT happens before any sequence number is taken, so a frame that has to
	// queue behind others does not reserve a number and then hand it over out of
	// order. Only once there is room is the frame stamped and enqueued atomically.
	deadline := time.Now().Add(streamBackpressureBudget)
	for {
		select {
		case <-t.closed:
			return
		default:
		}

		// A FAILED writer never drains again; waiting would park the agent loop until
		// teardown. Give up at once — the parent is already gone.
		if t.sendFailed.Load() {
			return
		}

		// Already shedding: give up immediately rather than re-paying the budget.
		t.stallMu.Lock()
		stalled := t.streamStalled
		t.stallMu.Unlock()
		if stalled {
			if len(t.outQ) >= streamHighWater {
				t.burnSeq() // keep the loss visible
				return
			}
			t.stallMu.Lock()
			t.streamStalled = false
			t.stallMu.Unlock()
			t.diag("host: stream backpressure cleared; resuming")
		}

		if len(t.outQ) < streamHighWater {
			// Whatever the outcome, the sequence number was consumed by
			// stampAndEnqueue, so a lost race for the last slot leaves a detectable hole
			// rather than a silent one. A STREAM frame never fails the session — that is
			// the difference between this lane and send's.
			_ = t.stampAndEnqueue(sessionID, ev)
			return
		}

		// A WEDGED-but-alive pipe must not deadlock us: a consumer that stopped reading
		// produces no write error, so sendFailed never trips and there is nothing to
		// wait for. Bound the wait, then latch — paying the budget per frame would turn
		// one unresponsive consumer into (frames × budget) of accumulated delay, which
		// is a hang wearing a bounded wait's clothes.
		if time.Now().After(deadline) {
			t.stallMu.Lock()
			first := !t.streamStalled
			t.streamStalled = true
			t.stallMu.Unlock()
			if first {
				t.diag("host: PROTOCOL GAP — stream backpressure exceeded " +
					streamBackpressureBudget.String() + "; shedding stream frames until the consumer drains")
			}
			t.burnSeq()
			return
		}

		select {
		case <-t.closed:
			return
		case <-time.After(streamBackpressurePoll):
		}
	}
}

// streamBackpressurePoll is how long a stream producer waits before re-checking for
// queue room. Short enough to be invisible next to model latency, long enough that
// a genuinely wedged stdout costs a handful of wakeups rather than a spin.
const streamBackpressurePoll = 2 * time.Millisecond

// streamBackpressureBudget bounds how long a stream producer will wait for room
// before giving the frame up. It exists because a consumer that simply STOPS READING
// is indistinguishable from a slow one at the write boundary — there is no error to
// observe — so an unbounded wait would park the agent loop forever on a peer that is
// never coming back. Generous relative to any real stdout, short relative to a turn.
const streamBackpressureBudget = 5 * time.Second

// sendSync emits the FINAL frame of the session (host:shutdown) and guarantees it is
// last on the wire. See sendPriority — this is the terminal case.
func (t *transport) sendSync(sessionID string, ev HostEvent) {
	t.sendPriority(sessionID, ev, true)
}

// sendPriorityError writes a control frame synchronously and BEFORE anything queued
// after it, WITHOUT ending the session — used by reportSync for a fatal pre-app
// error that must reach the parent before the host:shutdown reason that follows it.
//
// Unlike sendSync it is not terminal: the writer keeps looping afterward, because a
// real sendSync (the actual host:shutdown) still needs to go through right after.
// Ordering between the two is free — both calls happen sequentially on the same
// goroutine, so the error's write request is enqueued strictly before shutdown's.
func (t *transport) sendPriorityError(sessionID string, ev HostEvent) {
	t.sendPriority(sessionID, ev, false)
}

// sendPriority stamps ev last (so its sequence number reflects emission order),
// seals the transport (new enqueues are refused from here on — see
// stampAndEnqueue) and hands it to the SAME queue every other frame goes through.
// Routing it through the queue rather than writing it directly is what removes the
// old two-consumer hazard: the writer goroutine is the only thing that ever writes
// t.out, so a frame already queued ahead of this one is guaranteed to be written
// first (FIFO). When terminal, the writer stops reading the instant it processes
// this request, so nothing enqueued after it can ever reach the wire even if a
// producer's enqueue happens to still be admitted in the same instant.
//
// The wait for delivery is bounded — a wedged stdout must not hold the caller open
// forever — but the frame is on the queue either way, so if the writer is merely
// slow (not stuck) it still gets written after this function gives up waiting.
func (t *transport) sendPriority(sessionID string, ev HostEvent, terminal bool) {
	t.sealed.Store(true)

	// A consumer already known gone: nothing to deliver, and no point spending the
	// budget finding that out again.
	if t.isWedged() || t.sendFailed.Load() {
		t.diag("host: stdout already failed or wedged; skipping a priority frame")
		return
	}

	t.seqMu.Lock()
	t.seq++
	data, err := encodeOrShrink(ev, sessionID, t.seq, t.diag)
	t.seqMu.Unlock()
	if err != nil {
		t.diag(fmt.Sprintf("host: failed to encode event: %v", err))
		// A producer-side encoding bug, not evidence the consumer is gone — this does
		// NOT call fail()/failSend() (which would trip onSendFail's "parent is gone,
		// cancel everything" cancellation, the wrong signal for a bug in our own
		// event). It still marks sendFailed: the priority frame was never delivered
		// either way, and a caller checking "did that go out" (teardown's exit-code
		// decision) must see the same honest answer regardless of WHY it didn't.
		t.sendFailed.Store(true)
		return
	}
	if data == nil {
		// Refused as unshrinkable — same reasoning as the encode-error case above.
		t.sendFailed.Store(true)
		return
	}

	done := make(chan struct{})
	req := writeRequest{frame: data, terminal: terminal, done: done}

	deadline := time.After(sealDrainBudget)
	select {
	case t.outQ <- req:
	case <-deadline:
		// The queue could not take it within the budget: the same "nothing is
		// draining stdout" condition send() treats as fatal for a critical frame,
		// and this is never anything less than critical. Diagnostic-only here would
		// let the caller (teardown) carry on and exit 0 believing host:shutdown went
		// out when it never even reached the queue.
		t.diag("host: PROTOCOL GAP — could not enqueue a priority frame within " + sealDrainBudget.String() +
			"; treating the parent as gone")
		t.failSend(errControlFrameUndeliverable)
		return
	}

	select {
	case <-done:
	case <-time.After(sealDrainBudget):
		// The frame is still on the queue and MIGHT still be written a moment later
		// by a writer that is merely slow rather than truly wedged — but from here
		// there is no way to tell the two apart, and the doc's own "never silently
		// discarded" contract for a critical frame doesn't get to make an exception
		// for "probably fine." Fail the same way an enqueue timeout does, so the
		// caller (teardown) doesn't proceed to exit(0) believing this succeeded.
		t.diag("host: PROTOCOL GAP — gave up waiting for the writer to deliver a priority frame within " +
			sealDrainBudget.String() + "; treating the parent as gone")
		t.failSend(errControlFrameUndeliverable)
	}
}

// sealDrainBudget bounds how long teardown waits — first to enqueue the final
// frame, then to have it actually written — before giving up so a wedged stdout
// cannot hold the process open.
const sealDrainBudget = 2 * time.Second

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

// closeOnContext unblocks a reader goroutine stuck in Scan() when ctx is
// cancelled, by closing the underlying input directly — NOT by calling the full
// Close(). Run's own handling of the same ctx.Done() always follows immediately
// with teardown() -> sendSync() -> Close(), in that order; calling Close() here too
// raced that sequence, since Close() marks t.closed, and an idle writer that had
// nothing left in outQ at that instant would retire on it before sendSync ever got
// a chance to enqueue the terminal frame — silently losing host:shutdown. The
// reader's own bounded exit (it selects on t.closed for its final send) still works
// once teardown's own Close() runs a moment later.
func (t *transport) closeOnContext(ctx context.Context) {
	go func() {
		select {
		case <-ctx.Done():
			if c, ok := t.in.(io.Closer); ok {
				_ = c.Close()
			}
		case <-t.closed:
		}
	}()
}
