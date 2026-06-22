package agenttaskx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// SupervisorDefaultCadenceMs is the supervisor-watcher cadence.
const supervisorDefaultCadenceMs = 3000

// agentTask error codes (model-facing).
const (
	codeMCPUnavailable       = "MCP_UNAVAILABLE"
	codeCancelled            = "CANCELLED"
	codeAgentLaunchThrew     = "AGENT_LAUNCH_THREW"
	codeAgentLaunchFailed    = "AGENT_LAUNCH_FAILED"
	codeAgentLaunchAmbiguous = "AGENT_LAUNCH_AMBIGUOUS"
	codeNoTerminalID         = "NO_TERMINAL_ID"
	codeLaunchNotFound       = "LAUNCH_NOT_FOUND"
)

type spawnContext struct {
	FilePaths   []string `json:"filePaths,omitempty"`
	IncludeDiff bool     `json:"includeDiff,omitempty"`
}

type spawnWatcher struct {
	Create    bool   `json:"create"`
	Goal      string `json:"goal,omitempty"`
	CadenceMs *int   `json:"cadenceMs,omitempty"`
}

type spawnArgs struct {
	WorktreeID         string        `json:"worktreeId,omitempty"`
	AgentID            string        `json:"agentId,omitempty"`
	Mode               string        `json:"mode,omitempty"`
	Title              string        `json:"title"`
	TaskPrompt         string        `json:"taskPrompt"`
	AcceptanceCriteria string        `json:"acceptanceCriteria,omitempty"`
	Context            *spawnContext `json:"context,omitempty"`
	Watcher            *spawnWatcher `json:"watcher,omitempty"`
}

// Validate enforces the constraints the schema advertises but strict decoding
// alone can't (required-but-meaningful fields + the mode enum). An empty title or
// taskPrompt would otherwise reach spawn (the agent gets a blank prompt), and an
// arbitrary mode string would silently fall through to edit-mode — restrict it to
// the documented enum (edit|explore) so an unknown value is rejected, not coerced.
func (a *spawnArgs) Validate() error {
	if strings.TrimSpace(a.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if strings.TrimSpace(a.TaskPrompt) == "" {
		return fmt.Errorf("taskPrompt is required")
	}
	if a.Mode != "" && a.Mode != "edit" && a.Mode != "explore" {
		return fmt.Errorf("mode must be one of edit, explore")
	}
	return nil
}

var spawnSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "worktreeId": { "type": "string", "description": "Worktree to run the agent in. Omit to let Daintree choose." },
    "agentId": { "type": "string", "description": "Agent to launch (default \"claude\")." },
    "mode": { "type": "string", "enum": ["edit", "explore"], "description": "Spawn intent (default \"edit\"). \"edit\" tells the agent to make code changes; \"explore\" tells it to investigate read-only and not touch any files." },
    "title": { "type": "string", "description": "Short title for the task and any watcher." },
    "taskPrompt": { "type": "string", "description": "The instructions for the agent. Constraints are appended automatically." },
    "acceptanceCriteria": { "type": "string", "description": "Task-specific contract that defines 'done'. When set on an edit-mode task, a supervising watcher verifies completion against these criteria (not git cleanliness alone) before reporting success. Provide it whenever there is a concrete, checkable definition of done. Ignored for mode:\"explore\"." },
    "context": { "type": "object", "additionalProperties": false, "properties": { "filePaths": { "type": "array", "items": { "type": "string" } }, "includeDiff": { "type": "boolean" } } },
    "watcher": { "type": "object", "additionalProperties": false, "properties": { "create": { "type": "boolean" }, "goal": { "type": "string" }, "cadenceMs": { "type": "number" } }, "required": ["create"] }
  },
  "required": ["title", "taskPrompt"]
}`)

func newSpawnForEditsTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "agentTask.spawnForEdits",
		Description: "Spawn a visible Daintree agent in a worktree. Use mode:\"edit\" (default) to make code changes, or " +
			"mode:\"explore\" for a read-only investigation (the agent is told not to touch files). This is the ONLY way to spawn " +
			"an agent — never hand-roll a raw agent.launch via daintree.call. The CLI never edits files itself. Optionally attaches " +
			"a terminal watcher.",
		Consequence: "Opens a visible agent terminal in a worktree that can edit project files. Changes stay in the worktree until you commit them.",
		Risk:        domain.RiskProject,
		Schema:      spawnSchema,
		Decode:      tools.StrictDecoder(func() any { return &spawnArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a spawnArgs
			_ = json.Unmarshal(raw, &a)
			return spawn(ctx, deps, &a)
		},
	}
}

func spawn(ctx context.Context, deps Deps, a *spawnArgs) tools.ToolResult {
	if !deps.MCP.Connected() {
		return tools.Fail(codeMCPUnavailable,
			"Daintree MCP is not connected, so no agent can be spawned to make edits. Connect Daintree (set DAINTREE_MCP_URL / DAINTREE_MCP_TOKEN) and retry.")
	}
	// The user already cancelled before we issued the launch.
	if ctx.Err() != nil {
		return tools.Fail(codeCancelled, "Turn cancelled before the agent was launched.", tools.Unrecoverable())
	}

	agentID := strings.TrimSpace(a.AgentID)
	if agentID == "" {
		agentID = defaultAgentID
	}
	mode := a.Mode
	if mode == "" {
		mode = "edit"
	}
	a.Mode = mode
	name := buildAgentLaunchName(a.Title, agentID)
	prompt := buildAgentPrompt(a)
	// Normalize the worktree once: an explicit empty string is treated like an
	// omitted worktree (so it doesn't change the idempotency key or get forwarded).
	worktreeID := strings.TrimSpace(a.WorktreeID)
	idempotencyKey := computeIdempotencyKey(a.TaskPrompt, worktreeID, agentID, mode)

	// --- Idempotent retry: is there a live launch saga for this exact task? ---
	existing, _ := deps.DB.FindActiveAgentLaunch(idempotencyKey)
	if existing != nil {
		if existing.TerminalID != nil && *existing.TerminalID != "" {
			// A prior attempt already bound a terminal — the agent is running.
			logDebug(deps, "spawn.idempotent_hit", map[string]any{
				"via": "agentTask.spawnForEdits", "launchId": existing.ID,
				"idempotencyKey": idempotencyKey, "stage": existing.Stage, "terminalId": *existing.TerminalID,
			})
			return finishBoundLaunch(deps, a, existing, *existing.TerminalID,
				orStr(existing.WorktreeID, worktreeID), "", "idempotent")
		}
		// In-flight but unbound (ambiguous, or a crashed launch_requested/agent_started).
		reconciled := reconcileViaTerminalList(ctx, deps.MCP, existing.Name, agentID, orStr(existing.WorktreeID, worktreeID))
		if reconciled != "" {
			_ = deps.DB.UpdateAgentLaunch(existing.ID, map[string]any{
				"stage": string(domain.TerminalBound), "terminalId": reconciled,
				"errorCode": nil, "errorMessage": nil,
			})
			logDebug(deps, "spawn.reconciled", map[string]any{
				"via": "agentTask.spawnForEdits", "launchId": existing.ID,
				"idempotencyKey": idempotencyKey, "terminalId": reconciled,
			})
			return finishBoundLaunch(deps, a, existing, reconciled, orStr(existing.WorktreeID, worktreeID), "", "reconciled")
		}
		// No matching terminal — retire the dead-end record and fall through to a
		// fresh launch rather than deadlocking on `ambiguous`.
		_ = deps.DB.UpdateAgentLaunch(existing.ID, map[string]any{
			"stage": string(domain.LaunchFailed), "errorCode": codeLaunchNotFound,
			"errorMessage": "retry found no matching terminal; retired so a fresh launch can proceed",
		})
		logDebug(deps, "spawn.retire_unresolved", map[string]any{
			"via": "agentTask.spawnForEdits", "launchId": existing.ID,
			"idempotencyKey": idempotencyKey, "priorStage": existing.Stage,
		})
	}

	// --- Fresh launch: write the saga record BEFORE the side-effecting call. ---
	record, ierr := deps.DB.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: idempotencyKey,
		AgentID:        agentID,
		WorktreeID:     ptr(worktreeID),
		Mode:           mode,
		Title:          a.Title,
		Name:           name,
		Stage:          domain.LaunchRequested,
	})
	if ierr != nil {
		return tools.Fail(domain.CodeInternal, "agentTask.spawnForEdits: "+ierr.Error())
	}

	launchArgs := map[string]any{"agentId": agentID, "name": name, "prompt": prompt, "requestKey": idempotencyKey}
	if worktreeID != "" {
		launchArgs["worktreeId"] = worktreeID
	}
	res, err := deps.MCP.CallTool(ctx, "agent.launch", launchArgs)
	if err != nil {
		// A mid-launch cancellation is CANCELLED, not an ambiguous launch.
		if ctx.Err() != nil {
			_ = deps.DB.UpdateAgentLaunch(record.ID, map[string]any{
				"stage": string(domain.LaunchFailed), "errorCode": codeCancelled,
				"errorMessage": "Turn cancelled during agent launch.",
			})
			return tools.Fail(codeCancelled, "Turn cancelled during agent launch.",
				tools.WithDetails(map[string]any{"launchId": record.ID}))
		}
		// The transport threw — the request MAY have reached Daintree, so this is
		// ambiguous. Mark it and try to reconcile.
		msg := err.Error()
		_ = deps.DB.UpdateAgentLaunch(record.ID, map[string]any{
			"stage": string(domain.LaunchAmbiguous), "errorCode": codeAgentLaunchThrew, "errorMessage": msg,
		})
		reconciled := reconcileViaTerminalList(ctx, deps.MCP, name, agentID, worktreeID)
		if reconciled != "" {
			_ = deps.DB.UpdateAgentLaunch(record.ID, map[string]any{
				"stage": string(domain.TerminalBound), "terminalId": reconciled,
				"errorCode": nil, "errorMessage": nil,
			})
			return finishBoundLaunch(deps, a, &record, reconciled, worktreeID, "", "reconciled")
		}
		return tools.Fail(codeAgentLaunchAmbiguous,
			fmt.Sprintf("Could not confirm whether an agent for %q started (transport error: %s). Check Daintree's terminals before retrying.", a.Title, msg),
			tools.WithDetails(map[string]any{"launchId": record.ID}))
	}

	if res.IsError {
		detail := res.Text
		if detail == "" {
			detail = "(no detail)"
		}
		_ = deps.DB.UpdateAgentLaunch(record.ID, map[string]any{
			"stage": string(domain.LaunchFailed), "errorCode": codeAgentLaunchFailed, "errorMessage": detail,
		})
		return tools.Fail(codeAgentLaunchFailed, "agent.launch reported an error: "+detail,
			tools.WithDetails(res.StructuredContent))
	}

	_ = deps.DB.UpdateAgentLaunch(record.ID, map[string]any{"stage": string(domain.AgentStarted)})

	terminalID := extractField(res, "terminalId")
	resolvedWorktreeID := extractField(res, "worktreeId")
	if resolvedWorktreeID == "" {
		resolvedWorktreeID = worktreeID
	}
	taskID := extractField(res, "taskId")

	logDebug(deps, "spawn.launched", map[string]any{
		"via": "agentTask.spawnForEdits", "agentId": agentID, "mode": mode, "name": name,
		"title": a.Title, "terminalId": terminalID, "worktreeId": resolvedWorktreeID,
		"taskId": taskID, "idempotencyKey": idempotencyKey, "launchId": record.ID,
		"watcherRequested": a.Watcher != nil && a.Watcher.Create,
	})

	if terminalID == "" {
		// No terminalId means we DON'T KNOW whether an agent started — ambiguous.
		_ = deps.DB.UpdateAgentLaunch(record.ID, map[string]any{
			"stage": string(domain.LaunchAmbiguous), "errorCode": codeNoTerminalID,
			"errorMessage": "agent.launch returned no terminalId",
		})
		reconciled := reconcileViaTerminalList(ctx, deps.MCP, name, agentID, resolvedWorktreeID)
		if reconciled != "" {
			_ = deps.DB.UpdateAgentLaunch(record.ID, map[string]any{
				"stage": string(domain.TerminalBound), "terminalId": reconciled,
				"errorCode": nil, "errorMessage": nil,
			})
			return finishBoundLaunch(deps, a, &record, reconciled, resolvedWorktreeID, taskID, "reconciled")
		}
		logDebug(deps, "spawn.ambiguous", map[string]any{
			"via": "agentTask.spawnForEdits", "launchId": record.ID,
			"idempotencyKey": idempotencyKey, "reason": "no terminalId and no reconciling terminal",
		})
		return tools.Fail(codeAgentLaunchAmbiguous,
			fmt.Sprintf("agent.launch for %q returned no terminalId, so it is unknown whether an agent started. Check Daintree's terminals before retrying.", a.Title),
			tools.WithDetails(map[string]any{"launchId": record.ID}))
	}

	_ = deps.DB.UpdateAgentLaunch(record.ID, map[string]any{
		"stage": string(domain.TerminalBound), "terminalId": terminalID,
	})
	return finishBoundLaunch(deps, a, &record, terminalID, resolvedWorktreeID, taskID, "fresh")
}

// finishBoundLaunch attaches the supervising watcher (if requested and not yet
// attached), advances the saga to its terminal stage, and builds the success
// result. Shared by the fresh-launch, idempotent-retry, and reconciliation paths.
// Watcher attachment is best-effort: a failure returns ok() with a watcherWarning
// and leaves the record at terminal_bound so a retry can re-attach.
func finishBoundLaunch(deps Deps, a *spawnArgs, record *domain.AgentLaunchRecord, terminalID, worktreeID, taskID, kind string) tools.ToolResult {
	agentID := record.AgentID
	watcherID := ""
	if record.WatcherID != nil {
		watcherID = *record.WatcherID
	}
	watcherWarning := ""

	if a.Watcher != nil && a.Watcher.Create && watcherID == "" {
		goal := a.Watcher.Goal
		if goal == "" {
			goal = "Supervise: " + a.Title
		}
		cadence := supervisorDefaultCadenceMs
		if a.Watcher.CadenceMs != nil {
			cadence = *a.Watcher.CadenceMs
		}
		// Scope the post-completion git verification to this worktree (when known),
		// record the spawn mode, and persist the acceptance contract so the
		// supervisor gates completion on evidence, not git cleanliness alone. The
		// record is assembled by the shared builder so this attach can never drift
		// from the workflow / superviseTerminal attach paths.
		watcher, werr := deps.DB.InsertWatcher(domain.BuildSupervisorWatcherRecord(domain.SupervisorWatcherSpec{
			TerminalID:         terminalID,
			WorktreeID:         worktreeID,
			Title:              "watch " + a.Title,
			Goal:               goal,
			CadenceMs:          cadence,
			SpawnMode:          a.Mode,
			AcceptanceCriteria: strings.TrimSpace(a.AcceptanceCriteria),
		}))
		if werr != nil {
			// Watcher bookkeeping failed, but the agent IS running — surface the gap
			// instead of failing a successful launch. Record stays terminal_bound.
			watcherWarning = "watcher could not be attached: " + werr.Error()
			logDebug(deps, "watcher.create_failed", map[string]any{
				"via": "agentTask.spawnForEdits", "agentId": agentID, "title": a.Title, "error": watcherWarning,
			})
		} else {
			watcherID = watcher.ID
			_ = deps.DB.UpdateAgentLaunch(record.ID, map[string]any{
				"stage": string(domain.WatcherAttached), "watcherId": watcherID,
			})
			logDebug(deps, "watcher.created", map[string]any{
				"watcherId": watcher.ID, "kind": "terminal", "isSupervisor": true,
				"via": "agentTask.spawnForEdits", "agentId": agentID, "mode": a.Mode,
				"title": watcher.Title, "goal": watcher.Goal, "targets": []string{terminalID},
				"worktreeId": worktreeID, "cadenceMs": watcher.CadenceMs,
				"modelTier": watcher.ModelTier, "nextCheckAt": watcher.NextCheckAt,
			})
			if worktreeID == "" {
				watcherWarning = "watcher created without a known worktreeId, so post-completion verification will use the active worktree context"
			}
		}
	}

	// A watcher requested but unattached leaves the saga recoverable.
	settled := a.Watcher == nil || !a.Watcher.Create || watcherID != ""
	finalStage := domain.LaunchConfirmed
	if !settled {
		finalStage = domain.TerminalBound
	}
	_ = deps.DB.UpdateAgentLaunch(record.ID, map[string]any{"stage": string(finalStage)})

	lifecycleNote := ""
	if watcherID != "" {
		if deps.daemonActive() {
			lifecycleNote = " NOTE: supervision runs only while this assistant is open; this watcher is discarded when you close the assistant and does not resume on the next launch (watchers are session-scoped)."
		} else {
			lifecycleNote = " NOTE: no scheduler is running in this session, so this watcher will not check until the assistant runs interactively."
		}
	}

	verb := "Spawned"
	switch kind {
	case "idempotent":
		verb = "Reused running"
	case "reconciled":
		verb = "Recovered"
	}

	watcherClause := ""
	if watcherID != "" {
		watcherClause = "; watcher " + watcherID
	}
	warnClause := ""
	if watcherWarning != "" {
		warnClause = " — " + watcherWarning
	}
	summary := fmt.Sprintf("%s %s for %q (terminal %s)%s%s.%s",
		verb, agentID, a.Title, terminalID, watcherClause, warnClause, lifecycleNote)

	result := map[string]any{"launchId": record.ID, "terminalId": terminalID}
	if worktreeID != "" {
		result["worktreeId"] = worktreeID
	}
	if taskID != "" {
		result["taskId"] = taskID
	}
	if watcherID != "" {
		result["watcherId"] = watcherID
	}
	if watcherWarning != "" {
		result["watcherWarning"] = watcherWarning
	}
	return tools.Ok(summary, result)
}

// logDebug forwards a full-fidelity trace, wrapped so it can never break a call.
func logDebug(deps Deps, event string, fields map[string]any) {
	defer func() { _ = recover() }()
	debuglog.LogDebug(debuglog.Config{DebugLog: deps.Config.DebugLog, LogDir: deps.Config.LogDir}, event, fields)
}

func orStr(p *string, fallback string) string {
	if p != nil && *p != "" {
		return *p
	}
	return fallback
}
