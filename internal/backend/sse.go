package backend

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Stream-liveness and size bounds for the respond SSE stream.
const (
	// sseIdleTimeout is the rolling idle window for the respond stream: if NO bytes
	// arrive on the response body for this long (between events, mid-event, anywhere),
	// the attempt is aborted with a typed stream_idle_timeout error instead of hanging
	// a turn forever on a half-dead connection. The window resets on EVERY read
	// progress — including SSE comment lines, so it must comfortably exceed the
	// backend's heartbeat cadence once it starts emitting ": hb" comments every ~10s:
	// 90s tolerates several missed heartbeats before declaring the stream dead.
	sseIdleTimeout = 90 * time.Second
	// maxSSELineBytes bounds one SSE line. A single line carries at most one event's
	// JSON payload fragment; 1 MiB is far beyond anything the backend legitimately
	// emits, so exceeding it is a protocol error, not a big answer.
	maxSSELineBytes = 1 << 20 // 1 MiB
	// maxSSEEventBytes bounds one accumulated event payload (all its data lines).
	// Exceeding it aborts the stream with a typed protocol error rather than letting
	// a misbehaving peer grow the buffer without limit.
	maxSSEEventBytes = 4 << 20 // 4 MiB
)

// errSSELineTooLong is the internal sentinel readBoundedLine returns when a single
// line exceeds maxSSELineBytes; parseRespondStream converts it to a typed *Error.
var errSSELineTooLong = errors.New("sse line exceeds size bound")

// idleWatchdog aborts a stream attempt that makes no read progress within the
// window. It is armed at construction; Reset re-arms it on progress; once it fires
// (or after Stop) further resets are no-ops. The abort func runs on the timer
// goroutine, so it must be safe to call concurrently with the reader (context
// cancellation is).
//
// deadline guards against the timer-vs-Reset race: time.AfterFunc's callback can
// already be running (blocked on mu) when a concurrent Reset re-arms the timer,
// and Timer.Reset cannot recall it. Every Reset therefore also advances deadline
// under the mutex, and fire treats a callback that observes a still-future
// deadline as STALE — a no-op, never an abort of a stream that just made
// progress. The Reset that made it stale already re-armed the timer, so a
// genuine idle window still fires later.
type idleWatchdog struct {
	d        time.Duration
	abort    func()
	mu       sync.Mutex
	timer    *time.Timer
	deadline time.Time
	fired    bool
	done     bool
}

func newIdleWatchdog(d time.Duration, abort func()) *idleWatchdog {
	w := &idleWatchdog{d: d, abort: abort, deadline: time.Now().Add(d)}
	w.timer = time.AfterFunc(d, w.fire)
	return w
}

// fire is the timer callback. It aborts only when the CURRENT deadline has
// genuinely elapsed; a stale fire (a Reset raced past the timer's expiry) is a
// no-op because that Reset already re-armed the timer for the new deadline.
func (w *idleWatchdog) fire() {
	w.mu.Lock()
	if w.done || w.fired || time.Now().Before(w.deadline) {
		w.mu.Unlock()
		return
	}
	w.fired = true
	w.mu.Unlock()
	w.abort()
}

// Reset re-arms the window after read progress. No-op once fired or stopped.
func (w *idleWatchdog) Reset() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fired || w.done {
		return
	}
	w.deadline = time.Now().Add(w.d)
	w.timer.Reset(w.d)
}

// Fired reports whether the idle window elapsed (i.e. the abort was triggered).
func (w *idleWatchdog) Fired() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.fired
}

// Stop disarms the watchdog (stream finished, success or failure).
func (w *idleWatchdog) Stop() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.done = true
	w.timer.Stop()
}

// idleResetReader resets the idle watchdog on every read that makes progress —
// including bytes that are only SSE comments/heartbeats, which never reach the
// event dispatcher but still prove the connection is alive.
type idleResetReader struct {
	r     io.Reader
	reset func()
}

func (a *idleResetReader) Read(p []byte) (int, error) {
	n, err := a.r.Read(p)
	if n > 0 {
		a.reset()
	}
	return n, err
}

// readBoundedLine reads one '\n'-terminated line like bufio.Reader.ReadString,
// but fails with errSSELineTooLong once the line exceeds maxSSELineBytes instead
// of buffering without limit.
func readBoundedLine(r *bufio.Reader) ([]byte, error) {
	var b bytes.Buffer
	for {
		chunk, err := r.ReadSlice('\n')
		// Normal SSE lines fit in the reader buffer. Return that borrowed slice
		// directly; parseRespondStream consumes it before its next read.
		if b.Len() == 0 && err != bufio.ErrBufferFull {
			if len(chunk) > maxSSELineBytes {
				return nil, errSSELineTooLong
			}
			return chunk, err
		}
		_, _ = b.Write(chunk)
		if b.Len() > maxSSELineBytes {
			return nil, errSSELineTooLong
		}
		if err == bufio.ErrBufferFull {
			continue // line spans buffer chunks; keep accumulating under the bound
		}
		return b.Bytes(), err
	}
}

// StreamCallbacks receives streamed events as they arrive. All are optional. The
// final assembled message + usage are returned by the stream parser regardless;
// these callbacks exist for live UI (token streaming, surfacing newly-loaded
// skills up front). They are invoked synchronously on the reader goroutine.
type StreamCallbacks struct {
	// OnRawMeta fires immediately when each HTTP attempt's SSE meta event is
	// decoded. Unlike OnMeta, it is a transport-observation hook: retries can make
	// it fire more than once for one RespondStream call, and callers must not use it
	// to adopt state or perform visible side effects. It exists so diagnostics can
	// distinguish actual meta arrival from the retry-safe committed OnMeta callback.
	OnRawMeta func(StreamMeta)
	// OnMeta carries the refreshed state token, the skills outcome, and version
	// markers. Client.RespondStream defers it until the attempt commits so retries
	// cannot duplicate stateful side effects.
	OnMeta func(StreamMeta)
	// OnSkillLoaded fires as soon as a meta event reports newly-loaded skills,
	// before the upstream model needs to produce content. Client.RespondStream
	// de-duplicates identical refs across retry attempts, so callers can use this
	// for immediate user feedback without producing duplicate cards.
	OnSkillLoaded func([]SkillRef)
	// OnContent fires for each visible content fragment, in order.
	OnContent func(string)
	// OnReasoning fires for each chain-of-thought fragment (DeepSeek thinking mode),
	// in order, before the first content fragment. Optional; the parser accumulates
	// reasoning into the final message regardless. Empty stream when thinking is off.
	OnReasoning func(string)
	// OnStatus fires for each `status` event — once, with phase "thinking", the
	// instant chain-of-thought begins. Optional; never fires when thinking is off.
	OnStatus func(StreamStatus)
	// OnToolCallDelta fires for each raw tool-call fragment (optional; the parser
	// accumulates these internally regardless).
	//
	// REPLAY CONTRACT: RespondStream retries transient failures that occur before any
	// content streams, and a failed attempt may already have emitted tool-call
	// fragments — so this callback can fire for fragments from an attempt that is then
	// discarded and replayed. Treat the RETURNED RespondResult.Message.ToolCalls (built
	// from the final attempt's own fresh accumulator) as authoritative; do NOT execute
	// or accumulate tool calls off these raw fragments across the call.
	OnToolCallDelta func(ToolCallDelta)
	// OnRetry fires just before each backoff sleep when Client.RespondStream is about
	// to replay a transient failure. It exists so the caller can show a live cue: the
	// retry budget can now span a minute of wall clock, and an unexplained spinner is
	// indistinguishable from a hang. Observational only — it must not block, and it
	// never fires from the stream parser (only from the retry loop above it).
	OnRetry func(RetryInfo)
}

type sseEventKind uint8

const (
	sseEventNone sseEventKind = iota
	sseEventUnknown
	sseEventMeta
	sseEventStatus
	sseEventDelta
	sseEventDone
	sseEventError
)

func parseSSEEventKind(name []byte) sseEventKind {
	switch string(bytes.TrimSpace(name)) {
	case "meta":
		return sseEventMeta
	case "status":
		return sseEventStatus
	case "delta":
		return sseEventDelta
	case "done":
		return sseEventDone
	case "error":
		return sseEventError
	case "":
		return sseEventNone
	default:
		return sseEventUnknown
	}
}

// parseRespondStream reads a named-event SSE stream (meta / delta / done / error)
// and returns the accumulated RespondResult. Guarantees enforced:
//   - meta must arrive (a stream that ends without it is an error);
//   - done terminates successfully; a terminal error event terminates with that
//     error; EOF before done is an "interrupted" error — the parser NEVER
//     fabricates a successful finish the backend did not send;
//   - tool-call delta fragments are accumulated by index into whole tool calls;
//   - lines and event payloads are size-bounded (maxSSELineBytes / maxSSEEventBytes);
//     exceeding either aborts with a typed protocol error rather than buffering
//     without limit.
//
// There is no OpenAI choices envelope and no [DONE] sentinel. Idle-liveness is NOT
// enforced here — respondStreamOnce wraps r in an idleResetReader tied to a watchdog
// that cancels the request when the connection goes silent.
func parseRespondStream(r io.Reader, cb StreamCallbacks) (RespondResult, error) {
	reader := bufio.NewReaderSize(r, 64*1024)

	var (
		result    RespondResult
		eventKind sseEventKind
		// eventData accumulates the event's data lines, newline-joined AS THEY
		// ARRIVE, so its Len() — which the maxSSEEventBytes cap below reads —
		// counts every buffered byte including the separators. A per-line []string
		// buffer measured by summed payload length would let a flood of EMPTY
		// `data:` lines grow slice/header memory without ever moving the counter.
		eventData bytes.Buffer
		dataSeen  bool // one or more data lines buffered (an empty line still counts)
		content   strings.Builder
		reasoning strings.Builder
		acc       toolCallAccumulator
		metaSeen  bool
		doneSeen  bool
	)

	// dispatch decodes and applies one fully-buffered event, then clears the
	// buffer. A terminal `error` event returns a non-nil *Error.
	dispatch := func() error {
		kind := eventKind
		data := eventData.Bytes()
		eventKind = sseEventNone
		eventData.Reset()
		dataSeen = false
		if kind == sseEventNone && len(bytes.TrimSpace(data)) == 0 {
			return nil
		}
		switch kind {
		case sseEventMeta:
			var m StreamMeta
			if err := json.Unmarshal(data, &m); err != nil {
				return decodeErr("meta", err)
			}
			result.Meta = m
			metaSeen = true
			if cb.OnRawMeta != nil {
				cb.OnRawMeta(m)
			}
			if cb.OnSkillLoaded != nil && len(m.Skills.NewlyLoaded) > 0 {
				cb.OnSkillLoaded(m.Skills.NewlyLoaded)
			}
			if cb.OnMeta != nil {
				cb.OnMeta(m)
			}
		case sseEventStatus:
			// Optional thinking-phase marker (DeepSeek thinking mode). Decode for the
			// UX hook; a malformed/unknown status is ignored, never an error.
			var st StreamStatus
			if json.Unmarshal(data, &st) == nil && cb.OnStatus != nil {
				cb.OnStatus(st)
			}
		case sseEventDelta:
			var d StreamDelta
			if err := json.Unmarshal(data, &d); err != nil {
				return decodeErr("delta", err)
			}
			if d.ReasoningContent != "" {
				reasoning.WriteString(d.ReasoningContent)
				if cb.OnReasoning != nil {
					cb.OnReasoning(d.ReasoningContent)
				}
			}
			if d.Content != "" {
				content.WriteString(d.Content)
				if cb.OnContent != nil {
					cb.OnContent(d.Content)
				}
			}
			for _, tc := range d.ToolCalls {
				acc.add(tc)
				if cb.OnToolCallDelta != nil {
					cb.OnToolCallDelta(tc)
				}
			}
		case sseEventDone:
			var dn StreamDone
			if err := json.Unmarshal(data, &dn); err != nil {
				return decodeErr("done", err)
			}
			result.FinishReason = dn.FinishReason
			result.Usage = dn.Usage
			doneSeen = true
		case sseEventError:
			var env Envelope
			// Best-effort decode; a malformed error payload still terminates the
			// stream with a generic error rather than a fake success.
			_ = json.Unmarshal(data, &env)
			if env.Error.Message == "" && env.Error.Code == "" {
				env.Error.Code = "stream_error"
				env.Error.Message = "backend stream emitted an error event"
			}
			return newError(0, env, parseRetryAfter(env.RetryAfter), true)
		default:
			// Unknown event name: ignore for forward compatibility.
		}
		return nil
	}

	for {
		line, err := readBoundedLine(reader)
		if errors.Is(err, errSSELineTooLong) {
			return result, &Error{
				Code:    "stream_line_too_large",
				Message: fmt.Sprintf("SSE line exceeded %d bytes", maxSSELineBytes),
				Stream:  true,
			}
		}
		if len(line) > 0 {
			trimmed := bytes.TrimRight(line, "\r\n")
			switch {
			case len(trimmed) == 0:
				if derr := dispatch(); derr != nil {
					return result, derr
				}
				// `done` is terminal: stop reading immediately rather than blocking on
				// the next read for an EOF the server may never send promptly (keep-alive /
				// proxy buffering would otherwise hang the whole turn).
				if doneSeen {
					goto finish
				}
			case bytes.HasPrefix(trimmed, []byte(":")):
				// SSE comment line (e.g. the backend's ": hb" heartbeat): carries no
				// event data and is ignored here. Its BYTES still reset the idle
				// watchdog upstream (idleResetReader sees every read), which is the
				// whole point of the heartbeat.
			case bytes.HasPrefix(trimmed, []byte("event:")):
				eventKind = parseSSEEventKind(trimmed[len("event:"):])
			case bytes.HasPrefix(trimmed, []byte("data:")):
				d := bytes.TrimPrefix(trimmed[len("data:"):], []byte(" "))
				if dataSeen {
					eventData.WriteByte('\n')
				}
				_, _ = eventData.Write(d)
				dataSeen = true
				// The cap reads the BUFFERED size (payload + joining newlines), so
				// even a stream of millions of empty data lines advances it — one
				// byte per line — and aborts here instead of growing without bound.
				if eventData.Len() > maxSSEEventBytes {
					return result, &Error{
						Code:    "stream_event_too_large",
						Message: fmt.Sprintf("SSE event payload exceeded %d bytes", maxSSEEventBytes),
						Stream:  true,
					}
				}
			}
		}
		if err != nil {
			// Flush a trailing event that had no terminating blank line.
			if eventKind != sseEventNone || dataSeen {
				if derr := dispatch(); derr != nil {
					return result, derr
				}
			}
			if err == io.EOF {
				break
			}
			return result, &Error{Code: "stream_read", Message: "stream read failed: " + err.Error(), Stream: true}
		}
	}

finish:
	result.Message = RespondMessage{
		Role:             "assistant",
		Content:          content.String(),
		ReasoningContent: reasoning.String(),
		ToolCalls:        acc.build(),
	}

	if !metaSeen {
		return result, &Error{Code: "stream_no_meta", Message: "stream ended without a meta event", Stream: true}
	}
	if !doneSeen {
		// A truncated upstream stream is an error, never a silent success.
		return result, &Error{Code: "stream_interrupted", Message: "stream ended before a done event", Stream: true}
	}
	return result, nil
}

func decodeErr(event string, err error) *Error {
	return &Error{
		Code:    "stream_decode",
		Message: fmt.Sprintf("could not decode %s event: %v", event, err),
		Stream:  true,
	}
}

// --------------------------------------------------------------------------
// Tool-call delta accumulation (by index, OpenAI-style).
// --------------------------------------------------------------------------

type toolAccEntry struct {
	id   string
	typ  string
	name string
	args strings.Builder
}

type toolCallAccumulator struct {
	order   []int
	byIndex map[int]*toolAccEntry
}

// add folds one streamed fragment into the accumulator. Fragments for the same
// call share an index; id/name/type arrive once and argument text streams in
// pieces, so each non-empty field overwrites and arguments concatenate.
func (a *toolCallAccumulator) add(tc ToolCallDelta) {
	if a.byIndex == nil {
		a.byIndex = make(map[int]*toolAccEntry)
	}
	idx := 0
	if tc.Index != nil {
		idx = *tc.Index
	}
	e := a.byIndex[idx]
	if e == nil {
		e = &toolAccEntry{}
		a.byIndex[idx] = e
		a.order = append(a.order, idx)
	}
	if tc.ID != "" {
		e.id = tc.ID
	}
	if tc.Type != "" {
		e.typ = tc.Type
	}
	if tc.Function.Name != "" {
		e.name = tc.Function.Name
	}
	if tc.Function.Arguments != "" {
		e.args.WriteString(tc.Function.Arguments)
	}
}

// build materializes the accumulated fragments into whole tool calls, sorted by
// index. Nameless entries are dropped; a missing id is synthesized stably from the
// index (so the assistant message and its tool-result messages still pair); empty
// arguments default to "{}" (a parameterless call).
func (a *toolCallAccumulator) build() []ToolCall {
	if len(a.order) == 0 {
		return nil
	}
	order := append([]int(nil), a.order...)
	sort.Ints(order)
	out := make([]ToolCall, 0, len(order))
	for _, idx := range order {
		e := a.byIndex[idx]
		if e.name == "" {
			continue
		}
		id := e.id
		if id == "" {
			id = fmt.Sprintf("call_%d", idx)
		}
		args := e.args.String()
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out = append(out, ToolCall{ID: id, Type: "function", Function: FunctionCall{Name: e.name, Arguments: args}})
	}
	return out
}
