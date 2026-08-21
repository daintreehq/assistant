package commands

import (
	"strings"
	"testing"
)

// The registry is the single source of truth
// for slash commands; its exact ordered set is pinned (so silently dropping a
// command like /models — the original #50 bug — fails here), every entry carries
// non-empty palette/syntax/help with the syntax matching the name, and help lines
// left-pad the syntax ahead of the description.

// TestRegistryExactOrderedSet pins the canonical command order (issue #50).
func TestRegistryExactOrderedSet(t *testing.T) {
	want := []string{
		"status", "inbox", "tools", "timers", "watchers", "grants", "workflows", "workflow", "launches",
		"audit", "explain", "models", "cost", "backend", "routing", "permissions", "approvals", "memory", "compact",
		"clear", "doctor", "reconnect", "help", "quit",
	}
	if len(COMMAND_REGISTRY) != len(want) {
		t.Fatalf("registry has %d commands, want %d", len(COMMAND_REGISTRY), len(want))
	}
	for i, c := range COMMAND_REGISTRY {
		if c.Name != want[i] {
			t.Fatalf("registry[%d] = %q, want %q", i, c.Name, want[i])
		}
	}
}

// TestEveryEntryHasNonEmptySurfaces: palette/syntax/help all present; syntax begins
// with "/" then the command name.
func TestEveryEntryHasNonEmptySurfaces(t *testing.T) {
	for _, c := range COMMAND_REGISTRY {
		if c.Palette == "" {
			t.Errorf("%q has empty palette", c.Name)
		}
		if !strings.HasPrefix(c.Syntax, "/") {
			t.Errorf("%q syntax %q must start with /", c.Name, c.Syntax)
		}
		if !strings.HasPrefix(strings.TrimPrefix(c.Syntax, "/"), c.Name) {
			t.Errorf("%q syntax %q must start with the command name", c.Name, c.Syntax)
		}
		if c.Help == "" {
			t.Errorf("%q has empty help", c.Name)
		}
	}
}

// TestPaletteDerivedFromRegistry: the palette surface enumerates exactly the
// registry, in order (cannot silently diverge from hand-maintained literals).
func TestPaletteDerivedFromRegistry(t *testing.T) {
	pe := PaletteEntries()
	if len(pe) != len(COMMAND_REGISTRY) {
		t.Fatalf("palette has %d entries, registry %d", len(pe), len(COMMAND_REGISTRY))
	}
	for i, e := range pe {
		if name := strings.TrimPrefix(e[0], "/"); name != COMMAND_REGISTRY[i].Name {
			t.Fatalf("palette[%d] = %q, want %q", i, name, COMMAND_REGISTRY[i].Name)
		}
	}
}

// TestHelpLinePaddingFormat: the /models help line left-pads the syntax with ≥2
// spaces ahead of "model routing" (description never butted against the syntax).
func TestHelpLinePaddingFormat(t *testing.T) {
	var modelsLine string
	for _, l := range HelpLines() {
		if strings.HasPrefix(l, "/models") {
			modelsLine = l
		}
	}
	if modelsLine == "" {
		t.Fatal("no /models help line")
	}
	if !strings.Contains(modelsLine, "model routing") {
		t.Fatalf("/models help line missing description: %q", modelsLine)
	}
	rest := strings.TrimPrefix(modelsLine, "/models")
	gap := len(rest) - len(strings.TrimLeft(rest, " "))
	if gap < 2 {
		t.Fatalf("/models help line not padded (gap=%d): %q", gap, modelsLine)
	}
}
