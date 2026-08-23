package host

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/projectinstructions"
)

// hostState is the descriptor-handshake state machine. It starts await-descriptor
// (first inbound line must be the SessionDescriptor); after a valid descriptor it
// is running (subsequent lines are commands). Descriptors are not accepted twice.
type hostState int

const (
	stateAwaitDescriptor hostState = iota
	stateRunning
)

// Host owns the transport, the descriptor handshake, the command loop, the wake
// reactor, boot, and teardown. It is driven by Run(ctx). Process-level state
// (busy/ready/pendingWake/…) is owned by the single command-loop goroutine, EXCEPT
// the Bridge's own state (the agent loop calls the sink concurrently and uses its
// own mutex). The command loop MUST NOT block on a running Send — send runs on a
// worker goroutine while the loop keeps servicing interrupt/approval:decide (the
// key structural property of the event loop).
type Host struct {
	factory AppFactory
	tr      *transport

	exit func(code int) // injectable exit (flush-then-os.Exit); default flushExit

	// sessionID/state/ready/bridge/app are set during boot and then only read; the
	// command loop owns them. (boot runs on the loop goroutine before any worker.)
	sessionID string
	state     hostState
	ready     bool

	bridge *Bridge
	app    App

	// session is the turn engine the prompt/wake paths drive, narrowed to the
	// turnSession seam. Set once in boot (h.app.Session()) before any worker runs;
	// tests inject a cooperative fake to exercise the REAL loop wiring.
	session turnSession

	// turnMu guards every field touched by BOTH the command loop AND a worker
	// goroutine (prompt/wake run Send off-loop and finish off-loop): busy, closing,
	// turnCancel, wakeCancel, turnGen, pendingWake, wakeRetried, wakeSweepDone,
	// summarizedTerminals, and the turnWG.Add gate. The command loop must stay
	// non-blocking, so it only ever takes this short lock.
	turnMu              sync.Mutex
	busy                bool
	closing             bool // latched by teardown; no new prompt/wake worker may start
	turnCancel          context.CancelFunc
	wakeCancel          context.CancelFunc // aborts the in-flight WAKE turn (shutdown paths only)
	turnGen             uint64
	pendingWake         []domain.QueueEvent
	wakeRetried         bool
	wakeSweepDone       bool // teardown's durable re-arm sweep ran; late requeues go straight to rearmWakeEvents
	summarizedTerminals map[string]struct{}

	// turnWG tracks live prompt/wake worker goroutines. Add happens under turnMu
	// with `closing` checked (the supervisor wakeWG pattern), and teardown latches
	// `closing` under the same mutex before its bounded Wait — that mutual
	// exclusion is what makes the Add/Wait pair race-free, so app.Shutdown can
	// never close the store/MCP under a Send that is still unwinding.
	turnWG sync.WaitGroup

	// turnJoinTimeout bounds teardown's wait for in-flight turns to unwind after
	// their contexts are cancelled (a Send that ignores cancellation must not
	// wedge shutdown). Tests shrink it.
	turnJoinTimeout time.Duration

	// errorGuard phase: before host:ready a panic reports "bootstrap-error" + exit
	// 1; after ready the steady-state path reports "uncaught" + teardown error.
	guardActive bool

	runCtx    context.Context
	runCancel context.CancelFunc

	teardownOnce sync.Once
}

// NewHost builds a Host over the given App factory and stdio streams (in =
// command stdin, out = NDJSON stdout, errw = diagnostics stderr).
func NewHost(factory AppFactory, in io.Reader, out, errw io.Writer) *Host {
	return &Host{
		factory:             factory,
		tr:                  newTransport(in, out, errw),
		state:               stateAwaitDescriptor,
		summarizedTerminals: map[string]struct{}{},
		guardActive:         true,
		turnJoinTimeout:     defaultTurnJoinTimeout,
	}
}

// report emits a host:error ONLY when sessionId is set — a pre-descriptor crash
// names the empty session (a leaked event with empty sessionId would be dropped
// by Daintree's session match anyway).
func (h *Host) report(code, message string) {
	if h.sessionID == "" {
		// No session yet: still surface to stderr so the failure isn't silent.
		h.tr.diag(fmt.Sprintf("host: %s (no session): %s", code, message))
		return
	}
	h.post(EvError{Code: code, Message: message})
}

// reportSync emits a host:error like report, but SYNCHRONOUSLY and FLUSHED via the
// same direct write path teardown uses for the final host:shutdown. It exists for
// the FATAL PRE-APP paths (bad descriptor, protocol mismatch): there the host
// reports an error and immediately tears down before any App is running, so a
// queued (async) host:error would race the synchronous host:shutdown + process exit
// and could be dropped or reordered. Writing the error synchronously here guarantees
// the parent receives the SPECIFIC error first, then the shutdown reason. Do NOT use
// this on the steady-state path: there shutdown-first (so the reason escapes an App
// hang) is intentional.
func (h *Host) reportSync(code, message string) {
	if h.sessionID == "" {
		// No session yet: still surface to stderr so the failure isn't silent.
		h.tr.diag(fmt.Sprintf("host: %s (no session): %s", code, message))
		return
	}
	h.tr.sendSync(h.sessionID, EvError{Code: code, Message: message})
}

// post sends an event through the transport, stamping the current session id.
func (h *Host) post(ev HostEvent) { h.tr.send(h.sessionID, ev) }

// postStream is the backpressure lane for high-volume events (tokens, tool progress,
// phase beats). It WAITS for queue room instead of dropping the frame — see
// streamHighWater. Control events keep using post, which never waits, so an approval
// decision or an interrupt can never stall behind a token burst.
func (h *Host) postStream(ev HostEvent) { h.tr.sendStream(h.sessionID, ev) }

// Run is the entry point: install the stdout-fail hook, run the command loop, and
// (on a terminal inbound) tear down. It blocks until teardown calls exit. The
// caller wires os.Stdin/os.Stdout/os.Stderr.
func (h *Host) Run(parent context.Context) {
	h.runCtx, h.runCancel = context.WithCancel(parent)
	defer h.runCancel()

	if h.exit == nil {
		h.exit = func(code int) { flushExit(code) }
	}

	// A broken stdout means the parent is gone → cancel + teardown.
	h.tr.onSendFail = func(err error) {
		h.tr.diag(fmt.Sprintf("host: stdout write failed (parent gone?): %v", err))
		h.runCancel()
	}

	// Start the serialized writer goroutine, and make a parent-context cancel
	// unblock the stdin reader (no goroutine leak on a non-os.Exit shutdown).
	h.tr.start()
	h.tr.closeOnContext(h.runCtx)

	// Boot-phase panic guard: a panic during heavy init reports + exits 1 instead
	// of hanging the readiness wait. After host:ready, onFatal handles steady state.
	defer func() {
		if r := recover(); r != nil {
			h.onPanic(r)
		}
	}()

	commands := h.tr.commands()
	for {
		select {
		case <-h.runCtx.Done():
			// Parent exit / stdout failure → cancel any turn + teardown.
			h.cancelTurn()
			h.teardown(ShutdownExit, "")
			return
		case msg, ok := <-commands:
			if !ok {
				h.cancelTurn()
				h.teardown(ShutdownExit, "")
				return
			}
			if msg.err != nil {
				// EOF / read failure / oversize: parent closed the stream.
				h.cancelTurn()
				h.teardown(ShutdownExit, "")
				return
			}
			h.onLine(msg.line)
		}
	}
}

// onLine drives the descriptor/command state machine.
func (h *Host) onLine(line []byte) {
	switch h.state {
	case stateAwaitDescriptor:
		desc, err := ParseDescriptor(line)
		if err != nil {
			// First message wasn't a valid descriptor → bad-descriptor + teardown.
			// Fatal pre-app path: write the error SYNCHRONOUSLY + flushed so it
			// reliably reaches the parent BEFORE the synchronous host:shutdown (a
			// queued error would race the writer-close + process exit).
			h.sessionIDFromBadDescriptor(line)
			h.reportSync("bad-descriptor", fmt.Sprintf("invalid session descriptor: %v", err))
			h.teardown(ShutdownError, "")
			return
		}
		h.state = stateRunning
		h.boot(desc)
	case stateRunning:
		cmd, err := ParseCommand(line)
		if err != nil || cmd.SessionID != h.sessionID {
			// Foreign / garbled / wrong-session → silently DROP (no error event).
			return
		}
		h.handleCommand(cmd)
	}
}

// sessionIDFromBadDescriptor best-effort lifts a sessionId out of a malformed
// descriptor so the bad-descriptor error can be attributed; if absent the report
// falls back to stderr (empty session).
func (h *Host) sessionIDFromBadDescriptor(line []byte) {
	var probe struct {
		SessionID string `json:"sessionId"`
	}
	if json.Unmarshal(line, &probe) == nil && probe.SessionID != "" {
		h.sessionID = probe.SessionID
	}
}

// boot wires the App and announces readiness (TS boot()). Order is load-bearing.
func (h *Host) boot(desc SessionDescriptor) {
	h.sessionID = desc.SessionID

	if desc.ProtocolVersion != ProtocolVersion {
		// Fatal pre-app path (no App built yet): synchronous + flushed so the
		// specific protocol-mismatch error precedes the host:shutdown reason.
		h.reportSync("protocol-mismatch", fmt.Sprintf(
			"protocol version mismatch: host=%d descriptor=%d", ProtocolVersion, desc.ProtocolVersion))
		h.teardown(ShutdownError, "")
		return
	}

	// appSessionId = resumeSessionId ?? sessionId (resume replays conversation).
	appSessionID := desc.SessionID
	if desc.ResumeSessionID != "" {
		appSessionID = desc.ResumeSessionID
	}

	// DAINTREE.md: a warning goes to stderr; content is passed as an override.
	pi := projectinstructions.Load(desc.Cwd)
	if pi.Warning != "" {
		h.tr.diag("host: " + pi.Warning)
	}

	app, err := h.factory(h.runCtx, AppParams{
		SessionID:           appSessionID,
		ProjectPath:         desc.Cwd,
		ProjectInstructions: pi.Content,
	})
	if err != nil {
		// Fatal pre-app path (factory failed → no running App): synchronous +
		// flushed so the bootstrap-error precedes the host:shutdown reason.
		h.reportSync("bootstrap-error", fmt.Sprintf("failed to build app: %v", err))
		h.teardown(ShutdownError, "")
		return
	}
	h.app = app
	h.session = app.Session()

	// startDebugLog (no-op unless enabled). Best-effort; never fatal.
	cfg := app.Config()
	debuglog.StartDebugLog(debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir},
		map[string]any{"event": "session.start", "sessionId": appSessionID})

	// Bridge: maps the agent event stream + confirm hook → wire events.
	h.bridge = NewBridge(BridgeOptions{
		SessionID:  h.sessionID,
		Post:       h.post,
		PostStream: h.postStream,
		RiskOf:     app.RiskOf,
	})

	app.SetHooks(AppHooks{
		AgentEvents: h.bridge,
		Confirm: func(ctx context.Context, req ConfirmRequest) bool {
			return h.bridge.Confirm(ctx, req)
		},
		AskChoice: func(ctx context.Context, req AskChoiceRequest) (AskChoiceAnswer, error) {
			return h.bridge.AskChoice(ctx, req)
		},
	})

	// Best-effort MCP — a degraded MCP is NOT a boot failure.
	if err := app.ConnectMCP(h.runCtx); err != nil {
		h.tr.diag(fmt.Sprintf("host: MCP connect degraded: %v", err))
	}

	// Daemon: surfaced attention events feed actionable wakes. The callback runs on
	// a daemon goroutine; it must not touch loop-owned state directly, so it hands
	// the actionable burst to the loop via reactWake (which re-checks busy/ready).
	app.StartScheduler(func(events []domain.QueueEvent) {
		actionable := make([]domain.QueueEvent, 0, len(events))
		for _, e := range events {
			if agent.IsActionableWake(e) {
				actionable = append(actionable, e)
			}
		}
		if len(actionable) == 0 {
			return
		}
		h.turnMu.Lock()
		if len(h.pendingWake) == 0 {
			h.wakeRetried = false
		}
		h.pendingWake = append(h.pendingWake, actionable...)
		h.turnMu.Unlock()
		go h.reactWake()
	})

	// Hand off from the boot guard to the steady-state fatal path.
	h.guardActive = false
	h.ready = true
	ev := EvReady{
		ProtocolVersion: ProtocolVersion,
		Version:         BuildVersion,
		AutoApprove:     app.Config().AutoApprove,
	}
	if desc.ResumeSessionID != "" {
		ev.ResumedSessionID = desc.ResumeSessionID
	}
	h.post(ev)
}
