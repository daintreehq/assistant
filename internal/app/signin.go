package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/credentials"
	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/redact"
)

// signin.go owns runtime re-authentication: the `/login` sheet's engine, and the
// read-only view `/auth` renders. Startup sign-in is a different problem, solved
// earlier and elsewhere (internal/cli/login.go, before app.Create exists).

// signInVerifyTimeout bounds the capability probe a re-auth runs. Matches the startup
// flow's budget — the deployed backend scales to zero, so a cold instance is slow.
const signInVerifyTimeout = 30 * time.Second

// backendClientConfig builds the backend client options from resolved config. Shared by
// Create and SignIn so a re-authenticated client is configured identically to the one
// built at boot — the debug-log hooks especially, which are easy to silently drop on a
// rebuild and whose absence only shows up much later as a session log with a hole in it.
func backendClientConfig(cfg config.AppConfig) backend.ClientConfig {
	baseURL := strings.TrimSpace(cfg.BackendURL)
	if baseURL == "" {
		baseURL = backend.DefaultBaseURL
	}
	dbg := debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir}
	clientCfg := backend.ClientConfig{
		BaseURL: baseURL,
		APIKey:  cfg.APIKey,
		ClientInfo: backend.ClientInfo{
			Name:     "daintree-cli",
			Platform: runtime.GOOS,
		},
		// Surface each transient-failure retry to the session log — otherwise a
		// retried turn leaves no trace and a later log read dead-ends at the last
		// successful tool call (the gap that hid the wild 502).
		OnRetry: func(info backend.RetryInfo) {
			debuglog.LogDebug(dbg, "backend.retry", map[string]any{
				"op":          info.Op,
				"attempt":     info.Attempt,
				"maxAttempts": info.MaxAttempts,
				"delayMs":     info.Delay.Milliseconds(),
				"error":       info.Err.Error(),
			})
		},
	}
	// Every utility-task round trip lands in the session log. Without this the tasks
	// were the one backend surface the trace couldn't see — a /compact's checkpoint +
	// memory_distill calls left literally zero log lines (observed 2026-07-12), so
	// compaction archaeology was impossible. Wired ONLY when debug logging is on (same
	// rule as the Session trace seam): the hook re-serializes the task input to measure
	// it, and that must cost nothing in a normal run.
	if cfg.DebugLog {
		clientCfg.OnTask = func(info backend.TaskTraceInfo) {
			fields := map[string]any{
				"task":        info.Task,
				"durationMs":  info.Duration.Milliseconds(),
				"inputBytes":  info.InputBytes,
				"outputBytes": info.OutputBytes,
				"ok":          info.Err == nil,
			}
			if info.Err != nil {
				fields["error"] = info.Err.Error()
			}
			debuglog.LogDebug(dbg, "backend.task", fields)
		}
	}
	return clientCfg
}

// LastSignInWarning returns any caveat from the most recent successful SignIn — an
// endpoint that could not fully check the key, for instance. Empty when the sign-in was
// verified end to end. The cockpit sheet surfaces it so a partial verification is never
// silently presented as a full one.
func (a *App) LastSignInWarning() string {
	// Under cfgMu with the sign-in fields it belongs to: SignIn writes it from the
	// cockpit's command goroutine and the UI reads it from the event loop.
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.lastSignInWarning
}

// APIKey returns the key currently in force, under the config lock. Callers that only
// need to know WHETHER one is set should prefer SignInStatus.
func (a *App) APIKey() string { return a.snapshotConfig().APIKey }

// SignInStatus is the read-only view of the active sign-in, for `/auth` and doctor.
type SignInStatus struct {
	// Endpoint is what the live client is actually talking to — read from the client,
	// not the config, so it stays truthful after a `/login` swap.
	Endpoint string
	// KeyRedacted is safe to display; the raw key is never exposed here.
	KeyRedacted string
	SignedIn    bool
	// StoredPath is where a sign-in is persisted.
	StoredPath string
	// EnvOverride names the env var currently overriding the stored sign-in, if any.
	// Without this, `/login` appears to succeed while every turn keeps hitting the old
	// endpoint — the override wins silently and the UI would be lying.
	EnvOverride string
}

// SignInStatus reports the active sign-in.
func (a *App) SignInStatus() SignInStatus {
	// Snapshot under cfgMu: SignIn mutates the same fields from the cockpit's command
	// goroutine, so an unlocked read here races it.
	cfg := a.snapshotConfig()
	st := SignInStatus{
		Endpoint:    a.Backend.BaseURL(),
		KeyRedacted: credentials.Redact(cfg.APIKey),
		SignedIn:    strings.TrimSpace(cfg.APIKey) != "",
		StoredPath:  cfg.CredentialsPath,
	}
	// os.Getenv is the right source here: config resolves both of these with trustedGet,
	// i.e. the real process environment, which is exactly what this reads. A project
	// .env can set neither, so there is nothing untrusted to confuse it with.
	if strings.TrimSpace(os.Getenv("DAINTREE_BACKEND_URL")) != "" {
		st.EnvOverride = "DAINTREE_BACKEND_URL"
	} else if strings.TrimSpace(os.Getenv("DAINTREE_API_KEY")) != "" {
		st.EnvOverride = "DAINTREE_API_KEY"
	}
	return st
}

// SignIn verifies a candidate sign-in, persists it, and swaps the live backend client
// so the NEXT turn uses it — no restart.
//
// Order matters and is deliberate: verify, then persist, then swap. Verifying first
// means a typo never reaches disk or the running session. Persisting before swapping
// means a crash between the two leaves the sign-in recoverable on next launch rather
// than a live session running on credentials nothing recorded.
//
// A turn already streaming keeps its old client to completion (see backend.Swappable) —
// an endpoint cannot change mid-stream without corrupting the transcript.
func (a *App) SignIn(ctx context.Context, c credentials.Credentials) error {
	if a.backendSwap == nil {
		return errors.New("this session cannot re-authenticate (no swappable backend)")
	}
	if !c.Valid() {
		return errors.New("both an endpoint and a key are required")
	}

	// Verify against a THROWAWAY client: nothing observable changes until it passes.
	// One attempt — the user is watching, and the default policy would replay a refused
	// socket for a minute before reaching the same verdict.
	probeCfg := backendClientConfig(a.snapshotConfig())
	probeCfg.BaseURL, probeCfg.APIKey = c.BaseURL, c.APIKey
	probeCfg.Retry = backend.RetryPolicy{MaxAttempts: 1}
	probe := backend.NewClient(probeCfg)

	// CheckSignIn proves both that this is a reachable Daintree backend AND that the
	// provider actually accepts the key — the latter being the only check that can
	// catch a well-formed but wrong key before the user's next message does.
	vctx, cancel := context.WithTimeout(ctx, signInVerifyTimeout)
	defer cancel()
	_, warning, err := backend.CheckSignIn(vctx, probe)
	if err != nil {
		return signInVerifyError(c.BaseURL, err, c.APIKey)
	}
	warning = backend.ScrubKey(warning, c.APIKey)

	// Protect the NEW key from the trace before it is stored anywhere or used for
	// anything. Additive, so the outgoing key stays registered too — a log line written
	// before this swap still contains it, and un-protecting it would expose it the next
	// time that file is read or bundled.
	redact.RegisterSecret(c.APIKey)

	if err := credentials.Save(a.snapshotConfig().CredentialsPath, c); err != nil {
		return err
	}

	// Config and client move together, under cfgMu — buildContext copies the WHOLE
	// Config on agent goroutines, so an unlocked write here is a genuine data race with
	// any turn in progress (the same hazard SetTier already solves). The updated config
	// is captured while still holding the lock so the client we build cannot reflect a
	// later concurrent change.
	a.cfgMu.Lock()
	a.Config.BackendURL, a.Config.APIKey = c.BaseURL, c.APIKey
	a.lastSignInWarning = warning
	updated := a.Config
	a.cfgMu.Unlock()

	// The swap itself happens OUTSIDE the lock: building a client is cheap but the
	// Swappable is its own synchronization, and holding cfgMu across unrelated work is
	// how lock-ordering bugs start (see the startupMu nesting warning in app.go).
	a.backendSwap.Swap(backend.NewClient(backendClientConfig(updated)))

	// Redacted, always — this line exists so a session log shows WHEN the endpoint
	// changed, which is otherwise invisible archaeology. It must never carry the key.
	debuglog.LogDebug(debuglog.Config{DebugLog: updated.DebugLog, LogDir: updated.LogDir},
		"backend.signin", map[string]any{"endpoint": c.BaseURL, "key": credentials.Redact(c.APIKey)})
	return nil
}

// signInVerifyError names the likely cause and the next action instead of surfacing a
// raw transport error into a cockpit sheet.
func signInVerifyError(baseURL string, err error, key string) error {
	// A definite provider verdict is the actionable case: say so plainly rather than
	// wrapping it in "could not verify <url>", which reads as a connectivity problem.
	if errors.Is(err, backend.ErrKeyRejected) {
		return fmt.Errorf("%s did not accept this key: %v — check it is active and funded", baseURL, err)
	}
	// Nothing wrong with the key: the endpoint is missing a capability the release
	// contract requires. Re-pasting will not help, so say so and point at the fix.
	if errors.Is(err, backend.ErrBackendIncompatible) {
		// Not "your key is fine": no verdict on the key was obtained. Only the endpoint
		// is known-bad, and only that is claimed.
		return fmt.Errorf("%s: %v — an endpoint problem, not a verdict about your key; retry off any proxy, or pick a Local endpoint while this is fixed",
			baseURL, backend.ErrBackendIncompatible)
	}
	var berr *backend.Error
	if errors.As(err, &berr) {
		switch {
		case berr.IsAuth():
			return fmt.Errorf("%s rejected the key — check you pasted it in full", baseURL)
		case berr.IsUpstreamAuth():
			return fmt.Errorf("%s accepted the key but the provider rejected it — check the key is active and funded", baseURL)
		case berr.IsConnect():
			return fmt.Errorf("could not reach %s — is it running, and is the URL right?", baseURL)
		case berr.HTTPStatus == 404:
			return fmt.Errorf("%s answered, but not as a Daintree backend (404)", baseURL)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s did not answer within %s", baseURL, signInVerifyTimeout)
	}
	// Last resort: the message may carry backend-controlled text, so scrub the key we
	// just sent before it reaches a sheet or a log.
	return fmt.Errorf("could not verify %s: %s", baseURL, backend.ScrubKey(err.Error(), key))
}
