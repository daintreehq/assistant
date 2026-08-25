package app

import (
	"runtime"
	"strings"

	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/costledger"
	"github.com/daintreehq/assistant/internal/debuglog"
)

// backendclient.go builds the one backend client the App talks through. It was carved
// out of the old signin.go, which went away with the sign-in itself: the backend now
// holds its own upstream credential and serves a request that carries no Authorization
// header, so there is no key to prompt for, store, verify, or swap in place.

// backendClientConfig builds the backend client options from resolved config.
//
// cfg.APIKey is empty on virtually every install, and the client is built the same way
// either way: it simply omits the Authorization header, which is exactly what the
// backend's open door expects. A key that IS present rides along and overrides the
// backend's own upstream credential for the turn.
//
// ledger, when non-nil, receives a CostEvent for every billed upstream call the built
// client makes. It stays a parameter rather than being read off the App so an unbilled
// throwaway client — a probe that must not appear in the session's total — is built by
// passing nil rather than by remembering to unhook something.
func backendClientConfig(cfg config.AppConfig, ledger *costledger.Ledger, tokenSource backend.TokenSource) backend.ClientConfig {
	baseURL := strings.TrimSpace(cfg.BackendURL)
	if baseURL == "" {
		baseURL = backend.DefaultBaseURL
	}
	dbg := debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir}
	clientCfg := backend.ClientConfig{
		BaseURL: baseURL,
		APIKey:  cfg.APIKey,
		// The account credential. Without this the whole sign-in feature is inert: a
		// user runs `auth login`, it succeeds, and every subsequent turn still sends no
		// Authorization header because the client fell back to NoTokenSource.
		//
		// It is skipped when the deprecated DAINTREE_API_KEY is set, because the two
		// cannot both be right about who is calling and the client prefers TokenSource
		// over APIKey. Silently overriding an explicit key would be the more surprising
		// of the two — see the doctor warning, which tells the user the key is winning.
		TokenSource: tokenSource,
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
	// The routing preference rides the same seam: read live so a future runtime change
	// reaches the next request, and applied by the client to turns AND utility tasks
	// alike — a privacy choice honoured only on the visible path would be worse than
	// none. Captured from the config passed in rather than off the App, so the sign-in
	// probe (which passes no ledger either) stays a plain unconfigured client.
	routing := cfg.Routing
	clientCfg.RoutingPreference = func() backend.Routing { return routing }
	// One hook, at the only layer every billed call passes through. The ledger outlives
	// the client on purpose: anything that rebuilds the client mid-session must not
	// reset a running total.
	if ledger != nil {
		clientCfg.OnCost = ledger.Record
	}
	return clientCfg
}

// NewProbeBackendClient builds an App-free, UNBILLED client for a read-only probe of
// the configured endpoint — today, `--list-runbooks`.
//
// It resolves the endpoint, the optional caller key and the routing posture exactly the
// way the session client does, so a probe can never report on a different deployment
// than a turn would reach. It takes no cost ledger, because a capability GET is not
// billed and must not appear in any session total, and it needs no App: reading the
// catalog must not take the project's owner lease, open the database, or connect MCP.
// Listing what a backend can load is a question about the BACKEND, and it has to be
// answerable while another assistant owns the project.
func NewProbeBackendClient(cfg config.AppConfig) *backend.Client {
	return backend.NewClient(backendClientConfig(cfg, nil, nil))
}

// NewAccountManager builds the account manager for a resolved config, or nil when there
// is no account for this process to have.
//
// Two ways to get nil, and both mean "no account layer here" rather than a failure. A
// deprecated caller key overrides account identity for every request, so building a
// manager beside it would put two credentials in play with only one able to win. And a
// manager that cannot be constructed means no auth directory — a broken state root, not
// a reason to refuse to start; the client falls back to sending no credential, exactly
// as it does for a signed-out user.
func NewAccountManager(cfg config.AppConfig) *auth.Manager {
	if strings.TrimSpace(cfg.APIKey) != "" {
		return nil
	}
	mgr, err := auth.NewManager(auth.Options{
		StateRoot:  cfg.StateRoot,
		BackendURL: cfg.BackendURL,
	})
	if err != nil {
		return nil
	}
	return mgr
}

// accountTokenSource adapts a manager for the client, preserving the nil contract.
//
// The explicit nil return is load-bearing and cannot be replaced by returning the typed
// pointer: NewClient prefers TokenSource over APIKey, so a non-nil interface holding a
// nil *Manager would satisfy that preference and then yield "" for every request —
// silently disabling a key the user explicitly exported, with nothing to explain why.
func accountTokenSource(mgr *auth.Manager) backend.TokenSource {
	if mgr == nil {
		return nil
	}
	return mgr
}
