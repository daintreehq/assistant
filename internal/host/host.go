package host

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/ipc"
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
	// turnCancel, wakeCancel, turnGen, pendingWake, wakeRetries, wakeSweepDone,
	// summarizedTerminals, and the turnWG.Add gate. The command loop must stay
	// non-blocking, so it only ever takes this short lock.
	turnMu     sync.Mutex
	busy       bool
	closing    bool // latched by teardown; no new prompt/wake worker may start
	turnCancel context.CancelFunc
	wakeCancel context.CancelFunc // aborts the in-flight WAKE turn (shutdown paths only)
	// cmdCancel aborts the in-flight SLOW command (see handleSlashCommandAsync), and
	// cmdBusy is what keeps there being only one.
	//
	// A slow command is the only work here that waits on a person rather than on a
	// model: `/login` blocks until a browser round trip completes or its five-minute
	// callback window expires, and `/backend` with no argument blocks on a picker the
	// user has yet to answer. Without a cancel, shutdown reaches its bounded join with
	// that wait still running and tears the App down underneath it; without the busy
	// flag, two of them overlap and settle in whichever order they finish —
	// a `/logout` sent after a `/login` can finish first and leave the session signed in.
	cmdCancel context.CancelFunc
	cmdBusy   bool
	// cmdExclusive marks that the in-flight slow command has taken the SESSION, not
	// merely the command lane — `/backend` with no argument reserves it while its picker
	// is open.
	//
	// Set on the loop, before the worker is scheduled, which is what makes it a real
	// gate: the loop is single-threaded, so a prompt arriving after the command was
	// dispatched cannot slip past by reaching the runtime before the worker does. Prompts
	// and other commands are refused while it is set; answers, approvals, interrupt and
	// shutdown are not, or the very sheet holding the session could never be settled.
	cmdExclusive bool
	// cmdPromptsReleased opens PROMPT admission again while cmdExclusive is still set.
	//
	// The two are released at different moments on purpose. Prompts have to be
	// admissible by the time `command:result` reaches the host, because that frame is
	// what a host uses to re-enable its composer — a prompt submitted on the strength of
	// it and then refused would be accepted by the transport, cleared from the draft and
	// never seen by the model. COMMANDS have the opposite requirement: another one
	// admitted in that same window posts its own `command:result` FIRST, so the host
	// renders the older command's outcome last and shows a state that is no longer live.
	// They stay blocked until this command's result is out.
	cmdPromptsReleased bool
	// deferredPrompts holds prompts that were ALREADY ACCEPTED and could not be
	// dispatched because a session-owning command had the session — see
	// dispatchReclaimedPrompt. Drained when that command releases it.
	deferredPrompts     []string
	turnGen             uint64
	pendingWake         []domain.QueueEvent
	wakeRetries         agent.RetryLedger
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
	// appShutdownTimeout bounds teardown's wait for App.Shutdown. Tests shrink it.
	appShutdownTimeout time.Duration

	// errorGuard phase: before host:ready a panic reports "bootstrap-error" + exit
	// 1; after ready the steady-state path reports "uncaught" + teardown error.
	guardActive bool

	runCtx    context.Context
	runCancel context.CancelFunc
	// transportFailed records that the outbound stream broke, so teardown can report
	// `error` rather than a clean `exit`.
	transportFailed atomic.Bool

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
		appShutdownTimeout:  defaultAppShutdownTimeout,
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

// reportSync emits a host:error like report, but SYNCHRONOUSLY: it waits (bounded)
// for the transport to actually deliver the frame, via the same priority path
// teardown uses for the final host:shutdown — just non-terminal, so the writer
// keeps running afterward. It exists for the FATAL PRE-APP paths (bad descriptor,
// protocol mismatch): there the host reports an error and immediately tears down
// before any App is running, so a queued (async) host:error would race the
// synchronous host:shutdown + process exit and could be dropped or reordered.
// Writing the error this way guarantees the parent receives the SPECIFIC error
// first, then the shutdown reason. Do NOT use this on the steady-state path: there
// shutdown-first (so the reason escapes an App hang) is intentional.
func (h *Host) reportSync(code, message string) {
	if h.sessionID == "" {
		// No session yet: still surface to stderr so the failure isn't silent.
		h.tr.diag(fmt.Sprintf("host: %s (no session): %s", code, message))
		return
	}
	h.tr.sendPriorityError(h.sessionID, EvError{Code: code, Message: message})
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
	//
	// It is latched as a TRANSPORT FAILURE, not an ordinary exit. A session ended
	// because a critical frame could not be delivered is a protocol failure, and
	// reporting it with reason "exit" and status 0 told anyone reading the outcome —
	// a supervisor, a log, a person — that everything went fine. It did not: frames
	// this process produced never reached the host.
	h.tr.onSendFail = func(err error) {
		h.tr.diag(fmt.Sprintf("host: stdout write failed (parent gone?): %v", err))
		h.transportFailed.Store(true)
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
			// Parent exit / stdout failure → cancel any turn + teardown. A transport
			// failure is reported as `error`, not `exit`: this is the one cancellation
			// path where the session did not end cleanly.
			h.cancelTurn()
			h.teardown(h.shutdownReason(), "")
			return
		case msg, ok := <-commands:
			if !ok {
				h.cancelTurn()
				h.teardown(ShutdownExit, "")
				return
			}
			if msg.err != nil {
				h.cancelTurn()
				h.teardownForReadError(msg.err)
				return
			}
			h.onLine(msg.line)
		}
	}
}

// shutdownReason maps the cancellation cause onto the wire vocabulary: a broken
// transport is an `error`, everything else is a normal `exit`.
func (h *Host) shutdownReason() HostShutdownReason {
	if h.transportFailed.Load() {
		return ShutdownError
	}
	return ShutdownExit
}

// teardownForReadError classifies why the stdin reader stopped and tears down with
// the matching reason — collapsing every cause into `exit` (as this used to)
// reported a protocol violation identically to the parent closing stdin on
// purpose, which made support reports useless: nothing distinguished "the session
// ended normally" from "the parent sent something we could not read."
func (h *Host) teardownForReadError(err error) {
	switch {
	case errors.Is(err, io.EOF):
		// Clean EOF: the parent closed stdin on purpose. An ordinary, expected exit.
		h.teardown(ShutdownExit, "")
	case errors.Is(err, bufio.ErrTooLong):
		// The line exceeded maxFrameBytes — a protocol violation (or a hostile
		// peer), not a session ending normally.
		h.report("bad-frame", fmt.Sprintf("inbound line exceeded the %d byte frame cap", maxFrameBytes))
		h.teardown(ShutdownError, "")
	case h.runCtx.Err() != nil:
		// This read error is a side effect of OUR OWN context cancellation —
		// closeOnContext closes stdin to unblock a stuck reader, which can make the
		// scanner surface an error (a closed-pipe shape) at roughly the same instant
		// Run()'s own select observes <-h.runCtx.Done(). Which branch wins is
		// scheduler luck, and the two must not disagree about the outcome: the
		// runCtx.Done() branch already decides via shutdownReason() (transportFailed
		// → error, otherwise a normal exit), so route through the identical decision
		// here instead of hardcoding `error` — the same real-world cancellation must
		// not report a different exit reason depending on which case happened to fire.
		h.teardown(h.shutdownReason(), "")
	default:
		// Any other read failure (broken pipe, closed fd, …) NOT caused by our own
		// cancellation: the transport did not end cleanly.
		h.tr.diag(fmt.Sprintf("host: stdin read error: %v", err))
		h.teardown(ShutdownError, "")
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
		// The descriptor's own claim about what this session is bound to, so the
		// factory can check it against the environment the runtime will actually use.
		// Two independent statements of the same fact are only worth carrying if they
		// are compared; otherwise there are two truths and no way to tell which is live.
		Declared: Binding{
			ProjectID: desc.ProjectID,
			WindowID:  strconv.FormatInt(desc.WindowID, 10),
			Tier:      desc.Tier,
			Cwd:       desc.Cwd,
		},
	})
	var mismatch *BindingMismatchError
	if errors.As(err, &mismatch) {
		// Its own code, not a generic bootstrap failure: this is the one startup error
		// whose cause is a disagreement between the two repositories, and a host that
		// cannot tell it from "the database would not open" has no way to know its own
		// descriptor is wrong.
		h.reportSync("binding-mismatch", mismatch.Error())
		h.teardown(ShutdownError, "")
		return
	}
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
		// Teardown has already latched `closing` and/or run its one durable
		// re-arm sweep (`wakeSweepDone`): a burst landing here after that point
		// would otherwise sit in pendingWake forever — reactWake bails immediately
		// on `closing`, and nothing runs a second sweep. The scheduler already
		// marked these events notified before invoking this callback, so without a
		// durable re-arm here they are lost across a process restart too, not just
		// this run. Mirrors reactWake's own post-join requeue-vs-rearm check.
		if h.closing || h.wakeSweepDone {
			h.turnMu.Unlock()
			h.rearmWakeEvents(actionable)
			return
		}
		if len(h.pendingWake) == 0 {
			clear(h.wakeRetries)
		}
		// The scheduler marks a burst notified only AFTER this callback returns, and
		// discards the error if that fails — so the same event can arrive twice while
		// the first copy is still queued or in flight. A duplicated scheduled message
		// is the instruction carried out twice.
		h.pendingWake = append(h.pendingWake, agent.DedupeWakeEvents(actionable, h.pendingWake)...)
		h.turnMu.Unlock()
		go h.reactWake()
	}, func(timerID string) {
		// A fired timer is not a wake and never becomes one: the assistant is not
		// prompted, the host is simply told the schedule moved so it can re-read.
		// Posted directly — the bridge guards its own state, and this runs on a
		// scheduler goroutine that must not block.
		h.post(EvTimerFired{TimerID: timerID, FiredAt: domain.NowMS()})
	})

	// Hand off from the boot guard to the steady-state fatal path. h.ready is set
	// under turnMu (not just guardActive, which only the single command-loop
	// goroutine ever touches): reactWake reads h.ready under the same lock from a
	// goroutine the scheduler callback above can spawn concurrently with this line,
	// and an unsynchronized write on one side of that pair is a real data race
	// regardless of which value either side happens to observe in practice.
	h.guardActive = false
	h.turnMu.Lock()
	h.ready = true
	h.turnMu.Unlock()
	rcfg := app.Config()
	ev := EvReady{
		ProtocolVersion: ProtocolVersion,
		Version:         BuildVersion,
		AutoApprove:     rcfg.AutoApprove,
		Tier:            string(rcfg.Tier),
		TierGloss:       tierGloss(rcfg.Tier),
		Backend:         mastheadBackend(rcfg.BackendURL),
		Routing:         mastheadRouting(rcfg.Routing),
		// Resolvable here because StartDebugLog ran above; "" when logging is off.
		LogFile:  debuglog.CurrentDebugLogPath(),
		Commands: app.CommandCatalog(),
		StateDir: rcfg.StateDir,
	}
	// Best-effort: the socket path is a pure function of the state dir, but it also
	// creates the socket ROOT, and a host that cannot be told where to look is not a
	// reason to fail a session that otherwise works.
	if sock, err := ipc.SocketPathFor(rcfg.StateDir); err == nil {
		ev.ControlSocket = sock
	}
	if desc.ResumeSessionID != "" {
		ev.ResumedSessionID = desc.ResumeSessionID
	}
	h.post(ev)
	// AFTER host:ready, deliberately. A host cannot match events to a session until it
	// has been told the session id, so anything emitted before ready is dropped — the
	// startup status was going into exactly that gap and never arriving.
	h.postMcpStatus()

	h.checkPendingWakeAfterReady()
}

// checkPendingWakeAfterReady re-triggers the wake reactor for a burst that landed
// in pendingWake before h.ready was set. StartScheduler's callback (above, in
// boot) can invoke synchronously (or fast enough to win the race), appending to
// pendingWake and firing reactWake before h.ready flips true a few lines earlier
// in boot — reactWake bails on !h.ready and, without this check, nothing
// re-triggered it afterward, so that burst sat queued until unrelated later
// activity happened to call reactWake again. The supervisor already has this
// exact pattern; the native host needs the same one. Factored out so it can be
// exercised directly (see the wake-before-ready regression test).
func (h *Host) checkPendingWakeAfterReady() {
	h.turnMu.Lock()
	hasPending := len(h.pendingWake) > 0
	h.turnMu.Unlock()
	if hasPending {
		go h.reactWake()
	}
}
