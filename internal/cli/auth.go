package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/auth"
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
//     prompt, no warning — because Daintree's account UI parses it line by line and a
//     stray human sentence would break a login.
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
		"  disconnect  revoke Daintree Assistant for every device (opens the website)",
	}, "\n")
}

// authEventVersion versions the NDJSON event stream. Daintree validates every line
// against a shared schema keyed on this.
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
		return runAuthLogin(ctx, w, mgr, authOpts)
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

func runAuthLogin(ctx context.Context, w authWriter, mgr *auth.Manager, opts AuthOptions) int {
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
	return 0
}

func runAuthStatus(ctx context.Context, w authWriter, mgr *auth.Manager, cfg config.AppConfig, opts AuthOptions) int {
	// Hydrate FIRST. Status is deliberately I/O-free, so a manager freshly built in this
	// process knows nothing — without this, `auth status` immediately after a successful
	// login reports "unknown", which is the one answer that is never useful.
	mgr.Hydrate(ctx)

	// --refresh forces a live credential check rather than trusting what is on disk. It
	// is what someone runs right after completing a checkout, so it must actually reach
	// the network; a flag that silently did nothing would be worse than not having one.
	if opts.Refresh {
		if _, err := mgr.AccessToken(ctx); err != nil && !auth.IsCancelled(err) {
			// Recorded in the status below rather than failing outright: "could not
			// verify" is a legitimate thing for status to report.
			w.human("Could not verify the session: %s", authMessage(err))
		}
	}

	// Best-effort: a manifest fills in the environment and links, and its absence must
	// not stop status answering. "The backend is unreachable" is precisely when someone
	// runs this.
	st := mgr.Status()
	if man, err := mgr.Manifest(ctx); err == nil {
		st = st.WithManifest(man)
	}
	st.BackendURL = sanitizeURLForDisplay(st.BackendURL)

	exit := 0
	if st.State.NeedsLogin() {
		// A distinct exit code so a script can branch on "not signed in" without parsing
		// prose, while keeping it separate from an outright failure.
		exit = 3
	}

	if w.json {
		// ONE LINE. json.MarshalIndent would emit a multi-line document, and the first
		// line a caller read would be a bare "{" — Daintree parses this stream line by
		// line, so an indented status is not merely ugly, it is unparseable. The status
		// rides inside a versioned event for the same reason every other line does.
		w.event(authEvent{Type: "auth:status", Env: st.Environment, Extra: st})
		return exit
	}
	renderAuthStatus(w, st, cfg)
	return exit
}

// sanitizeURLForDisplay strips credentials from a URL before it is printed.
//
// The backend URL is operator-supplied and reaches stdout in both output modes. Nothing
// upstream rejects userinfo in an https:// endpoint, so
// `DAINTREE_BACKEND_URL=https://user:secret@example.test` would otherwise put `secret` on
// standard output of a command whose entire premise is that it prints no credentials.
func sanitizeURLForDisplay(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.User == nil {
		return raw
	}
	u.User = url.User("«redacted»")
	return u.String()
}

// renderAuthStatus writes the human status block.
func renderAuthStatus(w authWriter, st auth.Status, cfg config.AppConfig) {
	w.human("Account")
	w.human("  backend      %s", st.BackendURL)
	if st.Environment != "" {
		w.human("  environment  %s", st.Environment)
	}
	w.human("  state        %s", authStateLabel(st.State))
	if st.Email != "" {
		w.human("  email        %s", st.Email)
	}
	if st.Plan != "" {
		w.human("  plan         %s", st.Plan)
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
	case st.State.NeedsPlan() && st.Links.Subscribe != "":
		w.human("")
		w.human("Choose a plan: %s", st.Links.Subscribe)
	}
}

// authStateLabel renders a state for a person.
func authStateLabel(s auth.State) string {
	switch s {
	case auth.StateSignedOut:
		return "signed out"
	case auth.StateSignedInActive:
		return "signed in"
	case auth.StateSignedInUnverified:
		return "signed in (not verified this session)"
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
