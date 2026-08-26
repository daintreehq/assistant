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
		//
		// A nil source is NOT the same as "send nothing": credentialSource decides which
		// of the two nil readings this is, and fails the client closed when the account
		// layer could not be built at all.
		TokenSource: credentialSource(cfg, tokenSource),
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
	// ANONYMOUS ON PURPOSE, said out loud rather than by passing nil. credentialSource
	// reads a nil source as "no account layer was handed over", which on a machine with
	// no caller key is a construction fault and fails the request closed. The catalog read
	// is protected but not account-BOUND: it describes the deployment, not the user, so it
	// stays answerable on a broken state root — which is precisely when someone runs a
	// probe.
	//
	// The caller key is the one thing that must still reach it, and it reaches it by this
	// staying nil: NewClient prefers TokenSource over APIKey, so naming NoTokenSource here
	// unconditionally would silently drop the key the operator exported. nil with a key set
	// is the caller-key branch of credentialSource, which returns it unchanged.
	tokens := backend.TokenSource(backend.NoTokenSource{})
	if strings.TrimSpace(cfg.APIKey) != "" {
		tokens = nil
	}
	return backend.NewClient(backendClientConfig(cfg, nil, tokens))
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

// NewCourtesyAccountBackendClient builds the account client for the check that runs
// straight after a successful `auth login` or `/login`.
//
// It observes SOME of what it hears and structurally cannot observe the rest, which is a
// narrowing of what used to be a client that observed nothing at all. Both halves matter:
//
//   - The half that must never come back. The fully observing client acts on everything,
//     and `auth_session_revoked` reaches RemedyClear, which DELETES the refresh token —
//     moments after a login persisted it. The user would be told "Signed in.", the command
//     would exit 0, and the credential would be gone. A backend mid-deploy, a proxy
//     rewriting a body, or a misconfigured deployment all produce that code as easily as a
//     real revocation does. (An untyped 401 is less severe — it maps to RemedySignIn and
//     deletes nothing — but it would still report the fresh session as signed out.)
//   - The half that was missing. Observing NOTHING meant a settled refusal — a staging
//     allowlist answering 403 `auth_permission_denied` for a valid identity — was PRINTED
//     by the login and then forgotten: the state stayed `signed_in_unverified`, so
//     `/account` and a turn's prose disagreed with the sentence the user had just read.
//
// DESTRUCTION is the bar a verdict must clear to be forwarded at all — the reason the
// courtesy read was held at arm's length is that a verdict could delete a credential, and
// one that only writes two fields of a state machine cannot. It is not the only bar: a
// verdict also has to be one the surfaces can RENDER coherently beside the sentence the
// login prints, which is why the two non-destructive 402s are excluded too. See
// courtesySettleCodes, where each exclusion carries its own reason.
//
// One honest limit on the destruction claim: obtaining the credential in the first place
// is still the manager's own business, and a refresh whose grant the provider rejects ends
// the session wherever it happens. That is true of every caller of AccessToken and was true
// of this path before it observed anything at all. What this type guarantees is narrower
// and is the guarantee that was missing: nothing the BACKEND SAYS ABOUT THIS REQUEST can
// destroy anything.
func NewCourtesyAccountBackendClient(cfg config.AppConfig, mgr *auth.Manager) *backend.Client {
	if mgr == nil {
		return backend.NewClient(accountClientConfig(cfg, nil))
	}
	return backend.NewClient(accountClientConfig(cfg, courtesyAccountTokenSource{mgr: mgr}))
}

// courtesySettleCodes is the CLOSED set of account codes a courtesy read may fold into
// local state: the two 403s, and nothing else.
//
// They are the answer a private deployment gives a valid identity it has not approved,
// and they are SETTLED — repeating the login produces the identical refusal, so there is
// nothing for a courtesy read to protect by staying quiet. Locally they write `lastErr`
// and a state and touch no credential at all.
//
// Everything else is deliberately absent, and the two near misses are worth naming:
//
//   - `auth_session_revoked` — the destructive one this whole type exists to withhold.
//     It reaches RemedyClear, which deletes the refresh token.
//   - `auth_required`, `auth_token_invalid`, `auth_token_expired` — credential verdicts.
//     They drop the access token or report the session as signed out, which is exactly
//     what a courtesy check must not do to a session minted seconds ago.
//   - the three 503s — an outage is not an answer. Recording `temporarily_unavailable`
//     from a courtesy blip would leave a perfectly good fresh sign-in looking degraded,
//     with a `lastErr` beside it that nothing retires until a later read succeeds.
//   - THE TWO 402s, which were in this set and were taken out. They are equally
//     non-destructive and equally settled, and the case for them was symmetry: a 200 body
//     saying `access=subscription_required` already settles that state on this very read,
//     so a 402 saying the same thing in an error envelope makes the local answer depend on
//     which shape the backend chose. What defeats it is that a 402 ALSO returns an error,
//     and the surfaces render the two halves separately: the card would read "signed in —
//     no plan" and then, two lines down, "the account could not be re-checked just now".
//     Settling a state nothing is prepared to render coherently buys a contradiction, not
//     a truth. The symmetry is worth having once those surfaces have a settled-billing
//     branch to put it in; until then the plan is named by the sentence the login prints,
//     which is what a courtesy check is for.
var courtesySettleCodes = map[string]bool{
	backend.CodeAuthPermissionDenied: true,
	backend.CodeAuthClientNotAllowed: true,
}

// courtesyAccountTokenSource presents the manager's credential and lets only the
// non-destructive half of a verdict reach it.
//
// It implements TokenSource, TokenScrubber and AccountObserver — the last one is what
// changed, and the filter below is the whole of the safety argument. The client
// type-asserts for AccountObserver, so before this existed the guarantee was structural
// in the crudest possible way: the interface was absent, and every observation was inert.
// That guarantee is now carried by a filter, so it is written as two gates that must BOTH
// pass, neither of them trusted alone:
//
//	the CODE must be in courtesySettleCodes — a closed set with a stated reason per entry;
//	and the REMEDY must be RemedyReconfigure — an ALLOWLIST, checked at the moment of use.
//
// The second gate is positive rather than "not RemedyClear", and the difference is the
// whole point of having it. A negative gate only blocks the one remedy somebody thought
// of: if an allowlisted code were reclassified upstream as RemedyRefresh, it would sail
// through and the manager would drop the access token. RemedyReconfigure is the only
// remedy whose entire local effect is a state write, so naming it is the same sentence as
// "nothing this observer forwards can destroy anything", enforced rather than asserted.
//
// What it does NOT do — and cannot, from here — is make a reclassification harmless
// everywhere. The client reads the remedy independently: wantsRefreshReplay would see the
// new RemedyRefresh, call renewedCredential, and a refresh whose grant is rejected deletes
// the session, with this gate never consulted. Changing what a code MEANS is a change that
// has to be made deliberately at both ends; this gate makes sure the observer is not the
// end that gives way silently.
type courtesyAccountTokenSource struct{ mgr *auth.Manager }

// AccessToken delegates. Obtaining a credential is not a verdict about this request, and
// this type has no business substituting its own answer for the manager's.
//
// It is NOT free of consequences, and the comment used to imply it was: a refresh runs
// here, and a refresh whose grant the provider rejects ends the session. That is the
// manager's rule for every caller and there is nothing to filter — the alternative is a
// courtesy read that presents a token it knows to be expired.
func (c courtesyAccountTokenSource) AccessToken(ctx context.Context) (string, error) {
	return c.mgr.AccessToken(ctx)
}

// Invalidate is deliberately inert. Discarding the token a login has just minted, because
// one courtesy request came back unhappy, is the failure this whole type prevents.
func (c courtesyAccountTokenSource) Invalidate(string) {}

// Secrets reports the credentials worth masking in echoed error text.
func (c courtesyAccountTokenSource) Secrets() []string { return c.mgr.Secrets() }

// Generation delegates, because the staleness guard is only meaningful against the
// manager's own counter — an observer that reported a number of its own would let a
// verdict for a replaced session land on its replacement.
func (c courtesyAccountTokenSource) Generation() uint64 { return c.mgr.Generation() }

// MarkIdentityLive delegates. It stamps a liveness time and changes no state, so it is
// non-destructive by construction — and the account endpoint suppresses it anyway
// (accountAttempt.deferSuccess), since the decoded body is the verdict there.
func (c courtesyAccountTokenSource) MarkIdentityLive(gen uint64) { c.mgr.MarkIdentityLive(gen) }

// ApplyBackendVerdict forwards only the settled, non-destructive account verdicts.
//
// The default is to drop. A code this build does not classify, an error that is not a
// backend error at all, and every credential verdict alike leave local state exactly as
// the login left it — which is the behaviour this path had for all codes before, and is
// still the right one for everything outside the allowlist.
func (c courtesyAccountTokenSource) ApplyBackendVerdict(ctx context.Context, gen uint64, usedToken string, err error) {
	var be *backend.Error
	if !errors.As(err, &be) || be == nil {
		return
	}
	if !courtesySettleCodes[be.Code] {
		return
	}
	// The second gate, positive on purpose: only the remedy whose whole local effect is a
	// state write may pass. A code reclassified upstream stops here rather than reaching a
	// branch of the state machine this path has never been allowed to reach.
	if be.AuthRemedy() != backend.RemedyReconfigure {
		return
	}
	c.mgr.ApplyBackendVerdict(ctx, gen, usedToken, err)
}

// NewAccountManager builds the account manager for a resolved config, or nil when there
// is no account for this process to have.
//
// Two ways to get nil, and they are NOT the same thing — see AccountLayerFault, which is
// how a surface tells them apart. A deprecated caller key overrides account identity for
// every request, so building a manager beside it would put two credentials in play with
// only one able to win: a deliberate choice, and nothing to report. A manager that cannot
// be CONSTRUCTED is a broken state root — still not a reason to refuse to START, because
// the surfaces that diagnose it (doctor's account row, `/account`, `/backend`) read local
// configuration and are exactly what someone runs next, but the session's WORK routes fail
// closed from that point on (see credentialSource). It is a local fault the user has to be
// told about, because every account surface would otherwise describe their machine's
// problem as a property of the deployment.
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

// credentialSource decides what a client actually presents, and is the one place a
// session with NO account layer stops being an ANONYMOUS one.
//
// A nil tokenSource means no account manager was handed over, and NewAccountManager
// returns nil for exactly two reasons — which is why this re-asks accountLayerFault
// rather than assuming either:
//
//   - A deliberate DAINTREE_API_KEY. accountLayerFault is silent for it, and nil is
//     returned UNCHANGED so NewClient's own APIKey fallback takes it. Returning any
//     typed source here would win the client's TokenSource-beats-APIKey preference and
//     silently disable the key the operator exported — see accountTokenSource, where the
//     same trap is documented for the same reason.
//   - A construction fault: an unwritable state root, a plain file where the `auth`
//     directory belongs, EACCES, ENOSPC. This used to fall through to NoTokenSource, so
//     the client omitted the Authorization header and the turn went out as an anonymous
//     principal. Against a deployment whose door is open that SUCCEEDS, and this
//     machine's local fault is quietly attributed to whoever the open door resolves to;
//     against one that enforces accounts it comes back as a generic server rejection
//     naming the deployment, for a problem that is entirely on this disk. Neither
//     reading is visible, and neither is true.
//
// A HEALTHY manager with no credential is not this case at all: it is a real token
// source that returns "" with a nil error, so an account-optional deployment keeps
// running signed out exactly as it does today.
//
// The abort itself is already built: Client.credential refuses to send when its source
// errors, and raises CodeCredentialUnavailable rather than CodeAuthRequired precisely
// because nothing left the process. Public paths (health, version, auth discovery) never
// consult a source at all, and the surfaces that NAME this fault — doctor's account row,
// `/account`, `/backend` — read local configuration, so the diagnosis stays reachable.
// A protected diagnostic does not: doctor's upstream-credential row calls VerifyKey through
// the session client, so on this fault it reports the local credential failure instead of
// the deployment's verdict. That is honest — no request could be made — but it is a row
// that changes shape here, and the account row above it is the one carrying the remedy.
func credentialSource(cfg config.AppConfig, tokenSource backend.TokenSource) backend.TokenSource {
	if tokenSource != nil {
		return tokenSource
	}
	fault := accountLayerFault(cfg)
	if fault == nil {
		return nil
	}
	return backend.UnavailableTokenSource{Err: &accountCredentialFault{fault: fault}}
}

// accountCredentialFault carries a construction fault to the request path as the SAME
// sentence every other account surface prints.
//
// Two properties, both load-bearing. Error() renders through AccountFaultMessage, so the
// local auth code (`auth_exchange_failed`, which names a token exchange no code path
// attempted) and the wrapped os.MkdirAll cause (which embeds the state-root path) are
// both dropped before the text can reach a turn's prose — the boundary AccountFaultMessage
// documents. And Unwrap keeps the typed fault reachable, so IsAccountLayerFault still
// recognises it after the backend client has wrapped it in its own envelope, and a surface
// branching on the class does not have to read message text.
type accountCredentialFault struct{ fault error }

func (e *accountCredentialFault) Error() string {
	if e == nil {
		return ""
	}
	return AccountFaultMessage(e.fault)
}

func (e *accountCredentialFault) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.fault
}
