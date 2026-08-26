package app

import (
	"context"

	"github.com/daintreehq/assistant/internal/auth"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
)

// accountrefresh.go is THE live account read — the one operation behind every surface
// that asks the backend what this account currently is.
//
// There are four such surfaces: `auth status --refresh`, the courtesy check after a
// successful `auth login`, the embedded `/account` card, and the tail of an embedded
// `/login`. They used to be two implementations and two omissions: the CLI pair made the
// request, and the embedded pair rendered whatever happened to be in the manager's
// in-process snapshot — which, in a session that had never made an account request, is
// nothing at all. `/account` on a perfectly good keychain credential said "signed in (not
// yet verified against the backend)" and named no plan, while `auth status --refresh`
// against the same credential named it. Two answers to one question, in the same
// installation, with no way for a reader to tell which was the real one.
//
// So the sequence lives here once. Everything below it is about the two ways this
// operation can be handed a stale identity between its start and its finish.

// AccountRefresh is the outcome of one live account read.
//
// It is a value rather than a (status, error) pair because there are FOUR outcomes and
// two of them are neither a status nor a failure. A deployment with no identity provider
// has no account endpoint, no credential to renew and nothing to report — rendering that
// as an error would tell someone their sign-in is broken on an install where sign-in does
// not exist. And an answer can arrive for an endpoint this session has since left, which
// is not a failure of the request either; it is a fact about what happened underneath it.
type AccountRefresh struct {
	// Status is the decoded response, valid only when Applied() is true.
	Status backend.AccountStatus
	// Err is the reason the read did not produce a status. It is already folded into
	// local state where it should be — see RefreshAccountWith — so a caller REPORTS it
	// and does nothing else with it.
	Err error
	// Skipped reports that no request was made because this deployment does not use
	// accounts. Not an error and not a verdict.
	Skipped bool
	// Discarded reports that an answer arrived and was deliberately not believed, because
	// the identity it describes is no longer the one this session holds — a `/backend`
	// switch replaced the manager, or a login, logout or revocation moved the generation
	// while the request was in flight.
	Discarded bool
}

// Applied reports whether the read produced a status that REACHED the manager.
//
// The distinction between "no error" and "applied" is load-bearing and was a real bug:
// ApplyAccountStatus declines silently whenever the identity moved during the request,
// so a caller reading the absence of an error as success would report a refreshed plan
// from a call that deliberately changed nothing.
func (r AccountRefresh) Applied() bool {
	return r.Err == nil && !r.Skipped && !r.Discarded
}

// AccountRefreshOptions tunes one read.
type AccountRefreshOptions struct {
	// Courtesy selects the UNOBSERVING client, for the plan report that follows a
	// successful sign-in.
	//
	// The choice is load-bearing rather than stylistic. The observing client acts on what
	// it hears, and `auth_session_revoked` reaches RemedyClear, which DELETES the refresh
	// token — moments after a login persisted it. A backend mid-deploy, a proxy rewriting
	// a body or a misconfigured deployment all produce that code as easily as a real
	// revocation does. A post-login entitlement check is a courtesy: it exists to name
	// the plan, and it has no business revoking a session minted seconds ago by a token
	// exchange the provider itself completed.
	//
	// Everything else — an explicit `auth status --refresh` or `/account` — leaves this
	// false. The user asked, so a revocation SHOULD clear the credential, an expired
	// token should refresh, and an outage should be recorded.
	Courtesy bool
	// Availability, when Known, is used instead of asking the manager again.
	//
	// It exists for `auth status`, which resolves availability before deciding whether
	// there is anything to refresh at all. Asking twice is free when the answer is
	// cached — and it is not cached when it is UNKNOWN, which is exactly the outage this
	// command is most often run during. A second discovery attempt there can add ten
	// seconds to a status read whose entire value is answering quickly while things are
	// broken.
	Availability auth.Availability
}

// RefreshAccount asks the backend for the current account and folds the answer into local
// state, guarding the whole operation against an endpoint switch running beside it.
//
// The ordering is fetch-without-the-lock, then verify-and-commit under it. `/backend`
// takes cfgMu for WRITING to replace the manager, so a switch cannot slip between the
// verification and the commit — while no network request is ever made with the lock held.
// That is what makes the guarantee a real one rather than a report after the fact: an
// answer for an endpoint this session has left is never applied at all.
func (a *App) RefreshAccount(ctx context.Context, opts AccountRefreshOptions) AccountRefresh {
	// ONE atomic capture of the pair, not two reads.
	//
	// `/backend` replaces the config's endpoint and the manager TOGETHER, under cfgMu,
	// because a credential minted for one deployment must never be presented to another.
	// Reading them separately reintroduces exactly what that pairing prevents: a switch
	// landing between the two reads hands this function manager A and endpoint B, and the
	// request then sends A's bearer to B.
	a.cfgMu.RLock()
	cfg, mgr := a.Config, a.Auth
	a.cfgMu.RUnlock()

	fetched := fetchAccount(ctx, cfg, mgr, opts)
	if fetched.Skipped || fetched.Err != nil {
		// An ERROR is as endpoint-specific as a status. One raised against the endpoint
		// this session has since left describes a deployment the reader is no longer
		// talking to, and reporting it under the new endpoint's card would attribute a
		// failure to the wrong backend.
		a.cfgMu.RLock()
		moved := a.Auth != mgr
		a.cfgMu.RUnlock()
		if moved {
			return AccountRefresh{Discarded: true}
		}
		return AccountRefresh{Skipped: fetched.Skipped, Err: fetched.Err}
	}

	// VERIFY AND COMMIT under the same read lock. `/backend` needs the write lock, so it
	// waits for this — which is the whole point: there is no window between deciding the
	// manager is still current and making it true.
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	if a.Auth != mgr {
		return AccountRefresh{Discarded: true}
	}
	if !mgr.ApplyAccountStatus(fetched.gen, fetched.status) {
		// The endpoint held still and the IDENTITY did not: a login, a logout or a
		// revocation moved the generation while the request was in flight. Same reading,
		// same word — this answer describes somebody who is no longer signed in here.
		return AccountRefresh{Discarded: true}
	}
	return AccountRefresh{Status: fetched.status}
}

// RefreshAccountWith is the account read for callers that hold the config and the manager
// directly, and it commits as soon as the answer arrives.
//
// The standalone `auth` subcommands are those callers: they run in a process with no App
// at all — no store, no MCP, no tool registry — because `auth status` has to answer while
// everything else is broken. They also cannot race an endpoint switch, since there is no
// `/backend` in a one-shot command, which is why the two-phase guard above lives on the
// App method rather than in here.
func RefreshAccountWith(ctx context.Context, cfg config.AppConfig, mgr *auth.Manager, opts AccountRefreshOptions) AccountRefresh {
	fetched := fetchAccount(ctx, cfg, mgr, opts)
	switch {
	case fetched.Skipped:
		return AccountRefresh{Skipped: true}
	case fetched.Err != nil:
		return AccountRefresh{Err: fetched.Err}
	case !mgr.ApplyAccountStatus(fetched.gen, fetched.status):
		return AccountRefresh{Discarded: true}
	}
	return AccountRefresh{Status: fetched.status}
}

// fetchedAccount is one read, not yet committed anywhere.
type fetchedAccount struct {
	status  backend.AccountStatus
	gen     uint64
	Err     error
	Skipped bool
}

// fetchAccount performs the request and NOTHING else — no state is folded in here.
//
// Separated from the commit so the App can put the commit under the lock that keeps the
// endpoint still, without ever holding that lock across a network request.
func fetchAccount(ctx context.Context, cfg config.AppConfig, mgr *auth.Manager, opts AccountRefreshOptions) fetchedAccount {
	if mgr == nil {
		// No account layer in this process at all — DAINTREE_API_KEY names the caller
		// instead. There is no session to refresh and no endpoint to ask.
		return fetchedAccount{Skipped: true}
	}

	// A KNOWN "no accounts here" ends it before any credential is touched. Asking anyway
	// spends a round trip to be told 404, and reaching for the credential store would
	// report a keychain problem on a deployment where the answer is "nothing to do".
	avail := opts.Availability
	if !avail.Known {
		avail = mgr.Availability(ctx)
	}
	if avail.Known && !avail.Configured {
		return fetchedAccount{Skipped: true}
	}

	// The token FIRST. The account read is protected, and a credential that cannot be
	// produced is a different failure from one the backend rejected — the first is a
	// keychain or a lock, the second is a statement about the account.
	if _, err := mgr.AccessToken(ctx); err != nil {
		return fetchedAccount{Err: err}
	}

	// Sampled AFTER that call and BEFORE the request.
	//
	// After, because AccessToken is where this manager notices another process's revision
	// change and moves its own generation; sampling first would capture a number that is
	// already stale by the time the request goes out. Before, because a read can outlive a
	// logout, and the answer has to be recognisable as describing a session that has since
	// ended.
	//
	// It is NOT a complete guard, and the commit is what closes the gap rather than this:
	// the client reads the credential again for its own account attempt and once more for
	// the request, either of which can advance the generation. ApplyAccountStatus
	// rechecks under the manager's own lock and declines if it moved, and its answer —
	// not the absence of an error — is what the caller reports.
	gen := mgr.Generation()

	client := NewAccountBackendClient(cfg, mgr)
	if opts.Courtesy {
		client = NewUnobservingAccountBackendClient(cfg, mgr)
	}
	st, err := client.Account(ctx)
	if err != nil {
		return fetchedAccount{Err: err}
	}
	return fetchedAccount{status: st, gen: gen}
}
