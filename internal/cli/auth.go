package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/supervisor"
)

// auth.go is the `auth login|status|logout|disconnect` command surface.
//
// Two rules shape everything here, and both come from the same place: this output is
// consumed by a machine as well as a human.
//
//  1. STDOUT is the machine channel and stderr is the human one. Under --json, stdout
//     carries newline-delimited versioned events and nothing else — no spinner, no
//     prompt, no warning — because the caller driving this command, a host embedding
//     the binary or a script, parses it line by line and a stray human sentence would
//     break a login.
//  2. No command prints a credential, ever. There is deliberately no flag, no verbose
//     mode and no debug switch that reveals an access or refresh token. If support needs
//     token diagnostics they get the issuer, the client id, a subject hash and an expiry
//     — enough to correlate two reports, useless to anyone who intercepts them.

// AuthAction is one `auth` subcommand.
type AuthAction string

const (
	// AuthLogin runs the interactive browser sign-in.
	AuthLogin AuthAction = "login"
	// AuthStatus reports account state without changing anything.
	AuthStatus AuthAction = "status"
	// AuthLogout ends the local session on this machine.
	AuthLogout AuthAction = "logout"
	// AuthDisconnect sends the user to revoke the whole OAuth grant.
	AuthDisconnect AuthAction = "disconnect"
)

// AuthOptions are the parsed `auth` arguments.
type AuthOptions struct {
	Action AuthAction
	// NoOpen prints the authorization URL instead of launching a browser, for an SSH
	// session where the browser lives on the other machine.
	NoOpen bool
	// Refresh forces a fresh backend session check rather than trusting cached state —
	// what someone reaches for immediately after completing a checkout.
	Refresh bool
	// Yes skips the confirmation on `disconnect`.
	Yes bool
}

// ParseAuthAction resolves an action word.
func ParseAuthAction(s string) (AuthAction, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "login", "signin", "sign-in":
		return AuthLogin, true
	case "status", "account":
		return AuthStatus, true
	case "logout", "signout", "sign-out":
		return AuthLogout, true
	case "disconnect":
		return AuthDisconnect, true
	}
	return "", false
}

// AuthUsage is the action list, shown when one is missing or unrecognised.
func AuthUsage() string {
	return strings.Join([]string{
		"  login       sign in with your Daintree account (opens a browser)",
		"  status      show the signed-in account and plan",
		"  logout      sign out on this machine",
		"  disconnect  print the account page, where access can be revoked for every device",
	}, "\n")
}

// authEventVersion versions the NDJSON event stream, and a consumer keys its schema on
// it.
//
// It stays 1 through an additive change: new optional properties and new `state` values.
// That is only safe if the consumer's schema treats both as open — `additionalProperties`
// permitted, and `state` a string rather than a closed enum — because a strict v1 schema
// would reject the whole line rather than degrade. Those schemas live outside this repo,
// so this side cannot check them; a state added here needs that confirmed wherever the
// stream is parsed.
const authEventVersion = 1

// authEvent is one line of the --json stream.
//
// It has no field that can hold a credential, and specifically no field for the
// authorization URL: that URL carries a live request bound to this attempt's PKCE state,
// and this stream is exactly the thing a caller is most likely to log. The URL reaches
// the user only through --no-open, on stderr.
type authEvent struct {
	V     int    `json:"v"`
	Type  string `json:"type"`
	Env   string `json:"environment,omitempty"`
	URL   string `json:"url,omitempty"` // a SAFE account/consent origin only
	Code  string `json:"code,omitempty"`
	Msg   string `json:"message,omitempty"`
	Extra any    `json:"data,omitempty"`
}

// authWriter emits events to stdout and diagnostics to stderr.
type authWriter struct {
	json bool
	out  io.Writer
	err  io.Writer
}

// event writes one NDJSON line, or nothing in human mode.
func (w authWriter) event(e authEvent) {
	if !w.json {
		return
	}
	e.V = authEventVersion
	body, err := json.Marshal(e)
	if err != nil {
		return
	}
	fmt.Fprintln(w.out, string(body))
}

// human writes a line for a person. In --json mode it goes to stderr, so it can never
// corrupt the event stream a caller is parsing.
func (w authWriter) human(format string, args ...any) {
	dst := w.out
	if w.json {
		dst = w.err
	}
	fmt.Fprintf(dst, format+"\n", args...)
}

// RunAuth executes an `auth` action. It never runs a turn and never opens the project
// database — an account is a property of the user, not of a project, so signing in must
// work while another process owns the project lease.
func RunAuth(ctx context.Context, opts Options, authOpts AuthOptions) int {
	overrides, err := overridesFromOptions(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	cfg, err := config.LoadConfig(overrides)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}

	w := authWriter{json: opts.JSON, out: os.Stdout, err: os.Stderr}
	mgr, err := auth.NewManager(auth.Options{
		StateRoot:  cfg.StateRoot,
		BackendURL: cfg.BackendURL,
	})
	if err != nil {
		return authFail(w, "Sign-in", err)
	}

	switch authOpts.Action {
	case AuthLogin:
		return runAuthLogin(ctx, w, mgr, cfg, authOpts)
	case AuthStatus:
		return runAuthStatus(ctx, w, mgr, cfg, authOpts)
	case AuthLogout:
		return runAuthLogout(ctx, w, mgr, cfg)
	case AuthDisconnect:
		return runAuthDisconnect(ctx, w, mgr, authOpts)
	}
	fmt.Fprintf(os.Stderr, "error: unknown auth action\n")
	return 2
}

// authFail reports a failure on both channels and returns a non-zero exit code.
//
// It ALWAYS fails. Treating a cancellation as success belongs only to the login flow,
// where declining consent is a decision rather than a fault — and login handles that
// itself. Doing it here was wrong: the credential lock reports caller cancellation with
// the same code, so a Ctrl-C during `auth logout` under lock contention printed an error,
// left the credential in place, and exited 0.
func authFail(w authWriter, action string, err error) int {
	w.event(authEvent{Type: "auth:error", Code: auth.CodeOf(err), Msg: authMessage(err)})
	w.human("%s failed: %s", action, authMessage(err))
	if hint := auth.HintOf(err); hint != "" {
		w.human("  %s", hint)
	}
	return 1
}

// authMessage renders an error for a human without leaking anything.
func authMessage(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimPrefix(err.Error(), "auth: ")
}

func runAuthLogin(ctx context.Context, w authWriter, mgr *auth.Manager, cfg config.AppConfig, opts AuthOptions) int {
	w.human("Signing in to Daintree…")

	progress := func(event, detail string) {
		switch event {
		case "starting":
			w.event(authEvent{Type: "auth:starting", Env: detail})
		case "browser_opened":
			w.event(authEvent{Type: "auth:browser_opened", URL: detail})
			w.human("Opened your browser. Finish signing in there.")
		case "manual_url_required":
			w.event(authEvent{Type: "auth:manual_url_required"})
		case "waiting":
			w.event(authEvent{Type: "auth:waiting", Extra: map[string]any{
				"callback": detail, "timeoutSeconds": 300,
			}})
		case "authenticated":
			w.event(authEvent{Type: "auth:authenticated"})
		}
	}

	// The authorization URL goes to STDERR and only to stderr, whatever mode we are in.
	// It is a live credential-bearing URL with a five-minute life, so it must not enter
	// the event stream (which a caller may log or forward) and must not be pasted into a
	// bug report.
	manual := func(u string) {
		fmt.Fprintln(os.Stderr, "Open this URL to sign in (it expires in five minutes — do not share it):")
		fmt.Fprintln(os.Stderr, "  "+u)
	}

	res, err := mgr.Login(ctx, !opts.NoOpen, progress, manual)
	if err != nil {
		if auth.CodeOf(err) == auth.CodeAccountsUnavailable {
			// Not a failure. This deployment simply has no accounts, and the assistant
			// works without one — saying "sign-in failed" would send someone hunting a
			// fault that is not there.
			w.event(authEvent{Type: "auth:not_offered", Code: auth.CodeAccountsUnavailable})
			w.human("This backend does not use accounts — there is nothing to sign in to.")
			return 0
		}
		if auth.IsCancelled(err) {
			w.event(authEvent{Type: "auth:cancelled"})
			w.human("Sign-in cancelled.")
			return 0
		}
		return authFail(w, "Sign-in", err)
	}

	w.human("Signed in.")
	if !res.Persisted {
		// Never silent. The session works and then disappears on exit, and a user who
		// was not told experiences that as the assistant randomly forgetting them.
		w.human("Warning: no system credential store is available, so this sign-in will")
		w.human("         not persist after this process exits.")
	}
	// The login's OWN manifest, not a fresh lookup. Login has already validated one and
	// may have consumed the discovery cache doing it, so re-fetching risks a failure
	// that would silently drop the subscribe and billing links from the very message
	// that exists to show them.
	reportPlanAfterLogin(ctx, w, mgr, cfg, res.Manifest)
	return 0
}

// reportPlanAfterLogin makes ONE best-effort account check and says what it found.
//
// The rule it exists to enforce: OAUTH SUCCESS AND PAID ENTITLEMENT ARE SEPARATE
// OUTCOMES. A valid account with no plan has signed in perfectly — reporting "login
// failed" there sends someone to re-authenticate their way out of a billing problem,
// which cannot work. So every branch below keeps the credential and every branch exits
// 0; what changes is the sentence and the link.
//
// Best-effort throughout. The login has already succeeded and been persisted by the time
// this runs, so nothing here may undo it: a billing outage means the plan is unknown,
// not that the sign-in was bad.
func reportPlanAfterLogin(ctx context.Context, w authWriter, mgr *auth.Manager, cfg config.AppConfig, man *auth.Manifest) {
	// THE shared account read, in courtesy mode — the same operation `/login` and
	// `/account` perform, so the four surfaces cannot describe one credential
	// differently. Courtesy selects the UNOBSERVING client: this check must not be able
	// to revoke the session the token exchange just created. See app.RefreshAccountWith.
	res := app.RefreshAccountWith(ctx, cfg, mgr, app.AccountRefreshOptions{Courtesy: true})
	err := res.Err

	st := mgr.Status().WithManifest(man)
	// The same versioned event type `auth status` emits, carrying the same payload —
	// deliberately NOT a new type, because the schemas that validate this stream live
	// outside this repo and this side cannot check that a new one would be accepted.
	//
	// Emitted on EVERY outcome, including the failures below. Under --json this stream is
	// a machine consumer's whole view of the login, and a failed plan check that produced
	// nothing on stdout left it unable to tell "no plan", "could not check" and "no check
	// was made" apart — the human sentences go to stderr under --json and it never sees
	// them.
	//
	// The failure rides the event's own `code`, not the status payload. It cannot ride
	// the payload: Status.LastErrorCode reflects what was recorded AGAINST THE SESSION,
	// and this check is deliberately non-mutating (see
	// NewUnobservingAccountBackendClient), so nothing was recorded and nothing should be.
	// Putting it on the envelope says what happened without pretending the session
	// carries a fault it does not.
	w.event(authEvent{Type: "auth:status", Env: st.Environment, Code: backendCodeOf(err), Extra: st})

	if err != nil {
		reportPlanCheckFailure(w, err)
		return
	}

	switch {
	case st.State == auth.StateSignedInActive && st.Plan != "":
		w.human("Your %s plan is active.", st.Plan)
	case st.State == auth.StateSubscriptionRequired:
		w.human("This account does not have a plan that includes the assistant yet.")
		if st.Links.Subscribe != "" {
			w.human("Choose a plan: %s", st.Links.Subscribe)
		}
	case st.State == auth.StateSubscriptionInactive:
		// The account URL, never the checkout. Sending someone with a lapsed
		// subscription to buy a second one is how people end up paying twice.
		w.human("This account's plan is not currently active.")
		if st.Links.Account != "" {
			w.human("Manage billing: %s", st.Links.Account)
		}
	default:
		// Signed in, and the plan is a question this deployment did not answer —
		// `unverified` on a rollout with no entitlement lookup configured.
		w.human("Signed in. This backend did not report a plan for this account.")
	}
}

// reportPlanCheckFailure says what could not be checked, without ever suggesting the
// sign-in itself went wrong.
func reportPlanCheckFailure(w authWriter, err error) {
	var be *backend.Error
	if !errors.As(err, &be) {
		w.human("Signed in. The plan could not be checked just now.")
		return
	}
	switch {
	case be.AuthRemedy() == backend.RemedyReconfigure:
		// The credential is fine and this deployment will not act on it. Offering a
		// second login here opens a loop that mints another credential wrong in exactly
		// the same way.
		w.human("Signed in, but this backend does not accept this client's credentials.")
		w.human("Signing in again produces the same result; this needs a change at the backend.")
	case be.IsAccountDependency():
		w.human("Signed in. The plan could not be checked — the billing service is unavailable.")
		w.human("Your sign-in is unaffected; try `daintree-assistant auth status --refresh` later.")
	default:
		w.human("Signed in. The plan could not be checked (%s).", backendMessage(err))
	}
}

func runAuthStatus(ctx context.Context, w authWriter, mgr *auth.Manager, cfg config.AppConfig, opts AuthOptions) int {
	// THE BUDGET STARTS HERE, before the first thing that can block.
	//
	// Every step below is bounded on its own — hydrate by the credential store, discovery
	// by its ten seconds, the account read by its own budget — and the whole point is
	// that those bounds STACK. Starting the clock inside the account read left the two
	// preflights outside it, so a slow-but-working discovery still put a person in front
	// of a blank terminal for the sum rather than the cap. One command, one ceiling.
	ctx, cancel := context.WithTimeout(ctx, app.AccountOperationBudget)
	defer cancel()

	// Hydrate FIRST. Status is deliberately I/O-free, so a manager freshly built in this
	// process knows nothing — without this, `auth status` immediately after a successful
	// login reports "unknown", which is the one answer that is never useful.
	mgr.Hydrate(ctx)

	// Availability is resolved BEFORE the refresh, because it decides whether there is
	// anything to refresh. A deployment with no identity provider has no account to ask
	// about, and asking anyway spends a round trip to be told 404.
	avail := mgr.Availability(ctx)

	// --refresh is the only path that makes an ACCOUNT request. Not the only path that
	// touches the network at all — discovery above may fetch the manifest when it has no
	// cached answer, and must, since the deployment's shape is the first thing status
	// reports. The distinction is the one that matters: a plain status read never reaches
	// the billing authority, so it stays answerable while the backend is down, which is
	// precisely when someone runs it.
	if opts.Refresh {
		refreshAccount(ctx, w, mgr, cfg, avail)
	}

	// Read the status AFTER the refresh, so a live answer is reflected rather than the
	// pre-call state.
	//
	// A manifest fills in the environment and links, best-effort: its absence must not
	// stop status answering, because "the backend is unreachable" is precisely when
	// someone runs this. Availability is applied WHETHER OR NOT the manifest validated,
	// because the one case it answers for — a deployment with no identity provider — is
	// the case that makes the manifest fail. Reading it only on success is how "this
	// backend has no accounts" came to be indistinguishable from "we could not reach it".
	st := mgr.Status()
	if man, err := mgr.Manifest(ctx); err == nil {
		st = st.WithManifest(man)
	}
	st = st.WithAvailability(avail)

	exit := authStatusExit(st)

	if w.json {
		// ONE LINE. json.MarshalIndent would emit a multi-line document, and the first
		// line a caller read would be a bare "{" — machine consumers parse this stream
		// one event per line, so an indented status is not merely ugly, it is
		// unparseable. The status
		// rides inside a versioned event for the same reason every other line does.
		w.event(authEvent{Type: "auth:status", Env: st.Environment, Extra: st})
		return exit
	}
	renderAuthStatus(w, st, cfg)
	return exit
}

// refreshAccount performs the ONE live account check behind `auth status --refresh`.
//
// Exactly one request, and only when there is something to ask. Every failure is
// reported and swallowed: "could not verify" is a legitimate thing for status to report,
// and a status command that exited non-zero because billing was down would be worse than
// one that says so.
//
// It deliberately does NOT call ApplyBackendVerdict on the error. The manager IS the
// client's AccountObserver, so the client has already folded the verdict into local
// state — refreshing a token, clearing a revoked credential, recording a dependency
// outage — before the error ever reached here. Applying it a second time would run the
// destructive branch twice against a generation the first one moved.
func refreshAccount(ctx context.Context, w authWriter, mgr *auth.Manager, cfg config.AppConfig, avail auth.Availability) {
	// A KNOWN "no accounts here" ends it. There is no credential to renew and no account
	// endpoint to ask, and touching the credential store would report a keychain problem
	// on a deployment where the answer is simply "nothing to do".
	//
	// The shared operation checks this too, and cheaply — availability is cached. It is
	// repeated here because THIS caller has already paid for the answer above and uses it
	// for its own branching, and because a `return` here is the difference between a
	// silent skip and one that would have to be told apart from a success downstream.
	if avail.Known && !avail.Configured {
		return
	}

	// THE shared account read, in authoritative mode — the same operation `/account`
	// performs. Observing, because the user asked: a revocation clears the credential, an
	// expired token refreshes, an outage is recorded. See app.RefreshAccountWith.
	//
	// It deliberately does NOT call ApplyBackendVerdict on the error. The manager IS the
	// client's AccountObserver, so the client has already folded the verdict into local
	// state before the error reached here; applying it a second time would run the
	// destructive branch twice against a generation the first one moved.
	// The availability answer is HANDED OVER rather than looked up again. It is cached
	// when it is known — and deliberately not cached when it is UNKNOWN, which is the
	// outage this command is most often run during. A second discovery attempt there can
	// add ten seconds to a status read whose whole value is answering quickly while
	// things are broken.
	res := app.RefreshAccountWith(ctx, cfg, mgr, app.AccountRefreshOptions{Availability: avail})
	if res.Err == nil || auth.IsCancelled(res.Err) {
		// Cancellation is silent — the user stopped it.
		return
	}
	// A credential that cannot be PRODUCED is a different failure from one the backend
	// rejected: the first is a keychain, a lock or an expired grant, the second is a
	// statement about the account. Sending someone to check a billing service over a
	// locked keychain wastes the only useful thing the error said.
	//
	// The test is the CODE, not merely "did this come from the backend package". The
	// client wraps its own token-source failures as *backend.Error too
	// (CodeCredentialUnavailable, raised before any request is sent), so an errors.As on
	// the type alone would route half the credential failures into the billing branch.
	var be *backend.Error
	if errors.As(res.Err, &be) && be.Code != backend.CodeCredentialUnavailable {
		w.human("Could not check the plan: %s", backendMessage(res.Err))
		return
	}
	w.human("Could not verify the session: %s", authMessage(res.Err))
}

// backendCodeOf returns the stable account code an error carries, or "".
func backendCodeOf(err error) string {
	var be *backend.Error
	if errors.As(err, &be) && be != nil {
		return be.Code
	}
	return ""
}

// backendMessage renders a backend error for a human, preferring the stable code over
// prose the backend authored. The message is still shown — it is the part that says
// which dependency — but a caller reading the line gets the code first.
func backendMessage(err error) string {
	var be *backend.Error
	if errors.As(err, &be) && be.Code != "" {
		return be.Code
	}
	return err.Error()
}

// authStatusExit is the exit code for a rendered status.
//
// NeedsLogin, not "is not signed in". A deployment with no accounts is not signed in and
// never will be, and returning the not-signed-in code for it would have every script that
// branches on it try to log in against an endpoint with nothing to log in to. A plan
// problem, a dependency outage and a refused client are all exit 0 for the same reason
// in reverse: they are real answers, and none of them is fixed by signing in.
//
// Split out from the command so the table of deployment shapes can assert it without
// standing up a manager and a backend.
func authStatusExit(st auth.Status) int {
	if st.State.NeedsLogin() {
		// A distinct exit code so a script can branch on "not signed in" without parsing
		// prose, while keeping it separate from an outright failure.
		return 3
	}
	return 0
}

// renderAuthStatus writes the human status block.
func renderAuthStatus(w authWriter, st auth.Status, cfg config.AppConfig) {
	w.human("Account")
	w.human("  backend      %s", st.BackendURL)
	if st.Environment != "" {
		w.human("  environment  %s", st.Environment)
	}
	w.human("  accounts     %s", authAvailabilityLabel(st))
	w.human("  state        %s", authStateLabel(st.State))
	if st.Email != "" {
		w.human("  email        %s", st.Email)
	}
	if st.Plan != "" {
		w.human("  plan         %s", st.Plan)
	}
	// Where the billing answer came from, and how old it is. Both are shown because
	// "you are subscribed" and "you were subscribed when we last managed to ask" are
	// different claims, and only one of them is safe to act on.
	if st.EntitlementSource != "" {
		source := st.EntitlementSource
		if st.EntitlementStale {
			source += " (cached — may be out of date)"
		}
		w.human("  plan source  %s", source)
	}
	if st.EntitlementCheckedAt != nil {
		w.human("  plan checked %s", st.EntitlementCheckedAt.Local().Format(time.RFC1123))
	}
	if st.LastVerifiedAt != nil {
		w.human("  verified     %s", st.LastVerifiedAt.Local().Format(time.RFC1123))
	}
	if st.AccessExpiresAt != nil {
		if d := st.AccessExpiresIn(time.Now()); d > 0 {
			w.human("  session      renews in %s", roundDuration(d))
		} else {
			w.human("  session      renewing")
		}
	}
	if st.SessionMaxAgeSeconds > 0 {
		w.human("  sign-in for  %d days", st.SessionMaxAgeSeconds/86400)
	}
	w.human("  credentials  %s", authTierLabel(st.StorageTier))
	// Driven by the TIER, not by the state. The state now follows the account verdict —
	// a confirmed plan overwrites StateStorageUnavailable — so a warning that keyed off
	// the state string would vanish the moment a plan check succeeded, which is the one
	// run where the user is most likely to be reading this block.
	if st.StorageTier == auth.TierMemory {
		w.human("               this sign-in disappears when this process exits")
	}
	if st.LastErrorCode != "" {
		w.human("  last error   %s", st.LastErrorCode)
	}
	// The one place DAINTREE_API_KEY is still mentioned, and only when it is actually
	// set: it is being retired, and describing it in help text people read every day
	// would advertise a path we are removing.
	if strings.TrimSpace(cfg.APIKey) != "" {
		w.human("")
		w.human("Note: DAINTREE_API_KEY is set. It is deprecated and will stop overriding")
		w.human("      account sign-in; unset it once you have signed in.")
	}
	switch {
	case st.State.NeedsLogin():
		w.human("")
		w.human("Run `daintree-assistant auth login` to sign in.")
	case st.State == auth.StateSubscriptionRequired && st.Links.Subscribe != "":
		w.human("")
		w.human("Choose a plan: %s", st.Links.Subscribe)
	case st.State == auth.StateSubscriptionInactive:
		w.human("")
		// The billing portal, never a second checkout. The two plan states share
		// NeedsPlan() and must not share this line: telling someone whose payment
		// failed to choose a plan is how they end up paying for two.
		w.human("Your plan is not currently active. Check billing rather than buying again:")
		if st.Links.Account != "" {
			w.human("  %s", st.Links.Account)
		} else {
			w.human("  open your Daintree account page")
		}
	case st.State.NeedsPlan():
		w.human("")
		w.human("This account needs a plan. Run `daintree-assistant auth status --refresh`")
		w.human("after buying one to pick it up.")
	case st.State == auth.StateSignedInUnverified:
		w.human("")
		w.human("Run `daintree-assistant auth status --refresh` to check the account and plan.")
	case st.State == auth.StateTemporarilyUnavailable:
		w.human("")
		w.human("The account could not be checked just now. Your sign-in is unaffected;")
		w.human("try `daintree-assistant auth status --refresh` again shortly.")
	case st.State == auth.StateAccountsUnavailable:
		w.human("")
		w.human("This backend serves requests without an account. Nothing to do.")
	case st.State == auth.StateAccessRefused:
		w.human("")
		w.human("Your sign-in is intact and this deployment will not act on it.")
		w.human("Signing in again produces the same result; this needs a change at the backend.")
	}
}

// authAvailabilityLabel renders what the DEPLOYMENT offers, which is a different
// question from what this machine holds.
//
// The unknown case is spelled out rather than omitted. A missing row reads as "fine" to
// anyone skimming, and the one thing this line must never do is let an unreachable
// backend pass for one that simply has no accounts.
func authAvailabilityLabel(st auth.Status) string {
	if st.Configured == nil {
		return "could not ask this backend"
	}
	if !*st.Configured {
		return "not offered by this backend"
	}
	if st.AuthRequired != nil && *st.AuthRequired {
		return "required"
	}
	return "supported, not required"
}

// authStateLabel renders a state for a person.
func authStateLabel(s auth.State) string {
	switch s {
	case auth.StateSignedOut:
		return "signed out"
	case auth.StateSignedInActive:
		return "signed in"
	case auth.StateSignedInUnverified:
		return "signed in (plan not checked)"
	case auth.StateSubscriptionRequired:
		return "signed in — no plan yet"
	case auth.StateSubscriptionInactive:
		return "signed in — plan inactive"
	case auth.StateTemporarilyUnavailable:
		return "signed in — could not check just now"
	case auth.StateRevoked:
		return "access was disconnected"
	case auth.StateStorageUnavailable:
		return "signed in — will not persist after exit"
	case auth.StateAccountsUnavailable:
		return "this backend has no accounts"
	case auth.StateAccessRefused:
		return "signed in — this backend refuses these credentials"
	case auth.StateAuthorizing:
		return "waiting for the browser"
	case auth.StateRefreshing:
		return "renewing your session"
	}
	return string(s)
}

// authTierLabel renders where credentials live.
func authTierLabel(t auth.StorageTier) string {
	switch t {
	case auth.TierKeychain:
		return "system credential store"
	case auth.TierMemory:
		return "this process only (no system credential store)"
	}
	return "unknown"
}

// roundDuration renders a duration at a human scale.
func roundDuration(d time.Duration) string {
	switch {
	case d >= time.Hour:
		return d.Round(time.Minute).String()
	case d >= time.Minute:
		return d.Round(time.Second).String()
	}
	return d.Round(time.Second).String()
}

func runAuthLogout(ctx context.Context, w authWriter, mgr *auth.Manager, cfg config.AppConfig) int {
	revoked, err := mgr.Logout(ctx)
	if err != nil {
		// The local credential may still be gone even on an error path, so this reports
		// the failure without claiming the user is still signed in.
		return authFail(w, "Sign-out", err)
	}
	// Tell a running daemon NOW rather than leaving it to notice on its next poll. It
	// would stop either way — the marker check is the mechanism — but a user who just
	// signed out should not have to wonder whether background work is still spending.
	// Failure is ignored: no daemon listening is the ordinary case.
	nctx, ncancel := context.WithTimeout(ctx, 2*time.Second)
	_ = supervisor.NotifyAuthChanged(nctx, cfg.StateDir, mgr.Revision().Current().String())
	ncancel()

	w.event(authEvent{Type: "auth:signed_out"})
	w.human("Signed out on this machine.")
	if !revoked {
		// Honest about scope. Logging out here does not end the session on the user's
		// other devices, and implying otherwise would leave them believing they had
		// revoked access they still hold.
		w.human("Other devices stay signed in. To revoke access everywhere, run")
		w.human("`daintree-assistant auth disconnect`.")
	}
	return 0
}

func runAuthDisconnect(ctx context.Context, w authWriter, mgr *auth.Manager, opts AuthOptions) int {
	st := mgr.Status()
	man, err := mgr.Manifest(ctx)
	if err != nil {
		return authFail(w, "Disconnect", err)
	}
	st = st.WithManifest(man)
	target := st.Links.Account
	if target == "" {
		return authFail(w, "Disconnect", fmt.Errorf("this backend does not publish an account page"))
	}

	// Grant revocation affects EVERY installation this user has, including machines they
	// are not sitting at. That is a bigger action than the word "disconnect" suggests, so
	// it is confirmed explicitly — except in --json mode, where there is no human to ask
	// and the caller is expected to have confirmed already.
	if !opts.Yes && !w.json {
		// Same rule as `reset`: without a terminal, DEMAND --yes rather than reading
		// stdin. Reading it means `printf 'y\n' | ...` silently confirms a revocation
		// affecting every device, /dev/null silently cancels, and an open idle pipe
		// blocks forever.
		if !stdinIsTTY() {
			w.human("Disconnecting revokes access on every device. Re-run with --yes")
			w.human("(there is no terminal here to ask on).")
			return 1
		}
		w.human("Disconnecting revokes Daintree Assistant on EVERY device signed in to this")
		w.human("account, not just this one. Continue? [y/N]")
		var answer string
		_, _ = fmt.Fscanln(os.Stdin, &answer)
		if !strings.EqualFold(strings.TrimSpace(answer), "y") {
			w.human("Cancelled.")
			return 0
		}
	}

	// Named for what it actually does. Claiming "opened" while only printing a URL would
	// leave a caller waiting for a browser that never appears.
	w.event(authEvent{Type: "auth:disconnect_url", URL: target})
	w.human("Open your account page to disconnect Daintree Assistant:")
	w.human("  %s", target)
	return 0
}
