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
	codeUnknownAgent         = "UNKNOWN_AGENT"
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
	// Watcher supervision is requested with FLAT top-level scalars — the shape the
	// large model emits reliably. A goal (or watch:true) attaches a supervisor
	// watcher that reads the agent and surfaces its result when it settles. The
	// nested Watcher object below is the LEGACY shape: still honored, but no longer
	// the taught form, because the model mangles a nested key into a flat
	// "watcher<...>create" path that the strict decoder then rejects. Resolution
	// precedence: Watch (explicit) → WatchGoal present → legacy Watcher.Create.
	Watch          *bool  `json:"watch,omitempty"`
	WatchGoal      string `json:"watchGoal,omitempty"`
	WatchCadenceMs *int   `json:"watchCadenceMs,omitempty"`

	Watcher *spawnWatcher `json:"watcher,omitempty"`
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

// wantsWatcher resolves whether a spawn should attach a supervisor watcher. Flat
// fields are the taught shape; the legacy nested object is the fallback. Precedence:
// an explicit Watch wins; otherwise a non-empty WatchGoal implies "supervise it";
// otherwise the legacy watcher.create. SINGLE source of truth shared by the launch
// debug field, the attach block, and the saga "settled" decision — so a watcher
// requested via EITHER shape that fails to attach stays recoverable rather than being
// finalized as confirmed.
func wantsWatcher(a *spawnArgs) bool {
	switch {
	case a.Watch != nil:
		return *a.Watch
	case a.WatchGoal != "":
		return true
	case a.Watcher != nil:
		return a.Watcher.Create
	default:
		return false
	}
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
    "watch": { "type": "boolean", "description": "Attach a supervisor watcher that reads the agent for you and surfaces its result when it settles. Set true to supervise (recommended for any non-trivial spawn). Providing watchGoal also attaches one. Omit, or set false, to skip supervision." },
    "watchGoal": { "type": "string", "description": "What the supervisor should do once the agent settles, e.g. \"surface a concise summary of what the agent reported\". Providing this attaches a watcher (no need to also set watch)." },
    "watchCadenceMs": { "type": "number", "description": "Supervisor poll cadence in milliseconds (default 3000)." },
    "watcher": { "type": "object", "additionalProperties": false, "description": "LEGACY nested form of watch / watchGoal / watchCadenceMs. Prefer the FLAT watchGoal / watch fields above — pass those as top-level scalars, never a nested or dotted watcher key.", "properties": { "create": { "type": "boolean" }, "goal": { "type": "string" }, "cadenceMs": { "type": "number" } }, "required": ["create"] }
  },
  "required": ["title", "taskPrompt"]
}`)

func newSpawnForEditsTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "agentTask.spawnForEdits",
		Description: "Spawn a visible Daintree agent in a worktree. Use mode:\"edit\" (default) to make code changes, or " +
			"mode:\"explore\" for a read-only investigation (the agent is told not to touch files). This is the ONLY way to spawn " +
			"an agent — never hand-roll a raw agent.launch via daintree.call. The CLI never edits files itself. To supervise the " +
			"agent, set the FLAT fields watch:true and watchGoal:\"…\" (top-level scalars) — do NOT nest them under a \"watcher\" key.",
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
			"Daintree MCP is not connected, so no agent can be spawned to make edits. Connect Daintree (set DAINTREE_MCP_URL / DAINTREE_MCP_TOKEN), then use /reconnect to retry.")
	}
	// The user already cancelled before we issued the launch.
	if ctx.Err() != nil {
		return tools.Fail(codeCancelled, "Turn cancelled before the agent was launched.", tools.Unrecoverable())
	}

	// Resolve watcher attachment once so the launch debug field below and the attach /
	// "settled" logic in finishBoundLaunch key off the SAME decision (see wantsWatcher).
	wantWatcher := wantsWatcher(a)

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
	// Key the saga over the EXACT args forwarded to agent.launch (agentId, name,
	// prompt, worktreeId). Daintree rejects "same requestKey, different arguments", so
	// the key MUST be a faithful function of those args — keying a hand-picked subset
	// (the old (taskPrompt, mode) shape) made a title tweak reuse the key with a
	// changed name and collide. mode/context/acceptanceCriteria fold in via `prompt`.
	idempotencyKey := computeIdempotencyKey(prompt, worktreeID, agentID, name)

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

	// Validate the agent id against Daintree's configured agents BEFORE a FRESH launch
	// (after the idempotent-retry lookup, so reconciling an in-flight saga is never
	// blocked by a stale roster, and an already-bound idempotent hit skips this read).
	// Daintree's agent.launch silently accepts ANY agentId and launches a terminal that
	// immediately detaches with zero output, so a typo or a mis-transcribed name
	// (spoken "antigravity" heard as "antiravity") fails invisibly and then walls a
	// retry on a key collision. Catch it here with the available roster + a "did you
	// mean" hint; fail OPEN when the roster can't be read so a discovery hiccup never
	// blocks a legitimate spawn.
	if ok, available, suggestion := resolveAgentID(ctx, deps.MCP, agentID); !ok {
		msg := fmt.Sprintf("Unknown agent %q — Daintree has no agent configured with that id.", agentID)
		if suggestion != "" {
			msg += fmt.Sprintf(" Did you mean %q?", suggestion)
		}
		msg += " Available agents: " + strings.Join(available, ", ") +
			". A spoken or typed agent name is often approximate — pick the right id from this list and re-spawn."
		return tools.Fail(codeUnknownAgent, msg, tools.WithDetails(map[string]any{
			"requestedAgentId": agentID, "availableAgents": available, "suggestion": suggestion,
		}))
	}
	// The roster read above can take a beat; if the turn was cancelled meanwhile, stop
	// before writing the saga or launching (don't leave an orphaned record).
	if ctx.Err() != nil {
		return tools.Fail(codeCancelled, "Turn cancelled before the agent was launched.", tools.Unrecoverable())
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
		"watcherRequested": wantWatcher,
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
	// Same resolution as the spawn-launched debug field: whether a watcher was
	// requested (flat fields, legacy nested fallback). Drives both the attach below
	// and the saga "settled" decision so a flat-requested attach failure stays
	// recoverable (terminal_bound), never confirmed.
	wantWatcher := wantsWatcher(a)

	// --- Durable workflow ledger -------------------------------------------------
	// Record the spawned work so /workflows surfaces it and a persistent record
	// survives a restart. Best-effort: a ledger failure NEVER fails a successful
	// spawn (mirrors the watcher-attach contract below). Idempotent: a saga that
	// already carries a workflowRunId (an idempotent-hit / re-attach reaching this
	// path again) re-uses that row instead of inserting a duplicate.
	workflowRunID := ""
	if record.WorkflowRunID != nil {
		workflowRunID = *record.WorkflowRunID
	}
	if workflowRunID == "" {
		run, lerr := deps.DB.InsertWorkflowRun(domain.WorkflowRunRecord{
			Status:          domain.WorkflowActive,
			WorktreeID:      ptr(worktreeID),
			TerminalIdsJson: ptr(jsonIDArray(terminalID)),
		})
		if lerr != nil {
			logDebug(deps, "workflow.ledger_insert_failed", map[string]any{
				"via": "agentTask.spawnForEdits", "agentId": agentID, "title": a.Title, "error": lerr.Error(),
			})
		} else {
			workflowRunID = run.ID
			record.WorkflowRunID = &run.ID
			// Stamp the back-link so an idempotent retry of this saga re-uses the row.
			_ = deps.DB.UpdateAgentLaunch(record.ID, map[string]any{"workflowRunId": run.ID})
			logDebug(deps, "workflow.ledger_created", map[string]any{
				"via": "agentTask.spawnForEdits", "workflowRunId": run.ID,
				"terminalId": terminalID, "worktreeId": worktreeID, "title": a.Title,
			})
		}
	}

	// wantWatcher was resolved up front (flat fields, legacy nested fallback).
	if wantWatcher && watcherID == "" {
		goal := a.WatchGoal
		if goal == "" && a.Watcher != nil {
			goal = a.Watcher.Goal
		}
		if goal == "" {
			goal = "Supervise: " + a.Title
		}
		cadence := supervisorDefaultCadenceMs
		if a.WatchCadenceMs != nil {
			cadence = *a.WatchCadenceMs
		} else if a.Watcher != nil && a.Watcher.CadenceMs != nil {
			cadence = *a.Watcher.CadenceMs
		}
		// Scope the post-completion git verification to this worktree (when known),
		// record the spawn mode, and persist the acceptance contract so the
		// supervisor gates completion on evidence, not git cleanliness alone. The
		// record is assembled by the shared builder so this attach can never drift
		// from the workflow / superviseTerminal attach paths.
		wrec := domain.BuildSupervisorWatcherRecord(domain.SupervisorWatcherSpec{
			TerminalID:         terminalID,
			WorktreeID:         worktreeID,
			Title:              "watch " + a.Title,
			Goal:               goal,
			CadenceMs:          cadence,
			SpawnMode:          a.Mode,
			AcceptanceCriteria: strings.TrimSpace(a.AcceptanceCriteria),
		})
		// Back-link the watcher to its ledger row so the daemon advances that row's
		// status as the watcher reaches a terminal state.
		if workflowRunID != "" {
			wrec.WorkflowRunID = &workflowRunID
		}
		watcher, werr := deps.DB.InsertWatcher(wrec)
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
			// Record the supervising watcher on the ledger row (best-effort).
			if workflowRunID != "" {
				_ = deps.DB.UpdateWorkflowRun(workflowRunID, map[string]any{"watcherIdsJson": jsonIDArray(watcherID)})
			}
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

	// A watcher requested (flat OR legacy) but unattached leaves the saga recoverable
	// (terminal_bound), so a retry re-attaches instead of fresh-launching another agent.
	settled := !wantWatcher || watcherID != ""
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
	if workflowRunID != "" {
		result["workflowRunId"] = workflowRunID
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

// jsonIDArray marshals a single id into a JSON string-array ("[\"x\"]") for the
// ledger's *IdsJson columns, or "" when the id is empty (ptr() then maps "" → NULL).
func jsonIDArray(id string) string {
	if id == "" {
		return ""
	}
	b, err := json.Marshal([]string{id})
	if err != nil {
		return ""
	}
	return string(b)
}
