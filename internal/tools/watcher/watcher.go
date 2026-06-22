// Package watcher holds the terminal/PR watcher tools: watcher.terminal.create,
// watcher.watchPR, watcher.list, watcher.cancel. Watchers are session-scoped —
// they supervise terminals that live only for the session and never resume on a
// new launch (unlike durable timers). Every creator appends a foreground-only
// lifecycle NOTE.
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
	codeInvalidArgs     = "INVALID_ARGS"
	codeWatcherNotFound = "WATCHER_NOT_FOUND"
)

// Store is the slice of storage the watcher tools touch.
type Store interface {
	InsertWatcher(ctx context.Context, rec domain.WatcherRecord) (string, error)
	ListWatchers(ctx context.Context, status string) ([]domain.WatcherRecord, error)
	GetWatcher(ctx context.Context, id string) (*domain.WatcherRecord, error)
	UpdateWatcherStatus(ctx context.Context, id, status string) error
	RevokeGrantsByActor(ctx context.Context, actorID string) (int, error)
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
// resume on a new launch (distinct from timers).
func lifecycleNote(active bool) string {
	if active {
		return " NOTE: watchers are session-scoped and foreground-only — this one stops when the assistant closes and does not resume."
	}
	return " NOTE: the scheduler is NOT running, so this watcher will not check until the assistant is reopened (and watchers do not resume across sessions)."
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
// anyOf (Fireworks rejects oneOf), the combinators flatten to ONE level of
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
    "stopAfterMs": { "type": "integer", "minimum": 1 },
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
			active := daemonActive(tctx)
			return tools.Ok(
				fmt.Sprintf("Watching %d terminal(s): %q (%s).%s", len(a.TerminalIDs), a.Title, modelTier, lifecycleNote(active)),
				map[string]any{"id": id, "nextCheckAt": nextCheck, "daemonActive": active},
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
    "stopAfterMs": { "type": "integer", "minimum": 1 }
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
			active := daemonActive(tctx)
			return tools.Ok(
				fmt.Sprintf("Watching PR #%d.%s", a.PRNumber, lifecycleNote(active)),
				map[string]any{
					"id":           id,
					"prNumber":     a.PRNumber,
					"cadenceMs":    prWatcherCadenceMs,
					"nextCheckAt":  nextCheck,
					"daemonActive": active,
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
			if err := deps.Store.UpdateWatcherStatus(context.Background(), a.ID, "cancelled"); err != nil {
				return tools.Fail(domain.CodeInternal, "watcher.cancel: "+err.Error())
			}
			_, _ = deps.Store.RevokeGrantsByActor(context.Background(), a.ID)
			return tools.Ok("Cancelled watcher "+a.ID+".", map[string]any{"id": a.ID})
		},
	}
}
