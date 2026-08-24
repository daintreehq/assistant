package host

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// Wake-turn cancellation contract: wake turns run on their own child cancel
// context. The SHUTDOWN paths (shutdown/hibernate/parent-exit → teardown) cancel a
// live wake and JOIN it (bounded) before app.Shutdown, so the App's resources are
// never closed under a live Send. User interrupt deliberately does NOT abort a
// wake (the attention burst is already claimed; prompts fold in via InjectPrompt).
//
// These tests drive the REAL Host wiring — reactWake / handlePrompt /
// handleInterrupt / handleCommand / teardown — through the turnSession seam, with
// a cooperative fake session. Run with -race.

// wakeSession is a cooperative turnSession: Send parks until its context aborts
// or release is closed (returning a normal reply). Cancellation mirrors the REAL
// agent.Session contract: a cancelled turn returns (domain.CancelledReply, nil),
// NEVER an error — the old fake returned ctx.Err(), which masked the requeue path
// keying on err and let a shutdown-cancelled wake be durably lost. ignoreCancel
// models a Send that does not honor cancellation, for the bounded-join test.
// active tracks in-flight Sends so a fake App can detect a use-after-close-shaped
// overlap with Shutdown. Injected prompts are BUFFERED (never consumed), mirroring
// a turn already past its final fold check.
type wakeSession struct {
	mu           sync.Mutex
	ctxs         []context.Context
	texts        []string
	injected     []string
	release      chan struct{}
	started      chan struct{}
	ignoreCancel bool
	active       atomic.Int32
}

func newWakeSession() *wakeSession {
	return &wakeSession{
		release: make(chan struct{}),
		started: make(chan struct{}, 16),
	}
}

func (s *wakeSession) Send(ctx context.Context, text string, _ agent.SendOptions) (string, error) {
	s.active.Add(1)
	defer s.active.Add(-1)
	s.mu.Lock()
	s.ctxs = append(s.ctxs, ctx)
	s.texts = append(s.texts, text)
	s.mu.Unlock()
	select {
	case s.started <- struct{}{}:
	default:
	}
	if s.ignoreCancel {
		<-s.release
		return "done", nil
	}
	select {
	case <-ctx.Done():
		// The real Session reports cooperative cancellation as a REPLY, not an
		// error (Send's only error is ErrTurnInProgress).
		return domain.CancelledReply, nil
	case <-s.release:
		return "done", nil
	}
}

func (s *wakeSession) InjectPrompt(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.injected = append(s.injected, text)
}

// RetractPendingInjection mirrors Session.RetractPendingInjection (LIFO pop of
// the buffered-but-unfolded injections).
func (s *wakeSession) RetractPendingInjection() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.injected)
	if n == 0 {
		return "", false
	}
	text := s.injected[n-1]
	s.injected = s.injected[:n-1]
	return text, true
}

// DiscardPendingInjections mirrors Session.DiscardPendingInjections.
func (s *wakeSession) DiscardPendingInjections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.injected = nil
}

func (s *wakeSession) sendCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.ctxs)
}

func (s *wakeSession) ctxAt(i int) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctxs[i]
}

func (s *wakeSession) injectedPrompts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.injected...)
}

func (s *wakeSession) sentTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}

// shutdownProbeApp records whether App.Shutdown ran while a Send was still in
// flight — the resource-use-after-close race teardown's join exists to prevent.
type shutdownProbeApp struct {
	fakeApp
	sess               *wakeSession
	shutdownCalled     atomic.Bool
	shutdownDuringSend atomic.Bool
}

func (a *shutdownProbeApp) Shutdown(context.Context) error {
	a.shutdownCalled.Store(true)
	if a.sess.active.Load() > 0 {
		a.shutdownDuringSend.Store(true)
	}
	return nil
}

// newWakeHost builds a booted-state Host wired to the fake session/app, skipping
// the descriptor handshake (the wiring under test is the turn/teardown machinery,
// not boot). The injected exit closes `exited` instead of killing the process.
func newWakeHost(t *testing.T, sess *wakeSession) (*Host, *shutdownProbeApp, chan struct{}) {
	app := &shutdownProbeApp{sess: sess}
	h := NewHost(nil, strings.NewReader(""), io.Discard, io.Discard)
	h.tr.start() // this helper skips Run(), which normally starts the writer goroutine
	// Not every test using this helper drives the host all the way through
	// teardown/Close() itself, so guarantee the writer goroutine is retired either
	// way rather than leaking it past the test.
	t.Cleanup(h.tr.Close)
	h.runCtx, h.runCancel = context.WithCancel(context.Background())
	h.sessionID = "s"
	h.state = stateRunning
	h.ready = true
	h.app = app
	h.session = sess
	h.bridge = NewBridge(BridgeOptions{SessionID: "s", Post: func(HostEvent) {}})
	exited := make(chan struct{})
	var once sync.Once
	h.exit = func(int) { once.Do(func() { close(exited) }) }
	return h, app, exited
}

// startWake enqueues one pending wake burst and runs the reactor, waiting until
// the fake session's Send is actually in flight.
func startWake(t *testing.T, h *Host, sess *wakeSession) {
	t.Helper()
	h.turnMu.Lock()
	h.pendingWake = append(h.pendingWake, domain.QueueEvent{
		ID: "ev1", Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "agent finished", Summary: "terminal settled",
	})
	h.turnMu.Unlock()
	go h.reactWake()
	select {
	case <-sess.started:
	case <-time.After(2 * time.Second):
		t.Fatal("wake Send did not start")
	}
}

func waitClosed(t *testing.T, ch chan struct{}, timeout time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(timeout):
		t.Fatalf("%s did not complete in %s", what, timeout)
	}
}

// A command-driven shutdown arriving mid-wake must cancel the wake's context and
// join the unwinding worker BEFORE app.Shutdown — never close resources under a
// live Send — and still exit promptly.
func TestShutdownDuringWakeCancelsAndJoinsBeforeAppShutdown(t *testing.T) {
	sess := newWakeSession()
	h, app, exited := newWakeHost(t, sess)
	startWake(t, h, sess)

	go h.handleCommand(HostCommand{Type: CmdShutdown, SessionID: "s"})
	waitClosed(t, exited, 3*time.Second, "shutdown during wake")

	if err := sess.ctxAt(0).Err(); err == nil {
		t.Error("shutdown did not cancel the live wake turn's context")
	}
	if !app.shutdownCalled.Load() {
		t.Error("app.Shutdown was never called")
	}
	if app.shutdownDuringSend.Load() {
		t.Error("app.Shutdown ran while the wake Send was still in flight (resource use after close)")
	}
}

// Hibernate is the same teardown path: a live wake must be cancelled and joined.
func TestHibernateDuringWakeCancelsWake(t *testing.T) {
	sess := newWakeSession()
	h, app, exited := newWakeHost(t, sess)
	startWake(t, h, sess)

	go h.handleCommand(HostCommand{Type: CmdHibernate, SessionID: "s"})
	waitClosed(t, exited, 3*time.Second, "hibernate during wake")

	if err := sess.ctxAt(0).Err(); err == nil {
		t.Error("hibernate did not cancel the live wake turn's context")
	}
	if app.shutdownDuringSend.Load() {
		t.Error("app.Shutdown ran while the wake Send was still in flight")
	}
}

// A shutdown/hibernate-cancelled wake must be DURABLY re-armed, not just
// requeued in memory: the scheduler already marked the burst's queue events
// notified when it handed them to this process, pendingWake dies with the
// process, and the real Session reports cancellation as (CancelledReply, nil) —
// so without an explicit ctx-keyed re-arm the wake would be silently lost across
// the restart. The re-arm (App.RearmAttention → storage.ClearNotified) nulls
// notifiedAt so the NEXT run's scheduler pass over the same store re-digests and
// re-delivers the events (storage.TestClearNotifiedRearmsForNextDigest proves
// the store half).
func TestHibernateDuringWakeDurablyRearmsBurstForNextRun(t *testing.T) {
	sess := newWakeSession()
	h, app, exited := newWakeHost(t, sess)
	startWake(t, h, sess)

	go h.handleCommand(HostCommand{Type: CmdHibernate, SessionID: "s"})
	waitClosed(t, exited, 3*time.Second, "hibernate during wake")

	if got := app.rearmedIDs(); len(got) != 1 || got[0] != "ev1" {
		t.Fatalf("cancelled wake burst must be durably re-armed for the next run, rearmed=%v", got)
	}
}

// A burst QUEUED but never started (the host was busy with a command turn when
// shutdown arrived) is equally at risk: its events are already stamped notified
// and pendingWake dies with the process. Teardown's sweep must re-arm it too.
func TestShutdownRearmsQueuedNeverStartedWakeBurst(t *testing.T) {
	sess := newWakeSession()
	h, app, exited := newWakeHost(t, sess)

	// Occupy the host with a command turn so the queued burst cannot start.
	h.handlePrompt("busy work")
	select {
	case <-sess.started:
	case <-time.After(2 * time.Second):
		t.Fatal("command turn never started")
	}
	h.turnMu.Lock()
	h.pendingWake = append(h.pendingWake, domain.QueueEvent{
		ID: "ev9", Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
	})
	h.turnMu.Unlock()

	go h.handleCommand(HostCommand{Type: CmdShutdown, SessionID: "s"})
	waitClosed(t, exited, 3*time.Second, "shutdown with a queued wake burst")

	if got := app.rearmedIDs(); len(got) != 1 || got[0] != "ev9" {
		t.Fatalf("queued-never-started burst must be durably re-armed, rearmed=%v", got)
	}
}

// A wake that COMPLETES normally must not re-arm its burst (it was handled — a
// re-arm would duplicate the delivery on the next run).
func TestCompletedWakeDoesNotRearm(t *testing.T) {
	sess := newWakeSession()
	h, app, _ := newWakeHost(t, sess)
	startWake(t, h, sess)

	close(sess.release)
	waitNotBusy(t, h)
	if got := app.rearmedIDs(); len(got) != 0 {
		t.Fatalf("completed wake must not re-arm its events, rearmed=%v", got)
	}
}

// waitNotBusy polls until the host has cleared busy (a turn/wake fully unwound).
func waitNotBusy(t *testing.T, h *Host) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.turnMu.Lock()
		busy := h.busy
		h.turnMu.Unlock()
		if !busy {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("host never cleared busy")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// A wake Send that ignores cancellation must not wedge teardown: the join is
// bounded (turnJoinTimeout) and the host still exits.
func TestShutdownDuringUncancellableWakeExitsBounded(t *testing.T) {
	sess := newWakeSession()
	sess.ignoreCancel = true
	h, _, exited := newWakeHost(t, sess)
	h.turnJoinTimeout = 150 * time.Millisecond
	startWake(t, h, sess)

	start := time.Now()
	go h.handleCommand(HostCommand{Type: CmdShutdown, SessionID: "s"})
	waitClosed(t, exited, 3*time.Second, "shutdown with a wedged wake")
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("teardown took %s; the turn join must be bounded", elapsed)
	}
	close(sess.release) // unwedge the parked worker so the test doesn't leak it
}

// User interrupt deliberately does NOT abort a wake turn (the burst is already
// claimed); it must leave the wake context live, and the wake then completes
// normally and clears busy.
func TestInterruptDuringWakeDoesNotAbortWake(t *testing.T) {
	sess := newWakeSession()
	h, _, _ := newWakeHost(t, sess)
	startWake(t, h, sess)

	h.handleInterrupt()
	// Give a wrongly-wired cancellation a moment to propagate before asserting.
	time.Sleep(30 * time.Millisecond)
	if err := sess.ctxAt(0).Err(); err != nil {
		t.Fatalf("interrupt aborted the wake turn (ctx err %v); wakes are non-user-abortable by design", err)
	}

	close(sess.release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.turnMu.Lock()
		busy := h.busy
		h.turnMu.Unlock()
		if !busy {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("wake did not finish and clear busy after release")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A prompt arriving during a wake keeps the existing folding behavior: it is
// injected into the running turn (InjectPrompt), never started as a second Send.
func TestPromptDuringWakeFoldsViaInjectPrompt(t *testing.T) {
	sess := newWakeSession()
	h, _, _ := newWakeHost(t, sess)
	startWake(t, h, sess)

	h.handlePrompt("please also check the logs")
	if got := sess.injectedPrompts(); len(got) != 1 || got[0] != "please also check the logs" {
		t.Fatalf("prompt during wake was not folded via InjectPrompt: %+v", got)
	}
	if n := sess.sendCount(); n != 1 {
		t.Fatalf("prompt during wake started a second Send (%d sends)", n)
	}

	close(sess.release)
}

// After teardown latched closing, a queued wake burst must not start a new turn
// (the turnWG join would otherwise race a fresh Add).
func TestWakeAfterCloseLatchDoesNotStart(t *testing.T) {
	sess := newWakeSession()
	h, _, exited := newWakeHost(t, sess)

	h.handleCommand(HostCommand{Type: CmdShutdown, SessionID: "s"})
	waitClosed(t, exited, 2*time.Second, "idle shutdown")

	h.turnMu.Lock()
	h.pendingWake = append(h.pendingWake, domain.QueueEvent{
		ID: "ev2", Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
	})
	h.turnMu.Unlock()
	h.reactWake() // synchronous: must return without sending
	if n := sess.sendCount(); n != 0 {
		t.Fatalf("wake started after teardown latched closing (%d sends)", n)
	}
}
