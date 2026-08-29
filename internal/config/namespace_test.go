package config

import (
	"path/filepath"
	"testing"
)

// A namespace must separate the project's DATABASE without separating the ACCOUNT.
//
// This is what lets a dev build and an installed Daintree hold the same project at
// once: the owner lease is per state dir, so a distinct per-project directory is a
// distinct lease. Moving the state ROOT instead would work too and would be wrong —
// `auth/` and the endpoint preference live there, so the isolated host would need its
// own `/login`.
func TestStateNamespaceSplitsTheProjectDirButNotTheRoot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DAINTREE_ASSISTANT_PROJECT_ID", "")

	plain, err := LoadConfig(ConfigOverrides{ProjectID: strPtr("proj-a")})
	if err != nil {
		t.Fatalf("plain: %v", err)
	}

	t.Setenv(StateNamespaceEnv, "dev")
	namespaced, err := LoadConfig(ConfigOverrides{ProjectID: strPtr("proj-a")})
	if err != nil {
		t.Fatalf("namespaced: %v", err)
	}

	if plain.StateDir == namespaced.StateDir {
		t.Fatalf("namespace did not separate the project state dir: %s", plain.StateDir)
	}
	if plain.StateRoot != namespaced.StateRoot {
		t.Errorf(
			"namespace moved the state ROOT (%s -> %s); auth and the endpoint preference live there",
			plain.StateRoot, namespaced.StateRoot,
		)
	}
	if filepath.Dir(namespaced.StateDir) != plain.StateRoot {
		t.Errorf("namespaced state dir escaped the root: %s", namespaced.StateDir)
	}
}

// An empty or punctuation-only namespace must behave as unset rather than producing a
// stray directory that silently forks a user's project state.
func TestBlankStateNamespaceIsIgnored(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	plain, err := LoadConfig(ConfigOverrides{ProjectID: strPtr("proj-b")})
	if err != nil {
		t.Fatalf("plain: %v", err)
	}
	for _, blank := range []string{"", "   ", "---", "."} {
		t.Setenv(StateNamespaceEnv, blank)
		got, err := LoadConfig(ConfigOverrides{ProjectID: strPtr("proj-b")})
		if err != nil {
			t.Fatalf("blank %q: %v", blank, err)
		}
		if got.StateDir != plain.StateDir {
			t.Errorf("namespace %q should be ignored, got %s", blank, got.StateDir)
		}
	}
}

func strPtr(s string) *string { return &s }
