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
// It exists because dropping the sign-in dropped the endpoint picker with it. The
// endpoint itself still had two perfectly good mechanisms (DAINTREE_BACKEND_URL and
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
//     signed. The cockpit refuses /backend while its own turn is in flight, but that
//     flag only knows about the cockpit — an autonomous wake turn, the classic REPL, a
//     one-shot, or an MCP-driven session would all sail past it. Session.DropBackendState
//     returns ErrTurnInProgress from under the session lock, which is the only place that
//     knows for certain.
//   - Dropping that state token. It is server-SIGNED and endpoint-specific, so carrying
//     it across a switch hands the new backend a token it cannot verify. The conversation
//     survives; only the server's skill-selection state is endpoint-bound, and the new
//     backend simply re-runs selection.
func (a *App) SetBackendURL(rawURL string) (string, error) {
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
		if err := config.SaveBackendURL(a.snapshotConfig().EndpointPath, target); err != nil {
			return target, fmt.Errorf("already using this endpoint — but could not save the choice: %w", err)
		}
		return target, nil
	}
	// BEFORE the swap. If a turn is running we must change nothing at all — a swap that
	// happened and then reported failure would be the worst of both.
	if a.Session != nil {
		if err := a.Session.DropBackendState(); err != nil {
			return "", err
		}
	}

	// Rebuild through the SAME config builder Create uses, so the replacement inherits
	// every hook — the debug-log tracing especially, whose absence would only show up
	// much later as a session log with a hole in it from the moment of the switch.
	// The ledger is passed through deliberately: it outlives the client, so the session
	// total keeps counting across the switch instead of resetting to zero.
	cfg := a.snapshotConfig()
	cfg.BackendURL = target
	sw.Swap(backend.NewClient(backendClientConfig(cfg, a.CostLedger)))

	a.cfgMu.Lock()
	a.Config.BackendURL = target
	a.cfgMu.Unlock()

	// PERSIST. A switch that evaporated on restart is the thing this command exists to
	// stop: the daily case is a developer on a local backend who dips into the deployed
	// one to compare, and re-choosing on every launch is exactly the friction that gets
	// buried in a shell alias instead.
	//
	// Persisting is best-effort and reported, never fatal: the swap has ALREADY taken
	// effect for this session, so a read-only home directory should not turn a working
	// switch into a failed one. The caller says "switched, but it will not survive a
	// restart" rather than pretending either half did not happen.
	if err := config.SaveBackendURL(cfg.EndpointPath, target); err != nil {
		return target, fmt.Errorf("switched for this session only — could not save the choice: %w", err)
	}

	// The capability descriptor is pinned to the endpoint that answered it, so the
	// cached one is discarded on read rather than needing to be cleared here — a gate
	// opened on the old deployment's answer would 422 every turn against the new one.
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
	b.WriteString("\nSwitch with `/backend <number|alias|url>` — applies from the next message,\n")
	b.WriteString("and is remembered across restarts. `/backend " + BackendResetAlias + "` forgets it again.\n")

	cfg := a.snapshotConfig()
	stored, storedErr := config.LoadBackendURL(cfg.EndpointPath)
	switch {
	case storedErr != nil:
		// Surfaced, not swallowed. A preference that exists but cannot be read means
		// this session is on the default WITHOUT the user having chosen it.
		fmt.Fprintf(&b, "\nA remembered choice exists but could not be read, so it is being ignored:\n  %s\n", storedErr)
	case stored != "":
		fmt.Fprintf(&b, "\nRemembered: %s\n", mcp.SanitizeURL(stored))
	}
	if cfg.BackendURLPinnedByEnv {
		// The one failure this listing has to be able to explain. Someone switches, sees
		// it confirmed, restarts, and lands somewhere else because a shell profile
		// exports the variable — without this line the feature just looks broken.
		b.WriteString("\nNOTE: DAINTREE_BACKEND_URL (or --backend-url) is set, and it OVERRIDES the\n")
		b.WriteString("remembered choice on every launch. Unset it for `/backend` to stick.\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// ResetBackendURL forgets the stored choice and returns this session to the deployed
// default. Separate from SetBackendURL("official") because it also REMOVES the file: a
// user who wants "no preference" should end up with nothing on disk, not with the
// current default frozen into a file that would keep pinning it after the default moved.
func (a *App) ResetBackendURL() (string, error) {
	target, err := a.SetBackendURL(backend.DefaultBaseURL)
	// DELETE EVEN WHEN THE SWITCH FAILED TO PERSIST. SetBackendURL writes the default to
	// the file on its way through, so returning early on a save error would leave the
	// preference pinned to a value the user just asked to forget — the one outcome this
	// command exists to prevent. A refused switch (a turn in flight) is different: it
	// changed nothing, so there is nothing to clean up.
	if errors.Is(err, agent.ErrTurnInProgress) {
		return "", err
	}
	a.switchMu.Lock()
	defer a.switchMu.Unlock()
	if ferr := config.ForgetBackendURL(a.snapshotConfig().EndpointPath); ferr != nil {
		return target, fmt.Errorf("switched for this session only — could not clear the stored choice: %w", ferr)
	}
	return target, err
}

// HasStoredBackendURL reports whether a choice is currently remembered. The picker uses
// it to offer "forget it" only when there is something to forget, so the option never
// appears as a no-op.
func (a *App) HasStoredBackendURL() bool {
	stored, _ := config.LoadBackendURL(a.snapshotConfig().EndpointPath)
	return stored != ""
}
