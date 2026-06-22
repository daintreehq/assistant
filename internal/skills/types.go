// Package skills is the assistant's skill subsystem: a growing, validated,
// on-disk library of short procedural runbooks (markdown body + metadata) that
// are injected into the main model's context when relevant to the user's task.
// Skills are "the behavior layer that replaces fine-tuning".
//
// Flow:
//  1. Every skill's headers render into a static catalog (message[1]).
//  2. The model calls skill.find with a NL query.
//  3. A cheap small-model selector returns the best 0-3 skill ids.
//  4. Those bodies merge into the loaded set (capped at 3) into message[2].
//  5. While loaded, the per-turn tool list narrows to core ∪ requiredTools.
//
// This package owns loading, validation, the registry, the selector call, and
// bundle rendering. The active-skill merge / message rewrite stays in the agent
// loop; the model-facing tools stay with the tool registry — both consume this
// package through small interfaces.
package skills

// SkillRisk is the riskiest action class a skill's body drives. The value set
// mirrors tool risk classes; default when omitted is "read". Validation rejects
// any other value at load. (Set membership matters, not declaration order.)
type SkillRisk string

const (
	RiskRead     SkillRisk = "read"
	RiskLocal    SkillRisk = "local"
	RiskUI       SkillRisk = "ui"
	RiskTerminal SkillRisk = "terminal"
	RiskProject  SkillRisk = "project"
	RiskGit      SkillRisk = "git"
	RiskExternal SkillRisk = "external"
	RiskSystem   SkillRisk = "system"
)

var validSkillRisk = map[SkillRisk]bool{
	RiskRead: true, RiskLocal: true, RiskUI: true, RiskTerminal: true,
	RiskProject: true, RiskGit: true, RiskExternal: true, RiskSystem: true,
}

// Default field values.
const (
	defaultPriority = 0
	defaultMaxTurns = 8
	defaultRisk     = RiskRead
)

// Skill is the full skill: metadata plus the runbook body. The body is the text
// after the closing frontmatter fence, not a frontmatter field.
type Skill struct {
	ID            string    `json:"id"`
	Title         string    `json:"title"`
	Version       string    `json:"version"`
	Summary       string    `json:"summary"`
	WhenToUse     string    `json:"whenToUse"`
	Tags          []string  `json:"tags"`
	Priority      int       `json:"priority"`
	MaxTurns      int       `json:"maxTurns"`
	Risk          SkillRisk `json:"risk"`
	RequiredTools []string  `json:"requiredTools"`
	Body          string    `json:"body"`
}

// SkillMetadata is the .pick() subset the selector ever sees: exactly id, title,
// summary, whenToUse, tags, priority — NEVER body/version/risk/maxTurns/
// requiredTools. Keeping this subset exact bounds selector input cost and the
// injection surface.
type SkillMetadata struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Summary   string   `json:"summary"`
	WhenToUse string   `json:"whenToUse"`
	Tags      []string `json:"tags"`
	Priority  int      `json:"priority"`
}

// Metadata projects a Skill to the selector-visible subset. tags is copied so a
// mutation of the metadata view can't reach back into the registry's skill.
func (s Skill) Metadata() SkillMetadata {
	tags := make([]string, len(s.Tags))
	copy(tags, s.Tags)
	return SkillMetadata{
		ID:        s.ID,
		Title:     s.Title,
		Summary:   s.Summary,
		WhenToUse: s.WhenToUse,
		Tags:      tags,
		Priority:  s.Priority,
	}
}

// SkillSelection is the structured JSON the small selector model returns
// skillIds capped at 3; confidence in [0,1].
type SkillSelection struct {
	SkillIDs   []string `json:"skillIds"`
	Confidence float64  `json:"confidence"`
	Reason     string   `json:"reason"`
	TaskType   string   `json:"taskType"`
}

// SkillFindResult is the plain (non-validated) return of the skill.find engine
// ok is false ONLY when the selector model errored/cancelled; the
// loaded set is then left unchanged.
type SkillFindResult struct {
	Ok             bool            `json:"ok"`
	Matched        bool            `json:"matched"`
	Query          string          `json:"query"`
	Reason         string          `json:"reason"`
	Confidence     float64         `json:"confidence"`
	Selected       []SelectedSkill `json:"selected"`
	ActiveSkillIDs []string        `json:"activeSkillIds"`
}

// SelectedSkill is the slim view echoed in a find result: the skills THIS query
// pulled in (not the whole loaded set).
type SelectedSkill struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
}
