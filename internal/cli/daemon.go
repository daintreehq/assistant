package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/ipc"
	"github.com/daintreehq/assistant/internal/supervisor"
)

// NoDaemonEnv disables daemon auto-spawn (tests, and an operator kill switch).
// Ownership locking still applies — the flock is correctness, the daemon is
// durability.
const NoDaemonEnv = "DAINTREE_ASSISTANT_NO_DAEMON"

func daemonDisabled() bool {
	v := os.Getenv(NoDaemonEnv)
	return v == "1" || v == "true"
}

// acquireOwnership resolves config for the given overrides and takes the
// project owner lease: attach to (and optionally spawn) the supervisor daemon
// so it yields, then flock. Every App-creating path calls this FIRST and holds
// the result until after App.Shutdown — the lease is what makes the store's
// single-owner assumption true across processes. On a crash the kernel
// releases the flock and the attach connection drops, so the daemon resumes
// supervision without any cleanup code running here.
func acquireOwnership(ctx context.Context, overrides config.ConfigOverrides, spawn bool, wait time.Duration, logf func(string)) (*supervisor.Ownership, error) {
	cfg, err := config.LoadConfig(overrides)
	if err != nil {
		return nil, err
	}
	return supervisor.AcquireOwnership(ctx, cfg, supervisor.AcquireOptions{
		SpawnDaemon: spawn && !daemonDisabled(),
		Version:     buildVersion,
		WaitFor:     wait,
		Log:         logf,
	})
}

// buildVersion is stamped by main (SetVersion) for daemon descriptors/status.
var buildVersion = "dev"

// SetVersion records the build version for daemon spawn/status surfaces.
func SetVersion(v string) { buildVersion = v }

// RunDaemon is the `daintree-assistant daemon` subcommand: the persistent
// per-project supervisor process (normally spawned detached by an interactive
// launch, but equally runnable by hand in the foreground).
func RunDaemon(ctx context.Context, opts Options) int {
	r := render.Stdout()
	overrides, err := buildOverrides(opts, r)
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	// Gate BEFORE the supervisor starts. A signed-out daemon boots happily, adopts the
	// project's watchers and async work, and then 401s inside every autonomous wake turn
	// — supervision that looks alive and silently accomplishes nothing. Never prompt:
	// this process is detached and normally has no terminal.
	if err := ensureSignedIn(ctx, overrides, false); err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	sOpts := supervisor.Options{
		Overrides: overrides,
		Version:   buildVersion,
		Log:       func(s string) { r.Line(s) },
	}
	// TEST-ONLY knobs: compressed cadences + a short idle-exit window let the
	// subprocess e2e suite exercise outage/blocked/idle behavior in seconds.
	if v := os.Getenv("DAINTREE_ASSISTANT_DAEMON_FAST"); v == "1" || v == "true" {
		sOpts.Fast = true
	}
	if v := os.Getenv("DAINTREE_ASSISTANT_DAEMON_IDLE_EXIT_MS"); v != "" {
		if ms, err := time.ParseDuration(v + "ms"); err == nil && ms != 0 {
			sOpts.IdleExit = ms
		}
	}
	return supervisor.Run(ctx, sOpts)
}

// RunDaemonStop asks the project's daemon to exit.
func RunDaemonStop(ctx context.Context, opts Options) int {
	r := render.Stdout()
	cfg, err := loadConfigFromOptions(opts)
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	if err := supervisor.RequestShutdown(ctx, cfg.StateDir); err != nil {
		if errors.Is(err, ipc.ErrNoDaemon) {
			r.Line("no supervisor daemon is running for this project")
			return domain.OneShotExitCode.Success
		}
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	r.Line("supervisor daemon stopping")
	return domain.OneShotExitCode.Success
}

// RunStatus is the `daintree-assistant status` subcommand: the daemon-health
// sibling of `doctor`. It never takes the owner lease — a status probe must
// not disturb a live cockpit or the daemon.
func RunStatus(ctx context.Context, opts Options) int {
	r := render.Stdout()
	cfg, err := loadConfigFromOptions(opts)
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	r.Line("Daintree Assistant — supervisor status")
	r.Line("  state dir      : " + cfg.StateDir)
	r.Line("  db             : " + cfg.DBPath)

	st, err := supervisor.QueryStatus(ctx, cfg.StateDir)
	if err != nil {
		if errors.Is(err, ipc.ErrNoDaemon) {
			r.Line("  daemon         : not running")
			// The stamped pid alone is stale after a clean release (the lock FILE
			// stays on disk); only a failing probe proves a live owner.
			probe := ipc.NewFileLock(ownerLockPath(cfg))
			if got, _ := probe.TryAcquire(); got {
				probe.Release()
			} else if pid := ipc.ReadLockHolderPid(ownerLockPath(cfg)); pid > 0 {
				r.Line(fmt.Sprintf("  owner          : pid %d (an attached assistant)", pid))
			}
			return domain.OneShotExitCode.Success
		}
		r.Error("daemon query failed: " + err.Error())
		return domain.OneShotExitCode.Error
	}

	uptime := time.Duration(domain.NowMS()-st.StartedAtMs) * time.Millisecond
	r.Line(fmt.Sprintf("  daemon         : %s (pid %d, up %s, version %s)", st.State, st.Pid, uptime.Round(time.Second), st.Version))
	mcp := "not configured"
	if st.McpConfigured {
		switch {
		case st.McpConnected:
			mcp = "connected"
		case st.McpBlocked:
			mcp = "UNAVAILABLE — supervision blocked until it returns"
		default:
			mcp = "not connected"
		}
	}
	r.Line("  mcp            : " + mcp)
	r.Line(fmt.Sprintf("  supervision    : %d watcher(s), %d async operation(s), %d timer(s)",
		st.LiveWatchers, st.LiveAsync, st.ScheduledTimers))
	r.Line(fmt.Sprintf("  inbox          : %d open item(s), %d pending approval(s)",
		st.OpenAttention, st.PendingApproval))
	wake := "none yet"
	if st.WakeTurnsRun > 0 {
		wake = fmt.Sprintf("%d run, last %s ago", st.WakeTurnsRun,
			(time.Duration(domain.NowMS()-st.LastWakeAtMs) * time.Millisecond).Round(time.Second))
	}
	r.Line("  wake turns     : " + wake)
	if st.LastError != "" {
		r.Line("  last error     : " + st.LastError)
	}
	return domain.OneShotExitCode.Success
}

func ownerLockPath(cfg config.AppConfig) string {
	return cfg.StateDir + string(os.PathSeparator) + ipc.OwnerLockName
}
