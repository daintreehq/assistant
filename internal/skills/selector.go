package skills

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Selector constants.
const (
	maxQueryChars     = 2000 // query is sliced to this before use (bounds cost + injection surface)
	selectorMaxTokens = 500
)

// JSONRouter is the consumer-defined seam for the model router's structured-JSON
// call. The selector posts to the cheap "small" tier requesting a
// JSON object validated against SkillSelection. Declared locally (not imported
// from internal/ports) so this package compiles in isolation.
//
// signal/abort flows through the context: a user-cancel during selection tears
// the request down, surfacing as a non-nil error (treated as "selector
// unavailable" by the consumer in findSkills).
type JSONRouter interface {
	// JSON requests a structured JSON response from the given model tier and
	// unmarshals it into out (a pointer). temperature/maxTokens are passed so
	// the selector can pin temperature:0.
	JSON(ctx context.Context, tier domain.ModelTier, req JSONRequest, out any) error
}

// JSONRequest is the structured-JSON request payload (the router seam's input).
type JSONRequest struct {
	Messages    []SelectorMessage `json:"messages"`
	Temperature float64           `json:"temperature"`
	MaxTokens   int               `json:"maxTokens"`
}

// SelectorMessage is one chat message in the selector request.
type SelectorMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ToolPresence is the consumer-defined seam for validating a skill's
// requiredTools against the live tool registry. A skill that lists a tool the
// registry doesn't have is misconfigured (it would silently starve the model of
// a tool it can't actually get). Declared locally to avoid importing the tools
// package.
type ToolPresence interface {
	Has(name string) bool
}

// selectorSystemPrompt is byte-stable — a wire/behavior contract.
const selectorSystemPrompt = `You are the Daintree Assistant skill selector.
Return only JSON.
The main assistant has hit a point where it wants a procedural runbook ("skill") and has given you a query describing what it needs to figure out. Choose the 0-3 skills whose full instructions best answer that query.
Rules:
- Match the query against each candidate's "whenToUse" (the detailed signal) and "summary".
- Return 0 skills if none genuinely fit — do not force a match.
- Return 1 skill for a focused need; 2-3 only when the task clearly spans multiple skills.
- Order skillIds best-match first.
- Never invent skill ids. Choose only from the candidate list.
Return this JSON shape:
{
  "skillIds": ["string"],
  "confidence": 0.0,
  "reason": "string",
  "taskType": "string"
}`

// SelectSkills runs the small-model selector. candidates is the
// header-only metadata; the body is never sent. The query is sliced to
// maxQueryChars. The returned SkillSelection is validated (≤3 ids, confidence
// clamped to [0,1]). A router error (incl. cancellation) propagates so the
// caller can leave the loaded set unchanged.
func SelectSkills(ctx context.Context, router JSONRouter, candidates []SkillMetadata, query string) (SkillSelection, error) {
	q := query
	if len(q) > maxQueryChars {
		q = q[:maxQueryChars]
	}

	// candidates serialized as 2-space-indented JSON.
	candJSON, err := json.MarshalIndent(candidates, "", "  ")
	if err != nil {
		return SkillSelection{}, fmt.Errorf("marshaling skill candidates: %w", err)
	}

	user := fmt.Sprintf(
		"JSON selection task.\nQuery (what the assistant needs to figure out):\n%s\n\nCandidate skills (headers only):\n%s\n\nReturn the JSON object now.",
		q, string(candJSON),
	)

	req := JSONRequest{
		Messages: []SelectorMessage{
			{Role: "system", Content: selectorSystemPrompt},
			{Role: "user", Content: user},
		},
		Temperature: 0,
		MaxTokens:   selectorMaxTokens,
	}

	var sel SkillSelection
	if err := router.JSON(ctx, domain.ModelSmall, req, &sel); err != nil {
		return SkillSelection{}, err
	}

	// Enforce the wire-schema constraints: hard-cap ids at 3 and clamp confidence
	// into [0,1]. The selector model is *told* to obey these but we don't trust the
	// model output.
	if len(sel.SkillIDs) > 3 {
		sel.SkillIDs = sel.SkillIDs[:3]
	}
	if sel.Confidence < 0 {
		sel.Confidence = 0
	} else if sel.Confidence > 1 {
		sel.Confidence = 1
	}
	return sel, nil
}

// ValidateRequiredTools checks every loaded skill's requiredTools against the
// live tool registry and returns the set of (skillId, toolName) pairs that are
// missing. An empty result means all declared tools exist. This is advisory: the
// caller decides whether a missing tool is a boot error or a logged warning.
// An under-declared requiredTool would otherwise silently starve the skill, so
// surfacing it here lets the caller choose to fail loud.
func ValidateRequiredTools(skills []Skill, tools ToolPresence) []MissingTool {
	if tools == nil {
		return nil
	}
	var missing []MissingTool
	for _, sk := range skills {
		for _, t := range sk.RequiredTools {
			if !tools.Has(t) {
				missing = append(missing, MissingTool{SkillID: sk.ID, Tool: t})
			}
		}
	}
	return missing
}

// MissingTool names a requiredTool a skill declares that the registry lacks.
type MissingTool struct {
	SkillID string
	Tool    string
}
