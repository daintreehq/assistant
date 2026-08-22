package host

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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

	mu      sync.Mutex
	rearmed []string
}

func (f *fakeApp) SetHooks(h AppHooks)              { f.hooks = h }
func (f *fakeApp) ConnectMCP(context.Context) error { return nil }
func (f *fakeApp) RunCommand(context.Context, string) CommandOutcome {
	return CommandOutcome{}
}
func (f *fakeApp) StartScheduler(func(events []domain.QueueEvent)) {}
func (f *fakeApp) Session() *agent.Session                         { return f.session }
func (f *fakeApp) RiskOf(string) (domain.RiskClass, bool)          { return "", false }
func (f *fakeApp) Config() config.AppConfig                        { return config.AppConfig{} }
func (f *fakeApp) Shutdown(context.Context) error                  { return nil }

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
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":3}`
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
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":3}`
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

	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":3}`
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
	desc := `{"sessionId":"s","windowId":1,"projectId":"p","cwd":"/x","tier":"system","protocolVersion":3}`
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
