package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestRenderSkillBundleHashAndSort(t *testing.T) {
	mk := func(id, ver string) Skill {
		return Skill{ID: id, Version: ver, Title: id, Summary: "s", WhenToUse: "w", Body: "b"}
	}
	// Provide unsorted input; bundle must sort by id.
	in := []Skill{mk("c.three", "3.0.0"), mk("a.one", "1.0.0"), mk("b.two", "2.0.0")}
	b := RenderSkillBundle(in)

	wantIDs := []string{"a.one", "b.two", "c.three"}
	for i, id := range wantIDs {
		if b.IDs[i] != id {
			t.Errorf("ids[%d] = %q, want %q", i, b.IDs[i], id)
		}
	}

	// Hash = first 12 chars of sha256 over "id@version|…" of the SORTED skills.
	sig := "a.one@1.0.0|b.two@2.0.0|c.three@3.0.0"
	sum := sha256.Sum256([]byte(sig))
	wantHash := hex.EncodeToString(sum[:])[:12]
	if b.Hash != wantHash {
		t.Errorf("hash = %q, want %q", b.Hash, wantHash)
	}
	if len(b.Hash) != 12 {
		t.Errorf("hash length = %d, want 12", len(b.Hash))
	}
	if b.CacheKey != "daintree-main-v1-skills-"+wantHash {
		t.Errorf("cacheKey = %q", b.CacheKey)
	}
}

func TestRenderEmptyBundle(t *testing.T) {
	b := RenderSkillBundle(nil)
	if len(b.IDs) != 0 || len(b.Items) != 0 {
		t.Errorf("expected empty bundle, got %+v", b)
	}
	// Hash of the empty signature is stable.
	sum := sha256.Sum256([]byte(""))
	if b.Hash != hex.EncodeToString(sum[:])[:12] {
		t.Errorf("empty hash = %q", b.Hash)
	}
}

func TestRenderHashStableRegardlessOfInputOrder(t *testing.T) {
	mk := func(id, ver string) Skill { return Skill{ID: id, Version: ver, Body: "b"} }
	b1 := RenderSkillBundle([]Skill{mk("a", "1"), mk("b", "2")})
	b2 := RenderSkillBundle([]Skill{mk("b", "2"), mk("a", "1")})
	if b1.Hash != b2.Hash {
		t.Errorf("hash not order-stable: %q vs %q", b1.Hash, b2.Hash)
	}
}

func TestBuiltinRegistryLoads(t *testing.T) {
	reg, err := BuiltinRegistry()
	if err != nil {
		t.Fatalf("builtin load failed: %v", err)
	}
	for _, id := range requiredHandles {
		if !reg.Has(id) {
			t.Errorf("missing required handle %q", id)
		}
	}
	// Catalog metadata must never include bodies (compile-time guaranteed by the
	// SkillMetadata type; assert count matches).
	if len(reg.MetadataForSelection()) != len(reg.List()) {
		t.Errorf("metadata count mismatch")
	}
}

func TestDescribeAndMessages(t *testing.T) {
	reg, err := BuiltinRegistry()
	if err != nil {
		t.Fatalf("builtin: %v", err)
	}
	a := NewActiveSkills(reg, &stubRouter{}, nil, "sess", nil)
	if got := a.Describe(); got != "No skills are currently loaded." {
		t.Errorf("empty describe = %q", got)
	}
	if a.LoadedSkillsMessage() != loadedSkillsEmpty {
		t.Errorf("empty loaded message wrong")
	}
	a.SetSkills([]string{IDBasicOrchestration})
	if len(a.ActiveSkillIDs()) != 1 {
		t.Fatalf("expected 1 loaded")
	}
}
