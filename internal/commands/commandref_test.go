package commands

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite docs/generated/COMMANDS.md")

const commandRefPath = "../../docs/generated/COMMANDS.md"

// TestGeneratedCommandRefIsCurrent regenerates the slash-command reference and diffs it
// against the committed copy.
//
// A failure means a command was added, removed, or reworded without regenerating — the
// drift that left `/auth` and `/login` documented nowhere a tester would look, despite
// being the first two commands a new tester needs.
func TestGeneratedCommandRefIsCurrent(t *testing.T) {
	want := RenderCommandReference()

	if *update {
		if err := os.MkdirAll(filepath.Dir(commandRefPath), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(commandRefPath, []byte(want), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		t.Logf("regenerated %s", commandRefPath)
		return
	}

	got, err := os.ReadFile(commandRefPath)
	if err != nil {
		t.Fatalf("%s is missing — regenerate with: go test ./internal/commands -run TestGeneratedCommandRefIsCurrent -update\n  (%v)", commandRefPath, err)
	}
	if string(got) != want {
		t.Errorf("%s is stale — the command registry changed without regenerating.\nRun: go test ./internal/commands -run TestGeneratedCommandRefIsCurrent -update", commandRefPath)
	}
}

// Every command must actually appear in the generated reference. A command whose Syntax
// were empty would silently vanish from the table while still being callable — a
// capability no document mentions, which is the same class of bug from the other side.
func TestEveryCommandAppearsInTheGeneratedReference(t *testing.T) {
	ref := RenderCommandReference()
	for _, c := range COMMAND_REGISTRY {
		if c.Syntax == "" {
			t.Errorf("%s has no Syntax, so it cannot be documented", c.Name)
			continue
		}
		if !strings.Contains(ref, "`"+c.Syntax+"`") {
			t.Errorf("/%s (%q) is missing from the generated reference", c.Name, c.Syntax)
		}
		if c.Help == "" {
			t.Errorf("/%s has no Help text", c.Name)
		}
	}
}
