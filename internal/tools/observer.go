package tools

import (
	"context"
	"encoding/json"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// DispatchObservation is the record of one COMPLETED dispatch handed to the
// optional DispatchObserver: the (post-decode) args, the final ToolResult, and
// the call's identity/risk metadata. Purely after-the-fact — observation can
// never alter tool behaviour or safety decisions, which have already run.
type DispatchObservation struct {
	ToolName   string
	Args       json.RawMessage
	Result     ToolResult
	Risk       domain.RiskClass
	Outcome    string // ok | error | denied | grant_ok
	Actor      domain.ToolActor
	RunID      string
	ToolCallID string
	DurationMs int64
}

// DispatchObserver receives every completed dispatch (including fast-fails).
// Implementations MUST be fast and side-channel-safe: the registry calls them
// synchronously on the dispatch path, panic-guarded and best-effort, exactly
// like the audit sinks. The workflow-intelligence layer uses this to project
// tool outcomes into graph evidence and resource links.
type DispatchObserver interface {
	ObserveDispatch(ctx context.Context, obs DispatchObservation)
}

// SetDispatchObserver installs the observer (nil clears it). Called once at
// wiring time, before any dispatch runs.
func (r *Registry) SetDispatchObserver(obs DispatchObserver) { r.observer = obs }

// notifyObserver invokes the observer, wrapped so a panicking or slow-failing
// observer can never break the tool call it rides on (mirrors the audit sinks).
func (r *Registry) notifyObserver(ctx context.Context, name string, args json.RawMessage,
	tctx *ToolContext, outcome string, res ToolResult, durationMs int64) {
	if r.observer == nil {
		return
	}
	defer func() { _ = recover() }()
	risk := domain.RiskClass("")
	if t := r.tools[name]; t != nil {
		risk = t.Risk
	}
	obs := DispatchObservation{
		ToolName:   name,
		Args:       args,
		Result:     res,
		Risk:       risk,
		Outcome:    outcome,
		DurationMs: durationMs,
	}
	if tctx != nil {
		obs.Actor = tctx.Actor
		obs.RunID = tctx.RunID
		obs.ToolCallID = tctx.ToolCallID
	}
	r.observer.ObserveDispatch(ctx, obs)
}
