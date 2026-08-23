package app

import (
	"sort"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/host"
)

// Every tool the engine actually registers has a human verb.
//
// The activity tree in an embedded host draws each call as a verb and a target
// ("Read src/main.go"). A tool the presentation table does not know falls back to its
// raw dotted identifier — deliberately, because inventing a label would be worse — so
// a missing entry does not fail anything. It just quietly puts `git.getProjectPulse`
// in front of the user where "Read git state" belonged, and looks like the feature
// working.
//
// This lives in `app` rather than beside the table in `host` because `app` is the only
// package where the real registry is assembled. An earlier version of this check
// scanned the tool sources for `Name: "x.y"` and missed every tool registered through
// a helper — `forgeRead("git.getProjectPulse", …)` among them — which is exactly the
// gap it was written to close. Asking the registry cannot have that blind spot.
func TestEveryRegisteredToolHasAPresentationVerb(t *testing.T) {
	dir := t.TempDir()
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    &dir,
			ProjectPath: &dir,
			// The WIDEST tier, so every gated tool is registered. A narrower one would
			// pass by simply not registering the tools with no verb.
			Tier:                 strPtr("system"),
			WorkflowIntelligence: boolPtr(true),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	all := a.Registry.List()
	if len(all) < 30 {
		t.Fatalf("only %d tools registered, which cannot be right — this test would pass vacuously", len(all))
	}

	var missing []string
	for _, tool := range all {
		if !host.HasPresentation(tool.Name) {
			missing = append(missing, tool.Name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("%d registered tool(s) have no human verb and will render as raw ids:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}
