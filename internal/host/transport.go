package host

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
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
	// sealed stops writerLoop accepting frames once teardown has begun, so nothing
	// can be written after the terminal host:shutdown. writerBusy lets teardown wait
	// for an already-dequeued frame rather than racing it.
	sealed     bool
	writerBusy bool
	failOnce   sync.Once
	onSendFail func(error)
	// wedged is closed when the transport has concluded the consumer is gone. It is a
	// CHANNEL rather than a bool under writeMu because the one case it reports is a
	// writer blocked inside Write while holding writeMu — so anything that had to take
	// that lock to learn the state would block on exactly the stall it was checking for.
	// Teardown reads it before reaching for the lock.
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
	// enqueueClosed: teardown is running. The final frames go out through sendSync.
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
	select {
	case t.outQ <- data:
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
		in:     in,
		out:    out,
		err:    errw,
		outQ:   make(chan []byte, outQueueDepth),
		closed: make(chan struct{}),
		wedged: make(chan struct{}),
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
	// Sealed: teardown owns the stream from here, and nothing may follow the terminal
	// frame it is about to write.
	if t.sendFailed || t.sealed {
		t.writeMu.Unlock()
		return
	}
	// Published so teardown can wait for an in-flight write instead of racing it.
	//
	// The clear MUST re-take the lock. writeFrame releases writeMu before returning on
	// every path, so a bare `defer t.writerBusy = false` would write the field with no
	// lock held while sendSync reads it under one — a genuine data race, and one the
	// race detector catches immediately.
	t.writerBusy = true
	defer func() {
		t.writeMu.Lock()
		t.writerBusy = false
		t.writeMu.Unlock()
	}()
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

// send enqueues one CONTROL event. Non-blocking: the command loop resolving an
// approval, or an interrupt, must never stall behind a token burst — that is the
// structural property protocol v2 was protecting and v3 keeps. Stream producers stop
// at streamHighWater, leaving the rest of the queue depth as control headroom.
func (t *transport) send(sessionID string, ev HostEvent) {
	switch t.stampAndEnqueue(sessionID, ev) {
	case enqueueOK, enqueueClosed:
		// Delivered, or teardown is running and the final frames go out through sendSync.
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
// forever on a decision it was never asked for. Only the second kind is worth ending a
// session over, and treating them alike meant a burst of optional telemetry against a
// briefly slow consumer could kill a healthy run.
func criticalFrame(ev HostEvent) bool {
	switch ev.(type) {
	case EvTurnEnd, EvApprovalRequested, EvApprovalDecided,
		EvQuestionRequested, EvQuestionAnswered, EvShutdown, EvError, EvReady:
		return true
	}
	return false
}

// failSend trips the parent-gone hook exactly once, from a producer rather than from a
// write error. Used when a control frame cannot be delivered — see send.
//
// It shares failOnce with the write-error path, so a wedged consumer that later also
// produces a write error does not cancel twice. The hook runs on its own goroutine for
// the same reason the writer's does: it only signals cancellation, and re-entering send
// from here would deadlock against the caller that is still inside it.
func (t *transport) failSend(reason error) {
	// The latch is a CHANNEL, closed without a lock. The writer holds writeMu across its
	// Write, and the case this function exists for is a consumer that has stopped
	// reading — so the writer is blocked inside Write, holding writeMu, and touching
	// that lock here would deadlock the producer against the very stall it is reporting.
	//
	// Teardown then reads the latch before reaching for writeMu itself. Without it,
	// cancelling the host led straight into sendSync, which blocks on the same held
	// lock: the session would have failed by never exiting, which is the failure mode
	// the whole path exists to end.
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
// closes, or the writer has already failed (parent gone) — never spins.
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
		t.writeMu.Lock()
		failed := t.sendFailed
		t.writeMu.Unlock()
		if failed {
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
// last on the wire.
//
// "Last" is the whole contract, and bypassing the queue cannot provide it. The writer
// goroutine may already have dequeued a frame and be waiting on writeMu; a direct
// write that merely takes the lock first would be followed by that frame, putting an
// ordinary event AFTER a terminal one. A consumer that correctly stops reading at
// shutdown would then lose the tail of the turn.
//
// So teardown SEALS the stream instead: it stops the writer from accepting anything
// new, waits for whatever is already in flight to finish, and only then writes. The
// wait is bounded — a wedged stdout must not hold teardown open forever — and the
// frame still carries its sequence number, so a consumer can tell a truncated
// shutdown from a clean one.
func (t *transport) sendSync(sessionID string, ev HostEvent) {
	// A WEDGED consumer is checked FIRST, before any lock. The writer holds writeMu
	// across its Write, so when stdout has stopped draining that lock is held by a
	// goroutine that is not coming back — and teardown, whose whole job is to exit,
	// would block here forever. There is nothing to write to a consumer that is not
	// reading; skipping the frame is what lets the process actually shut down.
	if t.isWedged() {
		t.diag("host: stdout is wedged; skipping the final frame so teardown can complete")
		return
	}
	// Seal first: writeFrame refuses new frames from here on, so nothing can slip in
	// behind the shutdown after the drain below.
	//
	// The lock is acquired under a DEADLINE, not unconditionally. writeFrame holds it
	// across t.out.Write, so a consumer that stopped reading between the wedged check
	// above and this line leaves it held by a goroutine that will not return — and
	// teardown, whose entire purpose is to exit, would wait on it forever. "Bounded
	// teardown" has to include the lock acquisitions, or it is bounded only in the case
	// where nothing was wrong.
	if !t.lockWriteBefore(time.Now().Add(sealDrainBudget)) {
		t.diag("host: could not seal the stream within " + sealDrainBudget.String() +
			"; stdout is wedged, skipping the final frame so teardown can complete")
		t.failSend(errControlFrameUndeliverable)
		return
	}
	t.sealed = true
	t.writeMu.Unlock()

	// Wait for the writer to finish any frame it had already dequeued.
	deadline := time.Now().Add(sealDrainBudget)
	for {
		if t.isWedged() {
			return
		}
		t.writeMu.Lock()
		busy := t.writerBusy
		t.writeMu.Unlock()
		if !busy || time.Now().After(deadline) {
			break
		}
		time.Sleep(streamBackpressurePoll)
	}

	if !t.lockWriteBefore(time.Now().Add(sealDrainBudget)) {
		t.diag("host: stdout wedged while writing the final frame; teardown continues")
		return
	}
	defer t.writeMu.Unlock()
	if t.sendFailed {
		return
	}

	// Flush what the queue still holds, in order, so the tail of the turn precedes
	// the terminal frame rather than being discarded with it.
	t.drainQueuedLocked(sealDrainBudget)
	if t.sendFailed {
		return
	}

	// Stamped last, so its sequence number is genuinely the highest of the session.
	t.seqMu.Lock()
	t.seq++
	data, err := encodeOrShrink(ev, sessionID, t.seq, t.diag)
	t.seqMu.Unlock()
	if err != nil {
		t.diag(fmt.Sprintf("host: failed to encode event: %v", err))
		return
	}
	if data == nil {
		return
	}
	if _, werr := t.out.Write(append(data, '\n')); werr != nil {
		t.sendFailed = true
		return
	}
	flushWriter(t.out)
}

// lockWriteBefore acquires writeMu, giving up at the deadline. It reports whether the
// lock was taken.
//
// TryLock in a poll rather than a plain Lock, because the holder may be blocked inside
// t.out.Write on a consumer that has stopped reading — an unbounded wait there is a
// process that never exits. Only teardown uses it; ordinary writers still block, which is
// correct for them.
func (t *transport) lockWriteBefore(deadline time.Time) bool {
	for {
		if t.writeMu.TryLock() {
			return true
		}
		if t.isWedged() || time.Now().After(deadline) {
			return false
		}
		time.Sleep(streamBackpressurePoll)
	}
}

// sealDrainBudget bounds how long teardown waits for the writer to go idle and the
// queue to flush before emitting the terminal frame.
const sealDrainBudget = 2 * time.Second

// drainQueuedLocked writes every frame currently buffered in outQ, in order. The
// caller MUST hold writeMu (it writes t.out directly, exactly as writerLoop does).
//
// It races writerLoop for frames, which is harmless: both take from the same channel
// and both write under the same mutex, so each frame is written exactly once and in
// queue order. It stops at the budget, on a write failure, or when the queue is empty.
func (t *transport) drainQueuedLocked(budget time.Duration) {
	deadline := time.Now().Add(budget)
	for {
		select {
		case frame := <-t.outQ:
			if t.sendFailed {
				return
			}
			if _, werr := t.out.Write(append(frame, '\n')); werr != nil {
				t.sendFailed = true
				return
			}
			flushWriter(t.out)
			if time.Now().After(deadline) {
				t.diag("host: shutdown barrier gave up draining the writer queue")
				return
			}
		default:
			return
		}
	}
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
