package app

import (
	"context"
	"errors"
	"runtime"
	"strings"

	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/costledger"
	"github.com/daintreehq/assistant/internal/debuglog"
)

// backendclient.go builds the one backend client the App talks through, and the one
// account manager that credentials it. The two are built together on purpose: the object
// that issues a credential has to be the object told what the backend said about it.
//
// The backend holds its own upstream PROVIDER credential, so a request needs no key in
// order to have one to spend, and on a deployment with no account layer it carries no
// Authorization
// header, so there is no key to prompt for, store, verify, or swap in place.

// backendClientConfig builds the backend client options from resolved config.
//
// cfg.APIKey is empty on virtually every install, and the client is built the same way
// either way: it simply omits the Authorization header, which is exactly what the
// backend's open door expects. A key that IS present rides along as the CALLER's
// bearer — it says who is asking, never what pays. The backend funds the turn from its
// own upstream credential either way, so setting one changes which principal the
// request is attributed to and nothing about the money.
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

// accountClientConfig builds the shared shape of both account clients below.
//
// App-free for the same reason NewProbeBackendClient is: asking who is signed in must
// not take the project's owner lease or open its database. An account is a property of
// the USER, so the question has to be answerable while another assistant owns the
// project. Unbilled because a status read is not a turn — it carries no cost ledger, so
// a plan check can never appear in a session total.
//
// ONE ATTEMPT. The default policy replays a 503 or a 429 up to ten times, and an account
// status read is the one call where that is wrong twice over: a billing dependency being
// down is an ANSWER ("could not check"), not a blip to grind at, and a person waiting on
// `auth status --refresh` would sit through the whole backoff schedule to be told so.
// The auth ladder's single refresh-and-replay still applies underneath this — it is a
// renewed credential rather than a repeated question, and it is what makes an expired
// token invisible.
func accountClientConfig(cfg config.AppConfig, tokens backend.TokenSource) backend.ClientConfig {
	c := backendClientConfig(cfg, nil, tokens)
	c.Retry = backend.RetryPolicy{MaxAttempts: 1}
	return c
}

// NewAccountBackendClient builds the account client for `auth status --refresh`.
//
// It OBSERVES: the manager is handed over as the token source, so the client recognises
// it as an AccountObserver and folds every verdict into local state — a refresh for an
// expired token, a deletion for a revoked session, a retained credential for an outage.
// That is exactly what someone asking "what is the state of my account?" wants, and the
// caller must therefore NOT apply the verdict a second time.
func NewAccountBackendClient(cfg config.AppConfig, mgr *auth.Manager) *backend.Client {
	return backend.NewClient(accountClientConfig(cfg, accountTokenSource(mgr)))
}

// NewUnobservingAccountBackendClient builds the account client for the check that runs
// straight after a successful `auth login`.
//
// It deliberately does NOT observe, and that is the whole reason it exists. The observing
// client acts on what it hears, and `auth_session_revoked` reaches RemedyClear, which
// DELETES the refresh token — moments after a login persisted it. The user would be told
// "Signed in.", the command would exit 0, and the credential would be gone. A backend
// mid-deploy, a proxy rewriting a body, or a misconfigured deployment all produce that
// code as easily as a real revocation does. (An untyped 401 is less severe — it maps to
// RemedySignIn and deletes nothing — but it would still report the fresh session as
// signed out.)
//
// A post-login entitlement check is a courtesy: it exists to name the plan, and it has no
// business revoking a session that was minted seconds ago by a token exchange the
// provider itself completed. So it reads, reports, and changes nothing. The plan answer
// still lands, through ApplyAccountStatus, which cannot delete anything.
func NewUnobservingAccountBackendClient(cfg config.AppConfig, mgr *auth.Manager) *backend.Client {
	if mgr == nil {
		return backend.NewClient(accountClientConfig(cfg, nil))
	}
	return backend.NewClient(accountClientConfig(cfg, unobservedTokenSource{mgr: mgr}))
}

// unobservedTokenSource presents the manager's credential without letting the outcome
// mutate it.
//
// It implements TokenSource and TokenScrubber and NOTHING else. Omitting AccountObserver
// is the mechanism: the client type-asserts for it, so an absent implementation makes
// every observation an inert no-op rather than something that has to be remembered at
// each call site. Scrubbing is kept, because masking a bearer a backend echoes back is
// never the wrong thing to do.
type unobservedTokenSource struct{ mgr *auth.Manager }

// AccessToken delegates: obtaining a credential, including refreshing an expired one, is
// a normal read and not a verdict.
func (u unobservedTokenSource) AccessToken(ctx context.Context) (string, error) {
	return u.mgr.AccessToken(ctx)
}

// Invalidate is deliberately inert. Discarding the token a login has just minted, because
// one courtesy request came back unhappy, is the failure this whole type prevents.
func (u unobservedTokenSource) Invalidate(string) {}

// Secrets reports the credentials worth masking in echoed error text.
func (u unobservedTokenSource) Secrets() []string { return u.mgr.Secrets() }

// NewAccountManager builds the account manager for a resolved config, or nil when there
// is no account for this process to have.
//
// Two ways to get nil, and they are NOT the same thing — see AccountLayerFault, which is
// how a surface tells them apart. A deprecated caller key overrides account identity for
// every request, so building a manager beside it would put two credentials in play with
// only one able to win: a deliberate choice, and nothing to report. A manager that cannot
// be CONSTRUCTED is a broken state root — still not a reason to refuse to start (the
// client falls back to sending no credential, exactly as it does for a signed-out user),
// but a local fault the user has to be told about, because every account surface would
// otherwise describe their machine's problem as a property of the deployment.
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

// ErrAccountLayerUnbuilt is the fallback fault: this session has no account manager, the
// config says one was wanted, and re-deriving the cause no longer reproduces it.
//
// It exists so the third branch is never SILENT. The generic "accounts are not available"
// is a statement about the deployment, and saying it here would send someone reading it
// to a backend that is working perfectly while the fault sits on their own disk.
//
// The wording says "in this session" and deliberately not "at startup": boot is only one
// of the two construction sites. A `/backend` switch rebuilds the manager mid-session,
// and a root that was broken for that one attempt and repaired since lands here too.
var ErrAccountLayerUnbuilt = errors.New("the account layer is not built in this session")

// AccountLayerFaultError marks an error as a CONSTRUCTION fault rather than anything the
// account itself did.
//
// The marker is a type because the underlying error is not distinguishable by its own
// code: creating the auth directory is wrapped as `auth_exchange_failed`, which is also
// what a genuinely failed token exchange carries. Without this, a surface rendering a
// refresh error would have to guess from the wording, and the two need opposite copy —
// one says retry the sign-in, the other says fix a directory.
//
// Error() delegates so nothing is added to the text, and Unwrap keeps errors.Is/As and
// auth.CodeOf working straight through the marker.
type AccountLayerFaultError struct{ Cause error }

func (e *AccountLayerFaultError) Error() string {
	if e == nil || e.Cause == nil {
		return "the account layer is not built in this session"
	}
	return e.Cause.Error()
}

func (e *AccountLayerFaultError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// IsAccountLayerFault reports whether err is a construction fault, however deeply it is
// wrapped. Surfaces branch on this rather than on message text.
func IsAccountLayerFault(err error) bool {
	var f *AccountLayerFaultError
	return errors.As(err, &f)
}

// AccountLayerFault reports the local fault that left this session without an account
// manager, or nil when there is no fault to report.
//
// nil covers the two healthy readings: a manager exists, or a caller key deliberately
// replaced it. Non-nil means construction FAILED — an unwritable state root, a plain file
// where the `auth` directory belongs, EACCES, ENOSPC — and every account surface should
// say so instead of falling through to copy about the deployment.
//
// The fault is RE-DERIVED from the config rather than retained beside the manager, for
// two reasons. Both construction sites — boot and a `/backend` switch, which rebuilds the
// manager because a credential minted for one deployment must never be presented to
// another — are covered by one function of the live (config, manager) pair, with no field
// for the second site to forget to update. And re-deriving reports the CURRENT state of
// the root, so a fault that has since been repaired stops being asserted; what it cannot
// then explain is why this process still has no manager, which is what the sentinel
// above is for.
//
// The pair is read under the same lock and in one capture, for the reason RefreshAccount
// documents: `/backend` replaces the config and the manager together, and two separate
// reads can straddle that write and hand this function a manager from one endpoint beside
// the config of another.
func (a *App) AccountLayerFault() error {
	a.cfgMu.RLock()
	cfg, mgr := a.Config, a.Auth
	a.cfgMu.RUnlock()
	if mgr != nil {
		return nil
	}
	return accountLayerFault(cfg)
}

// accountLayerFault answers the same question from a config alone, for the paths that
// have no App — the account read below, which is shared with the standalone `auth`
// subcommands.
//
// It re-attempts the construction rather than probing the directory itself, so it stays
// truthful to whatever auth.NewManager actually requires. That is safe to call from a
// user-facing surface because construction performs no I/O beyond creating the auth
// directory: discovery and credential reads are deferred precisely so building one on an
// offline or signed-out machine cannot block.
func accountLayerFault(cfg config.AppConfig) error {
	if strings.TrimSpace(cfg.APIKey) != "" {
		// Deliberate, not degraded. The caller key names the principal for every request
		// in this process, and reporting a fault here would tell an operator who
		// configured exactly this that something is broken.
		return nil
	}
	if _, err := auth.NewManager(auth.Options{
		StateRoot:  cfg.StateRoot,
		BackendURL: cfg.BackendURL,
	}); err != nil {
		return &AccountLayerFaultError{Cause: err}
	}
	return &AccountLayerFaultError{Cause: ErrAccountLayerUnbuilt}
}

// AccountFaultMessage renders a construction fault as a human sentence, WITHOUT the local
// auth code the error carries.
//
// The code is dropped deliberately, and only for this fault. Everywhere else a local auth
// code is worth printing because it names something the user just did — a busy callback
// port, a declined consent — and it is the term to search for. This one names something
// that never happened: creating the auth directory is wrapped as `auth_exchange_failed`,
// so printing it sends a reader hunting a token exchange that no code path attempted.
//
// The wrapped cause is dropped with it, and that is the path boundary. os.MkdirAll's error
// embeds the directory it could not create, so echoing the cause would put a state-root
// path into a turn's prose. The path belongs in `doctor`, which is a diagnostic surface
// that already prints state-dir paths and is meant to be pasted somewhere; a conversation
// is not that surface, so this returns the fault and the fault only.
func AccountFaultMessage(err error) string {
	if err == nil {
		return ""
	}
	var ae *auth.Error
	if errors.As(err, &ae) && ae != nil && ae.Message != "" {
		return ae.Message
	}
	return strings.TrimPrefix(err.Error(), "auth: ")
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
