package backend

import "context"

// tokensource.go replaces the client's frozen `apiKey string` with a value that can
// change under it.
//
// The old field was set once in NewClient and read on every request, which made a
// credential rotation a CLIENT rebuild. That is survivable when the credential never
// changes — the backend holds its own and the CLI's is empty on virtually every
// install — and it stops being survivable the moment access tokens expire hourly.
// Rebuilding the client to rotate a token would drop the retry policy, the cost hook
// and the routing preference on the floor, or force every consumer to re-wire itself
// through Swappable on a one-hour clock.
//
// So the indirection goes one level lower than Swappable. Swappable exists to change
// the ENDPOINT (a different deployment, a different base URL, a whole new client);
// TokenSource exists to change the CREDENTIAL for the same endpoint. Keeping them
// separate means an hourly refresh touches neither the transport nor any consumer's
// handle, and a `/backend` switch still rebuilds exactly what it should.

// TokenSource supplies the bearer credential for protected backend requests.
//
// Implementations must be safe for concurrent use: a turn, a watcher poll and a
// utility task can all be in flight at once, and each will ask independently.
type TokenSource interface {
	// AccessToken returns the credential to send, or "" to send no Authorization
	// header at all. An empty string with a nil error is the NORMAL case today — the
	// backend's open door expects an unauthenticated request — so callers must not
	// treat it as a failure.
	//
	// A non-nil error means the credential could not be obtained (storage failure, a
	// refresh that could not complete). The request must not be sent: proceeding
	// without the header would silently downgrade an authenticated session to an
	// anonymous one, which is far worse than failing loudly.
	AccessToken(ctx context.Context) (string, error)

	// Invalidate marks a specific token as rejected, so the next AccessToken does not
	// hand back the same dead value.
	//
	// It takes the token rather than being a bare Reset() because refreshes race. Two
	// requests can fail on the SAME expired token while a third has already refreshed;
	// a bare reset from the first would discard the good token the third just minted.
	// Naming the value makes the operation a compare-and-clear.
	Invalidate(accessToken string)
}

// AccountObserver is an optional TokenSource capability: learning the ACCOUNT-relevant
// outcome of every protected request made with a credential it supplied.
//
// It exists because the two halves of the account contract were built and never joined.
// The taxonomy in account.go decides what a backend answer means; the local state
// machine in internal/auth decides what to do about it — and nothing carried an answer
// from one to the other. A 401 stopped the request and vanished: no state changed, the
// dead token was presented again on the next call, and a revoked session stayed on disk.
//
// Why it lives beside TokenSource rather than as a client callback: the observer has to
// be the SAME object that issued the credential. A verdict is about one credential from
// one login, and an observer that did not mint it cannot say whether the answer is still
// current — which is the entire purpose of the generation below.
type AccountObserver interface {
	// Generation returns the current local identity generation. A caller samples it
	// BEFORE a request and hands it back with the outcome, so an answer that arrives
	// after a logout or a re-login is recognised as describing a session that is gone.
	Generation() uint64

	// MarkActive records that a protected request SUCCEEDED under that generation.
	//
	// Success is a verdict too, and the one nothing else can supply: a stored
	// credential proves a login happened, never that the backend still honours it.
	// Without this the only route out of "signed in, unverified" is a failure.
	MarkActive(gen uint64)

	// ApplyBackendVerdict folds an account failure into local state: refresh, clear,
	// preserve, or ignore as stale. usedToken is the credential the request actually
	// carried, which distinguishes a verdict about the CURRENT token from one about a
	// token already replaced by a refresh in the same generation.
	//
	// It must never return an error or panic. It runs on the failure path of a request
	// that has already failed, and a fault here would replace a diagnosable backend
	// error with an unrelated one.
	ApplyBackendVerdict(ctx context.Context, gen uint64, usedToken string, err error)
}

// TokenScrubber is an optional TokenSource capability: reporting the credential values
// this source has handed out so error text echoing one can be masked.
//
// It exists because scrubbing used to read the client's single frozen key, and there
// is no longer a single frozen key to read. A backend we do not control can echo the
// Authorization header into an error body, and that text reaches the host's scrollback
// and the debug log. A source that knows which values it issued is the only thing that
// can say what to mask; sources with nothing to protect simply do not implement it.
type TokenScrubber interface {
	// Secrets returns the credential values currently worth masking. It must not mint,
	// refresh or block — it is called on an error path, and an implementation that
	// went to the network here would turn a failed request into a hung one.
	Secrets() []string
}

// StaticTokenSource serves one fixed credential forever.
//
// This is what DAINTREE_API_KEY resolves to, and what tests use. It is deliberately
// NOT the shape an account login should take: Invalidate is a no-op because there is
// nothing behind a static value to re-derive it from, so a rejected static key stays
// rejected. That is the honest behaviour — pretending otherwise would produce a client
// that retries a permanently wrong credential.
type StaticTokenSource struct{ Token string }

// AccessToken returns the fixed token.
func (s StaticTokenSource) AccessToken(context.Context) (string, error) { return s.Token, nil }

// Invalidate is a no-op: a static credential has no source to refresh from.
func (s StaticTokenSource) Invalidate(string) {}

// Secrets reports the fixed token so echoed copies of it can be scrubbed.
func (s StaticTokenSource) Secrets() []string {
	if s.Token == "" {
		return nil
	}
	return []string{s.Token}
}

// NoTokenSource sends no credential. It is the zero-configuration default and what the
// public probes use, so an unconfigured client behaves exactly as it did before this
// indirection existed.
type NoTokenSource struct{}

// AccessToken always returns the empty credential.
func (NoTokenSource) AccessToken(context.Context) (string, error) { return "", nil }

// Invalidate is a no-op.
func (NoTokenSource) Invalidate(string) {}

// publicPaths are the endpoints that must be reachable WITHOUT a credential, and
// therefore must never trigger a token fetch.
//
// This is not merely an optimisation. Once AccessToken can block on a refresh — or
// fail because the credential store is locked — attaching it to the liveness probes
// would make `doctor` unable to answer "is the backend up?" while signed out, which is
// precisely the question someone asks when their login is broken. The discovery
// endpoint is here for a sharper version of the same reason: it is what the auth layer
// reads to learn how to obtain a token in the first place, so requiring a token to
// read it is a bootstrap the CLI could never satisfy.
var publicPaths = map[string]bool{
	"/health":                  true,
	"/healthz":                 true,
	"/readyz":                  true,
	"/version":                 true,
	"/v1/daintree/auth/config": true,
}

// isPublicPath reports whether a request path may be sent unauthenticated.
func isPublicPath(path string) bool { return publicPaths[path] }
