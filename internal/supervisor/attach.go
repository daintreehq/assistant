package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/ipc"
)

// ErrProjectBusy reports that another live process owns this project's DB —
// most likely a second attached session. The caller should surface it and exit rather
// than wait forever.
type ErrProjectBusy struct{ Pid int }

func (e ErrProjectBusy) Error() string {
	if e.Pid > 0 {
		return fmt.Sprintf("another Daintree assistant (pid %d) is already using this project", e.Pid)
	}
	return "another Daintree assistant is already using this project"
}

// Ownership is what an interactive/one-shot process holds while it owns the
// project DB: the flock lease plus (when a daemon exists) the open attach
// connection that keeps the daemon standing down. Release order matters:
// callers shut the App down FIRST (closing the store), then Release — which
// frees the lease and finally drops the attach connection so the daemon
// resumes supervising immediately.
type Ownership struct {
	Lock   *ipc.FileLock
	Client *ipc.Client // nil when no daemon is running
}

// Release hands the project back (see ordering note on the type).
func (o *Ownership) Release() {
	if o == nil {
		return
	}
	if o.Lock != nil {
		o.Lock.Release()
	}
	if o.Client != nil {
		_ = o.Client.Close()
	}
}

// AcquireOptions tunes ownership acquisition.
type AcquireOptions struct {
	// SpawnDaemon starts a supervisor daemon when none is running (interactive
	// paths). One-shot/doctor runs leave it false — a script probe must not
	// litter the machine with daemons.
	SpawnDaemon bool
	// Version is stamped into a spawned daemon's descriptor.
	Version string
	// WaitFor bounds the whole acquisition (attach handover + flock). Zero ⇒ 60s.
	WaitFor time.Duration
	// Log receives progress lines ("waiting for the supervisor to hand over…").
	Log func(string)
}

// AcquireOwnership makes this process the project's DB owner: ensure/notify
// the daemon (spawn if requested and absent, attach so it yields and receives
// fresh credentials), then take the owner flock. Call BEFORE app.Create; hold
// the returned Ownership until after App.Shutdown.
func AcquireOwnership(ctx context.Context, cfg config.AppConfig, opts AcquireOptions) (*Ownership, error) {
	logf := opts.Log
	if logf == nil {
		logf = func(string) {}
	}
	waitFor := opts.WaitFor
	if waitFor <= 0 {
		waitFor = 60 * time.Second
	}
	deadline := time.Now().Add(waitFor)

	socketPath, err := ipc.SocketPathFor(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	client, err := ipc.Dial(socketPath, 2*time.Second)
	if errors.Is(err, ipc.ErrNoDaemon) && opts.SpawnDaemon {
		if spawnErr := spawnDaemon(cfg, opts.Version); spawnErr != nil {
			// A daemon is durability, not a prerequisite: log and run solo.
			logf("supervisor daemon could not be started: " + spawnErr.Error())
		} else {
			// Give the fresh daemon a moment to bind its socket; missing it is fine
			// (we attach on the NEXT launch, and the daemon stands by on the flock).
			for i := 0; i < 20 && client == nil; i++ {
				time.Sleep(150 * time.Millisecond)
				if c, derr := ipc.Dial(socketPath, time.Second); derr == nil {
					client = c
				}
			}
		}
	} else if err != nil && !errors.Is(err, ipc.ErrNoDaemon) {
		logf("supervisor socket: " + err.Error())
	}

	if client != nil {
		var rep ipc.AttachReply
		attachCtx, cancel := context.WithDeadline(ctx, deadline)
		logf("attaching to the project supervisor…")
		callErr := client.Call(attachCtx, ipc.ReqAttach, ipc.AttachRequest{
			ClientPid:   os.Getpid(),
			Credentials: credentialsFromConfig(cfg),
		}, &rep)
		cancel()
		switch {
		case callErr != nil:
			// A flaky daemon must not block the human's assistant: drop the
			// connection and rely on the flock below for correctness.
			logf("supervisor attach failed (continuing without it): " + callErr.Error())
			_ = client.Close()
			client = nil
		case rep.OwnerBusy:
			_ = client.Close()
			return nil, ErrProjectBusy{Pid: rep.OwnerPid}
		}
	}

	lock := ipc.NewFileLock(filepath.Join(cfg.StateDir, ipc.OwnerLockName))
	lockCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	stopNotices := noteOwnerLockWait(lockCtx, lock, logf)
	err = lock.Acquire(lockCtx, 250*time.Millisecond)
	stopNotices()
	if err != nil {
		if client != nil {
			_ = client.Close()
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, ErrProjectBusy{Pid: ipc.ReadLockHolderPid(lock.Path())}
		}
		return nil, err
	}
	return &Ownership{Lock: lock, Client: client}, nil
}

// How long a contended lease waits before saying so, and how often it repeats.
//
// The first is short on purpose: an uncontended lock is taken in microseconds, so
// anything still outstanding after a second is genuinely held by someone else and the
// person watching has already started wondering. The repeat is slow enough that a full
// 60s wait costs six lines rather than a scroll of them.
const (
	ownerLockFirstNotice = time.Second
	ownerLockNoticeEvery = 10 * time.Second
)

// noteOwnerLockWait narrates a contended owner lease until the returned stop is called.
//
// AcquireOptions.Log has always documented "waiting for the supervisor to hand over…"
// as one of the lines it receives, and no such line was ever emitted. The flock wait is
// the one step of acquisition that can last the entire deadline, and it was the only
// one that said nothing at all — so a second Daintree opening the same project spawned
// an engine that opened no file, no socket and no database, wrote nothing to stderr,
// and simply never became ready. Indistinguishable, from outside, from a hung process.
//
// The holder pid is the actionable half: it names the process to close. Reading it here
// respects ReadLockHolderPid's contract — it is diagnostic only, and this is a
// diagnostic; the flock remains the authority on who owns the project.
func noteOwnerLockWait(ctx context.Context, lock *ipc.FileLock, logf func(string)) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		timer := time.NewTimer(ownerLockFirstNotice)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if pid := ipc.ReadLockHolderPid(lock.Path()); pid > 0 {
				logf(fmt.Sprintf("waiting for the project lease held by pid %d…", pid))
			} else {
				logf("waiting for the project lease…")
			}
			timer.Reset(ownerLockNoticeEvery)
		}
	}()
	// Joins the goroutine so the caller cannot return while a notice is mid-write and
	// have it land after whatever the caller says next.
	return func() {
		cancel()
		<-done
	}
}

// credentialsFromConfig snapshots the freshest connection credentials this
// process resolved, for the daemon's later detached spans.
func credentialsFromConfig(cfg config.AppConfig) *ipc.Credentials {
	// The RESOLVED endpoint, not os.Getenv: --backend-url never reaches the environment,
	// so reading it there would hand an already-running daemon a stale endpoint (or
	// none) while this process talks to the one the operator actually named.
	return &ipc.Credentials{
		McpURL:     cfg.McpURL,
		McpToken:   cfg.McpToken,
		BackendURL: cfg.BackendURL,
	}
}

// QueryStatus asks the project's daemon for its status snapshot.
// (nil, ipc.ErrNoDaemon) when none is running.
func QueryStatus(ctx context.Context, stateDir string) (*ipc.StatusReply, error) {
	socketPath, err := ipc.SocketPathFor(stateDir)
	if err != nil {
		return nil, err
	}
	client, err := ipc.Dial(socketPath, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	var rep ipc.StatusReply
	if err := client.Call(ctx, ipc.ReqStatus, nil, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// QueryTimers asks the project's daemon what is scheduled and what recently fired.
// (nil, ipc.ErrNoDaemon) when no daemon is running.
//
// This is the answer for a project whose assistant is NOT open. While a session is
// attached it holds the owner lease and the daemon has no App to read from, so it
// answers with an error naming that — a caller with both routes available should
// prefer the attached session and fall back here.
func QueryTimers(ctx context.Context, stateDir string) (*ipc.TimersReply, error) {
	socketPath, err := ipc.SocketPathFor(stateDir)
	if err != nil {
		return nil, err
	}
	client, err := ipc.Dial(socketPath, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	var rep ipc.TimersReply
	if err := client.Call(ctx, ipc.ReqTimers, nil, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// CancelTimer asks the project's daemon to retire one timer and revoke the
// automation grants scoped to it. ipc.ErrNoDaemon when none is running.
//
// The CALLER is responsible for having confirmed it with a human: this is a D1
// mutation and the socket carries no confirmation channel.
func CancelTimer(ctx context.Context, stateDir, timerID string) (*ipc.TimerCancelReply, error) {
	socketPath, err := ipc.SocketPathFor(stateDir)
	if err != nil {
		return nil, err
	}
	client, err := ipc.Dial(socketPath, 2*time.Second)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	var rep ipc.TimerCancelReply
	if err := client.Call(ctx, ipc.ReqTimerCancel, ipc.TimerCancelRequest{TimerID: timerID}, &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// RequestShutdown asks the project's daemon to exit. ipc.ErrNoDaemon when none
// is running.
func RequestShutdown(ctx context.Context, stateDir string) error {
	socketPath, err := ipc.SocketPathFor(stateDir)
	if err != nil {
		return err
	}
	client, err := ipc.Dial(socketPath, 2*time.Second)
	if err != nil {
		return err
	}
	defer client.Close()
	return client.Call(ctx, ipc.ReqShutdown, nil, nil)
}

// NotifyAuthChanged tells a running daemon for this project that the account credential
// changed, carrying only the new revision marker.
//
// Best effort by design, and the caller ignores the error. The daemon already polls the
// shared marker before every wake, so one that is stopped, wedged, or simply not running
// stops on its own within a tick — this only removes the delay for the daemon that is
// reachable, which is the common case and the one where someone watching their terminal
// expects logout to take effect immediately.
//
// It sends a MARKER, never a token. See ipc.Credentials for why the daemon is never
// handed a credential.
func NotifyAuthChanged(ctx context.Context, stateDir, revision string) error {
	socketPath, err := ipc.SocketPathFor(stateDir)
	if err != nil {
		return err
	}
	client, err := ipc.Dial(socketPath, 2*time.Second)
	if err != nil {
		return err // no daemon listening: nothing to tell, and nothing to report
	}
	defer client.Close()
	return client.Call(ctx, ipc.ReqAuthChanged, ipc.AuthChangedRequest{Revision: revision}, nil)
}
