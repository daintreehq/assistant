package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/mcp"
)

// account.go renders /login, /logout and /account.
//
// Sign-in is the engine's own business and always was: the PKCE exchange, the browser,
// the loopback listener that catches the callback and the keychain the credential lands
// in all live in internal/auth. What was missing was a way to REACH it from a session —
// there were CLI subcommands (`daintree-assistant auth login`) and nothing a person
// inside a running assistant could type. An embedding host filled the gap by shelling
// out to those subcommands from its own settings screen, which put a second surface in
// charge of an account it could not see the state of.
//
// So these are deliberately thin. Every decision — whether this deployment has accounts
// at all, whether a cancel is a failure, whether the credential could be persisted —
// already has an answer in the manager, and the job here is to say it in one card.
//
// Nothing rendered here can be a credential. Status is a redacted type by construction
// (internal/auth/status.go) and the authorization URL never enters progress reporting;
// `Login` is called with openBrowser=true, on which path the manual-URL sink is never
// invoked and the only URL reported is the safe account origin.

// loginText runs an interactive sign-in and reports how it ended.
//
// progress is called with short human-readable stages. It matters more here than
// anywhere else this mechanism is used: the browser opens in another window and the
// callback wait is allowed five minutes, so without it the surface says nothing at all
// during the one part of this that requires the user to go and do something.
func loginText(ctx context.Context, a *app.App, progress func(stage string)) string {
	if a == nil {
		return "Accounts are not available in this session."
	}
	// Through the accessor, and read ONCE: `/backend` replaces the manager under a lock,
	// and this command runs on a worker while ordinary commands keep being serviced.
	mgr := a.AuthManager()
	if mgr == nil {
		return noAccountManagerText(a)
	}

	// Hydrate BEFORE the attempt, not only in /account.
	//
	// A manager built fresh in this process starts at StateUnknown and deliberately
	// reads no credential. `Login` snapshots that state before it begins and RESTORES it
	// on cancellation or failure — so without this, a sign-in that fails immediately
	// after start restores "signed out" over a perfectly good credential still sitting in
	// the keychain, and every other process goes on using it. The status and the
	// behaviour would simply disagree.
	mgr.Hydrate(ctx)

	// Asked BEFORE the browser opens. A deployment with no accounts answers this
	// without a round trip, and opening a browser at a provider that does not exist —
	// then failing five minutes later — is the worst version of that answer.
	if avail := mgr.Availability(ctx); avail.Known && !avail.Configured {
		return "This backend does not use accounts — there is nothing to sign in to.\n" +
			"Turns work without one."
	}

	progress("Opening your browser…")
	_, err := mgr.Login(ctx, true, func(event, detail string) {
		switch event {
		case "browser_opened":
			// `detail` is the account ORIGIN, never the authorization URL — see
			// Manager.Login, which reports the safe one by construction.
			progress("Waiting for you to finish signing in at " + detail)
		case "waiting":
			progress("Waiting for the browser to come back…")
		case "authenticated":
			progress("Signed in — storing the credential…")
		}
	}, nil)

	if err != nil {
		switch {
		case auth.CodeOf(err) == auth.CodeAccountsUnavailable:
			// Not a failure, and must not read as one: the assistant works without an
			// account here, so "sign-in failed" would send someone hunting a fault that
			// is not there.
			return "This backend does not use accounts — there is nothing to sign in to."
		case auth.IsCancelled(err):
			return "Sign-in cancelled. Nothing changed."
		default:
			msg := "Sign-in failed: " + authMessage(err)
			// The HINT is the half that says what to do, and it was being dropped here
			// while the standalone CLI rendered it. A browser that will not open is the
			// common case, and its remedy is a specific command — without it the card
			// names a fault and offers no way out of it.
			if hint := auth.HintOf(err); hint != "" {
				msg += "\n" + hint
			}
			return msg + "\n\nRun /account to see where this session stands."
		}
	}

	// A COURTESY plan check, exactly as `auth login` performs one — and through the
	// UNOBSERVING client for exactly the same reason: this read exists to name the plan,
	// and a spurious `auth_session_revoked` from a backend mid-deploy would otherwise
	// reach RemedyClear and delete the refresh token the sign-in persisted seconds
	// earlier. The user would read "Signed in." with the credential already gone.
	//
	// Best-effort throughout. The login has succeeded and been persisted by the time this
	// runs, so nothing here may undo it: a billing outage means the plan is unknown, not
	// that the sign-in was bad.
	// THE ENDPOINT MAY HAVE MOVED while the browser was open. `/backend` runs inline on
	// the command loop and is not excluded by the slow-command gate, so an ordinary
	// switch is free to complete during a sign-in that is parked on a callback for up to
	// five minutes. The credential was stored for the endpoint the login STARTED against,
	// and the session is now on a different one — where nobody is signed in.
	//
	// Reported rather than papered over. Rendering the current manager under a bare
	// "Signed in." produces a card that says signed in and then, two lines down, signed
	// out; and re-rendering the OLD manager would describe an endpoint the session has
	// left. Naming what happened is the only version that is true.
	if now := a.AuthManager(); now != mgr {
		return "Signed in to the backend this sign-in started against.\n" +
			"The session has since switched to a different backend, where this account is\n" +
			"not signed in — run /login again to sign in to the current one."
	}

	// A COURTESY plan check, exactly as `auth login` performs one, and through the
	// UNOBSERVING client for the same reason — see AccountRefreshOptions.Courtesy.
	//
	// Best-effort throughout. The login has succeeded and been persisted by the time this
	// runs, so nothing here may undo it: a billing outage means the plan is unknown, not
	// that the sign-in was bad.
	res := a.RefreshAccount(ctx, app.AccountRefreshOptions{Courtesy: true})

	// The summary carries the storage-tier warning itself, so a non-persistent sign-in
	// is reported once rather than twice — the storage tier and `LoginResult.Persisted`
	// are the same fact seen from two sides, and saying it in both voices reads as two
	// problems.
	out := "Signed in.\n\n" + accountSummary(ctx, mgr)
	if note := refreshNote(res, mgr.Status()); note != "" {
		out += "\n\n" + note
	}
	return strings.TrimRight(out, "\n")
}

// logoutText signs out on this machine.
//
// Local by design, and it says so: revoking every device is the account website's job,
// not a command's. Someone who signs out expecting their other machines to follow, and
// is not told otherwise, has been misled about where their credential still is.
func logoutText(ctx context.Context, a *app.App) string {
	if a == nil {
		return "Accounts are not available in this session."
	}
	mgr := a.AuthManager()
	if mgr == nil {
		return noAccountManagerText(a)
	}
	revokedRemotely, err := mgr.Logout(ctx)
	if err != nil {
		return "Sign-out failed: " + authMessage(err)
	}
	if revokedRemotely {
		return "Signed out, and this session was revoked at the backend."
	}
	return "Signed out on this machine.\n" +
		"Other machines you are signed in on are unaffected — the account page disconnects those."
}

// accountText answers "who am I, on which backend, and does it still work".
//
// It ASKS THE BACKEND rather than formatting whatever this process happens to remember.
// The difference is the whole point of the command: Status() performs no I/O, so in a
// session that has never made an account request it knows the credential exists and
// nothing else — no email, no plan — and a keychain-backed sign-in rendered as "signed in
// (not yet verified against the backend)" with no plan line at all. Meanwhile
// `auth status --refresh` against that same credential named the plan. Two answers to one
// question in one installation.
//
// It is also what makes a returning customer's session correct without another sign-in.
// Someone who has just bought a plan on the website comes back to a process whose local
// state still says `subscription_required`; only a live read can move it, and requiring a
// fresh OAuth round trip to pick up a purchase is not a thing to ask of anyone.
func accountText(ctx context.Context, a *app.App) string {
	if a == nil {
		return "Accounts are not available in this session."
	}
	mgr := a.AuthManager()
	if mgr == nil {
		return noAccountManagerText(a)
	}
	// Hydrate FIRST. Status is deliberately I/O-free, so a manager built fresh in this
	// process knows nothing about a credential already on disk — without this, /account
	// immediately after a successful /login reports "unknown", which is the one answer
	// that is never useful. It also has to precede the read below, which needs a
	// credential to present.
	mgr.Hydrate(ctx)

	// OBSERVING, because the user asked: a revocation should clear the credential, an
	// expired token should refresh, an outage should be recorded. See App.RefreshAccount.
	res := a.RefreshAccount(ctx, app.AccountRefreshOptions{})
	// The manager is re-read rather than reused: `/backend` may have replaced it while
	// this command sat on a worker waiting for the request, and the summary must describe
	// the endpoint the session is on NOW.
	if now := a.AuthManager(); now != nil {
		mgr = now
	}

	// The state is read ONCE and shared with the note, so the card and its footnote
	// cannot disagree about the same credential — the failure that produces is "access
	// disconnected" above "your sign-in is unaffected".
	st := accountStatus(ctx, mgr)
	summary := accountSummaryOf(st)
	if note := refreshNote(res, st); note != "" {
		// APPENDED, never substituted. A failed re-check does not erase what is already
		// known — "here is your account, and here is what we could not confirm about it"
		// is both halves of the truth, where replacing the card with an error would drop
		// the half that is still good.
		summary += "\n\n" + note
	}
	return summary
}

// refreshNote says what a live read could not establish, or nothing when it succeeded.
//
// Every branch is a REPORT. The verdict, if there was one, has already reached local
// state through the observing client, so nothing here acts — and the card above it
// already renders whatever that verdict changed.
//
// `after` is the state as it stands AFTER that verdict landed, and it is what decides the
// second sentence. "Your sign-in is unaffected" is true of a dependency outage and false
// of a revocation, which the observing client acts on by deleting the credential — so
// deciding from the error alone produced a card reading "access disconnected", "Run
// /login", and "your sign-in is unaffected", all about the same session.
func refreshNote(res app.AccountRefresh, after auth.Status) string {
	switch {
	case res.Err != nil:
		if auth.IsCancelled(res.Err) {
			return ""
		}
		// Never "you are not subscribed". A read that failed established nothing about
		// the plan, and saying otherwise sends a paying customer to a checkout page.
		note := "! The account could not be re-checked just now (" + authMessage(res.Err) + ")."
		if after.State.SignedIn() {
			note += "\n  Your sign-in is unaffected; the state above is what was last known."
		}
		return note
	case res.Discarded:
		return "! The account changed while this was being checked — a different backend\n" +
			"  or a different sign-in — so the answer was discarded. Run /account again."
	default:
		// Skipped is silent: a deployment with no accounts already says so in the state
		// line, and a second sentence about a check that was never applicable is noise.
		return ""
	}
}

// noAccountManagerText explains a session with NO account layer at all.
//
// Three different causes land here and they need different sentences. A caller key names
// the principal for this process, which deliberately leaves App.Auth nil — telling that
// operator "this backend does not use accounts" would be false and would send them
// looking for a deployment problem that does not exist.
//
// The second is the one this used to hide: the manager could not be BUILT, because the
// auth directory under the state root could not be created. That is a fault on this
// machine and it has nothing to do with the deployment, so the generic sentence sent
// people to check a backend that was answering fine. It is also the only one of the three
// that is fixable, which is why it is the only one carrying a next action.
//
// The generic line is left for the third: an App with a manager the deployment simply has
// no use for. It stays a statement about availability and never about a fault, because
// every install today runs against a backend with no identity provider at all.
func noAccountManagerText(a *app.App) string {
	if a == nil {
		return "Accounts are not available in this session."
	}
	if a.SnapshotConfig().APIKey != "" {
		return "This session identifies itself with DAINTREE_API_KEY, so there is no\n" +
			"account to sign in to or out of. Unset it to use a managed sign-in."
	}
	if fault := a.AccountLayerFault(); fault != nil {
		msg := "Accounts are unavailable in this session: " + accountFaultMessage(fault) + ".\n" +
			"That is a fault on this machine, not on the backend — turns still work, but\n" +
			"signing in cannot. Run `daintree-assistant doctor` for the path it needs, fix\n" +
			"it, then start a new session."
		if hint := auth.HintOf(fault); hint != "" {
			msg += "\n" + hint
		}
		return msg
	}
	return "Accounts are not available in this session."
}

// accountFaultMessage renders a construction fault WITHOUT the local error code.
//
// Every other card here goes through authMessage, code and all, because those codes name
// something the user just did — a busy callback port, a declined consent. This one names
// nothing of the sort: creating the auth directory is wrapped as `auth_exchange_failed`,
// and no token exchange has happened or could have. Printing that code sends a reader
// hunting a sign-in attempt that was never made.
func accountFaultMessage(err error) string {
	var ae *auth.Error
	if errors.As(err, &ae) && ae != nil && ae.Message != "" {
		return ae.Message
	}
	return authMessage(err)
}

// accountSummary is the shared body of /account and the tail of a successful /login.
//
// One composer for both so the two can never describe the same credential differently —
// the failure that would produce is "signed in as X" from one command and something
// else from the other, in the same transcript.
func accountSummary(ctx context.Context, mgr *auth.Manager) string {
	return accountSummaryOf(accountStatus(ctx, mgr))
}

// accountStatus resolves the enriched, sanitized status one time.
//
// Separated from the rendering so a caller that needs BOTH the card and a footnote about
// the same credential reads the state once. Two reads is two chances for the two lines to
// describe different moments.
func accountStatus(ctx context.Context, mgr *auth.Manager) auth.Status {
	// ENRICHED, exactly as `auth status` enriches it. `Status()` carries what this
	// PROCESS knows — the local state, the endpoint, the storage tier, and the account
	// snapshot IF a live read has already landed one. The manifest supplies the
	// environment and the billing links; availability supplies whether this deployment
	// has accounts at all. Neither supplies the email or the plan: those come only from
	// a `/v1/daintree/account` response, which is why the caller performs one first.
	//
	// Both reads are best-effort and deliberately independent. A manifest that fails to
	// validate must not stop status answering — "the backend is unreachable" is exactly
	// when someone asks — and availability is read either way, because the one case it
	// answers for (a deployment with no identity provider) is the case that makes the
	// manifest fail.
	st := mgr.Status()
	if man, err := mgr.Manifest(ctx); err == nil {
		st = st.WithManifest(man)
	}
	st = st.WithAvailability(mgr.Availability(ctx))
	// Belt and braces. An endpoint can no longer carry userinfo: every source goes
	// through backend.NormalizeBaseURL, which refuses `https://user:secret@host`
	// outright — at startup (config.LoadConfig) and on the interactive `/backend <url>`
	// path (app.backendswitch) alike — so by the time it reaches here it should be
	// clean. It is sanitized anyway because this value renders into the transcript AND
	// onto the host's NDJSON stream, both of which get pasted into issues and logs
	// verbatim; a displayed URL should not be the thing that has to be right. The
	// sanitizer fails closed, and that is the correct trade here: a blank endpoint costs
	// a reader one fact, a leaked credential is unrecoverable.
	st.BackendURL = mcp.SanitizeURL(st.BackendURL)
	return st
}

// accountSummaryOf renders a resolved status.
func accountSummaryOf(st auth.Status) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-9s %s\n", "state", accountStateLabel(st.State))
	if st.Email != "" {
		fmt.Fprintf(&b, "%-9s %s\n", "account", st.Email)
	}
	if st.BackendURL != "" {
		fmt.Fprintf(&b, "%-9s %s\n", "backend", st.BackendURL)
	}
	if st.Plan != "" {
		plan := st.Plan
		if st.EntitlementStale {
			// Said, not hidden. A plan we could not re-confirm is still the best answer
			// available, and presenting it as current would be a claim nobody verified.
			plan += " (not re-confirmed)"
		}
		fmt.Fprintf(&b, "%-9s %s\n", "plan", plan)
	}
	// NOTHING POPULATES THIS YET, and the branch is kept deliberately rather than
	// deleted. Usage allowances are explicitly out of scope until the plans have numbers
	// behind them, and the field is already on the Status wire type a native consumer
	// decodes — so this is the render waiting for a producer, not a line that once worked
	// and stopped. It stays empty and prints nothing until one exists.
	if st.UsageRemainingText != "" {
		fmt.Fprintf(&b, "%-9s %s\n", "usage", st.UsageRemainingText)
	}
	// ONCE. StateStorageUnavailable already says the same thing in the state line, and
	// two sentences about one fact read as two problems — the second of which the user
	// then looks for a second remedy to.
	if st.StorageTier == auth.TierMemory && st.State != auth.StateStorageUnavailable {
		b.WriteString("\n! This credential is held in memory only and will not survive a restart.\n")
	}
	if next := accountNextStep(st); next != "" {
		b.WriteString("\n" + next + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// accountStateLabel puts each state in words rather than leaving the wire name on
// screen. `signed_in_subscription_required` is a protocol value, not a sentence.
func accountStateLabel(s auth.State) string {
	switch s {
	case auth.StateSignedInActive:
		return "signed in"
	case auth.StateSignedInUnverified:
		return "signed in (plan not checked)"
	case auth.StateSubscriptionRequired:
		return "signed in — no plan"
	case auth.StateSubscriptionInactive:
		return "signed in — plan not granting access"
	case auth.StateSignedOut:
		return "signed out"
	case auth.StateAuthorizing:
		return "signing in"
	case auth.StateRefreshing:
		return "refreshing the session"
	case auth.StateRevoked:
		return "access disconnected"
	case auth.StateTemporarilyUnavailable:
		// NOT "could not reach the backend". The backend is frequently reached and
		// answers that ITS billing dependency is down — a typed outage, not a network
		// one — and sending someone to check their connection over that wastes the one
		// piece of information the verdict actually carried.
		return "signed in — the plan could not be confirmed"
	case auth.StateStorageUnavailable:
		return "signed in, but the credential cannot be stored"
	case auth.StateAccountsUnavailable:
		return "this backend does not use accounts"
	case auth.StateAccessRefused:
		return "access refused"
	case auth.StateUnknown, "":
		return "unknown"
	default:
		// A state this build has never heard of renders its WIRE VALUE rather than
		// collapsing to "unknown". The vocabulary is open — a newer backend, or a newer
		// engine talking to an older reader, can introduce one — and mapping it to
		// "unknown" would report a settled, specific answer as an absence of one. The
		// underscores are ugly and the honesty is worth more: someone reading it can at
		// least search for the term.
		return string(s)
	}
}

// accountNextStep names the ONE thing worth doing about the current state, and only
// when there is one.
//
// A state that needs nothing gets no line: a card that ends with advice on every read
// trains people to stop reading the last line, including the times it mattered. The
// billing states point at a URL rather than a command, because no command here can fix
// them — and a plan that exists but is not granting access must never be answered with
// "buy a plan", since the user has already paid once and a second checkout is not the
// fix.
func accountNextStep(st auth.Status) string {
	switch st.State {
	case auth.StateSignedOut, auth.StateRevoked:
		return "Run /login to sign in."
	case auth.StateSubscriptionRequired:
		if st.Links.Subscribe != "" {
			return "Choose a plan: " + st.Links.Subscribe
		}
		return "This account has no plan yet."
	case auth.StateSubscriptionInactive:
		if st.Links.Account != "" {
			return "Check billing: " + st.Links.Account
		}
		return "This account's plan is not granting access right now."
	case auth.StateTemporarilyUnavailable:
		return "The session is still stored — try again, or run /doctor."
	default:
		return ""
	}
}

// authMessage strips the package prefix the auth errors carry, so a card does not read
// "auth: auth: ...". Mirrors the CLI's own helper.
func authMessage(err error) string {
	if err == nil {
		return ""
	}
	return strings.TrimPrefix(err.Error(), "auth: ")
}
