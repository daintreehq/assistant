package skills

import (
	"encoding/json"
	"strings"
	"testing"
)

// bodyMarker is the runbook-body sentinel used across the parity tests: a real
// skill body contains procedural text but the selector-visible metadata/catalog
// must never leak it. We assert the marker is absent from any header-only view.
const bodyMarker = "Procedure:"

func parityRegistry(t *testing.T) *SkillRegistry {
	t.Helper()
	mk := func(id string) Skill {
		return Skill{
			ID: id, Title: id, Version: "1.0.0", Summary: "summary-" + id,
			WhenToUse: "whenToUse-" + id, Tags: []string{},
			Risk: RiskRead, RequiredTools: []string{},
			Body: "Procedure: do the thing for " + id,
		}
	}
	reg, err := NewRegistry([]Skill{mk("a.one"), mk("b.two"), mk("c.three")})
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	return reg
}

// Validates the built-in skills and exposes all of them.
func TestRegistryExposesAllSkills(t *testing.T) {
	reg := parityRegistry(t)
	if len(reg.List()) != 3 {
		t.Fatalf("list len = %d, want 3", len(reg.List()))
	}
	for _, id := range []string{"a.one", "b.two", "c.three"} {
		if !reg.Has(id) {
			t.Errorf("missing %q", id)
		}
		sk, ok := reg.Get(id)
		if !ok || sk.Title != id {
			t.Errorf("get(%q) = %+v ok=%v", id, sk, ok)
		}
	}
}

// Mirrors "throws on a duplicate skill id".
func TestRegistryRejectsDuplicateID(t *testing.T) {
	mk := func() Skill {
		return Skill{ID: "dup", Title: "T", Version: "1.0.0", Summary: "s",
			WhenToUse: "w", Risk: RiskRead, Body: "b"}
	}
	_, err := NewRegistry([]Skill{mk(), mk()})
	if err == nil || !strings.Contains(err.Error(), "Duplicate skill id") {
		t.Fatalf("expected duplicate-id error, got %v", err)
	}
}

// Mirrors "rejects a structurally invalid skill" ({ id: "x" } with no other
// required fields).
func TestRegistryRejectsStructurallyInvalidSkill(t *testing.T) {
	_, err := NewRegistry([]Skill{{ID: "x"}})
	if err == nil {
		t.Fatalf("expected validation error for skeletal skill")
	}
}

// Mirrors "metadataForSelection excludes skill bodies" — the serialized metadata
// must not leak the body marker, and the count matches the catalog.
func TestMetadataForSelectionExcludesBodies(t *testing.T) {
	reg := parityRegistry(t)
	meta := reg.MetadataForSelection()
	if len(meta) != len(reg.List()) {
		t.Fatalf("metadata count = %d, want %d", len(meta), len(reg.List()))
	}
	serialized, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(serialized), bodyMarker) {
		t.Errorf("serialized metadata leaked a skill body: %s", serialized)
	}
}

// Mirrors "getMany resolves known ids and drops unknown ones" (input order
// preserved, unknown silently dropped).
func TestGetManyDropsUnknown(t *testing.T) {
	reg := parityRegistry(t)
	got := reg.GetMany([]string{"a.one", "does.not.exist"})
	if len(got) != 1 || got[0].ID != "a.one" {
		t.Fatalf("getMany = %+v", got)
	}
}

// Mirrors "every built-in skill's requiredTools names a real registered tool".
// The TS test cross-checks against buildAllTools(); in the Go layering that live
// cross-check lives in the tool-families group (importing the tools registry
// here would create a skills↔tools import cycle). What survives as a pure
// in-package contract: every builtin requiredTool name is non-empty and not
// blank/whitespace — a typo guard so a renamed/dropped tool can't slip in as an
// empty or stray entry.
func TestBuiltinRequiredToolsWellFormed(t *testing.T) {
	reg, err := BuiltinRegistry()
	if err != nil {
		t.Fatalf("builtin: %v", err)
	}
	for _, sk := range reg.List() {
		for _, name := range sk.RequiredTools {
			if strings.TrimSpace(name) == "" {
				t.Errorf("%s declares an empty/blank requiredTool", sk.ID)
			}
		}
	}
}
