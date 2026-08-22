// Package ipc is the local coordination layer for the persistent supervisor:
// flock-based ownership leases, the per-project control socket path, and the
// NDJSON request/response protocol the daemon serves on it.
//
// Ownership model (see docs/SUPERVISOR.md): exactly ONE process at a time —
// an attached session/REPL/one-shot OR the supervisor daemon — holds the
// project's owner lock and with it the right to open state.db and run the
// scheduler + async coordinator. flock is the primitive because the kernel
// releases it when the holder dies, which is what makes an attached-session crash hand
// supervision back to the daemon without any cleanup code running.
package ipc

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"
)

// OwnerLockName / DaemonLockName are the two lease files, both living in the
// per-project state dir (0700). owner.lock serializes DB ownership between a
// attached session and the daemon; daemon.lock makes the daemon a per-project singleton.
const (
	OwnerLockName  = "owner.lock"
	DaemonLockName = "daemon.lock"
)

// FileLock is a held (or not-yet-held) flock on a named file. The *os.File
// must stay referenced while the lock is held — a GC'd file closes its fd and
// silently releases the lease. Safe for concurrent use: the daemon's run loop
// acquires/releases while its attach handler reads Held() on a connection
// goroutine.
type FileLock struct {
	path string
	mu   sync.Mutex
	f    *os.File
}

// NewFileLock prepares a lock handle without acquiring anything.
func NewFileLock(path string) *FileLock { return &FileLock{path: path} }

// Path returns the lock file path.
func (l *FileLock) Path() string { return l.path }

// TryAcquire attempts a non-blocking exclusive lock. (false, nil) means a live
// process holds it. On success the holder's pid is written into the file —
// purely diagnostic (the flock is the authority; the pid is for `status` and
// humans poking at the state dir).
func (l *FileLock) TryAcquire() (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		return true, nil // already held by this handle
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("open lock %s: %w", l.path, err)
	}
	ok, err := flockExclusive(int(f.Fd()))
	if err != nil || !ok {
		_ = f.Close()
		return false, err
	}
	// Best-effort pid stamp; never a reason to fail an acquired lock.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	l.f = f
	return true, nil
}

// Acquire polls TryAcquire until it succeeds, ctx is done, or a real error
// occurs. retryEvery bounds the handoff latency (the daemon reclaiming after a
// attached session exits, or an attached session waiting out a daemon's in-flight wake turn).
func (l *FileLock) Acquire(ctx context.Context, retryEvery time.Duration) error {
	if retryEvery <= 0 {
		retryEvery = 250 * time.Millisecond
	}
	for {
		ok, err := l.TryAcquire()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryEvery):
		}
	}
}

// Held reports whether this handle currently holds the lock.
func (l *FileLock) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f != nil
}

// Release drops the lock and closes the file. Idempotent. The lock FILE is
// deliberately left on disk — unlinking a lock file that another process may
// be about to open reintroduces the classic stale-lock race.
func (l *FileLock) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	_ = flockRelease(int(l.f.Fd()))
	_ = l.f.Close()
	l.f = nil
}

// ReadLockHolderPid returns the pid stamped into a lock file, 0 when absent or
// unparseable. Diagnostic only — never use it to decide liveness (that is the
// flock's job).
func ReadLockHolderPid(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	s := string(b)
	for i, c := range s {
		if c == '\n' || c == '\r' {
			s = s[:i]
			break
		}
	}
	pid, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return pid
}
