//go:build unix

package supervisor

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/daintreehq/assistant/internal/config"
)

// spawnDaemon launches `<self> daemon` detached (own session, no controlling
// TTY) so it survives this process. The resolved config travels explicitly:
// the state dir and connection credentials as env (trusted-env boundary — the
// daemon must land on the SAME state dir even if it was originally picked via
// a per-project default), the project binding as the working directory.
// Stdout/stderr append to <stateDir>/daemon.log for post-mortems.
func spawnDaemon(cfg config.AppConfig, version string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}
	cmd := exec.Command(exe, "daemon")
	cmd.Dir = cfg.ProjectPath
	env := append(os.Environ(),
		"DAINTREE_ASSISTANT_STATE_DIR="+cfg.StateDir,
		"DAINTREE_ASSISTANT_TIER="+string(cfg.Tier),
	)
	if cfg.McpURL != "" {
		env = append(env, "DAINTREE_MCP_URL="+cfg.McpURL)
	}
	if cfg.McpToken != "" {
		env = append(env, "DAINTREE_MCP_TOKEN="+cfg.McpToken)
	}
	if cfg.ProjectID != "" {
		env = append(env, "DAINTREE_PROJECT_ID="+cfg.ProjectID)
	}
	// The RESOLVED sign-in, not whatever happens to be in os.Environ(). These two can
	// now come from --backend-url / --api-key-file, and a daemon that inherited only the
	// environment would either boot signed out (and 401 inside every autonomous wake
	// turn, looking alive while accomplishing nothing) or, worse, resume the same
	// conversation against a DIFFERENT spendable key than the launch that spawned it.
	// Env rather than argv deliberately: argv is world-readable through `ps`.
	if cfg.BackendURL != "" {
		env = append(env, "DAINTREE_BACKEND_URL="+cfg.BackendURL)
	}
	if cfg.APIKey != "" {
		env = append(env, "DAINTREE_API_KEY="+cfg.APIKey)
	}
	cmd.Env = env

	logPath := filepath.Join(cfg.StateDir, "daemon.log")
	if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); ferr == nil {
		cmd.Stdout = f
		cmd.Stderr = f
		defer f.Close() // the child holds its own descriptor after Start
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}
	// Reap in the background so a daemon that exits while we're still alive
	// never lingers as a zombie; if WE exit first, init adopts it.
	go func() { _ = cmd.Wait() }()
	return nil
}
