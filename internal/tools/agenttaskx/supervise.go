package agenttaskx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// foregroundOnlyNote is appended to a supervise summary: watchers tick only while
// the assistant is open and never resume on the next launch. Stated explicitly so
// the model never implies background supervision.
const foregroundOnlyNote = " NOTE: supervision is foreground-only — the watcher ticks only while the assistant is open and does not resume on the next launch."

type superviseArgs struct {
	TerminalID         string `json:"terminalId"`
	Title              string `json:"title,omitempty"`
	Goal               string `json:"goal,omitempty"`
	AcceptanceCriteria string `json:"acceptanceCriteria,omitempty"`
	WorktreeID         string `json:"worktreeId,omitempty"`
	CadenceMs          *int   `json:"cadenceMs,omitempty"`
	SpawnMode          string `json:"spawnMode,omitempty"`
}

// Validate enforces the one required field and the mode enum strict decoding can't.
func (a *superviseArgs) Validate() error {
	if strings.TrimSpace(a.TerminalID) == "" {
		return fmt.Errorf("terminalId is required")
	}
	if a.SpawnMode != "" && a.SpawnMode != "edit" && a.SpawnMode != "explore" {
		return fmt.Errorf("spawnMode must be one of edit, explore")
	}
	return nil
}

var superviseSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["terminalId"],
  "properties": {
    "terminalId": { "type": "string", "description": "The id of the already-running terminal to adopt into supervision." },
    "title": { "type": "string", "description": "Short label for the watcher (a \"watch \" prefix is added). Defaults to the terminal id." },
    "goal": { "type": "string", "description": "What the supervisor should watch for. Defaults to general completion supervision." },
    "acceptanceCriteria": { "type": "string", "description": "Task-specific contract that defines 'done'. When set, the supervisor verifies completion against these criteria (not git cleanliness alone) before reporting success." },
    "worktreeId": { "type": "string", "description": "Worktree the agent is working in; scopes the post-completion git verification." },
    "cadenceMs": { "type": "number", "description": "Check cadence in ms (floored to the scheduler tick)." },
    "spawnMode": { "type": "string", "enum": ["edit", "explore"], "description": "The agent's intent (default \"edit\"). \"explore\" marks a read-only investigation so the supervisor does not expect file changes." }
  }
}`)

// newSuperviseTerminalTool builds agentTask.superviseTerminal: adopt an ALREADY-
// running terminal into the acceptance-gated supervision loop WITHOUT re-spawning.
// Risk "terminal" (AlwaysConfirm for the interactive actor; an automation grant for
// watcher/timer actors). Unlike spawnForEdits it never inserts an agent_launch saga
// — adoption is not a launch — and never calls agent.launch.
func newSuperviseTerminalTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "agentTask.superviseTerminal",
		Description: "Adopt an ALREADY-RUNNING agent terminal into supervision without re-spawning it. Use this to re-attach a " +
			"supervisor watcher to a terminal that is running but unsupervised — e.g. an agent from a previous session surfaced " +
			"by boot reconciliation, or a terminal that lost its watcher. Provide the terminalId; supply acceptanceCriteria when " +
			"there is a concrete definition of done. This does NOT launch an agent and does NOT edit files.",
		Consequence: "Attaches a session-scoped supervisor watcher to a running agent terminal (no new agent is launched).",
		Risk:        domain.RiskTerminal,
		Schema:      superviseSchema,
		Decode:      tools.StrictDecoder(func() any { return &superviseArgs{} }),
		Handle: func(_ context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a superviseArgs
			_ = json.Unmarshal(raw, &a)
			return superviseTerminal(deps, &a)
		},
	}
}

func superviseTerminal(deps Deps, a *superviseArgs) tools.ToolResult {
	terminalID := strings.TrimSpace(a.TerminalID)
	if terminalID == "" {
		return tools.Fail(codeNoTerminalID, "terminalId is required to adopt a terminal into supervision.")
	}

	// Dedup: an active supervisor already targeting this terminal ⇒ reuse it rather
	// than stacking a second watcher on the same agent (idempotent re-attach). Best-
	// effort, mirroring the workflow.startWorkOnIssue attach: a ListWatchers error is
	// non-fatal and we fall through to insert — a rare transient scan failure should
	// not block adopting a running agent, and the common path has no duplicate.
	if existing, err := deps.DB.ListWatchers("active"); err == nil {
		for i := range existing {
			w := &existing[i]
			if w.IsSupervisor != nil && *w.IsSupervisor && watcherTargets(w, terminalID) {
				summary := fmt.Sprintf("Terminal %s is already supervised by watcher %s.%s",
					terminalID, w.ID, foregroundOnlyNote)
				return tools.Ok(summary, map[string]any{
					"watcherId": w.ID, "terminalId": terminalID, "deduped": true,
				})
			}
		}
	}

	cadence := supervisorDefaultCadenceMs
	if a.CadenceMs != nil && *a.CadenceMs > 0 {
		cadence = *a.CadenceMs
	}
	mode := a.SpawnMode
	if mode == "" {
		mode = "edit"
	}
	title := strings.TrimSpace(a.Title)
	if title == "" {
		title = "watch terminal " + terminalID
	} else {
		title = "watch " + title
	}
	goal := strings.TrimSpace(a.Goal)
	if goal == "" {
		goal = "Supervise the adopted agent terminal until the work is complete and verified."
	}

	// adoptMode marks the watcher as adopted (not freshly spawned) so the daemon can
	// distinguish it without overloading the spawnMode "edit"|"explore" enum.
	rec := domain.BuildSupervisorWatcherRecord(domain.SupervisorWatcherSpec{
		TerminalID:         terminalID,
		WorktreeID:         strings.TrimSpace(a.WorktreeID),
		Title:              title,
		Goal:               goal,
		CadenceMs:          cadence,
		SpawnMode:          mode,
		AcceptanceCriteria: strings.TrimSpace(a.AcceptanceCriteria),
		ExtraOptions:       map[string]any{"adoptMode": true},
	})
	watcher, werr := deps.DB.InsertWatcher(rec)
	if werr != nil {
		return tools.Fail(domain.CodeInternal, "agentTask.superviseTerminal: could not attach watcher: "+werr.Error())
	}

	logDebug(deps, "watcher.adopted", map[string]any{
		"watcherId": watcher.ID, "kind": "terminal", "isSupervisor": true,
		"via": "agentTask.superviseTerminal", "mode": mode, "title": watcher.Title,
		"goal": watcher.Goal, "targets": []string{terminalID},
		"worktreeId": strings.TrimSpace(a.WorktreeID), "cadenceMs": watcher.CadenceMs,
	})

	// Lifecycle note: foreground-only when the daemon is live, else flag that nothing
	// will tick until the assistant runs interactively (mirrors spawnForEdits).
	note := foregroundOnlyNote
	if !deps.daemonActive() {
		note = " NOTE: no scheduler is running in this session, so this watcher will not check until the assistant runs interactively."
	}
	summary := fmt.Sprintf("Adopted terminal %s into supervision (watcher %s).%s", terminalID, watcher.ID, note)
	return tools.Ok(summary, map[string]any{"watcherId": watcher.ID, "terminalId": terminalID})
}

// watcherTargets reports whether a watcher's targetsJson includes terminalID.
func watcherTargets(w *domain.WatcherRecord, terminalID string) bool {
	var targets []string
	if err := json.Unmarshal([]byte(w.TargetsJson), &targets); err != nil {
		return false
	}
	for _, t := range targets {
		if t == terminalID {
			return true
		}
	}
	return false
}
