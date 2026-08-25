package commands

import (
	"context"
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
		return "Accounts are not available in this session."
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
			return "Sign-in failed: " + authMessage(err) + "\n\nRun /account to see where this session stands."
		}
	}

	// The summary carries the storage-tier warning itself, so a non-persistent sign-in
	// is reported once rather than twice — `res.Persisted` and `StorageTier` are the
	// same fact seen from two sides, and saying it in both voices reads as two problems.
	return strings.TrimRight("Signed in.\n\n"+accountSummary(ctx, mgr), "\n")
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
		return "Accounts are not available in this session."
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
func accountText(ctx context.Context, a *app.App) string {
	if a == nil {
		return "Accounts are not available in this session."
	}
	mgr := a.AuthManager()
	if mgr == nil {
		return "Accounts are not available in this session."
	}
	// Hydrate FIRST. Status is deliberately I/O-free, so a manager built fresh in this
	// process knows nothing about a credential already on disk — without this, /account
	// immediately after a successful /login reports "unknown", which is the one answer
	// that is never useful.
	mgr.Hydrate(ctx)
	return accountSummary(ctx, mgr)
}

// accountSummary is the shared body of /account and the tail of a successful /login.
//
// One composer for both so the two can never describe the same credential differently —
// the failure that would produce is "signed in as X" from one command and something
// else from the other, in the same transcript.
func accountSummary(ctx context.Context, mgr *auth.Manager) string {
	// ENRICHED, exactly as `auth status` enriches it. `Status()` alone carries the local
	// state, the endpoint and the storage tier and nothing else — the email, the plan,
	// the usage line and the billing links all arrive from the manifest and the
	// availability read. Rendering the bare snapshot left every one of those branches
	// dead: a signed-in account with an active plan displayed neither.
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
	// The endpoint is operator-supplied and may carry userinfo — `https://user:secret@host`
	// is a valid thing to have typed into `/backend`. This renders into the transcript
	// and onto the host's NDJSON stream, so it goes through the same fail-closed
	// sanitizer every other displayed URL uses: a blank endpoint costs a reader one
	// fact, a leaked credential is unrecoverable.
	st.BackendURL = mcp.SanitizeURL(st.BackendURL)

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
	if st.UsageRemainingText != "" {
		fmt.Fprintf(&b, "%-9s %s\n", "usage", st.UsageRemainingText)
	}
	if st.StorageTier == auth.TierMemory {
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
		return "signed in (not yet verified against the backend)"
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
		return "could not reach the backend"
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
