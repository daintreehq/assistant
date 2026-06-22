package skills

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// SelectionStore is the consumer-defined seam for best-effort selection logging
// The active set inserts a skill_selection_log row after a
// find; logging must NEVER break a turn, so callers wrap errors and ignore them.
// Declared locally to avoid importing the storage package.
type SelectionStore interface {
	InsertSkillSelection(ctx context.Context, rec domain.SkillSelectionLogRecord) error
}

// maxLoaded is the hard cap on simultaneously-loaded skills.
const maxLoaded = 3

// maxLoggedInput is the slice cap on the logged userInput.
const maxLoggedInput = 1000

// ActiveSkills owns the live loaded-skill state for a session: which skill ids
// are loaded and the rendered bundle. It is the consumer-side of the merge
// logic (the agent loop wires the rendered bundle into messages[2] and uses the
// catalog string for messages[1]).
//
// NOTE: this struct does NOT touch the message array itself — that index
// invariant (messages[0]/[1]/[2]) lives in the agent loop. Here we expose the
// pure decisions (which ids, what bundle, what tool filter) so the loop can
// rewrite ONLY messages[2].
type ActiveSkills struct {
	registry  *SkillRegistry
	router    JSONRouter
	store     SelectionStore // may be nil
	sessionID string
	coreTools []string

	activeIDs []string
	bundle    RenderedSkillBundle
	catalog   string // built once; static for the session
}

// NewActiveSkills constructs the live state. The catalog is built ONCE from the
// registry metadata (skills don't change mid-session). store may be nil
// (logging then no-ops). coreTools is CORE_TOOL_NAMES.
func NewActiveSkills(reg *SkillRegistry, router JSONRouter, store SelectionStore, sessionID string, coreTools []string) *ActiveSkills {
	a := &ActiveSkills{
		registry:  reg,
		router:    router,
		store:     store,
		sessionID: sessionID,
		coreTools: coreTools,
		activeIDs: []string{},
		bundle:    RenderSkillBundle(nil),
		catalog:   BuildSkillCatalogMessage(reg.MetadataForSelection()),
	}
	return a
}

// Catalog is the static skill-catalog string appended to messages[1].
func (a *ActiveSkills) Catalog() string { return a.catalog }

// Bundle returns the current rendered bundle (the agent loop renders
// messages[2] from this via BuildLoadedSkillsMessage).
func (a *ActiveSkills) Bundle() RenderedSkillBundle { return a.bundle }

// ActiveSkillIDs returns a copy of the loaded id set.
func (a *ActiveSkills) ActiveSkillIDs() []string {
	out := make([]string, len(a.activeIDs))
	copy(out, a.activeIDs)
	return out
}

// applyBundle is the single mutation point. It re-renders the bundle
// and sets activeIDs to the bundle's sorted ids. Callers (the agent loop) then
// rewrite ONLY messages[2] from Bundle().
func (a *ActiveSkills) applyBundle(skills []Skill) {
	a.bundle = RenderSkillBundle(skills)
	a.activeIDs = a.bundle.IDs
}

// resolveKnownIds is the cap/dedup gate. Order is load-bearing:
// dedup (first-seen order) → drop unknown/hallucinated → cap at 3. Dropping
// unknowns BEFORE slicing means a hallucinated id can never push a valid skill
// out of the top 3.
func (a *ActiveSkills) resolveKnownIds(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		if !a.registry.Has(id) {
			continue
		}
		out = append(out, id)
		if len(out) == maxLoaded {
			break
		}
	}
	return out
}

// FindSkills is the skill.find engine. It runs the small-model
// selector, merges the result into the loaded set (capped at 3, new ids first),
// logs the selection, and returns a SkillFindResult. On selector error/cancel
// the loaded set is left unchanged.
func (a *ActiveSkills) FindSkills(ctx context.Context, query string) SkillFindResult {
	active := a.ActiveSkillIDs()

	selection, err := SelectSkills(ctx, a.router, a.registry.MetadataForSelection(), query)
	if err != nil {
		// Distinguish a cancellation from a generic failure for the reason text,
		// but both leave the loaded set untouched and ok=false.
		reason := "skill selector unavailable"
		if ctx.Err() != nil {
			reason = "cancelled"
		}
		return SkillFindResult{
			Ok: false, Matched: false, Query: query, Reason: reason,
			Confidence: 0, Selected: []SelectedSkill{}, ActiveSkillIDs: active,
		}
	}
	// If the context was cancelled after the call returned, don't mutate the live
	// set with an abandoned result.
	if ctx.Err() != nil {
		return SkillFindResult{
			Ok: false, Matched: false, Query: query, Reason: "cancelled",
			Confidence: 0, Selected: []SelectedSkill{}, ActiveSkillIDs: active,
		}
	}

	// The skills THIS query actually resolved (hallucinated ids dropped).
	newlyKnown := a.resolveKnownIds(selection.SkillIDs)
	// Merge new ids FIRST so they survive the cap-of-3.
	merged := a.resolveKnownIds(append(append([]string{}, selection.SkillIDs...), active...))
	a.applyBundle(a.registry.GetMany(merged))

	// Log what the query resolved (NOT the post-merge active set), best-effort.
	a.logSelection(ctx, query, selection, newlyKnown)

	selectedSkills := a.registry.GetMany(newlyKnown)
	selected := make([]SelectedSkill, 0, len(selectedSkills))
	for _, sk := range selectedSkills {
		selected = append(selected, SelectedSkill{ID: sk.ID, Title: sk.Title, Summary: sk.Summary})
	}

	return SkillFindResult{
		Ok:             true,
		Matched:        len(selected) > 0,
		Query:          query,
		Reason:         selection.Reason,
		Confidence:     selection.Confidence,
		Selected:       selected,
		ActiveSkillIDs: a.ActiveSkillIDs(),
	}
}

// LoadAdditionalSkills is the skill.load engine. New ids go first so
// an explicit load evicts the lowest-priority prior skill rather than being
// dropped when 3 are already loaded. Returns a copy of the active id set.
func (a *ActiveSkills) LoadAdditionalSkills(ids []string) []string {
	merged := a.resolveKnownIds(append(append([]string{}, ids...), a.activeIDs...))
	a.applyBundle(a.registry.GetMany(merged))
	return a.ActiveSkillIDs()
}

// SetSkills is the manual-set path. SetSkills(nil) clears.
func (a *ActiveSkills) SetSkills(ids []string) {
	a.applyBundle(a.registry.GetMany(a.resolveKnownIds(ids)))
}

// BuildToolFilter is the per-turn tool projection. No loaded skills
// ⇒ nil ("unconstrained, send the full registry" — never tool-starve an
// unconstrained turn). Else ⇒ unique(core ∪ loaded skills' requiredTools).
func (a *ActiveSkills) BuildToolFilter() []string {
	if len(a.activeIDs) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	out := make([]string, 0, len(a.coreTools))
	add := func(name string) {
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, t := range a.coreTools {
		add(t)
	}
	for _, sk := range a.registry.GetMany(a.activeIDs) {
		for _, t := range sk.RequiredTools {
			add(t)
		}
	}
	return out
}

// logSelection inserts a skill_selection_log row, best-effort. Any
// error is swallowed — logging must never break a turn. userInput is sliced to
// maxLoggedInput.
func (a *ActiveSkills) logSelection(ctx context.Context, userInput string, sel SkillSelection, selectedIDs []string) {
	if a.store == nil {
		return
	}
	defer func() { _ = recover() }() // a side-channel must never break the turn

	input := userInput
	if len(input) > maxLoggedInput {
		input = input[:maxLoggedInput]
	}
	idsJSON, err := marshalStringSlice(selectedIDs)
	if err != nil {
		return
	}
	rec := domain.SkillSelectionLogRecord{
		ID:                   domain.NewID(domain.PrefixSkillSel),
		Ts:                   domain.NowMS(),
		SessionID:            a.sessionID,
		UserInput:            input,
		SelectedSkillIdsJson: idsJSON,
		Confidence:           sel.Confidence,
	}
	if sel.TaskType != "" {
		tt := sel.TaskType
		rec.TaskType = &tt
	}
	if sel.Reason != "" {
		r := sel.Reason
		rec.Reason = &r
	}
	_ = a.store.InsertSkillSelection(ctx, rec)
}
