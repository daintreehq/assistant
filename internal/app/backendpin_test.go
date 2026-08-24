package app

import (
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
)

// A session whose endpoint was PINNED by whatever launched it cannot be switched from
// inside the conversation.
//
// This is a security boundary for an embedding host, not a nicety. Daintree spawns this
// engine with a loopback endpoint precisely because its native panel is unauthenticated
// and carries every prompt, path and tool argument over that wire. Without this,
// `/backend https://elsewhere` — one line of prose, from the model or from a user who
// did not read what they pasted — moves the live client off-box for the rest of the
// session, while the pin silently moves it back on the next launch.
//
// It is also simply honest: the pin outranks the stored preference at every startup, so
// a switch could only ever produce a session whose requests go somewhere its own
// configuration says they do not.
func TestPinnedSessionRefusesBackendSwitch(t *testing.T) {
	dir := t.TempDir()
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:              boolPtr(true),
			StateDir:             &dir,
			ProjectPath:          &dir,
			Tier:                 strPtr("operator"),
			WorkflowIntelligence: boolPtr(false),
			// The pin. `--backend-url` and DAINTREE_BACKEND_URL are the same decision
			// made two ways, and config.Load sets BackendURLPinnedByEnv for both.
			BackendURL: strPtr(backend.LocalBaseURL),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	if !a.snapshotConfig().BackendURLPinnedByEnv {
		t.Fatal("precondition: an explicit BackendURL override should pin the session")
	}

	// The remote case is the one that matters, but an alias and a same-endpoint no-op
	// are refused too: a pinned session is not switchable, and a partial rule would
	// leave the stored preference writable from inside a session that cannot honour it.
	for _, target := range []string{
		"https://evil.test",
		"official",
		backend.LocalBaseURL,
	} {
		if _, err := a.SetBackendURL(target); err == nil {
			t.Fatalf("SetBackendURL(%q) was allowed on a pinned session", target)
		} else if !strings.Contains(err.Error(), "pinned") {
			t.Fatalf("SetBackendURL(%q): expected a refusal naming the pin, got %v", target, err)
		}
	}

	// And nothing moved. A refusal that had already swapped the client would be worse
	// than allowing it, because the error would say the opposite of what happened.
	if got := a.Backend.BaseURL(); got != backend.LocalBaseURL {
		t.Fatalf("the endpoint moved despite the refusal: %q", got)
	}
}

// The refusal is scoped to PINNED sessions: an ordinary one still switches, or the
// escape hatch this guards would have swallowed the feature.
func TestUnpinnedSessionStillSwitches(t *testing.T) {
	dir := t.TempDir()
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:              boolPtr(true),
			StateDir:             &dir,
			ProjectPath:          &dir,
			Tier:                 strPtr("operator"),
			WorkflowIntelligence: boolPtr(false),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	if _, err := a.SetBackendURL(backend.LocalBaseURL); err != nil {
		t.Fatalf("SetBackendURL on an unpinned session: %v", err)
	}
	if got := a.Backend.BaseURL(); got != backend.LocalBaseURL {
		t.Fatalf("the swap did not take: %q", got)
	}
}

// `/backend reset` is a switch route like any other, so the pin has to hold there too.
//
// It nearly did not: ResetBackendURL runs its own ForgetBackendURL cleanup AFTER
// SetBackendURL returns, and it only recognized the turn-in-progress refusal. The pin
// refusal fell through to the cleanup, so the command reported "pinned, nothing changed"
// while having DELETED the stored preference — a refusal that lies, and a way to reach
// the one write the pin exists to prevent by asking for it under another name.
func TestPinnedSessionRefusesBackendReset(t *testing.T) {
	dir := t.TempDir()
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:              boolPtr(true),
			StateDir:             &dir,
			ProjectPath:          &dir,
			Tier:                 strPtr("operator"),
			WorkflowIntelligence: boolPtr(false),
			BackendURL:           strPtr(backend.LocalBaseURL),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	// A preference on disk is what the refusal has to protect.
	endpointPath := a.snapshotConfig().EndpointPath
	if err := config.SaveBackendURL(endpointPath, "https://remembered.test"); err != nil {
		t.Fatalf("SaveBackendURL: %v", err)
	}

	if _, err := a.ResetBackendURL(); err == nil {
		t.Fatal("ResetBackendURL was allowed on a pinned session")
	} else if !errors.Is(err, ErrBackendPinned) {
		t.Fatalf("ResetBackendURL: expected the pin sentinel, got %v", err)
	}

	stored, err := config.LoadBackendURL(endpointPath)
	if err != nil {
		t.Fatalf("LoadBackendURL: %v", err)
	}
	if stored != "https://remembered.test" {
		t.Fatalf("the stored preference was cleared by a refused reset: %q", stored)
	}
}
