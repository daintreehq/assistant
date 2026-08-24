package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// loopback.go receives the OAuth authorization response on a FIXED loopback port.
//
// Fixed, not ephemeral, and that is forced on us rather than chosen: Supabase matches
// registered redirect URIs exactly, with no allowance for arbitrary loopback ports. The
// common desktop pattern — bind :0, take whatever port the OS gives, rewrite
// redirect_uri to match — depends on a provider that accepts any loopback port, and
// against Supabase it produces an unregistered URI and a provider error naming a URL
// the user has never seen. So the port is compiled in, and a collision is a NAMED
// failure rather than a fallback. See errPortInUse.
//
// Changing CallbackPort means changing four things together: this constant, the
// backend's manifest response, the Supabase OAuth client registration, and the tests.

const (
	// CallbackHost is IPv4 loopback, written as a literal.
	//
	// Not "localhost": that resolves through the system resolver and can answer ::1, a
	// hosts-file entry, or on a badly configured machine something else entirely. The
	// address the authorization code arrives on is not a place to accept a resolver's
	// opinion.
	CallbackHost = "127.0.0.1"
	// CallbackPort is registered with the identity provider and cannot vary at runtime.
	CallbackPort = 42813
	// CallbackPath is the only path the listener answers.
	CallbackPath = "/oauth/callback"

	// callbackTimeout bounds a login attempt end to end. Long enough to sign in and
	// approve consent, short enough that an abandoned attempt does not hold the port.
	callbackTimeout = 5 * time.Minute
	// maxCallbackHeaderBytes bounds one request's headers. The real callback is a short
	// GET; anything larger is not a browser completing an OAuth flow.
	maxCallbackHeaderBytes = 8 << 10
	// shutdownGrace lets the success page finish writing before the socket closes. A
	// browser that gets a reset instead of a body shows an error page on a login that
	// actually succeeded.
	shutdownGrace = 2 * time.Second
)

// RedirectURI is the exact string registered with the provider, sent in the
// authorization request, and required to appear in the backend's manifest. One
// function so the three can never drift apart.
func RedirectURI() string {
	return fmt.Sprintf("http://%s:%d%s", CallbackHost, CallbackPort, CallbackPath)
}

// callbackAddr is the host:port the listener binds and the Host header must match.
func callbackAddr() string { return fmt.Sprintf("%s:%d", CallbackHost, CallbackPort) }

// callbackOutcome is one settled authorization response.
type callbackOutcome struct {
	code string
	err  error
}

// listener owns the bound socket and HTTP server for ONE login attempt.
//
// Both are built in listen(), before any goroutine exists, and neither field is written
// again. That is not incidental tidiness: wait() serves on a goroutine while the caller
// may still want to close the listener, and a server constructed inside wait() would be
// written by one goroutine and read by another with nothing between them. Constructing
// everything up front makes every field immutable for the listener's whole life, so
// Close is safe from anywhere.
//
// It also makes the type honest about its scope: a listener is bound to exactly one
// attempt's state, which is why state is a constructor argument rather than a parameter
// of wait.
type listener struct {
	srv  *http.Server
	ln   net.Listener
	done chan callbackOutcome
	// mismatches counts callbacks refused for a bad state. Refusing them silently is
	// correct — see handle — but it would also make a GENUINE mismatch (two sign-in
	// flows open at once, a stale tab replayed) indistinguishable from nobody ever
	// coming back, and the user would get a bare five-minute timeout with no clue. The
	// count turns that timeout into an accurate diagnosis without letting an
	// unauthenticated request settle anything.
	mismatches atomic.Int64
	// once makes the attempt single-shot. A second callback — a refresh of the tab, a
	// replayed URL, a second attempt racing this one — must not be able to settle an
	// outcome that was already decided, or to hand a stale code to the exchange.
	once sync.Once
}

// listen binds the fixed loopback callback port for an attempt with the given state.
//
// Binding happens BEFORE the browser is opened. The other order has a real race: a fast
// provider redirect can arrive before the socket is up, and the user sees a connection
// refused for a login that was otherwise fine.
func listen(state string) (*listener, error) {
	// Explicitly "tcp4". On a dual-stack machine "tcp" plus a bare IPv4 literal is
	// fine, but being explicit documents that ::1 is deliberately not served: the
	// registered URI names 127.0.0.1, so a request arriving over IPv6 is not the one we
	// asked for.
	ln, err := net.Listen("tcp4", callbackAddr())
	if err != nil {
		if isAddrInUse(err) {
			return nil, errPortInUse(CallbackPort, err)
		}
		return nil, wrapError(CodeInteractiveRequired,
			fmt.Sprintf("could not open the local sign-in callback on %s", callbackAddr()), err).
			withHint("Signing in needs a local loopback listener and a browser on this machine.")
	}
	l := &listener{ln: ln, done: make(chan callbackOutcome, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", l.handle(state))
	l.srv = &http.Server{
		Handler:           mux,
		MaxHeaderBytes:    maxCallbackHeaderBytes,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
	}
	return l, nil
}

// Close shuts the listener down and releases the port. Idempotent, and safe to call
// from any goroutine — including while wait() is serving.
func (l *listener) close() {
	shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	_ = l.srv.Shutdown(shutCtx)
	// Shutdown closes only the listeners the server has ADOPTED via Serve. Between
	// listen() and wait() there are none, so a failure in between — the browser refusing
	// to open, a retry that never gets that far — would otherwise leave this process
	// holding port 42813 for its whole lifetime, and every subsequent sign-in would
	// report a collision against itself. Closing the socket directly is idempotent; the
	// already-closed error on the normal path is not interesting.
	_ = l.ln.Close()
}

// isAddrInUse reports the specific errno for a busy port, so it can get its own
// message rather than being folded into a generic bind failure.
func isAddrInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}

// settle records the first outcome and ignores every later one.
func (l *listener) settle(out callbackOutcome) {
	l.once.Do(func() { l.done <- out })
}

// wait serves until an authorization response settles the attempt, the context is
// cancelled, or the deadline expires. It always closes the socket before returning.
func (l *listener) wait(ctx context.Context) (string, error) {
	serveErr := make(chan error, 1)
	go func() { serveErr <- l.srv.Serve(l.ln) }()

	ctx, cancel := context.WithTimeout(ctx, callbackTimeout)
	defer cancel()

	defer l.close()

	select {
	case out := <-l.done:
		return out.code, out.err
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return "", newError(CodeCancelled, "the sign-in listener closed")
		}
		return "", wrapError(CodeInteractiveRequired, "the local sign-in listener failed", err)
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			// Something DID come back; it just did not belong to this attempt. Saying so
			// is the difference between an actionable message and "it timed out".
			if l.mismatches.Load() > 0 {
				return "", newError(CodeStateMismatch,
					"a sign-in response arrived but did not match this attempt").
					withHint("Another sign-in may already be in progress. Close other sign-in tabs and run it again.")
			}
			return "", newError(CodeTimeout, "the sign-in attempt timed out waiting for the browser").
				withHint("Run sign-in again and complete it in the browser within five minutes.")
		}
		return "", newError(CodeCancelled, "sign-in was cancelled")
	}
}

// handle serves the callback. Every rejection below is deliberate; see each comment.
func (l *listener) handle(state string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// The remote address must be loopback. The socket is bound to 127.0.0.1 so this
		// should be redundant — but "should be" is not a security argument, and this is
		// the one request in the process that hands over an authorization code.
		if !isLoopbackRemote(r.RemoteAddr) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// The Host header must name our exact address. Without this, a page on any
		// origin the user visits can point a request at 127.0.0.1:42813 under its own
		// hostname (classic DNS rebinding), and the listener would answer it.
		if !hostMatches(r.Host) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet {
			// The provider redirects with a GET. Anything else is not the flow.
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path != CallbackPath {
			http.NotFound(w, r)
			return
		}

		q := r.URL.Query()

		// STATE IS CHECKED FIRST, before the error branch and before the code is read.
		//
		// The ordering is itself the security property. Any page the user visits can
		// issue `<img src="http://127.0.0.1:42813/oauth/callback?error=access_denied">`:
		// the browser sets a valid Host header, the peer is loopback, the method is GET
		// and the path matches, so every other check here passes. With the error branch
		// first, that image tag silently cancels a sign-in in progress — the response
		// never has to be readable for the damage to be done.
		//
		// OAuth 2.0 §4.1.2.1 requires the provider to echo `state` on error responses
		// too, so checking it first costs the legitimate path nothing.
		//
		// A mismatch gets a flat 403 and does NOT settle the attempt. An unauthenticated
		// request must not decide the outcome of a flow it cannot prove it started;
		// consuming the attempt here would turn the same image tag into a denial of
		// service with one extra step. Comparison is constant-time: an attacker who
		// could probe state one byte at a time could then forge a whole callback.
		if !sameState(state, q.Get("state")) {
			l.mismatches.Add(1)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// A denial is a normal ending. Treating it as a crash — or worse, retrying —
		// would fight a decision the user just made at the consent screen.
		if errCode := q.Get("error"); errCode != "" {
			// error_description is provider-controlled text arriving on a URL. It is
			// never rendered into the page and never put in the message: it would reach
			// terminal scrollback and the debug log, and it can carry anything. Only
			// the fixed `errCode` comparison below reads it at all.
			if errCode == "access_denied" {
				writePage(w, "Sign-in cancelled", "You can close this tab and return to the terminal.")
				l.settle(callbackOutcome{err: newError(CodeCancelled, "sign-in was declined at the consent screen")})
				return
			}
			writePage(w, "Sign-in failed", "Something went wrong. Return to the terminal for details.")
			l.settle(callbackOutcome{err: newError(CodeExchangeFailed, "the identity provider refused the sign-in request")})
			return
		}

		code := q.Get("code")
		if strings.TrimSpace(code) == "" {
			writePage(w, "Sign-in failed", "The response carried no authorization code. Return to the terminal.")
			l.settle(callbackOutcome{err: newError(CodeExchangeFailed, "the sign-in response carried no authorization code")})
			return
		}

		writePage(w, "Signed in", "You can close this tab and return to the terminal.")
		l.settle(callbackOutcome{code: code})
	}
}

// hostMatches reports whether the Host header names exactly our bound address.
func hostMatches(host string) bool {
	return strings.EqualFold(strings.TrimSpace(host), callbackAddr())
}

// isLoopbackRemote reports whether a RemoteAddr is a loopback peer.
func isLoopbackRemote(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// writePage renders the browser-facing result.
//
// The content is entirely compiled-in: no code, no state, no token, no email, and
// nothing echoed from the query string. That rules out reflected injection by
// construction rather than by escaping — there is no dynamic text to escape. It is also
// why the page cannot say WHICH error occurred; the terminal has that, and the terminal
// is where the user is going anyway.
func writePage(w http.ResponseWriter, title, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The page is inert, but it is served on a loopback origin a browser may treat as
	// same-site with other local services, so it declares its own lack of privileges.
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>%s</title>
<style>body{font:16px system-ui,sans-serif;margin:4rem auto;max-width:32rem;padding:0 1rem;color:#1a1a1a}
h1{font-size:1.25rem;margin:0 0 .5rem}p{margin:0;color:#555}
@media(prefers-color-scheme:dark){body{background:#111;color:#eee}p{color:#aaa}}</style>
</head><body><h1>%s</h1><p>%s</p></body></html>`, title, title, body)
}

// buildAuthorizeURL composes the authorization request.
//
// The result carries a live authorization request bound to this attempt's state, so it
// must never be logged or emitted as a structured event. Only --no-open prints it, to
// stderr, with a warning.
func buildAuthorizeURL(m *Manifest, attempt pkceAttempt) (string, error) {
	u, err := url.Parse(m.AuthorizationEndpoint)
	if err != nil {
		return "", newError(CodeDiscoveryInvalid, "the authorization endpoint is not a valid URL")
	}
	scopes := m.Scopes
	if len(scopes) == 0 {
		scopes = []string{"openid", "email"}
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", m.ClientID)
	q.Set("redirect_uri", RedirectURI())
	q.Set("state", attempt.State)
	q.Set("code_challenge", attempt.Challenge)
	q.Set("code_challenge_method", "S256")
	q.Set("scope", strings.Join(scopes, " "))
	u.RawQuery = q.Encode()
	return u.String(), nil
}
