package app

import (
	"errors"
	"fmt"
	"strings"

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

// ResolveBackendTarget maps what a user typed to a base URL. An alias, a bare number
// (the menu position, which is what people actually type after reading a list), or a URL.
//
// A URL is taken almost as-is: there is no normalisation step anywhere else in this
// process either, so inventing one here would make `/backend` accept spellings that
// DAINTREE_BACKEND_URL rejects. The one thing enforced is a scheme, because "127.0.0.1:8473"
// parses as a URL with an empty host and would otherwise fail much later as an
// unhelpful transport error.
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
	if !strings.HasPrefix(a, "http://") && !strings.HasPrefix(a, "https://") {
		return "", fmt.Errorf("%q is not one of %s and is not a URL — a custom endpoint needs its scheme (http:// or https://)",
			a, backendAliasList())
	}
	return strings.TrimRight(a, "/"), nil
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
// It is the CALLER's job to refuse this mid-turn. A turn is multi-round, and swapping
// between rounds would send the next round to a different endpoint carrying a `state`
// token the previous one signed. The cockpit gates on its own in-flight flag; the
// classic REPL runs commands and turns on one goroutine, so it cannot be mid-turn here.
func (a *App) SetBackendURL(rawURL string) (string, error) {
	target, err := ResolveBackendTarget(rawURL)
	if err != nil {
		return "", err
	}
	sw, ok := a.Backend.(*backend.Swappable)
	if !ok {
		// The invariant App.Create establishes. A test double that bypassed it should
		// hear about it here rather than silently ignore the switch.
		return "", errors.New("this session's backend cannot be switched")
	}
	if target == a.snapshotConfig().BackendURL {
		return target, nil
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
	if stored := config.LoadBackendURL(cfg.EndpointPath); stored != "" {
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
	if err != nil {
		return target, err
	}
	if err := config.ForgetBackendURL(a.snapshotConfig().EndpointPath); err != nil {
		return target, fmt.Errorf("switched for this session only — could not clear the stored choice: %w", err)
	}
	return target, nil
}

// HasStoredBackendURL reports whether a choice is currently remembered. The picker uses
// it to offer "forget it" only when there is something to forget, so the option never
// appears as a no-op.
func (a *App) HasStoredBackendURL() bool {
	return config.LoadBackendURL(a.snapshotConfig().EndpointPath) != ""
}
