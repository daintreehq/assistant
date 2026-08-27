package app

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/mcp"
)

// backendswitch.go is `/backend`: choosing which Daintree backend answers this session.
//
// It exists because the endpoint deserves a first-class choice at runtime, not only at
// launch. The endpoint itself had two perfectly good mechanisms (DAINTREE_BACKEND_URL and
// --backend-url), but both are decided BEFORE launch, and switching meant quitting. This
// puts the choice back where it is actually made — mid-session, while comparing a local
// backend against the deployed one — without putting a credential store back with it.

// BackendChoice is one selectable endpoint. Alias is what a user types.
type BackendChoice struct {
	Alias string
	URL   string
	Note  string
}

// BackendChoices is the offered menu. Deliberately short: these are the two endpoints
// that exist, and anything else is a URL the caller types in full.
var BackendChoices = []BackendChoice{
	{Alias: "official", URL: backend.DefaultBaseURL, Note: "the deployed Daintree backend"},
	{Alias: "local", URL: backend.LocalBaseURL, Note: "a backend you are running yourself"},
}

// BackendResetAlias forgets the stored choice rather than storing another one. Distinct
// from picking "official": one means "go back to having no preference", the other pins
// the deployed endpoint explicitly. They resolve to the same URL today and would not if
// the default ever moved.
const BackendResetAlias = "default"

// maxBackendURLLength bounds a custom endpoint. Any real one is far shorter; the cap
// only stops an absurd value being persisted and rendered.
const maxBackendURLLength = 2048

// ResolveBackendTarget maps what a user typed to a base URL. An alias, a bare number
// (the menu position, which is what people actually type after reading a list), or a URL.
//
// A custom URL is VALIDATED, not taken as typed. Everything this rejects is something
// that fails silently or dangerously if it is allowed through:
//
//   - **userinfo** (`https://user:pass@host`). Go's http.Client turns URL userinfo into a
//     Basic `Authorization` header automatically when no other one is set, so this
//     quietly starts authenticating every request with a credential nothing in this
//     process knows it is sending. It would also be persisted in cleartext and rendered.
//   - **query or fragment**. The client joins the API path onto this base, so
//     `https://host?token=x` becomes `https://host?token=x/v1/daintree/respond` and the
//     request lands on `/`. A fragment is never sent at all. Both produce a baffling
//     404 rather than an obviously wrong endpoint.
//   - **plaintext http:// to a REMOTE host**. Every turn carries the whole conversation,
//     the project context, tool arguments and tool results across that wire, and an
//     on-path attacker can also rewrite the streamed response to inject tool calls that
//     then run under the session's tier and grants. Loopback is exempt: there is no
//     network to intercept, and it is the local development loop.
//   - **control characters**, which would otherwise reach the terminal through the
//     masthead and command cards before request construction ever rejected them.
//
// The old sign-in flow normalised endpoints and was deleted with it; this is that
// guarantee restored at the one door a custom endpoint now comes through.
func ResolveBackendTarget(arg string) (string, error) {
	a := strings.TrimSpace(arg)
	if a == "" {
		return "", errors.New("no endpoint given")
	}
	for i, c := range BackendChoices {
		if strings.EqualFold(a, c.Alias) || a == fmt.Sprint(i+1) {
			return c.URL, nil
		}
	}
	if strings.EqualFold(a, BackendResetAlias) {
		return backend.DefaultBaseURL, nil
	}
	lower := strings.ToLower(a)
	if !strings.HasPrefix(lower, "http://") && !strings.HasPrefix(lower, "https://") {
		return "", fmt.Errorf("%q is not one of %s, %s, a menu number, or a URL — a custom endpoint needs its scheme (http:// or https://)",
			a, backendAliasList(), BackendResetAlias)
	}
	if len(a) > maxBackendURLLength {
		return "", fmt.Errorf("endpoint is too long (%d bytes, max %d)", len(a), maxBackendURLLength)
	}
	for _, r := range a {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("endpoint contains control characters — check for a stray paste")
		}
	}
	u, err := url.Parse(a)
	if err != nil {
		return "", fmt.Errorf("%q is not a usable URL: %w", a, err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("%q has no host", a)
	}
	if u.User != nil {
		return "", errors.New("an endpoint must not embed a username or password — Go would send it as an Authorization header on every request")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("an endpoint must not carry a query string or fragment — the API path is joined onto it, so the request would never reach the API")
	}
	if u.Scheme == "http" && !backend.IsLoopbackURL(a) {
		return "", fmt.Errorf("%s is plaintext http to a remote host — every turn would cross that wire in the clear. Use https://, or a loopback address for local development", u.Host)
	}
	// Rebuild from the parsed form rather than returning the input, so the stored and
	// displayed value is the canonical one and two spellings of the same endpoint
	// compare equal.
	u.Path = strings.TrimRight(u.Path, "/")
	return u.String(), nil
}

func backendAliasList() string {
	names := make([]string, 0, len(BackendChoices))
	for _, c := range BackendChoices {
		names = append(names, c.Alias)
	}
	return strings.Join(names, ", ")
}

// SetBackendURL points this session at another backend, in place.
//
// The swap is the whole reason App.Backend is a backend.Swappable: every consumer —
// Session, the watcher engine, the async coordinator, the workflow layer — holds the
// wrapper, so none of them has to be told. A call already in flight finishes on the old
// client, which is correct rather than a compromise: a stream cannot be moved to another
// endpoint halfway through without corrupting the transcript.
//
// Two things have to happen before the swap, and both are handled here rather than left
// to callers, so no surface can forget them:
//
//   - The turn gate. A turn is multi-round, and swapping between rounds would send the
//     next round to a backend that cannot read the `state` token the previous one
//     signed. The attached session refuses /backend while its own turn is in flight, but that
//     flag only knows about the attached session — an autonomous wake turn, the line REPL, a
//     one-shot, or an MCP-driven session would all sail past it. Session.SwapBackend
//     returns ErrTurnInProgress from under the session lock, and HOLDS that lock across
//     the swap — the only place that knows for certain, and the only way to know it for
//     longer than an instant.
//   - Dropping that state token. It is server-SIGNED and endpoint-specific, so carrying
//     it across a switch hands the new backend a token it cannot verify. The conversation
//     survives; only the server's runbook-selection state is endpoint-bound, and the new
//     backend simply re-runs selection.
//
// ErrBackendPinned is the refusal every switch route must be able to recognize, which
// is why it is a sentinel rather than an inline errors.New: ResetBackendURL runs its own
// cleanup after SetBackendURL returns, and an unrecognizable refusal there meant the
// command reported "pinned, nothing changed" while having DELETED the stored preference.
var ErrBackendPinned = errors.New("this session's endpoint was pinned by whatever launched it (DAINTREE_BACKEND_URL or --backend-url), so it cannot be switched from in here — change it where the session is started")

func (a *App) SetBackendURL(rawURL string) (string, error) {
	return a.setBackendURL(0, rawURL)
}

// setBackendURL is SetBackendURL with the caller's endpoint reservation, if it holds
// one. A zero token means "no reservation", which is refused while somebody else holds
// one — see Session.SwapBackendReserved.
func (a *App) setBackendURL(token uint64, rawURL string) (string, error) {
	// PINNED sessions cannot be switched at all.
	//
	// DAINTREE_BACKEND_URL (or --backend-url) means a HOST chose this session's
	// endpoint, and it outranks the stored preference at every startup — so a switch
	// here could only ever move the live client while leaving the pin in place,
	// producing a session whose requests go somewhere its own configuration says they
	// do not, until the next launch silently moves them back.
	//
	// That gap is a security boundary for an embedding host, not just a confusing
	// state: Daintree spawns this engine with a loopback endpoint precisely because the
	// panel is unauthenticated, and a session that can be talked into switching to a
	// remote endpoint from inside undoes that with one line of prose. The refusal
	// belongs here rather than in the host, because here is the only place that knows
	// the pin exists and every route to a swap passes through it.
	if a.snapshotConfig().BackendURLPinnedByEnv {
		return "", ErrBackendPinned
	}
	target, err := ResolveBackendTarget(rawURL)
	if err != nil {
		return "", err
	}
	// One switch at a time, end to end. Without this the three writes below (delegate,
	// config, disk) can interleave with another switch and settle on three different
	// endpoints — diagnostics reporting one while requests go somewhere else.
	a.switchMu.Lock()
	defer a.switchMu.Unlock()
	sw, ok := a.Backend.(*backend.Swappable)
	if !ok {
		// The invariant App.Create establishes. A test double that bypassed it should
		// hear about it here rather than silently ignore the switch.
		return "", errors.New("this session's backend cannot be switched")
	}
	if target == a.snapshotConfig().BackendURL {
		// Already live, so nothing to swap — but STILL persist. Choosing the endpoint you
		// are already on is exactly how someone pins it (`/backend official` on a fresh
		// install, or `/backend local` while the environment happens to supply local),
		// and returning here silently reported "Remembered for future sessions" while
		// writing nothing at all.
		//
		// Routed through the SESSION even though there is nothing to swap. It is still a
		// durable write to the same preference a parked picker is about to make, and a
		// fast path that skipped the gate let an unreserved `/backend <current>` report
		// success and pin the endpoint while somebody else owned the decision.
		var saveErr error
		gated := func() {
			saveErr = config.SaveBackendURL(a.snapshotConfig().EndpointPath, target)
		}
		if a.Session != nil {
			if err := a.Session.SwapBackendReserved(token, gated); err != nil {
				return "", err
			}
		} else {
			gated()
		}
		if saveErr != nil {
			return target, fmt.Errorf("already using this endpoint — but could not save the choice: %w", saveErr)
		}
		return target, nil
	}
	// The swap, as ONE indivisible act against the session.
	//
	// Rebuild through the SAME config builder Create uses, so the replacement inherits
	// every hook — the debug-log tracing especially, whose absence would only show up
	// much later as a session log with a hole in it from the moment of the switch.
	// The ledger is passed through deliberately: it outlives the client, so the session
	// total keeps counting across the switch instead of resetting to zero.
	swap := func() {
		cfg := a.snapshotConfig()
		cfg.BackendURL = target
		// A new endpoint means a new credential key (see auth.CredentialKey), so the
		// manager is rebuilt too — carrying the old one over would present a credential
		// minted for one deployment to another, and would leave the new endpoint's
		// verdicts landing on the old endpoint's state.
		//
		// Replaced on the App as well as handed to the client, so the two cannot
		// diverge: every account question after a switch is about the endpoint now in
		// use.
		mgr := NewAccountManager(cfg)
		sw.Swap(backend.NewClient(backendClientConfig(cfg, a.CostLedger, accountTokenSource(mgr))))

		a.cfgMu.Lock()
		a.Config.BackendURL = target
		a.Auth = mgr
		a.cfgMu.Unlock()
	}

	// Held ACROSS the whole swap, not merely checked before it. If a turn is running we
	// must change nothing at all — a swap that happened and then reported failure would
	// be the worst of both — and a check that releases the session before swapping lets
	// a turn start in the gap, open against the old endpoint and finish against the new
	// one. See Session.SwapBackend.
	if a.Session != nil {
		if err := a.Session.SwapBackendReserved(token, swap); err != nil {
			return "", err
		}
	} else {
		swap()
	}

	// Read AFTER the swap, not from inside it. EndpointPath is derived from the state
	// root and does not move with the endpoint, but taking it from the live config keeps
	// this reading one source of truth rather than a value carried out of a closure.
	endpointPath := a.snapshotConfig().EndpointPath

	// PERSIST. A switch that evaporated on restart is the thing this command exists to
	// stop: the daily case is a developer on a local backend who dips into the deployed
	// one to compare, and re-choosing on every launch is exactly the friction that gets
	// buried in a shell alias instead.
	//
	// Persisting is best-effort and reported, never fatal: the swap has ALREADY taken
	// effect for this session, so a read-only home directory should not turn a working
	// switch into a failed one. The caller says "switched, but it will not survive a
	// restart" rather than pretending either half did not happen.
	if err := config.SaveBackendURL(endpointPath, target); err != nil {
		return target, fmt.Errorf("switched for this session only — could not save the choice: %w", err)
	}

	// The capability descriptor is pinned to the endpoint that answered it, so the
	// cached one is discarded on read rather than needing to be cleared here — a gate
	// opened on the old deployment's answer would 422 every turn against the new one.
	// Server-side context compaction reads the same cache and therefore goes quiet after
	// a switch until something negotiates with the new endpoint — which is correct while
	// nothing has: a block spliced on the strength of the old deployment's contract would
	// rewrite this conversation's history on a promise nobody made.
	return target, nil
}

// DescribeBackendChoices renders the menu `/backend` shows when given no argument. The
// live endpoint is marked, because "which am I on?" is half the reason to run it.
func (a *App) DescribeBackendChoices() string {
	current := a.snapshotConfig().BackendURL
	var b strings.Builder
	b.WriteString("Backend endpoint for this session.\n\n")
	matched := false
	for i, c := range BackendChoices {
		marker := "  "
		if c.URL == current {
			marker, matched = "→ ", true
		}
		fmt.Fprintf(&b, "%s%d. %-9s %s\n", marker, i+1, c.Alias, c.URL)
		fmt.Fprintf(&b, "     %s\n", c.Note)
	}
	if !matched && current != "" {
		// A custom endpoint is not in the menu but IS what is answering, so it has to be
		// named — a menu with nothing marked would read as "none of these".
		fmt.Fprintf(&b, "→    custom    %s\n", mcp.SanitizeURL(current))
	}
	// One physical line per paragraph. These are read in a narrow side panel as often as
	// in a terminal, and a line already broken at 80 columns wraps a second time there —
	// which is what turned this listing into the ragged block that made it look broken.
	b.WriteString("\nSwitch with `/backend <number|alias|url>` — it applies from the next message and is remembered across restarts. `/backend " + BackendResetAlias + "` forgets it again.\n")

	cfg := a.snapshotConfig()
	stored, storedErr := config.LoadBackendURL(cfg.EndpointPath)
	switch {
	case storedErr != nil:
		// Surfaced, not swallowed. A preference that exists but cannot be read means
		// this session is on the default WITHOUT the user having chosen it.
		fmt.Fprintf(&b, "\nA remembered choice exists but could not be read, so it is being ignored: %s\n", storedErr)
	case stored != "":
		fmt.Fprintf(&b, "\nRemembered: %s\n", mcp.SanitizeURL(stored))
	}
	if cfg.BackendURLPinnedByEnv {
		// The one failure this listing has to be able to explain, and it has to explain
		// the RIGHT one. This note used to say the pin would override the choice at the
		// next launch, which describes a switch that succeeds and is quietly undone
		// later; what actually happens is that every switch is refused now
		// (ErrBackendPinned). Someone reading the old wording would try, be refused, and
		// have been told to expect something else entirely.
		b.WriteString("\nNOTE: DAINTREE_BACKEND_URL (or --backend-url) pinned this session's endpoint, so it cannot be switched from in here. Change it where the session is started.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ResetBackendURL forgets the stored choice and returns this session to the deployed
// default. Separate from SetBackendURL("official") because it also REMOVES the file: a
// user who wants "no preference" should end up with nothing on disk, not with the
// current default frozen into a file that would keep pinning it after the default moved.
func (a *App) ResetBackendURL() (string, error) {
	return a.ResetReservedBackendURL(0)
}

// ResetReservedBackendURL is ResetBackendURL carrying the caller's endpoint
// reservation, if it holds one. A zero token means "no reservation".
func (a *App) ResetReservedBackendURL(token uint64) (string, error) {
	target, err := a.setBackendURL(token, backend.DefaultBaseURL)
	// DELETE EVEN WHEN THE SWITCH FAILED TO PERSIST. SetBackendURL writes the default to
	// the file on its way through, so returning early on a save error would leave the
	// preference pinned to a value the user just asked to forget — the one outcome this
	// command exists to prevent. A refused switch (a turn in flight) is different: it
	// changed nothing, so there is nothing to clean up. A PINNED session is the same
	// case and for a stronger reason: the pin is a security boundary (see
	// SetBackendURL), and "forget the preference" is a switch route like any other, so
	// clearing the file here would let the refusal be worked around by asking to reset.
	if errors.Is(err, agent.ErrTurnInProgress) || errors.Is(err, ErrBackendPinned) {
		return "", err
	}
	a.switchMu.Lock()
	defer a.switchMu.Unlock()
	if ferr := config.ForgetBackendURL(a.snapshotConfig().EndpointPath); ferr != nil {
		return target, fmt.Errorf("switched for this session only — could not clear the stored choice: %w", ferr)
	}
	// `err` is DISCARDED once the file is gone, and only here.
	//
	// The one error it can still carry at this point is SetBackendURL's "could not save
	// the choice" — and this command's whole purpose is for no choice to be saved. The
	// write failing and the delete succeeding is the requested end state reached by a
	// shorter route, so reporting the failed write would tell the user a reset they got
	// in full only half worked.
	return target, nil
}

// BackendPick is one row of the `/backend` picker: what the user sees, and the argument
// that applies it. Target is fed straight back to SetBackendURL/ResetBackendURL through
// BackendSwitchText, so the picker and the typed command take exactly the same route —
// two surfaces that reached the same switch by different code is how they end up
// disagreeing about what "default" means.
type BackendPick struct {
	// Text is the option as it is rendered, one line, no label letter (the client
	// assigns those).
	Text string
	// Target is what `/backend <target>` would have been typed as.
	Target string
}

// BackendChoiceQuestion builds the multiple-choice form of `/backend` with no argument:
// the same menu DescribeBackendChoices prints, as a question a surface with a real picker
// can render.
//
// It is a QUESTION rather than a listing because "which backend answers?" is a choice
// with a fixed, short answer set — the exact shape the question channel exists for. The
// listing survives for surfaces that cannot ask (one-shot, a non-TTY), which is why this
// returns data rather than rendering: the caller decides which of the two it can use.
//
// The picks are ordered menu-first so a number typed by habit still means what it meant
// in the printed listing. The live endpoint is marked in its own row rather than only
// through Default, because Default is the initial highlight and stops saying anything
// the moment someone presses an arrow.
func (a *App) BackendChoiceQuestion() (question string, picks []BackendPick, defaultIndex int) {
	cfg := a.snapshotConfig()
	current := cfg.BackendURL
	picks = make([]BackendPick, 0, len(BackendChoices)+2)
	matched := false
	for _, c := range BackendChoices {
		text := c.Alias + " — " + c.URL + " · " + c.Note
		if c.URL == current {
			text += " · current"
			matched = true
			defaultIndex = len(picks)
		}
		picks = append(picks, BackendPick{Text: text, Target: c.Alias})
	}
	if !matched && current != "" {
		// A custom endpoint is not in the menu but IS what is answering. Without a row
		// of its own the picker would offer no way to KEEP it — every option would be a
		// switch, and the highlight would start on an endpoint the user is not using.
		defaultIndex = len(picks)
		picks = append(picks, BackendPick{
			Text:   "custom — " + mcp.SanitizeURL(current) + " · current",
			Target: current,
		})
	}
	// Offered only when there is something to forget. "Forget" and "pick the deployed
	// one" resolve to the same URL today and would not if the default ever moved, so the
	// row appears only when the two are genuinely different acts.
	stored, storedErr := config.LoadBackendURL(cfg.EndpointPath)
	if storedErr == nil && stored != "" {
		picks = append(picks, BackendPick{
			Text:   "forget the remembered choice — new sessions use whatever the default is",
			Target: BackendResetAlias,
		})
	}

	// The question DISCLOSES that answering it writes something down.
	//
	// "Which backend should answer this session?" was the honest half of a durable act:
	// every ordinary choice is saved for future sessions, including re-picking the row
	// already marked current, which is exactly how someone pins an endpoint they only
	// meant to confirm. A picker that hides its own persistence is a picker people
	// answer without meaning to.
	question = "Which backend should answer? Your choice applies from the next message and is remembered for future sessions."
	if storedErr != nil {
		// Surfaced, not swallowed — the printed listing says this and the picker must
		// not be the quieter of the two. A preference that exists but cannot be read
		// means this session is on the default WITHOUT the user having chosen it, which
		// changes what every row below means.
		question += " (A remembered choice exists but could not be read, so it is being ignored: " + storedErr.Error() + ")"
	}
	return question, picks, defaultIndex
}

// ReserveEndpointSwitch claims the session for a switch that is about to be DECIDED, and
// blocks turn admission until the returned release is called. It is always safe to call
// the release, including on the error path, which is why one is returned either way.
//
// The command layer holds this across the `/backend` picker. Without it the sheet can be
// opened during a turn, read, answered — and then refused, which is the one outcome a
// question must never produce: the user made the decision it asked for and it did not
// count. See agent.Session.ReserveEndpoint.
func (a *App) ReserveEndpointSwitch() (token uint64, release func(), err error) {
	if a.Session == nil {
		return 0, func() {}, nil
	}
	tok, err := a.Session.ReserveEndpoint()
	if err != nil {
		return 0, func() {}, err
	}
	return tok, func() { a.Session.ReleaseEndpoint(tok) }, nil
}

// SetReservedBackendURL is SetBackendURL for the holder of a reservation from
// ReserveEndpointSwitch. Passing the token is what lets the swap through the
// reservation that exists to guarantee it; everyone else is refused while one is held.
func (a *App) SetReservedBackendURL(token uint64, rawURL string) (string, error) {
	return a.setBackendURL(token, rawURL)
}

// BackendSwitchable reports whether a switch could succeed at all.
//
// False means the endpoint was PINNED by whatever launched this session, and every
// route to a swap refuses (see ErrBackendPinned) — so the picker must not open. A sheet
// whose every option is refused the moment it is chosen is worse than no sheet: it
// presents a decision the session is not allowed to make, and the refusal arrives only
// after the user has committed to one.
func (a *App) BackendSwitchable() bool {
	return !a.snapshotConfig().BackendURLPinnedByEnv
}
