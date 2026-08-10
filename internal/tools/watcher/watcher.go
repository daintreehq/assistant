// Package watcher holds the terminal/PR watcher tools: watcher.terminal.create,
// watcher.watchPR, watcher.list, watcher.cancel. Watchers are PROJECT-scoped
// and durable: they keep running after the assistant closes — the persistent
// supervisor daemon adopts them, keeps checking, and integrates results with
// autonomous wake turns until the next attach (see docs/SUPERVISOR.md). The
// creators still hard-fail (non-retryable WATCHER_REQUIRES_INTERACTIVE) when
// no supervision engine is running at all (one-shot / --json mode with the
// daemon disabled) instead of inserting a row nothing will check.
package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// Cadence constants. PR cadence is fixed (not
// user-configurable); the terminal default is the user-background monitor rate.
const (
	monitorDefaultCadenceMs = 120000
	prWatcherCadenceMs      = 60000
)

const (
	codeInvalidArgs                = "INVALID_ARGS"
	codeWatcherNotFound            = "WATCHER_NOT_FOUND"
	codeWatcherRequiresInteractive = "WATCHER_REQUIRES_INTERACTIVE"
)

// Store is the slice of storage the watcher tools touch.
type Store interface {
	InsertWatcher(ctx context.Context, rec domain.WatcherRecord) (string, error)
	ListWatchers(ctx context.Context, status string) ([]domain.WatcherRecord, error)
	GetWatcher(ctx context.Context, id string) (*domain.WatcherRecord, error)
	// CancelWatcher flips the watcher to 'cancelled' and stamps WHY (the reason) +
	// WHEN, so a deliberate user cancel is distinguishable from a session-boundary
	// teardown. The watcher tools always pass "user_cancelled".
	CancelWatcher(ctx context.Context, id, reason string) error
	RevokeGrantsByActor(ctx context.Context, actorID string) (int, error)
	// UpdateWorkflowRun advances a workflow ledger row this watcher back-links. A
	// user cancel must close the linked run here — the daemon never re-checks a
	// cancelled watcher, so the row would otherwise stay 'active' forever.
	UpdateWorkflowRun(ctx context.Context, id string, patch map[string]any) error
}

// reasonUserCancelled is the endedReason stamped when a user cancels a watcher via
// the watcher.cancel tool (vs. 'session_cleared' stamped by the /clear teardown).
// Kept in sync with internal/storage/sweeps.go.
const reasonUserCancelled = "user_cancelled"

// isTerminalStatus reports whether a watcher has already reached an end state, so
// watcher.cancel won't re-cancel it and clobber its endedReason. Mirrors the
// watcher status vocabulary in domain.WatcherRecord; the cancellable states are
// created/active/paused.
func isTerminalStatus(status string) bool {
	switch status {
	case "condition_met", "timeout", "error", "cancelled":
		return true
	default:
		return false
	}
}

// Deps is the dependency set for the watcher family.
type Deps struct {
	Store Store
}

// Tools returns the watcher tool family.
func Tools(deps Deps) []*tools.Tool {
	return []*tools.Tool{
		newTerminalCreateTool(deps),
		newWatchPRTool(deps),
		newListTool(deps),
		newCancelTool(deps),
	}
}

func daemonActive(tctx *tools.ToolContext) bool {
	if tctx.DaemonActive == nil {
		return true
	}
	return tctx.DaemonActive()
}

// lifecycleNote is the project-scoped durability NOTE appended to a successful
// create. Watchers survive the assistant closing: the persistent supervisor
// daemon adopts them and keeps checking. The one honest caveat is credentials —
// supervision pauses (blocked, never abandoned) if Daintree/its MCP becomes
// unreachable, and resumes when the assistant is next opened.
func lifecycleNote() string {
	return " NOTE: watchers are project-scoped and KEEP RUNNING after the assistant closes — the background supervisor" +
		" continues checking and will integrate the outcome (you may promise the user results after they close this window)." +
		" Supervision pauses only if Daintree itself closes or its credentials expire, and resumes on the next launch."
}

// requireDaemon returns a non-retryable failure when NO supervision engine is
// running — a one-shot / --json invocation with the background supervisor
// disabled. Creating a watcher there would insert a row nothing checks until
// some later interactive launch. We short-circuit before any insert. A nil
// DaemonActive means the caller did not wire the field, so we assume active
// (daemonActive handles that). Returns nil when creation may proceed.
func requireDaemon(tctx *tools.ToolContext, tool string) *tools.ToolResult {
	if daemonActive(tctx) {
		return nil
	}
	res := tools.Fail(codeWatcherRequiresInteractive,
		tool+": no supervision engine is running in this one-shot invocation, so the watcher would never be checked. Run the assistant interactively to create it.",
		tools.Unrecoverable())
	return &res
}

// --- watcher.terminal.create ---

type terminalCreateArgs struct {
	TerminalIDs  []string               `json:"terminalIds"`
	Title        string                 `json:"title"`
	Goal         string                 `json:"goal"`
	CadenceMs    *int                   `json:"cadenceMs,omitempty"`
	StartAfterMs *int64                 `json:"startAfterMs,omitempty"`
	StopAfterMs  *int64                 `json:"stopAfterMs,omitempty"`
	StopWhen     *domain.WatchCondition `json:"stopWhen,omitempty"`
	AlertWhen    *domain.WatchCondition `json:"alertWhen,omitempty"`
	ModelTier    string                 `json:"modelTier,omitempty"` // small | medium
}

// watchConditionSchema renders the hand-written WATCH_CONDITION subschema with a
// role-specific lead description (stopWhen vs alertWhen). The union's exactly-one-
// key rule (domain.WatchCondition.Validate) is machine-encoded as minProperties/
// maxProperties 1 + an enumerated property set (no $ref/deep recursion — nested
// combinator members are described as the same one-key shape in prose); "not" is a
// PROPERTY literally named not, NOT the JSON-Schema not keyword. Unlike the
// extract tools' wait (which rejects modelJudge), watchers support modelJudge, so
// it is enumerated here. Keep in lockstep with the domain validator.
//
// leafDocs controls only the per-LEAF description prose, never structure. The
// subschema is rendered TWICE in this one tool (stopWhen + alertWhen), and the tool
// inventory ships on every model round, so documenting each leaf twice was pure
// duplication — it made watcher.terminal.create the third-largest tool in the
// registry, ~62% of it description text. The FIRST occurrence (stopWhen) carries the
// full prose, including the hard-won warnings; the second points at it.
//
// What must NOT differ is the machine-readable half: both copies keep every
// structural keyword (type/enum/minLength/minimum/minProperties/maxProperties/
// additionalProperties/items). A model that reads only the schema keywords sees two
// identical unions — pinned by TestStopWhenAndAlertWhenAreStructurallyIdentical.
func watchConditionSchema(role string, leafDocs bool) string {
	doc := func(verbose, terse string) string {
		if leafDocs {
			return verbose
		}
		return terse
	}
	return `{
      "type": "object",
      "minProperties": 1,
      "maxProperties": 1,
      "additionalProperties": false,
      "description": "` + role + ` A WatchCondition object with EXACTLY ONE of the keys below` + doc(".", ` — the same shape as stopWhen, see its per-key descriptions.`) + `",
      "properties": {
        "stateIs": { "type": "string", "enum": ["idle", "working", "waiting", "directing", "completed", "exited"], "description": "` + doc("Fires when the agent state equals this value exactly. A bare stateIs:'waiting' fires too early for 'agent finished' (pre-start, paused, and backgrounded agents also read waiting) — pair it with a modelJudge under all:[...], or rely on the default supervisor's confirmed completion.", "Agent state equals this value exactly.") + `" },
        "runtimeStatusIs": { "type": "string", "enum": ["running", "exited"], "description": "` + doc("Fires on the coarse terminal runtime status.", "Coarse terminal runtime status.") + `" },
        "contains": { "type": "string", "minLength": 1, "description": "` + doc("Fires when the terminal tail contains this literal substring (non-empty).", "Tail contains this literal substring.") + `" },
        "regex": { "type": "string", "minLength": 1, "description": "` + doc("Fires when the tail matches this Go/RE2 regular expression (must compile).", "Tail matches this Go/RE2 regex.") + `" },
        "noOutputForMs": { "type": "integer", "minimum": 1, "description": "` + doc("Fires once no NEW output has appeared for this many ms.", "No new output for this many ms.") + `" },
        "modelJudge": { "type": "string", "minLength": 1, "description": "` + doc("A yes/no question answered against the terminal tail on each check. Costs one model call per check at the watcher's modelTier (deduped across stopWhen/alertWhen) — prefer the free deterministic leaves when they can express the condition.", "Yes/no question against the tail (costs a model call per check).") + `" },
        "all": { "type": "array", "minItems": 1, "items": { "type": "object", "minProperties": 1, "maxProperties": 1 }, "description": "` + doc("AND — every nested condition (each the same one-key WatchCondition shape) must hold.", "AND over nested one-key conditions.") + `" },
        "any": { "type": "array", "minItems": 1, "items": { "type": "object", "minProperties": 1, "maxProperties": 1 }, "description": "` + doc("OR — at least one nested condition (same one-key shape) holds.", "OR over nested one-key conditions.") + `" },
        "not": { "type": "object", "minProperties": 1, "maxProperties": 1, "description": "` + doc("Negates ONE nested condition (same one-key shape).", "Negates ONE nested one-key condition.") + `" }
      }
    }`
}

var terminalCreateSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["terminalIds", "title", "goal"],
  "properties": {
    "terminalIds": { "type": "array", "items": { "type": "string", "minLength": 1 }, "minItems": 1, "maxItems": 256, "description": "Terminals to watch — full terminal-<uuid> ids exactly as listed." },
    "title": { "type": "string", "description": "Short human label for the watcher (shown in watcher.list and attention events)." },
    "goal": { "type": "string", "description": "What the watcher is observing for, in plain language — guides its per-check classification." },
    "cadenceMs": { "type": "integer", "minimum": 1, "default": 120000, "description": "How often the watcher checks, in ms." },
    "startAfterMs": { "type": "integer", "minimum": 0 },
    "stopAfterMs": { "type": "integer", "minimum": 1, "description": "Lifetime ceiling in ms; defaults to 86400000 (24 h) when omitted — a watcher never runs forever." },
    "stopWhen": ` + watchConditionSchema("Condition that ENDS the watcher (status condition_met).", true) + `,
    "alertWhen": ` + watchConditionSchema("Condition that publishes an attention alert (the watcher keeps running).", false) + `,
    "modelTier": { "type": "string", "enum": ["small", "medium"], "default": "small", "description": "Model used for per-check classification and modelJudge questions." }
  }
}`)

func newTerminalCreateTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "watcher.terminal.create",
		Description: "Attach a durable background watcher to one or more terminals: it classifies their state on a cadence (default 120s), publishes an attention event when alertWhen fires, and ends on stopWhen or after 24h. Use it for work you are NOT waiting on this turn — for an in-turn wait use terminal.awaitAll, and note agentTask.spawnForEdits attaches its own supervisor only when you pass watch:true or watchGoal. stopWhen/alertWhen are one-key WatchCondition objects, e.g. {\"stateIs\": \"idle\"}. Returns the wch_… id.",
		Risk:        domain.RiskLocal,
		Schema:      terminalCreateSchema,
		// No StrictDecoder: WatchCondition has a custom UnmarshalJSON guard; we
		// decode + validate by hand to surface its precise rejection messages.
		Handle: func(_ context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a terminalCreateArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for watcher.terminal.create: "+err.Error())
			}
			if len(a.TerminalIDs) < 1 || len(a.TerminalIDs) > 256 {
				return tools.Fail(codeInvalidArgs, "watcher.terminal.create: terminalIds must have 1..256 entries")
			}
			if strings.TrimSpace(a.Title) == "" || strings.TrimSpace(a.Goal) == "" {
				return tools.Fail(codeInvalidArgs, "watcher.terminal.create: title and goal are required")
			}
			modelTier := domain.ModelSmall
			if a.ModelTier == "medium" {
				modelTier = domain.ModelMedium
			} else if a.ModelTier != "" && a.ModelTier != "small" {
				return tools.Fail(codeInvalidArgs, "watcher.terminal.create: modelTier must be small|medium")
			}
			cadence := monitorDefaultCadenceMs
			if a.CadenceMs != nil {
				if *a.CadenceMs <= 0 {
					return tools.Fail(codeInvalidArgs, "watcher.terminal.create: cadenceMs must be a positive integer")
				}
				cadence = *a.CadenceMs
			}

			now := domain.NowMS()
			nextCheck := now
			if a.StartAfterMs != nil {
				if *a.StartAfterMs < 0 {
					return tools.Fail(codeInvalidArgs, "watcher.terminal.create: startAfterMs must be >= 0")
				}
				nextCheck = now + *a.StartAfterMs
			}
			// Enforce the schema's minimum:1 — an explicit non-positive lifetime would
			// beat the storage default and time the watcher out immediately.
			if a.StopAfterMs != nil && *a.StopAfterMs < 1 {
				return tools.Fail(codeInvalidArgs, "watcher.terminal.create: stopAfterMs must be a positive integer (omit it for the 24h default)")
			}

			// Hard-fail before any insert when no supervision engine is running
			// (one-shot with the supervisor disabled): the row would sit unchecked.
			// Args are validated above, so INVALID_ARGS still beats this gate.
			if fail := requireDaemon(tctx, "watcher.terminal.create"); fail != nil {
				return *fail
			}

			targets, _ := json.Marshal(a.TerminalIDs)
			isSup := false
			rec := domain.WatcherRecord{
				ID:           domain.NewID(domain.PrefixWatcher),
				Kind:         "terminal",
				Title:        a.Title,
				Goal:         a.Goal,
				TargetsJson:  string(targets),
				CadenceMs:    cadence,
				IsSupervisor: &isSup,
				ModelTier:    modelTier,
				StartAfterMs: a.StartAfterMs,
				StopAfterMs:  a.StopAfterMs,
				Status:       "active",
				NextCheckAt:  nextCheck,
				CreatedAt:    now,
			}
			if a.StopWhen != nil {
				sj, _ := json.Marshal(a.StopWhen)
				s := string(sj)
				rec.StopWhenJson = &s
			}
			if a.AlertWhen != nil {
				aj, _ := json.Marshal(a.AlertWhen)
				s := string(aj)
				rec.AlertWhenJson = &s
			}

			id := rec.ID
			if deps.Store != nil {
				newID, err := deps.Store.InsertWatcher(context.Background(), rec)
				if err != nil {
					return tools.Fail(domain.CodeInternal, "watcher.terminal.create: "+err.Error())
				}
				if newID != "" {
					id = newID
				}
			}
			return tools.Ok(
				fmt.Sprintf("Watching %d terminal(s): %q (%s).%s", len(a.TerminalIDs), a.Title, modelTier, lifecycleNote()),
				map[string]any{"id": id, "nextCheckAt": nextCheck},
			)
		},
	}
}

// --- watcher.watchPR ---

type watchPRArgs struct {
	PRNumber     int    `json:"prNumber"`
	CWD          string `json:"cwd,omitempty"`
	Title        string `json:"title,omitempty"`
	StartAfterMs *int64 `json:"startAfterMs,omitempty"`
	StopAfterMs  *int64 `json:"stopAfterMs,omitempty"`
}

var watchPRSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["prNumber"],
  "properties": {
    "prNumber": { "type": "integer", "minimum": 1 },
    "cwd": { "type": "string" },
    "title": { "type": "string" },
    "startAfterMs": { "type": "integer", "minimum": 0 },
    "stopAfterMs": { "type": "integer", "minimum": 1, "description": "Lifetime ceiling in ms; defaults to 86400000 (24 h) when omitted — a watcher never runs forever." }
  }
}`)

// prWatcherOptions is the optionsJson shape for a pr_state watcher. The last*
// baselines are undefined initially.
type prWatcherOptions struct {
	CWD           string `json:"cwd,omitempty"`
	PRNumber      int    `json:"prNumber"`
	LastState     string `json:"lastState,omitempty"`
	LastIsDraft   *bool  `json:"lastIsDraft,omitempty"`
	LastUpdatedAt string `json:"lastUpdatedAt,omitempty"`
}

func newWatchPRTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "watcher.watchPR",
		Description: "Create a durable background watcher on ONE GitHub PR: it polls every 60s and publishes an attention event when the PR's state changes (open/merged/closed) or the draft flag flips — use it instead of re-polling forge.getPR yourself. Returns the wch_… id. Project-scoped: it keeps running after the assistant closes and self-expires after 24h unless stopAfterMs says otherwise. It only OBSERVES; it never merges or comments.",
		Risk:        domain.RiskLocal,
		Schema:      watchPRSchema,
		Decode:      tools.StrictDecoder(func() any { return &watchPRArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a watchPRArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for watcher.watchPR: "+err.Error())
			}
			if a.PRNumber <= 0 {
				return tools.Fail(codeInvalidArgs, "watcher.watchPR: prNumber must be a positive integer")
			}
			title := a.Title
			if title == "" {
				title = fmt.Sprintf("PR #%d", a.PRNumber)
			}
			now := domain.NowMS()
			nextCheck := now
			if a.StartAfterMs != nil {
				if *a.StartAfterMs < 0 {
					return tools.Fail(codeInvalidArgs, "watcher.watchPR: startAfterMs must be >= 0")
				}
				nextCheck = now + *a.StartAfterMs
			}
			// Enforce the schema's minimum:1 (see watcher.terminal.create).
			if a.StopAfterMs != nil && *a.StopAfterMs < 1 {
				return tools.Fail(codeInvalidArgs, "watcher.watchPR: stopAfterMs must be a positive integer (omit it for the 24h default)")
			}

			// Hard-fail before any insert when no supervision engine is running
			// (see watcher.terminal.create): the row would sit unchecked.
			if fail := requireDaemon(tctx, "watcher.watchPR"); fail != nil {
				return *fail
			}

			// targetsJson is a display label so the NOT NULL column stays valid.
			targets, _ := json.Marshal([]string{fmt.Sprintf("PR #%d", a.PRNumber)})
			opts := prWatcherOptions{CWD: a.CWD, PRNumber: a.PRNumber}
			optJSON, _ := json.Marshal(opts)
			optStr := string(optJSON)

			rec := domain.WatcherRecord{
				ID:           domain.NewID(domain.PrefixWatcher),
				Kind:         "pr_state",
				Title:        title,
				Goal:         fmt.Sprintf("Watch PR #%d for state changes.", a.PRNumber),
				TargetsJson:  string(targets),
				CadenceMs:    prWatcherCadenceMs, // fixed; no model consulted
				ModelTier:    domain.ModelSmall,
				StartAfterMs: a.StartAfterMs,
				StopAfterMs:  a.StopAfterMs,
				OptionsJson:  &optStr,
				Status:       "active",
				NextCheckAt:  nextCheck,
				CreatedAt:    now,
			}
			id := rec.ID
			if deps.Store != nil {
				newID, err := deps.Store.InsertWatcher(context.Background(), rec)
				if err != nil {
					return tools.Fail(domain.CodeInternal, "watcher.watchPR: "+err.Error())
				}
				if newID != "" {
					id = newID
				}
			}
			return tools.Ok(
				fmt.Sprintf("Watching PR #%d.%s", a.PRNumber, lifecycleNote()),
				map[string]any{
					"id":          id,
					"prNumber":    a.PRNumber,
					"cadenceMs":   prWatcherCadenceMs,
					"nextCheckAt": nextCheck,
				},
			)
		},
	}
}

// --- watcher.list ---

func newListTool(deps Deps) *tools.Tool {
	schema, _ := json.Marshal(tools.NoArgs)
	return &tools.Tool{
		Name:        "watcher.list",
		Description: "List the project's ACTIVE watchers: id, kind (terminal|pr_state), title, goal, targets, cadence, status, last classification and nextCheckAt. Live watchers do NOT ride the turn context — this is the ONLY way to see what is being supervised, so call it before attaching a watcher to a terminal (never double-supervise one) and to get the wch_… id for watcher.cancel. Ended watchers are never listed.",
		Risk:        domain.RiskRead,
		Schema:      schema,
		Handle: func(_ context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			if deps.Store == nil {
				return tools.Ok("No watchers (storage unavailable).", map[string]any{"watchers": []any{}})
			}
			rows, err := deps.Store.ListWatchers(context.Background(), "active")
			if err != nil {
				return tools.Fail(domain.CodeInternal, "watcher.list: "+err.Error())
			}
			return tools.Ok(fmt.Sprintf("%d active watcher(s).", len(rows)), map[string]any{"watchers": rows})
		},
	}
}

// --- watcher.cancel ---

type cancelArgs struct {
	ID string `json:"id"`
}

var cancelSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": { "id": { "type": "string" } }
}`)

func newCancelTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "watcher.cancel",
		Description: "Stop an ACTIVE watcher by its wch_… id: it stops checking, its automation grants are revoked, and any workflow ledger row it supervises closes as cancelled. Use it when the watched work is done or the user no longer wants it supervised. An already-ended watcher (condition_met/timeout/error/cancelled) returns WATCHER_NOT_FOUND — do not re-cancel. This NEVER touches the terminal: nothing is closed or killed.",
		Risk:        domain.RiskLocal,
		Schema:      cancelSchema,
		Decode:      tools.StrictDecoder(func() any { return &cancelArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a cancelArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for watcher.cancel: "+err.Error())
			}
			if a.ID == "" {
				return tools.Fail(codeInvalidArgs, "watcher.cancel: id is required")
			}
			if deps.Store == nil {
				return tools.Fail(codeWatcherNotFound, "watcher.cancel: no such watcher: "+a.ID, tools.Unrecoverable())
			}
			existing, err := deps.Store.GetWatcher(context.Background(), a.ID)
			if err != nil || existing == nil {
				return tools.Fail(codeWatcherNotFound, "watcher.cancel: no such watcher: "+a.ID, tools.Unrecoverable())
			}
			// Refuse to re-cancel an already-terminal watcher: it has run its course
			// (condition_met/timeout/error) or already ended (cancelled — including a
			// /clear teardown). Overwriting would clobber its endedReason and destroy
			// the very distinction this records.
			if isTerminalStatus(existing.Status) {
				return tools.Fail(codeWatcherNotFound,
					"watcher.cancel: watcher "+a.ID+" already ended ("+existing.Status+")", tools.Unrecoverable())
			}
			if err := deps.Store.CancelWatcher(context.Background(), a.ID, reasonUserCancelled); err != nil {
				return tools.Fail(domain.CodeInternal, "watcher.cancel: "+err.Error())
			}
			_, _ = deps.Store.RevokeGrantsByActor(context.Background(), a.ID)
			// If this watcher supervises a durable workflow run, close that row too —
			// the daemon never re-checks a cancelled watcher, so the run would otherwise
			// stay 'active' forever. Best-effort: a ledger failure never fails the cancel.
			if existing.WorkflowRunID != nil && *existing.WorkflowRunID != "" {
				_ = deps.Store.UpdateWorkflowRun(context.Background(), *existing.WorkflowRunID, map[string]any{
					"status":      string(domain.WorkflowCancelled),
					"completedAt": domain.NowMS(),
				})
			}
			return tools.Ok("Cancelled watcher "+a.ID+".", map[string]any{"id": a.ID})
		},
	}
}
