// Package watcher holds the terminal/PR watcher tools: watcher.terminal.create,
// watcher.watchPR, watcher.list, watcher.cancel. Watchers are session-scoped —
// they supervise terminals that live only for the session and never resume on a
// new launch (unlike durable timers). Because they only tick while the
// foreground scheduler is running, both creators hard-fail (non-retryable
// WATCHER_REQUIRES_INTERACTIVE) when the daemon is inactive (one-shot / --json
// mode) instead of inserting a row that the next Store.Open would orphan; a
// successful create appends a foreground-only lifecycle NOTE.
package watcher

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
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
// the watcher.cancel tool (vs. 'session_ended' stamped by the session-boundary
// sweep). Kept in sync with internal/storage/sweeps.go.
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

// lifecycleNote is the session-scoped foreground-only NOTE. Watchers do NOT
// resume on a new launch (distinct from timers). Creators hard-fail before
// reaching this note when the daemon is inactive (see requireDaemon), so the
// "scheduler not running" case never appears on a successful create.
func lifecycleNote() string {
	return " NOTE: watchers are session-scoped and foreground-only — this one stops when the assistant closes and does not resume."
}

// requireDaemon returns a non-retryable failure when the foreground scheduler is
// not running (one-shot / --json mode). Watchers are session-scoped and only
// tick while the assistant is open, so creating one without a live daemon would
// insert a row that the next Store.Open immediately cancels — an orphan. We
// short-circuit before any insert. A nil DaemonActive means the caller did not
// wire the field, so we assume active (daemonActive handles that). Returns nil
// when the daemon is active and creation may proceed.
func requireDaemon(tctx *tools.ToolContext, tool string) *tools.ToolResult {
	if daemonActive(tctx) {
		return nil
	}
	res := tools.Fail(codeWatcherRequiresInteractive,
		tool+": watchers are foreground-only and require an interactive session — they do not run in one-shot or --json mode. Open the assistant cockpit to create a watcher.",
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

// terminalCreateSchema embeds the hand-written WATCH_CONDITION schema. It uses
// anyOf (DeepSeek rejects oneOf), the combinators flatten to ONE level of
// atomic leaves (no $ref/deep recursion), and "not" is a property literally
// named not (NOT the JSON-Schema not keyword).
var terminalCreateSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["terminalIds", "title", "goal"],
  "properties": {
    "terminalIds": { "type": "array", "items": { "type": "string" }, "minItems": 1, "maxItems": 256 },
    "title": { "type": "string" },
    "goal": { "type": "string" },
    "cadenceMs": { "type": "integer", "minimum": 1 },
    "startAfterMs": { "type": "integer", "minimum": 0 },
    "stopAfterMs": { "type": "integer", "minimum": 1, "description": "Lifetime ceiling in ms; defaults to 86400000 (24 h) when omitted — a watcher never runs forever." },
    "stopWhen": { "$comment": "WATCH_CONDITION", "type": "object" },
    "alertWhen": { "$comment": "WATCH_CONDITION", "type": "object" },
    "modelTier": { "type": "string", "enum": ["small", "medium"] }
  }
}`)

func newTerminalCreateTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "watcher.terminal.create",
		Description: "Create a background watcher over one or more terminals that classifies their state and alerts on a condition.",
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

			// Hard-fail before any insert when the daemon is not running:
			// watchers only tick in the foreground, so persisting one here would
			// orphan a row that the next Store.Open cancels. Args are validated
			// above, so INVALID_ARGS still beats this gate.
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
		Description: "Watch a pull request's state (open/merged/draft/closed) and alert on a change.",
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

			// Hard-fail before any insert when the daemon is not running (see
			// watcher.terminal.create): a PR watcher created without a live
			// foreground scheduler would orphan a row cancelled on next open.
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
		Description: "List active watchers.",
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
		Description: "Cancel an active watcher by id.",
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
			// 'session_ended' session-boundary teardown). Overwriting would clobber its
			// endedReason and destroy the very distinction this records.
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
