package skills

import (
	"fmt"
	"sync"
)

// SkillSource is the narrow swappable seam (spec §3) that callers (the skill.load
// tool) rely on, so the backing store can later become a hosted service.
// SkillRegistry satisfies it.
type SkillSource interface {
	Has(id string) bool
	Get(id string) (Skill, bool)
}

// SkillRegistry is the in-memory catalog. TS backs it with an insertion-ordered
// Map; Go maps iterate randomly, so we keep a parallel ordered id slice to make
// list()/metadataForSelection() deterministic — catalog text stability matters
// for prompt caching (spec §4 ordering caveat).
type SkillRegistry struct {
	byID  map[string]Skill
	order []string
}

// NewRegistry builds a registry from an initial set. Each entry is re-validated
// (a zero-id or invalid-risk skill is rejected) and duplicate ids are an error
// (spec §4).
func NewRegistry(initial []Skill) (*SkillRegistry, error) {
	r := &SkillRegistry{byID: make(map[string]Skill, len(initial))}
	for _, sk := range initial {
		if err := validateSkill(sk); err != nil {
			return nil, err
		}
		if _, dup := r.byID[sk.ID]; dup {
			return nil, fmt.Errorf("Duplicate skill id: %s", sk.ID)
		}
		r.byID[sk.ID] = sk
		r.order = append(r.order, sk.ID)
	}
	return r, nil
}

// validateSkill re-checks a Skill the way the Zod schema would on registry
// construction. The file loader already validates, but a registry can be built
// from any source, so we guard here too.
func validateSkill(sk Skill) error {
	if sk.ID == "" {
		return fmt.Errorf("skill id must not be empty")
	}
	if sk.Title == "" {
		return fmt.Errorf("skill %q: title must not be empty", sk.ID)
	}
	if sk.Version == "" {
		return fmt.Errorf("skill %q: version must not be empty", sk.ID)
	}
	if sk.Summary == "" {
		return fmt.Errorf("skill %q: summary must not be empty", sk.ID)
	}
	if sk.WhenToUse == "" {
		return fmt.Errorf("skill %q: whenToUse must not be empty", sk.ID)
	}
	if sk.Body == "" {
		return fmt.Errorf("skill %q: body must not be empty", sk.ID)
	}
	if !validSkillRisk[sk.Risk] {
		return fmt.Errorf("skill %q: invalid risk %q", sk.ID, sk.Risk)
	}
	if sk.MaxTurns <= 0 {
		return fmt.Errorf("skill %q: maxTurns must be positive", sk.ID)
	}
	return nil
}

// List returns all skills in insertion (filename-sorted) order. The slice is a
// copy so callers can't mutate the registry's ordering.
func (r *SkillRegistry) List() []Skill {
	out := make([]Skill, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// Has reports membership.
func (r *SkillRegistry) Has(id string) bool {
	_, ok := r.byID[id]
	return ok
}

// Get returns the skill and whether it exists (nil-equivalent via the bool).
func (r *SkillRegistry) Get(id string) (Skill, bool) {
	sk, ok := r.byID[id]
	return sk, ok
}

// All returns every registered skill in catalog order. Used at boot to validate
// each skill's declared requiredTools against the live tool registry.
func (r *SkillRegistry) All() []Skill {
	out := make([]Skill, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id])
	}
	return out
}

// GetMany resolves ids → skills, silently dropping unknown ids and preserving
// input order (spec §4).
func (r *SkillRegistry) GetMany(ids []string) []Skill {
	out := make([]Skill, 0, len(ids))
	for _, id := range ids {
		if sk, ok := r.byID[id]; ok {
			out = append(out, sk)
		}
	}
	return out
}

// MetadataForSelection maps each skill to the 6-field selector subset, in
// catalog order. Never includes bodies (spec §4).
func (r *SkillRegistry) MetadataForSelection() []SkillMetadata {
	out := make([]SkillMetadata, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id].Metadata())
	}
	return out
}

// --- Built-in registry & named handles (spec §5) ---

// Named-handle ids. These exist so code/tests referencing a specific skill fail
// fast on a rename/removal.
const (
	IDBasicOrchestration = "daintree.orchestration.basic"
	IDSpawnAgentForEdits = "daintree.edits.spawn-visible-agent"
	IDRecipeRunner       = "daintree.recipe.run-or-create"
	IDWorkflowStartWork  = "daintree.workflow.start-work-on-issue"
	IDWorkflowPrepBranch = "daintree.workflow.prep-branch-for-review"
)

// requiredHandles asserts the five shipped skills exist after load.
var requiredHandles = []string{
	IDBasicOrchestration,
	IDSpawnAgentForEdits,
	IDRecipeRunner,
	IDWorkflowStartWork,
	IDWorkflowPrepBranch,
}

var (
	builtinOnce sync.Once
	builtinReg  *SkillRegistry
	builtinErr  error
)

// BuiltinRegistry loads the embedded (or env-overridden) skills once and returns
// a registry over them. One bad file or a missing named handle errors at first
// use, preserving fail-loud (spec §5). The result is memoized; subsequent calls
// return the same registry.
func BuiltinRegistry() (*SkillRegistry, error) {
	builtinOnce.Do(func() {
		loaded, err := LoadSkills()
		if err != nil {
			builtinErr = err
			return
		}
		reg, err := NewRegistry(loaded)
		if err != nil {
			builtinErr = err
			return
		}
		for _, id := range requiredHandles {
			if !reg.Has(id) {
				builtinErr = fmt.Errorf("Built-in skill '%s' is missing from skills/", id)
				return
			}
		}
		builtinReg = reg
	})
	return builtinReg, builtinErr
}
