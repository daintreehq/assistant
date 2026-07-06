// Package supervisor is the persistent per-project daemon: the process that
// keeps supervising Daintree work after the interactive cockpit closes.
//
// Ownership model (docs/SUPERVISOR.md): exactly one process at a time owns the
// project DB and its supervision engines, serialized by the flock owner lease
// (internal/ipc). The daemon is a persistent contender for that lease — while
// a cockpit is attached (an open attach connection on the control socket) it
// stands down; the moment the cockpit exits or crashes (connection drops, and
// the kernel releases the cockpit's flock) the daemon re-acquires, builds a
// headless App, ADOPTS the persisted watchers/async futures/timers, and runs
// the scheduler + async coordinator + autonomous wake turns exactly where the
// cockpit left off — same conversation (runtime_state session pointer), same
// backend state token.
//
// A daemon wake turn dispatches tools as domain.ActorWake: reads run freely
// under the tier gate, mutations need a scoped automation grant (actor id
// domain.WakeActorID) or they become a blocked inbox item — the pending
// approval the user resolves on the next attach. There is deliberately NO
// confirm hook here: nobody is present to answer one.
package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/ipc"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
)

// Timing defaults. Test seams override via Options.
const (
	// acquireRetryEvery is the owner-lease contention poll. Cheap (one flock
	// syscall), so a cockpit exit hands supervision back within a second.
	acquireRetryEvery = 1 * time.Second
	// monitorEvery drives the supervise-side housekeeping tick (MCP health,
	// idle-exit accounting).
	monitorEvery = 15 * time.Second
	// mcpBlockedAfter is how long MCP must be continuously unreachable (with
	// live supervision) before the daemon publishes the blocked inbox item.
	mcpBlockedAfter = 60 * time.Second
	// mcpReconnectEvery rate-limits reconnect attempts while down.
	mcpReconnectEvery = 30 * time.Second
	// defaultIdleExit: with NOTHING to supervise (no live watchers/async, no
	// scheduled timers, no open inbox, no wake activity) the daemon exits so a
	// machine doesn't accumulate idle per-project daemons. Any new work simply
	// respawns one on the next cockpit start.
	defaultIdleExit = 15 * time.Minute
	// handoverTimeout bounds an attach handover (cancel wake turn → drain →
	// close store → release lease).
	handoverTimeout = 60 * time.Second
)

// Options configures a daemon run.
type Options struct {
	// Overrides feed config.LoadConfig exactly like the CLI paths.
	Overrides config.ConfigOverrides
	// Version is the build version (status/descriptor).
	Version string
	// IdleExit overrides defaultIdleExit; negative disables idle exit (tests).
	IdleExit time.Duration
	// Log receives human-readable daemon lifecycle lines (stdout by default).
	Log func(string)
	// Fast compresses every internal cadence (monitor tick, MCP-blocked
	// threshold, reconnect budget) to sub-second values. TEST-ONLY — enabled by
	// the DAINTREE_ASSISTANT_DAEMON_FAST env var so subprocess e2e tests can
	// exercise outage/blocked/idle behavior without minute-scale sleeps.
	Fast bool
}

// tuning is the resolved cadence set (see Options.Fast).
type tuning struct {
	monitorEvery      time.Duration
	mcpBlockedAfter   time.Duration
	mcpReconnectEvery time.Duration
}

func (o Options) tuning() tuning {
	if o.Fast {
		return tuning{monitorEvery: 200 * time.Millisecond, mcpBlockedAfter: 500 * time.Millisecond, mcpReconnectEvery: time.Second}
	}
	return tuning{monitorEvery: monitorEvery, mcpBlockedAfter: mcpBlockedAfter, mcpReconnectEvery: mcpReconnectEvery}
}

// superviseReason says why one supervision span ended.
type superviseReason int

const (
	reasonYield superviseReason = iota // an attach arrived — stand down
	reasonStop                         // shutdown RPC / signal
	reasonIdle                         // nothing left to supervise
	reasonFatal                        // App construction failed
)

// Runtime is the daemon process state.
type Runtime struct {
	opts      Options
	cfg       config.AppConfig
	tun       tuning
	ownerLock *ipc.FileLock
	startedAt int64

	mu           sync.Mutex
	state        ipc.DaemonState
	attachConnID uint64
	creds        ipc.Credentials
	app          *app.App           // non-nil while supervising
	cancelSup    context.CancelFunc // cancels the active supervision span
	handoverDone chan struct{}      // closed when the span fully released the lease
	lastError    string
	wakeTurns    int
	lastWakeAt   int64

	// wake reactor state (guarded by mu; the reactor goroutine is single-flight)
	wakeBusy    bool
	wakeRetried bool
	pendingWake []domain.QueueEvent
	summarized  map[string]struct{}
	wakeWG      sync.WaitGroup

	blockedEventID string
	mcpDownSince   time.Time
	lastReconnect  time.Time
	idleSince      time.Time
	// credsRevoked latches when the MCP status shows an auth/binding-terminal
	// failure: the per-session bearer is dead (Daintree revoked it on window
	// close / displacement / re-provision) and reconnecting with it would trip
	// the host's abuse policy (docs/DAINTREE_HOST.md §4). No reconnects happen
	// while latched; a fresh credentials push clears it (and rebuilds the span).
	credsRevoked bool

	resumeCh chan struct{} // signaled when an attach connection closes
	stopCh   chan struct{}
	stopOnce sync.Once
}

// Run is the `daintree-assistant daemon` body: resolve config, become the
// project's daemon singleton, serve the control socket, and contend for the
// owner lease forever (until shutdown or idle-exit). Returns the process exit
// code.
func Run(ctx context.Context, opts Options) int {
	logf := opts.Log
	if logf == nil {
		logf = func(s string) { fmt.Fprintln(os.Stdout, s) }
	}
	cfg, err := config.LoadConfig(opts.Overrides)
	if err != nil {
		logf("daemon: config: " + err.Error())
		return 1
	}
	r := &Runtime{
		opts:       opts,
		cfg:        cfg,
		tun:        opts.tuning(),
		startedAt:  domain.NowMS(),
		state:      ipc.StateStarting,
		resumeCh:   make(chan struct{}, 1),
		stopCh:     make(chan struct{}),
		summarized: map[string]struct{}{},
	}
	if opts.Log == nil {
		r.opts.Log = logf
	}

	// Daemon singleton: one daemon per project state dir. A held lock means a
	// live daemon already serves this project — exit quietly (the spawner treats
	// "already running" as success).
	daemonLock := ipc.NewFileLock(filepath.Join(cfg.StateDir, ipc.DaemonLockName))
	ok, err := daemonLock.TryAcquire()
	if err != nil {
		logf("daemon: lock: " + err.Error())
		return 1
	}
	if !ok {
		logf(fmt.Sprintf("daemon: another supervisor (pid %d) already serves this project", ipc.ReadLockHolderPid(daemonLock.Path())))
		return 0
	}
	defer daemonLock.Release()

	r.ownerLock = ipc.NewFileLock(filepath.Join(cfg.StateDir, ipc.OwnerLockName))

	socketPath, err := ipc.SocketPathFor(cfg.StateDir)
	if err != nil {
		logf("daemon: socket: " + err.Error())
		return 1
	}
	server := ipc.NewServer(socketPath, r)
	if err := server.Start(ctx); err != nil {
		logf("daemon: " + err.Error())
		return 1
	}
	defer server.Close()

	if err := ipc.WriteDaemonDescriptor(cfg.StateDir, ipc.DaemonDescriptor{
		Pid: os.Getpid(), SocketPath: socketPath, StartedAtMs: r.startedAt, Version: opts.Version,
	}); err != nil {
		logf("daemon: descriptor: " + err.Error())
	}
	defer ipc.RemoveDaemonDescriptor(cfg.StateDir)

	// The daemon owns its own debug-log identity (one long-lived per-process
	// file, distinct from any cockpit session log).
	if cfg.DebugLog {
		path := debuglog.StartDebugLog(debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir}, map[string]any{
			"sessionId": fmt.Sprintf("daemon-%d", os.Getpid()),
			"kind":      "supervisor-daemon",
			"stateDir":  cfg.StateDir,
		})
		if path != "" {
			logf("daemon: logging to " + path)
		}
	}

	logf(fmt.Sprintf("daemon: serving %s (socket %s)", cfg.StateDir, socketPath))
	code := r.runLoop(ctx)
	logf("daemon: exiting")
	return code
}

// runLoop contends for the owner lease and supervises whenever it wins.
func (r *Runtime) runLoop(ctx context.Context) int {
	for {
		select {
		case <-ctx.Done():
			return 0
		case <-r.stopCh:
			return 0
		default:
		}
		if r.attached() {
			r.setState(ipc.StateYielded)
			select {
			case <-ctx.Done():
				return 0
			case <-r.stopCh:
				return 0
			case <-r.resumeCh:
			}
			continue
		}
		got, err := r.ownerLock.TryAcquire()
		if err != nil {
			r.setError("owner lock: " + err.Error())
			return 1
		}
		if !got {
			// Another process (an attached cockpit, or one that predates this
			// daemon) owns the DB. Stand by and retry — the kernel releases the
			// flock the instant that process exits, crash included.
			r.setState(ipc.StateStandby)
			select {
			case <-ctx.Done():
				return 0
			case <-r.stopCh:
				return 0
			case <-time.After(acquireRetryEvery):
			}
			continue
		}
		// Won the lease — but an attach may have landed between the check and the
		// acquire; yield immediately rather than fight the arriving cockpit.
		if r.attached() {
			r.ownerLock.Release()
			r.signalHandover()
			continue
		}
		reason := r.supervise(ctx)
		r.ownerLock.Release()
		r.signalHandover()
		switch reason {
		case reasonStop:
			return 0
		case reasonIdle:
			r.logf("daemon: nothing left to supervise — idle exit")
			return 0
		case reasonFatal:
			// Constructing the App failed (bad DB, stale schema, …). Back off hard
			// rather than hot-loop; the error is visible via `status`.
			select {
			case <-ctx.Done():
				return 0
			case <-r.stopCh:
				return 0
			case <-time.After(30 * time.Second):
			}
		}
		// reasonYield loops back to the attached-wait at the top.
	}
}

// supervise runs one ownership span: build the headless App, adopt persisted
// supervision, run engines + wake turns, and watch for yield/stop/idle.
func (r *Runtime) supervise(parent context.Context) superviseReason {
	sctx, cancel := context.WithCancel(parent)
	defer cancel()

	// Rebuild the App from the daemon's own overrides each span — config stays
	// identical across spans except the credentials, where the freshest values
	// pushed by the last cockpit win over whatever this process inherited at
	// spawn (Daintree rotates MCP tokens).
	overrides := r.opts.Overrides
	r.mu.Lock()
	if r.creds.McpURL != "" {
		overrides.McpURL = strPtr(r.creds.McpURL)
	}
	if r.creds.McpToken != "" {
		overrides.McpToken = strPtr(r.creds.McpToken)
	}
	r.cancelSup = cancel
	r.handoverDone = make(chan struct{})
	r.mu.Unlock()

	a, err := app.Create(app.CreateOptions{
		Overrides: overrides,
		// Headless: no Confirm/AskChoice/AgentEvents hooks. The durable
		// RunEventSink still records every turn event; mutations gate on wake
		// grants (DispatchActor below).
		Hooks:                app.AppHooks{Log: func(msg string) { r.logf("daemon: " + msg) }},
		DispatchActor:        domain.ActorWake,
		ResumeCurrentSession: true,
	})
	if err != nil {
		r.setError("app: " + err.Error())
		r.setState(ipc.StateStandby)
		return reasonFatal
	}
	r.mu.Lock()
	r.app = a
	r.lastError = ""
	r.idleSince = time.Time{}
	r.mcpDownSince = time.Time{}
	r.mu.Unlock()
	r.setState(ipc.StateSupervising)
	defer func() {
		// Wait for an in-flight wake turn to unwind (its ctx is already
		// cancelled) before tearing the App down under it.
		waitTimeout(&r.wakeWG, 10*time.Second)
		_ = a.Shutdown()
		r.mu.Lock()
		r.app = nil
		r.cancelSup = nil
		r.mu.Unlock()
	}()

	// Continue the project's current conversation (or pin this fresh one so the
	// next span continues it).
	a.AdoptAsCurrentSession()

	st := a.ConnectMcp(sctx)
	own := a.Ownership()
	r.logf(fmt.Sprintf("daemon: supervising (mcp connected=%v, resumed watchers=%d async=%d, publish retries=%d, open inbox=%d)",
		st.Connected, len(own.ResumedWatcherTitles), own.ResumedAsyncCount, own.UnpublishedAsyncCount, own.OpenAttentionCount))

	// The scheduler + async coordinator: adoption of persisted rows happens
	// inside coordinator.Start. Attention events route into the wake reactor.
	a.StartScheduler(sctx, func(events []domain.QueueEvent) { r.onAttention(sctx, events) })

	ticker := time.NewTicker(r.tun.monitorEvery)
	defer ticker.Stop()
	for {
		select {
		case <-sctx.Done():
			// Cancelled by an attach handover or process stop.
			select {
			case <-r.stopCh:
				return reasonStop
			default:
				return reasonYield
			}
		case <-r.stopCh:
			return reasonStop
		case <-ticker.C:
			r.monitorMcp(sctx, a)
			if r.idleExitDue(a) {
				return reasonIdle
			}
		}
	}
}

// onAttention is the scheduler's delivery callback: actionable events feed the
// autonomous wake reactor; everything else already sits in the durable inbox
// for the next attach.
func (r *Runtime) onAttention(ctx context.Context, events []domain.QueueEvent) {
	var actionable []domain.QueueEvent
	for _, e := range events {
		if agentIsActionableWake(e) {
			actionable = append(actionable, e)
		}
	}
	if len(actionable) == 0 {
		return
	}
	r.mu.Lock()
	r.pendingWake = append(r.pendingWake, actionable...)
	r.mu.Unlock()
	go r.reactWake(ctx)
}

// monitorMcp watches connectivity while work is live: attempts reconnects on a
// budget, publishes ONE deduped blocked inbox item after a sustained outage,
// and resolves it the moment the link is back. This is the "blocked, never
// silently abandoned" contract — the engines themselves already freeze safely
// (watchers keep re-arming, async deadlines pause while disconnected).
//
// Credential revocation is TERMINAL, not an outage (docs/DAINTREE_HOST.md):
// Daintree rotates the per-session bearer on every re-provision and revokes it
// on window close/eviction/displacement, and repeated auth failures trip the
// host's abuse policy. On an auth/binding-terminal status the daemon stops
// reconnecting entirely and waits for the fresh credentials the next assistant
// launch pushes over the control socket.
func (r *Runtime) monitorMcp(ctx context.Context, a *app.App) {
	if a.Config.McpURL == "" {
		return // MCP genuinely unconfigured — degraded local mode, nothing to watch
	}
	st := a.MCP.Status()
	now := time.Now()
	if st.Connected {
		r.mu.Lock()
		r.mcpDownSince = time.Time{}
		r.credsRevoked = false
		blocked := r.blockedEventID
		r.blockedEventID = ""
		r.mu.Unlock()
		if blocked != "" {
			_, _ = a.Queue.Resolve(ctx, blocked)
			r.logf("daemon: MCP reconnected — supervision resumed")
		}
		return
	}
	revoked := mcp.IsCredentialTerminalStatus(st.Error)
	r.mu.Lock()
	if r.mcpDownSince.IsZero() {
		r.mcpDownSince = now
	}
	if revoked && !r.credsRevoked {
		r.credsRevoked = true
		r.logf("daemon: MCP credentials revoked — reconnects paused until a new assistant launch pushes fresh ones")
	}
	downFor := now.Sub(r.mcpDownSince)
	shouldReconnect := !r.credsRevoked && now.Sub(r.lastReconnect) >= r.tun.mcpReconnectEvery
	if shouldReconnect {
		r.lastReconnect = now
	}
	alreadyBlocked := r.blockedEventID != ""
	r.mu.Unlock()

	if shouldReconnect {
		if st := a.ReconnectMcp(ctx); st.Connected {
			return // next tick clears the blocked state
		}
	}
	if downFor < r.tun.mcpBlockedAfter || alreadyBlocked {
		return
	}
	if !r.hasLiveWork(a) {
		return // nothing supervised is starved; stay quiet
	}
	summary := "The supervisor daemon cannot reach the Daintree MCP (Daintree closed or unreachable). " +
		"Watchers and async futures are paused — nothing is abandoned — and supervision resumes " +
		"automatically when the connection returns."
	if revoked {
		summary = "This session's Daintree MCP credentials were revoked (the assistant window closed or a new " +
			"session displaced it). Watchers and async futures are paused — nothing is abandoned — and " +
			"supervision resumes automatically the next time the assistant is opened in Daintree (a fresh " +
			"launch pushes new credentials to the supervisor)."
	}
	ev, err := a.Queue.Publish(ctx, domain.QueuePublishArgs{
		Source:    domain.SourceSystem,
		Severity:  domain.SeverityBlocked,
		Title:     "Supervision blocked: Daintree MCP unavailable",
		Summary:   summary,
		DedupeKey: "supervisor:mcp_unavailable",
	})
	if err == nil {
		r.mu.Lock()
		r.blockedEventID = ev.ID
		r.mu.Unlock()
		r.logf("daemon: MCP unreachable — supervision blocked (published to inbox)")
	}
}

// hasLiveWork reports whether anything is actually being supervised.
func (r *Runtime) hasLiveWork(a *app.App) bool {
	if n, err := a.Store.CountLiveAsyncInvocations(); err == nil && n > 0 {
		return true
	}
	if ws, err := a.Store.ListWatchers("active"); err == nil && len(ws) > 0 {
		return true
	}
	return false
}

// idleExitDue tracks the idle window: no live supervision, no scheduled
// timers, no open inbox, no wake activity — held for the full idle-exit
// duration.
func (r *Runtime) idleExitDue(a *app.App) bool {
	idleAfter := r.opts.IdleExit
	if idleAfter < 0 {
		return false
	}
	if idleAfter == 0 {
		idleAfter = defaultIdleExit
	}
	idle := !r.hasLiveWork(a)
	if idle {
		if timers, err := a.Store.ListTimers("scheduled"); err != nil || len(timers) > 0 {
			idle = false
		}
	}
	if idle {
		if open, err := a.Store.ListEvents(domain.QueueDigestOptions{}); err != nil || len(open) > 0 {
			idle = false
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.wakeBusy || len(r.pendingWake) > 0 {
		idle = false
	}
	if !idle {
		r.idleSince = time.Time{}
		return false
	}
	if r.idleSince.IsZero() {
		r.idleSince = time.Now()
		return false
	}
	return time.Since(r.idleSince) >= idleAfter
}

// ---- ipc.Handler ----

// HandleRequest serves the control socket. Attach runs the full handover on
// the connection's goroutine (the client is waiting for exactly that).
func (r *Runtime) HandleRequest(ctx context.Context, req ipc.Request, conn *ipc.ServerConn) ipc.Response {
	switch req.Type {
	case ipc.ReqStatus:
		return okResponse(r.statusReply())

	case ipc.ReqCredentials:
		var c ipc.Credentials
		if err := json.Unmarshal(req.Payload, &c); err != nil {
			return errResponse("malformed credentials payload")
		}
		if r.storeCreds(c) {
			// Fresh credentials while a span runs on the OLD (likely revoked)
			// token: rebuild the span. The runLoop re-acquires immediately and
			// the new App connects with the new token — the displaced-cockpit
			// race (docs/DAINTREE_HOST.md one-backend rule) heals here.
			r.interruptSupervision()
		}
		return ipc.Response{OK: true}

	case ipc.ReqAttach:
		var a ipc.AttachRequest
		if len(req.Payload) > 0 {
			if err := json.Unmarshal(req.Payload, &a); err != nil {
				return errResponse("malformed attach payload")
			}
		}
		return r.handleAttach(a, conn)

	case ipc.ReqShutdown:
		// Reply OK first (the response write happens after return), then stop.
		r.stopOnce.Do(func() { close(r.stopCh) })
		r.interruptSupervision()
		return ipc.Response{OK: true}

	default:
		return errResponse("unknown request type " + req.Type)
	}
}

// handleAttach yields ownership to the calling client for as long as its
// connection stays open.
func (r *Runtime) handleAttach(a ipc.AttachRequest, conn *ipc.ServerConn) ipc.Response {
	r.mu.Lock()
	if r.attachConnID != 0 && r.attachConnID != conn.ID() {
		r.mu.Unlock()
		return errResponse("another assistant is already attached to this project")
	}
	r.attachConnID = conn.ID()
	if a.Credentials != nil {
		r.applyCredsLocked(*a.Credentials)
	}
	cancel := r.cancelSup
	done := r.handoverDone
	supervising := r.app != nil
	r.mu.Unlock()
	conn.SetAttached(true)

	reply := ipc.AttachReply{}
	if supervising && cancel != nil {
		reply.WakeInFlight = r.wakeInFlight()
		cancel()
		select {
		case <-done:
			reply.OwnerReleased = true
		case <-time.After(handoverTimeout):
			// The span is stuck (a hung Shutdown). Do NOT pretend the lease is
			// free; the client's flock attempt will tell the truth.
			return errResponse("handover timed out — the supervisor could not release the project")
		}
	} else {
		// Not supervising: either the lease is free, or some OTHER process holds
		// it (a second cockpit). Probe non-destructively so the client gets an
		// honest OwnerBusy instead of a mysterious flock timeout. The probe can't
		// steal from the arriving client — it hasn't tried to lock yet (it is
		// blocked on this reply).
		if got, _ := r.ownerLock.TryAcquire(); got {
			r.ownerLock.Release()
		} else {
			reply.OwnerBusy = true
			reply.OwnerPid = ipc.ReadLockHolderPid(r.ownerLock.Path())
		}
	}
	r.setState(ipc.StateYielded)
	return okResponse(reply)
}

// ConnClosed ends an attach lease: the daemon resumes contending for the
// owner lock. Exactly the path a cockpit CRASH takes (its flock is released by
// the kernel; its socket connection drops here).
func (r *Runtime) ConnClosed(conn *ipc.ServerConn) {
	if !conn.Attached() {
		return
	}
	r.mu.Lock()
	if r.attachConnID == conn.ID() {
		r.attachConnID = 0
	}
	r.mu.Unlock()
	select {
	case r.resumeCh <- struct{}{}:
	default:
	}
}

// ---- helpers ----

func (r *Runtime) statusReply() ipc.StatusReply {
	r.mu.Lock()
	rep := ipc.StatusReply{
		State:        r.state,
		Pid:          os.Getpid(),
		Version:      r.opts.Version,
		StartedAtMs:  r.startedAt,
		ProjectID:    r.cfg.ProjectID,
		StateDir:     r.cfg.StateDir,
		DBPath:       r.cfg.DBPath,
		LastError:    r.lastError,
		WakeTurnsRun: r.wakeTurns,
		LastWakeAtMs: r.lastWakeAt,
		McpBlocked:   r.blockedEventID != "",
	}
	a := r.app
	r.mu.Unlock()
	rep.McpConfigured = r.cfg.McpURL != ""
	if a != nil {
		st := a.MCP.Status()
		rep.McpConnected = st.Connected
		if n, err := a.Store.CountLiveAsyncInvocations(); err == nil {
			rep.LiveAsync = n
		}
		if ws, err := a.Store.ListWatchers("active"); err == nil {
			rep.LiveWatchers = len(ws)
		}
		if ts, err := a.Store.ListTimers("scheduled"); err == nil {
			rep.ScheduledTimers = len(ts)
		}
		if evs, err := a.Store.ListEvents(domain.QueueDigestOptions{}); err == nil {
			rep.OpenAttention = len(evs)
			for _, e := range evs {
				if e.Severity == domain.SeverityBlocked {
					rep.PendingApproval++
				}
			}
		}
	}
	return rep
}

// storeCreds records a credentials push; the return value reports whether a
// supervision span is running on now-stale values (caller decides to rebuild).
func (r *Runtime) storeCreds(c ipc.Credentials) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := r.applyCredsLocked(c)
	return changed && r.app != nil
}

func (r *Runtime) applyCredsLocked(c ipc.Credentials) (changed bool) {
	if c.McpURL != "" && c.McpURL != r.creds.McpURL {
		r.creds.McpURL = c.McpURL
		changed = true
	}
	if c.McpToken != "" && c.McpToken != r.creds.McpToken {
		r.creds.McpToken = c.McpToken
		changed = true
	}
	if c.BackendURL != "" && c.BackendURL != r.creds.BackendURL {
		r.creds.BackendURL = c.BackendURL
		changed = true
	}
	if changed {
		// New credentials supersede a revocation latch — the next span connects
		// with them.
		r.credsRevoked = false
	}
	return changed
}

func (r *Runtime) attached() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.attachConnID != 0
}

func (r *Runtime) wakeInFlight() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.wakeBusy
}

func (r *Runtime) setState(s ipc.DaemonState) {
	r.mu.Lock()
	r.state = s
	r.mu.Unlock()
}

func (r *Runtime) setError(msg string) {
	r.mu.Lock()
	r.lastError = msg
	r.mu.Unlock()
	r.logf("daemon: " + msg)
}

// interruptSupervision cancels the active span (shutdown path).
func (r *Runtime) interruptSupervision() {
	r.mu.Lock()
	cancel := r.cancelSup
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// signalHandover closes the current handover channel (the attach handler may
// be waiting on it).
func (r *Runtime) signalHandover() {
	r.mu.Lock()
	done := r.handoverDone
	r.handoverDone = nil
	r.mu.Unlock()
	if done != nil {
		close(done)
	}
}

func (r *Runtime) logf(s string) {
	if r.opts.Log != nil {
		r.opts.Log(s)
	}
}

func okResponse(payload any) ipc.Response {
	raw, err := json.Marshal(payload)
	if err != nil {
		return errResponse("encode reply: " + err.Error())
	}
	return ipc.Response{OK: true, Payload: raw}
}

func errResponse(msg string) ipc.Response {
	return ipc.Response{OK: false, Error: msg}
}

func strPtr(s string) *string { return &s }

// waitTimeout waits for wg up to d.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}
