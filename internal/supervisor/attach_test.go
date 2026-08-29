package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/ipc"
)

// newOwnershipTestDirs isolates a run from the real home: the socket root would
// otherwise default to ~/.daintree/sockets, and a test has no business creating
// directories there. The state dir is hashed into the socket name, so a temp one
// resolves to a socket nothing is listening on — which is the no-daemon path these
// tests want, without a daemon ever being spawned.
//
// The socket root is NOT t.TempDir(): that names the directory after the test, and
// darwin caps a unix socket path at 104 bytes — `SocketPathFor` refuses anything over
// 100. A test name is easily enough to cross it, so the root gets a short fixed prefix
// and only the state dir uses the descriptive temp dir.
func newOwnershipTestDirs(t *testing.T) string {
	t.Helper()
	sockRoot, err := os.MkdirTemp("", "dsock")
	if err != nil {
		t.Fatalf("socket root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockRoot) })
	t.Setenv(ipc.SocketDirEnv, sockRoot)
	return t.TempDir()
}

// collectLog accumulates AcquireOptions.Log lines. The notice goroutine writes from
// its own goroutine while the caller's path writes from this one, so the mutex is
// load-bearing rather than decorative — `go test -race` fails without it.
type collectLog struct {
	mu    sync.Mutex
	lines []string
}

func (c *collectLog) log(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lines = append(c.lines, line)
}

func (c *collectLog) contains(substr string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range c.lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func (c *collectLog) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.lines...)
}

// A contended lease must SAY it is waiting, and name who holds it.
//
// This is the regression: taking the owner flock is the one step of acquisition that
// can block for the entire deadline, and it used to do so in total silence. A second
// Daintree opening the same project spawned an engine that opened no file, no socket
// and no database, wrote nothing to stderr, and never became ready — from outside,
// indistinguishable from a hang. The holder pid is the actionable half of the notice:
// it names the process to close.
func TestAcquireOwnershipNarratesAContendedLease(t *testing.T) {
	stateDir := newOwnershipTestDirs(t)

	// flock leases attach to the open file description, not the process, so a second
	// handle in THIS process contends exactly as another process would.
	held := ipc.NewFileLock(filepath.Join(stateDir, ipc.OwnerLockName))
	ok, err := held.TryAcquire()
	if err != nil || !ok {
		t.Fatalf("could not pre-hold the owner lock: ok=%v err=%v", ok, err)
	}
	t.Cleanup(held.Release)

	var logged collectLog
	own, err := AcquireOwnership(context.Background(), config.AppConfig{StateDir: stateDir}, AcquireOptions{
		WaitFor: 3 * time.Second,
		Log:     logged.log,
	})
	if own != nil {
		own.Release()
		t.Fatal("acquired an owner lease that another handle already held")
	}

	var busy ErrProjectBusy
	if !errors.As(err, &busy) {
		t.Fatalf("want ErrProjectBusy, got %v", err)
	}
	if busy.Pid != os.Getpid() {
		t.Errorf("busy error named pid %d, want the holder %d", busy.Pid, os.Getpid())
	}
	if !logged.contains("waiting for the project lease") {
		t.Errorf("a contended lease said nothing; lines: %q", logged.snapshot())
	}
	// The pid must be rendered, not just the bare phrase: without it the notice says
	// something is in the way but not what, which is the half that cannot be acted on.
	if want := fmt.Sprintf("pid %d", os.Getpid()); !logged.contains(want) {
		t.Errorf("notice did not name the holder (%s); lines: %q", want, logged.snapshot())
	}
}

// The uncontended path must stay silent and fast.
//
// The notice fires on a one-second timer, which is only correct if an uncontended
// lease is granted well inside it. If that ever stops being true the log fills with
// "waiting…" on every ordinary launch, and a warning that appears every time is a
// warning nobody reads.
func TestAcquireOwnershipSaysNothingWhenTheLeaseIsFree(t *testing.T) {
	stateDir := newOwnershipTestDirs(t)

	var logged collectLog
	started := time.Now()
	own, err := AcquireOwnership(context.Background(), config.AppConfig{StateDir: stateDir}, AcquireOptions{
		WaitFor: 3 * time.Second,
		Log:     logged.log,
	})
	if err != nil {
		t.Fatalf("free lease should be granted, got %v", err)
	}
	t.Cleanup(own.Release)

	if took := time.Since(started); took >= ownerLockFirstNotice {
		t.Errorf("free lease took %s, past the %s notice threshold", took, ownerLockFirstNotice)
	}
	if logged.contains("waiting for the project lease") {
		t.Errorf("uncontended lease announced a wait; lines: %q", logged.snapshot())
	}
}

// The notice goroutine must not outlive the acquisition that started it.
//
// `stop` joins it deliberately: a notice still in flight when AcquireOwnership returns
// would interleave with whatever the caller logs next, and on the host that stream is
// the user's only view of startup.
func TestAcquireOwnershipLeavesNoNoticeGoroutineBehind(t *testing.T) {
	stateDir := newOwnershipTestDirs(t)

	held := ipc.NewFileLock(filepath.Join(stateDir, ipc.OwnerLockName))
	if ok, err := held.TryAcquire(); err != nil || !ok {
		t.Fatalf("could not pre-hold the owner lock: ok=%v err=%v", ok, err)
	}
	t.Cleanup(held.Release)

	var logged collectLog
	if _, err := AcquireOwnership(context.Background(), config.AppConfig{StateDir: stateDir}, AcquireOptions{
		WaitFor: 1500 * time.Millisecond,
		Log:     logged.log,
	}); err == nil {
		t.Fatal("want a busy error")
	}

	// Past the repeat interval: anything still running would append here.
	after := len(logged.snapshot())
	time.Sleep(200 * time.Millisecond)
	if now := len(logged.snapshot()); now != after {
		t.Errorf("notice goroutine still logging after return: %d -> %d lines", after, now)
	}
}
