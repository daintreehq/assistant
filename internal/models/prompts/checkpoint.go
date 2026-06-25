package prompts

import (
	"encoding/json"
	"strings"
)

// CompactionCheckpoint is the structured state object that replaces the free-prose
// compaction summary (issue #256). The old prose path discarded tool-call detail and
// re-summarized its own prior summaries, so detail bled away across passes. A typed
// object instead captures exactly what the orchestrator needs to resume — goals, live
// terminals/watchers/workflow runs, decisions, pending work, next actions, open
// questions, and (a P0 correctness field) any approval/automation grant in effect — and
// renders deterministically. Every field is a []string for v1: flat lists parse
// reliably from a best-effort model reply and dodge the nested-object JSON-shape
// confusion that has bitten the model before (see the spawnForEdits arg lesson in
// CLAUDE.md). PreservedIDs is the safety net — load-bearing identifiers the model
// dropped, re-injected by the agent's validation pass (see validateCheckpoint).
type CompactionCheckpoint struct {
	Goal             string   `json:"goal,omitempty"`
	ActiveTerminals  []string `json:"active_terminals,omitempty"`
	ActiveWatchers   []string `json:"active_watchers,omitempty"`
	WorkflowRunIDs   []string `json:"workflow_run_ids,omitempty"`
	Decisions        []string `json:"decisions,omitempty"`
	PendingToolState []string `json:"pending_tool_state,omitempty"`
	NextActions      []string `json:"next_actions,omitempty"`
	OpenQuestions    []string `json:"open_questions,omitempty"`
	// ApprovalsGrants captures approval/automation grants still in effect — a safety-model
	// field, not optional: losing a grant token across compaction would silently re-gate
	// a non-interactive actor mid-task.
	ApprovalsGrants []string `json:"approvals_grants,omitempty"`
	// PreservedIDs holds any other load-bearing identifier worth keeping that didn't fit a
	// named field. The agent's validation pass also dumps here any ID it found in the
	// discarded transcript but couldn't locate anywhere in the model's checkpoint.
	PreservedIDs []string `json:"preserved_ids,omitempty"`
}

// CheckpointSystemPrompt instructs the small model to emit a STRUCTURED STATE object
// (not a prose summary) for a conversation about to be discarded during compaction. It
// is deliberately strict (a single JSON object with exactly these keys) so a best-effort
// parse is reliable; Fireworks/deepseek-v4-flash only supports response_format
// json_object (no json_schema), so the schema is taught in the prompt and enforced by a
// lenient parser. Preserving every load-bearing identifier verbatim is the load-bearing
// instruction — a dropped terminal/watcher/workflow/grant ID is unrecoverable once the
// history is gone.
const CheckpointSystemPrompt = `You are compacting a conversation that is about to be discarded. Capture the orchestrator's working state as a STRUCTURED CHECKPOINT — not a prose summary.
Return ONLY a single JSON object with EXACTLY these keys (use [] for any list with nothing to report, "" for an empty goal):
{
  "goal": "the current overarching objective, one sentence",
  "active_terminals": ["term_xxxx — what it is running and its state"],
  "active_watchers": ["wch_xxxx — what it is watching"],
  "workflow_run_ids": ["wfr_xxxx — what it is doing"],
  "decisions": ["a decision made and the reason"],
  "pending_tool_state": ["work started but not finished, awaiting a result"],
  "next_actions": ["the next concrete step to take"],
  "open_questions": ["something still unresolved"],
  "approvals_grants": ["grt_xxxx — an approval or automation grant in effect"],
  "preserved_ids": ["any other load-bearing identifier worth keeping"]
}
Preserve EVERY load-bearing identifier VERBATIM — terminal IDs (term_*), watcher IDs (wch_*), workflow run IDs (wfr_*), agent launch IDs (agt_*), timer IDs (tmr_*), grant/approval tokens (grt_*), and branch names. A dropped identifier is unrecoverable after compaction.
No prose, no markdown, no commentary outside the JSON object.`

// ParseCheckpoint parses the small model's checkpoint reply (expected: one JSON object)
// into a normalized CompactionCheckpoint. Like ParseDistilledFacts it is lenient — it
// tolerates a leading ```json / ``` fence, trims whitespace, and on ANY parse failure
// returns the zero value rather than erroring. A zero-value checkpoint is intentional and
// safe: the agent's validation pass still mines load-bearing IDs from the discarded
// transcript into PreservedIDs, so a model that returns prose instead of JSON degrades to
// "IDs preserved, nothing else" rather than blocking compaction. Each list field is
// trimmed, blank-dropped, and de-duplicated; the goal is trimmed.
func ParseCheckpoint(raw string) CompactionCheckpoint {
	raw = strings.TrimSpace(raw)
	// Strip a fenced code block (```json … ``` or ``` … ```) if the model wrapped one.
	if strings.HasPrefix(raw, "```") {
		if i := strings.IndexByte(raw, '\n'); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
		raw = strings.TrimSpace(raw)
	}
	var cp CompactionCheckpoint
	if raw == "" {
		return cp
	}
	if err := json.Unmarshal([]byte(raw), &cp); err != nil {
		return CompactionCheckpoint{}
	}
	cp.Goal = strings.TrimSpace(cp.Goal)
	cp.ActiveTerminals = cleanStrings(cp.ActiveTerminals)
	cp.ActiveWatchers = cleanStrings(cp.ActiveWatchers)
	cp.WorkflowRunIDs = cleanStrings(cp.WorkflowRunIDs)
	cp.Decisions = cleanStrings(cp.Decisions)
	cp.PendingToolState = cleanStrings(cp.PendingToolState)
	cp.NextActions = cleanStrings(cp.NextActions)
	cp.OpenQuestions = cleanStrings(cp.OpenQuestions)
	cp.ApprovalsGrants = cleanStrings(cp.ApprovalsGrants)
	cp.PreservedIDs = cleanStrings(cp.PreservedIDs)
	return cp
}

// cleanStrings trims each entry, drops blanks, and de-duplicates (case-insensitively),
// preserving first-seen order. Returns nil for an all-empty input so the field marshals
// away under omitempty.
func cleanStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		key := strings.ToLower(s)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
