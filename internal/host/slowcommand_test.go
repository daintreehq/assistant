package host

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowApp is a fake App that parks a command until released.
//
// Embeds the interface rather than implementing all nineteen methods: anything this
// path does not use is nil and panics loudly if it is ever called, which is a better
// failure than a silent no-op pretending to be behaviour.
type slowApp struct {
	App
	entered   chan struct{}
	release   chan struct{}
	mu        sync.Mutex
	cancelled bool
}

func (s *slowApp) IsSlowCommand(string) bool { return true }

// Slow but NOT exclusive — the `/login` shape: it waits on a browser and holds nothing,
// so the session stays open to prompts while it does.
func (s *slowApp) IsExclusiveCommand(string) bool { return false }

func (s *slowApp) RunCommandWithProgress(ctx context.Context, line string, progress func(string)) CommandOutcome {
	if progress != nil {
		progress("working")
	}
	select {
	case s.entered <- struct{}{}:
	default:
	}
	select {
	case <-s.release:
		return CommandOutcome{Text: "done"}
	case <-ctx.Done():
		s.mu.Lock()
		s.cancelled = true
		s.mu.Unlock()
		return CommandOutcome{Text: "cancelled"}
	}
}

func (s *slowApp) RunCommand(ctx context.Context, line string) CommandOutcome {
	return s.RunCommandWithProgress(ctx, line, nil)
}

func (s *slowApp) McpStatus() (bool, *int, string) { return false, nil, "" }

func (s *slowApp) wasCancelled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}

// The transport's sink is the package's own lockedBuffer (transport_test.go): the
// writer goroutine writes while the test reads, and a bare bytes.Buffer across those
// two is a race in the fixture.
func newSlowHost(t *testing.T) (*Host, *slowApp, *lockedBuffer) {
	t.Helper()
	out := &lockedBuffer{}
	tr := newTransport(strings.NewReader(""), out, &lockedBuffer{})
	tr.start()
	app := &slowApp{entered: make(chan struct{}, 1), release: make(chan struct{})}
	h := &Host{app: app, tr: tr, sessionID: "ses_slow"}
	h.runCtx, h.runCancel = context.WithCancel(context.Background())
	t.Cleanup(func() { h.runCancel(); close(app.release) })
	return h, app, out
}

// A second account command is refused while one is still running.
//
// Not a style preference. These commands change the account, so two in flight settle in
// whichever order the network returns rather than the order they were typed — a /logout
// sent after a /login can land first and leave the session signed in. The manager's own
// `inLogin` guard only excludes a second LOGIN; it says nothing about a logout arriving
// beside one.
func TestASecondSlowCommandIsRefusedWhileOneIsRunning(t *testing.T) {
	h, app, sink := newSlowHost(t)

	h.handleSlashCommandAsync("/login")
	select {
	case <-app.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the first command never started")
	}

	h.handleSlashCommandAsync("/logout")

	h.turnMu.Lock()
	busy := h.cmdBusy
	h.turnMu.Unlock()
	if !busy {
		t.Fatal("the host did not record a command in flight")
	}
	// The refusal has to be legible: the user is being told to go and finish something.
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(sink.String(), "still waiting on you") {
		if time.Now().After(deadline) {
			t.Fatalf("no refusal was reported for the second command:\n%s", sink.String())
		}
		time.Sleep(time.Millisecond)
	}
}

// Cancelling unwinds a parked command instead of leaving shutdown to time out on it.
//
// A slow command is the only worker that waits on a PERSON — a sign-in blocks until a
// browser round trip finishes or its five-minute window lapses. Before this, teardown
// cancelled prompts and wakes only, so every shutdown during a login burned the whole
// join timeout and then closed the App underneath a command still holding it.
func TestCancellingUnwindsAParkedCommand(t *testing.T) {
	h, app, _ := newSlowHost(t)

	h.handleSlashCommandAsync("/login")
	select {
	case <-app.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the command never started")
	}

	h.cancelCommand()

	deadline := time.Now().Add(2 * time.Second)
	for !app.wasCancelled() {
		if time.Now().After(deadline) {
			t.Fatal("the parked command never observed cancellation — shutdown would wait out its join timeout")
		}
		time.Sleep(time.Millisecond)
	}

	// And the slot is released, so a retry is possible rather than the host believing a
	// command is still running forever.
	for time.Now().Before(deadline) {
		h.turnMu.Lock()
		busy := h.cmdBusy
		h.turnMu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("cmdBusy stayed set after the command unwound — no further account command could ever run")
}
