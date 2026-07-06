package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/ipc"
)

// ErrProjectBusy reports that another live process owns this project's DB —
// most likely a second cockpit. The caller should surface it and exit rather
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
	if err := lock.Acquire(lockCtx, 250*time.Millisecond); err != nil {
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

// credentialsFromConfig snapshots the freshest connection credentials this
// process resolved, for the daemon's later detached spans.
func credentialsFromConfig(cfg config.AppConfig) *ipc.Credentials {
	c := &ipc.Credentials{McpURL: cfg.McpURL, McpToken: cfg.McpToken}
	if v := os.Getenv("DAINTREE_BACKEND_URL"); v != "" {
		c.BackendURL = v
	}
	return c
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
