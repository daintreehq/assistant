package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
)

// fakeApp is a host.App test double. SetHooks captures the sink so the test can
// drive turn events; Send blocks on a channel so the test controls turn timing.
// rearmed records RearmAttention calls (the durable re-arm of a cancelled wake
// burst).
type fakeApp struct {
	hooks   AppHooks
	session *agent.Session
	// command, when set, is what RunCommand returns — the seam for asserting that the
	// host carries an engine outcome onto the wire rather than inventing one.
	command CommandOutcome

	mu      sync.Mutex
	rearmed []string
	// timerRows is what Timers() returns; cancelOutcome is what CancelTimer()
	// returns, and cancelled records the ids it was asked for.
	timerRows     []TimerRow
	timersFailed  bool
	operations    OperationsSnapshot
	cancelOutcome TimerCancelOutcome
	cancelled     []string
}

func (f *fakeApp) SetHooks(h AppHooks)              { f.hooks = h }
func (f *fakeApp) ConnectMCP(context.Context) error { return nil }
func (f *fakeApp) RunCommand(context.Context, string) CommandOutcome {
	return f.command
}
func (f *fakeApp) CostSnapshot() (float64, bool)                   { return 0, false }
func (f *fakeApp) McpStatus() (bool, *int, string)                 { return false, nil, "" }
func (f *fakeApp) CommandCatalog() []CommandMeta                   { return nil }
func (f *fakeApp) Operations(context.Context) OperationsSnapshot   { return f.operations }
func (f *fakeApp) Timers(context.Context) ([]TimerRow, bool)       { return f.timerRows, !f.timersFailed }
func (f *fakeApp) StartScheduler(func(events []domain.QueueEvent)) {}
func (f *fakeApp) Session() *agent.Session                         { return f.session }
func (f *fakeApp) RiskOf(string) (domain.RiskClass, bool)          { return "", false }
func (f *fakeApp) Config() config.AppConfig                        { return config.AppConfig{} }
func (f *fakeApp) Shutdown(context.Context) error                  { return nil }

func (f *fakeApp) CancelTimer(_ context.Context, id string) TimerCancelOutcome {
	f.mu.Lock()
	f.cancelled = append(f.cancelled, id)
	f.mu.Unlock()
	out := f.cancelOutcome
	out.TimerID = id
	return out
}

func (f *fakeApp) cancelledIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.cancelled...)
}

func (f *fakeApp) RearmAttention(ids []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rearmed = append(f.rearmed, ids...)
	return nil
}

func (f *fakeApp) rearmedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.rearmed...)
}

// syncBuf is a goroutine-safe writer collecting NDJSON output.
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncBuf) lines() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]any
	sc := bufio.NewScanner(strings.NewReader(s.buf.String()))
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// drive runs a host with a scripted set of input lines, returning the captured
// stdout maps. The injected exit func cancels run instead of os.Exit.
func driveHost(t *testing.T, factory AppFactory, inputs []string) []map[string]any {
	t.Helper()
	pr, pw := io.Pipe()
	out := &syncBuf{}
	var errb syncBuf

	h := NewHost(factory, pr, out, &errb)
	// A pre-ready panic exits without ever running teardown/Close, so the writer
	// goroutine would otherwise leak past the test — harmless (idempotent) for
	// every other scenario, which already closes it via a normal teardown.
	t.Cleanup(h.tr.Close)
	exited := make(chan struct{})
	var once sync.Once
	h.exit = func(int) { once.Do(func() { close(exited) }); runtimeGoexit() }

	go h.Run(context.Background())

	go func() {
		for _, line := range inputs {
			_, _ = pw.Write([]byte(line + "\n"))
			time.Sleep(15 * time.Millisecond)
		}
	}()

	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("host did not exit")
	}
	_ = pw.Close()
	_ = pr.Close()
	time.Sleep(10 * time.Millisecond)
	return out.lines()
}

// runtimeGoexit terminates the calling goroutine (the loop goroutine that called
// teardown→exit) so the injected non-os.Exit teardown stops the loop cleanly,
// mimicking process exit. Goexit runs deferreds but the Run boot-guard recover()
// returns nil during a Goexit, so it does not interfere.
func runtimeGoexit() { runtime.Goexit() }

func TestHostBadDescriptor(t *testing.T) {
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }
	// Has a sessionId (so the error is attributable to stdout) but is missing the
	// required protocolVersion → bad-descriptor + teardown error.
	bad := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system"}`
	lines := driveHost(t, factory, []string{bad})
	var sawBad, sawShutdown bool
	for _, m := range lines {
		if m["type"] == "host:error" && m["code"] == "bad-descriptor" {
			sawBad = true
		}
		if m["type"] == "host:shutdown" && m["reason"] == "error" {
			sawShutdown = true
		}
	}
	if !sawBad || !sawShutdown {
		t.Fatalf("bad-descriptor flow missing: %+v", lines)
	}
}

// TestHostBadDescriptorErrorBeforeShutdown is the ordering regression guard: on a
// fatal pre-app path the SPECIFIC host:error bad-descriptor must reach the parent
// BEFORE the host:shutdown reason. The bug was the error going through the async
// writer queue while host:shutdown was written synchronously then the process
// exited — racing (and sometimes reordering) the two. reportSync now writes the
// error synchronously+flushed first, so its index must precede shutdown's.
func TestHostBadDescriptorErrorBeforeShutdown(t *testing.T) {
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }
	bad := `{"sessionId":"s","not":"a descriptor"}`
	lines := driveHost(t, factory, []string{bad})

	errIdx, shutdownIdx := -1, -1
	for i, m := range lines {
		if m["type"] == "host:error" && m["code"] == "bad-descriptor" && errIdx < 0 {
			errIdx = i
		}
		if m["type"] == "host:shutdown" && shutdownIdx < 0 {
			shutdownIdx = i
		}
	}
	if errIdx < 0 {
		t.Fatalf("no host:error bad-descriptor on stream: %+v", lines)
	}
	if shutdownIdx < 0 {
		t.Fatalf("no host:shutdown on stream: %+v", lines)
	}
	if errIdx > shutdownIdx {
		t.Fatalf("host:error bad-descriptor (idx %d) must precede host:shutdown (idx %d): %+v",
			errIdx, shutdownIdx, lines)
	}
}

func TestHostProtocolMismatch(t *testing.T) {
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":999}`
	lines := driveHost(t, factory, []string{desc})
	var saw bool
	for _, m := range lines {
		if m["type"] == "host:error" && m["code"] == "protocol-mismatch" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("no protocol-mismatch: %+v", lines)
	}
}

func TestHostReadyAndShutdown(t *testing.T) {
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	lines := driveHost(t, factory, []string{desc, `{"type":"shutdown","sessionId":"s"}`})

	var sawReady, sawShutdown bool
	for _, m := range lines {
		if m["type"] == "host:ready" {
			sawReady = true
			if int(m["protocolVersion"].(float64)) != ProtocolVersion {
				t.Errorf("ready protocolVersion=%v", m["protocolVersion"])
			}
		}
		if m["type"] == "host:shutdown" && m["reason"] == "exit" {
			sawShutdown = true
		}
	}
	if !sawReady || !sawShutdown {
		t.Fatalf("ready/shutdown missing: %+v", lines)
	}
}

func TestHostForeignMessageDropped(t *testing.T) {
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	// Wrong-session command must be dropped silently (no error), then shutdown.
	lines := driveHost(t, factory, []string{
		desc,
		`{"type":"interrupt","sessionId":"OTHER"}`,
		`{"type":"shutdown","sessionId":"s"}`,
	})
	for _, m := range lines {
		if m["type"] == "host:error" {
			t.Fatalf("foreign message produced an error: %+v", m)
		}
	}
}

// blockingWriter blocks every Write until release is closed, modeling a wedged
// stdout. The command loop must NEVER block waiting on it.
type blockingWriter struct {
	release chan struct{}
	mu      sync.Mutex
	buf     bytes.Buffer
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

// Fix 3: the command loop must keep servicing commands even when stdout is wedged.
// With a blocking writer, an approval:decide (which posts approval:decided) must
// NOT park the loop; a subsequent shutdown must still reach teardown. The teardown
// path's synchronous final frame would block on the wedged writer, so we release
// the writer after observing the loop has advanced — the key assertion is the loop
// did not deadlock before teardown.
func TestHostCommandLoopNotBlockedByWedgedStdout(t *testing.T) {
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }

	pr, pw := io.Pipe()
	bw := &blockingWriter{release: make(chan struct{})}
	var errb syncBuf

	h := NewHost(factory, pr, bw, &errb)
	exited := make(chan struct{})
	var once sync.Once
	h.exit = func(int) { once.Do(func() { close(exited) }); runtimeGoexit() }

	go h.Run(context.Background())

	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	// Feed: descriptor, a decide for a (non-existent) approval (a no-op resolve that
	// still proves the loop services decide), then shutdown — all while stdout is
	// wedged. The loop processing these without blocking is the regression guard.
	feedDone := make(chan struct{})
	go func() {
		defer close(feedDone)
		for _, line := range []string{
			desc,
			`{"type":"approval:decide","sessionId":"s","approvalId":"missing","decision":"approved"}`,
			`{"type":"shutdown","sessionId":"s"}`,
		} {
			_, _ = pw.Write([]byte(line + "\n"))
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// The loop must reach teardown (handleCommand→shutdown→teardown) even though no
	// write has drained. teardown.sendSync will block on the wedged writer, so release
	// it shortly after the commands are fed; the loop must have advanced to teardown
	// WITHOUT having blocked on the earlier ready/decided enqueues.
	<-feedDone
	time.Sleep(30 * time.Millisecond)
	close(bw.release) // unblock the final synchronous shutdown frame

	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("host command loop deadlocked on a wedged stdout")
	}
	_ = pw.Close()
	_ = pr.Close()
}

func TestHostHibernateCarriesResume(t *testing.T) {
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	lines := driveHost(t, factory, []string{desc, `{"type":"hibernate","sessionId":"s"}`})
	var saw bool
	for _, m := range lines {
		if m["type"] == "host:shutdown" && m["reason"] == "hibernate" && m["resumeSessionId"] == "s" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("hibernate resume handle missing: %+v", lines)
	}
}

// driveHostCode is driveHost plus the exit code teardown actually reported —
// driveHost's own injected exit discards it, but the regression this section
// guards against IS the exit code (a fatal or lost-frame outcome silently
// reported as a clean 0).
func driveHostCode(t *testing.T, factory AppFactory, inputs []string) ([]map[string]any, int) {
	t.Helper()
	pr, pw := io.Pipe()
	out := &syncBuf{}
	var errb syncBuf

	h := NewHost(factory, pr, out, &errb)
	t.Cleanup(h.tr.Close)
	exited := make(chan struct{})
	var once sync.Once
	code := -1
	h.exit = func(c int) { once.Do(func() { code = c; close(exited) }); runtimeGoexit() }

	go h.Run(context.Background())

	go func() {
		for _, line := range inputs {
			_, _ = pw.Write([]byte(line + "\n"))
			time.Sleep(15 * time.Millisecond)
		}
	}()

	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("host did not exit")
	}
	_ = pw.Close()
	_ = pr.Close()
	time.Sleep(10 * time.Millisecond)
	return out.lines(), code
}

// Regression for doc finding NH-008 (exit code half): a fatal shutdown reason must
// never be reported as a clean process exit — a supervisor or log reading exit
// status 0 would conclude the session ended normally when it did not.
func TestFatalTeardownReasonExitsNonZero(t *testing.T) {
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }
	// A bad descriptor drives teardown with ShutdownError.
	_, code := driveHostCode(t, factory, []string{`{"sessionId":"s","not":"a descriptor"}`})
	if code == 0 {
		t.Error("a bad-descriptor (fatal) teardown reported exit code 0")
	}
}

// Counterpart: an ordinary clean shutdown must still exit 0 — the fix must not
// turn every exit non-zero, only the ones that actually went wrong.
func TestCleanTeardownExitsZero(t *testing.T) {
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	_, code := driveHostCode(t, factory, []string{desc, `{"type":"shutdown","sessionId":"s"}`})
	if code != 0 {
		t.Errorf("an ordinary clean shutdown reported exit code %d, want 0", code)
	}
}

// blockingApp's Shutdown never returns until release is closed, modeling a stuck
// subsystem — the case appShutdownTimeout exists for.
type blockingApp struct {
	fakeApp
	release chan struct{}
}

func (a *blockingApp) Shutdown(context.Context) error {
	<-a.release
	return nil
}

// Regression for doc finding NH-008 (bounded-wait half): App.Shutdown hanging must
// not hang teardown forever — it exits (non-zero, since a subsystem never
// confirmed it closed) once the bound elapses, rather than leaving an invisible
// process holding the project lease.
func TestAppShutdownTimeoutStillExits(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // don't leak the blocked goroutine past the test
	app := &blockingApp{release: release}
	factory := func(context.Context, AppParams) (App, error) { return app, nil }

	pr, pw := io.Pipe()
	out := &syncBuf{}
	var errb syncBuf
	h := NewHost(factory, pr, out, &errb)
	h.appShutdownTimeout = 50 * time.Millisecond // don't make the test wait 10s
	exited := make(chan struct{})
	var once sync.Once
	code := -1
	h.exit = func(c int) { once.Do(func() { code = c; close(exited) }); runtimeGoexit() }

	go h.Run(context.Background())
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	go func() {
		_, _ = pw.Write([]byte(desc + "\n"))
		time.Sleep(30 * time.Millisecond)
		_, _ = pw.Write([]byte(`{"type":"shutdown","sessionId":"s"}` + "\n"))
	}()

	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("teardown hung forever on a stuck App.Shutdown instead of exiting on the bound")
	}
	_ = pw.Close()
	_ = pr.Close()
	if code == 0 {
		t.Error("a timed-out App.Shutdown reported exit code 0, want non-zero")
	}
}

// Regression for doc finding NH-009: a wake burst that landed in pendingWake
// before h.ready was set must not be stranded until unrelated later activity
// happens to call reactWake again. This reproduces the race's exact precondition
// deterministically — no goroutine-timing luck involved — rather than hoping a
// StartScheduler callback's `go h.reactWake()` happens to lose that race, which it
// is not GUARANTEED to (Go does not promise a freshly spawned goroutine runs
// before the next few statements in its parent), and uses a real cooperative
// session (agent.Session's methods are not nil-receiver-safe, so fakeApp's default
// nil session would panic inside reactWake and could mask the very thing this test
// checks). It calls checkPendingWakeAfterReady directly — the actual fix — rather
// than re-deriving its logic inline, so a regression in that method's wiring shows
// up here too.
func TestPendingWakeQueuedBeforeReadyFiresOnceReadyIsSet(t *testing.T) {
	sess := newWakeSession()
	h := NewHost(nil, strings.NewReader(""), io.Discard, io.Discard)
	h.tr.start()
	t.Cleanup(h.tr.Close)
	h.runCtx, h.runCancel = context.WithCancel(context.Background())
	t.Cleanup(h.runCancel)
	h.sessionID = "s"
	h.state = stateRunning
	h.app = &shutdownProbeApp{sess: sess}
	h.session = sess
	h.bridge = NewBridge(BridgeOptions{SessionID: "s", Post: func(HostEvent) {}})

	// h.ready is still false: reproduce a scheduler callback that appended to
	// pendingWake and tried reactWake before readiness was published.
	h.turnMu.Lock()
	h.pendingWake = append(h.pendingWake, domain.QueueEvent{
		ID: "ev1", Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "agent finished", Summary: "terminal settled",
	})
	h.turnMu.Unlock()
	h.reactWake() // synchronous: must bail immediately on !h.ready
	if n := sess.sendCount(); n != 0 {
		t.Fatalf("test setup invalid: reactWake started %d turn(s) before ready was ever set", n)
	}

	h.turnMu.Lock()
	h.ready = true
	h.turnMu.Unlock()

	h.checkPendingWakeAfterReady() // the fix under test

	select {
	case <-sess.started:
	case <-time.After(2 * time.Second):
		t.Fatal("a wake burst queued before ready never fired after checkPendingWakeAfterReady")
	}
	close(sess.release)
}

// Regression for doc finding NH-016: an oversized inbound line is a protocol
// violation, not a session ending on purpose — it must not be reported identically
// to a clean parent-driven exit.
func TestOversizedInboundLineIsBadFrameNotCleanExit(t *testing.T) {
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	huge := strings.Repeat("a", maxFrameBytes+1)
	lines := driveHost(t, factory, []string{desc, huge})

	var sawBadFrame, sawErrorShutdown bool
	for _, m := range lines {
		if m["type"] == "host:error" && m["code"] == "bad-frame" {
			sawBadFrame = true
		}
		if m["type"] == "host:shutdown" && m["reason"] == "error" {
			sawErrorShutdown = true
		}
	}
	if !sawBadFrame {
		t.Errorf("no bad-frame host:error for an oversized inbound line: %+v", lines)
	}
	if !sawErrorShutdown {
		t.Errorf("an oversized inbound line was reported as a clean exit instead of error: %+v", lines)
	}
}

// Regression for doc finding NH-017: a panic during boot (before host:ready) must
// reliably report bootstrap-error to the parent. onPanic's pre-ready branch now
// uses the same synchronous, delivery-confirmed path (reportSync) every other
// fatal pre-app path already uses, instead of the fire-and-forget report().
func TestPreReadyPanicReportsBootstrapError(t *testing.T) {
	factory := func(context.Context, AppParams) (App, error) { panic("boom") }
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	lines := driveHost(t, factory, []string{desc})

	var saw bool
	for _, m := range lines {
		if m["type"] == "host:error" && m["code"] == "bootstrap-error" {
			saw = true
		}
	}
	if !saw {
		t.Fatalf("pre-ready panic did not report bootstrap-error: %+v", lines)
	}
}

// gatedRecordingWriter blocks the first Write until release is closed (closing
// entered the instant that call begins), then records every write's bytes.
type gatedRecordingWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	buf     bytes.Buffer
}

func (w *gatedRecordingWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *gatedRecordingWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// This is the ordering guarantee TestPreReadyPanicReportsBootstrapError cannot
// prove: driveHost's injected exit only kills the Run() goroutine via
// runtime.Goexit (the writer goroutine survives) and driveHost sleeps 10ms before
// reading output, so the OLD fire-and-forget report() would usually still get its
// queued frame flushed in time — that test can pass against either the old or new
// code. This one wedges the writer mid-delivery and asserts exit is NOT called
// until the writer is released and the frame is actually on the wire, which only
// reportSync's bounded wait-for-delivery can satisfy.
func TestPreReadyPanicBlocksExitUntilTheReportIsDelivered(t *testing.T) {
	w := &gatedRecordingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	factory := func(context.Context, AppParams) (App, error) { panic("boom") }

	pr, pw := io.Pipe()
	h := NewHost(factory, pr, w, io.Discard)
	t.Cleanup(h.tr.Close)
	exited := make(chan struct{})
	var once sync.Once
	code := -1
	h.exit = func(c int) { once.Do(func() { code = c; close(exited) }); runtimeGoexit() }

	go h.Run(context.Background())
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	go func() { _, _ = pw.Write([]byte(desc + "\n")) }()
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })

	select {
	case <-w.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the panic report never attempted delivery")
	}

	select {
	case <-exited:
		t.Fatal("exit was called before the bootstrap-error frame was confirmed delivered")
	case <-time.After(100 * time.Millisecond):
	}

	close(w.release)
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("host never exited after the writer was released")
	}
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if !strings.Contains(w.String(), "bootstrap-error") {
		t.Errorf("bootstrap-error frame was not written: %q", w.String())
	}
}

// Regression for doc finding NH-016 (default branch): a generic stdin read error
// (not clean EOF, not an oversized line) must be reported as `error`, not folded
// into the same `exit` a normal parent-driven close reports.
func TestGenericReadErrorIsFatalNotCleanExit(t *testing.T) {
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }
	pr, pw := io.Pipe()
	out := &syncBuf{}
	var errb syncBuf
	h := NewHost(factory, pr, out, &errb)
	t.Cleanup(h.tr.Close)
	exited := make(chan struct{})
	var once sync.Once
	code := -1
	h.exit = func(c int) { once.Do(func() { code = c; close(exited) }); runtimeGoexit() }

	go h.Run(context.Background())
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	go func() { _, _ = pw.Write([]byte(desc + "\n")) }()

	// Give the descriptor a moment to be processed, then break the pipe with a
	// genuine read error (distinct from a clean Close(), which reports io.EOF).
	time.Sleep(30 * time.Millisecond)
	_ = pr.CloseWithError(errors.New("simulated broken pipe"))

	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("host did not exit")
	}
	if code == 0 {
		t.Error("a generic stdin read error reported exit code 0, want non-zero")
	}
	var sawErrorShutdown bool
	for _, m := range out.lines() {
		if m["type"] == "host:shutdown" && m["reason"] == "error" {
			sawErrorShutdown = true
		}
	}
	if !sawErrorShutdown {
		t.Error("a generic stdin read error was not reported as an error shutdown")
	}
}

// Counterpart: a genuinely clean EOF (the parent closing stdin on purpose, with no
// explicit shutdown command) must still exit cleanly — the read-error
// classification must not turn every EOF into a false positive.
func TestCleanEOFWithoutShutdownCommandExitsZero(t *testing.T) {
	// Not driveHostCode: it closes the pipe only AFTER waiting for exit, but here
	// EOF (closing the pipe) is what MUST cause the exit — closing it early, right
	// after the descriptor, is the whole point of this test.
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }
	pr, pw := io.Pipe()
	out := &syncBuf{}
	var errb syncBuf
	h := NewHost(factory, pr, out, &errb)
	t.Cleanup(h.tr.Close)
	exited := make(chan struct{})
	var once sync.Once
	code := -1
	h.exit = func(c int) { once.Do(func() { code = c; close(exited) }); runtimeGoexit() }

	go h.Run(context.Background())
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	_, _ = pw.Write([]byte(desc + "\n"))
	time.Sleep(30 * time.Millisecond) // let the descriptor be processed and host:ready post
	_ = pw.Close()                    // clean EOF: no shutdown command ever sent

	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		t.Fatal("host did not exit on clean EOF")
	}
	_ = pr.Close()
	time.Sleep(10 * time.Millisecond)

	var sawExit bool
	for _, m := range out.lines() {
		if m["type"] == "host:shutdown" && m["reason"] == "exit" {
			sawExit = true
		}
	}
	if !sawExit {
		t.Errorf("clean EOF was not reported as an exit shutdown: %+v", out.lines())
	}
	if code != 0 {
		t.Errorf("clean EOF exited with code %d, want 0", code)
	}
}

// Regression for the exit-code half of doc finding NH-008: teardown's OWN attempt
// to deliver host:shutdown can itself fail (the queue never had room, or the
// writer never confirmed within budget) even when everything up to that point was
// clean — that must still be reported as a non-clean exit, not folded into 0.
func TestTerminalFrameDeliveryFailureExitsNonZero(t *testing.T) {
	w := &enteringBlockingWriter{entered: make(chan struct{}), release: make(chan struct{})}
	factory := func(context.Context, AppParams) (App, error) { return &fakeApp{}, nil }

	pr, pw := io.Pipe()
	h := NewHost(factory, pr, w, io.Discard)
	t.Cleanup(func() { close(w.release); h.tr.Close() })
	exited := make(chan struct{})
	var once sync.Once
	code := -1
	h.exit = func(c int) { once.Do(func() { code = c; close(exited) }); runtimeGoexit() }

	go h.Run(context.Background())
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":4}`
	go func() {
		_, _ = pw.Write([]byte(desc + "\n"))
		time.Sleep(30 * time.Millisecond)
		_, _ = pw.Write([]byte(`{"type":"shutdown","sessionId":"s"}` + "\n"))
	}()
	t.Cleanup(func() { _ = pw.Close(); _ = pr.Close() })

	// Wait for the writer to enter Write() (the ready frame), then wedge it and
	// flood the queue so the LATER host:shutdown priority frame has no room to
	// enqueue within its budget — sendPriority's own failure path, not a write
	// error on an already-accepted frame.
	select {
	case <-w.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the writer never entered Write()")
	}
	for i := 0; i < outQueueDepth+8; i++ {
		h.tr.send("s", EvTurnToken{TurnID: "t", Chunk: "filler"})
	}

	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("host did not exit")
	}
	if code == 0 {
		t.Error("a host:shutdown that failed to enqueue reported exit code 0, want non-zero")
	}
}

// Regression: a parent-context cancellation racing closeOnContext's stdin close
// must not report a different exit reason than the SAME cancellation would via
// Run()'s own <-h.runCtx.Done() branch — the two must agree regardless of which
// select case happens to fire. Exercises teardownForReadError directly rather than
// trying to force the actual goroutine-scheduling race deterministically.
func TestReadErrorAfterOwnContextCancelUsesShutdownReason(t *testing.T) {
	out := &syncBuf{}
	h := NewHost(nil, strings.NewReader(""), out, io.Discard)
	h.tr.start()
	t.Cleanup(h.tr.Close)
	h.runCtx, h.runCancel = context.WithCancel(context.Background())
	h.sessionID = "s"
	h.runCancel() // simulate: our own cancellation already happened

	exited := make(chan struct{})
	h.exit = func(int) { close(exited); runtimeGoexit() }
	go func() {
		defer func() { _ = recover() }()
		h.teardownForReadError(errors.New("read tcp: use of closed network connection"))
	}()

	select {
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("teardownForReadError did not tear down")
	}
	time.Sleep(10 * time.Millisecond)

	// h.transportFailed was never set (no actual write failure occurred here), so
	// shutdownReason() resolves to ShutdownExit — a generic read error caused by
	// OUR OWN prior cancellation must not be misclassified as a transport failure
	// just because it happened to arrive through the read-error path instead of the
	// <-h.runCtx.Done() select case.
	var reason string
	for _, m := range out.lines() {
		if m["type"] == "host:shutdown" {
			reason, _ = m["reason"].(string)
		}
	}
	if reason != "exit" {
		t.Errorf("host:shutdown reason = %q, want %q (a read error after our own context cancel "+
			"must agree with what the <-h.runCtx.Done() branch would have reported)", reason, "exit")
	}
}
