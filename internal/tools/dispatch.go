package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/safety"
)

// Dispatch error codes (model-facing recovery signals — exact strings).
const (
	codeUnknownTool     = "UNKNOWN_TOOL"
	codeInvalidArgs     = "INVALID_ARGS"
	codeTierDenied      = "TIER_DENIED"
	codeConfirmRequired = "CONFIRMATION_REQUIRED"
	codeUserDeclined    = "USER_DECLINED"
	codeToolThrew       = "TOOL_THREW"
	// codeNotOffered rejects a tool that schema-projection excluded this turn. A
	// direct caller (loop bug, test, future autonomous wake) must not be able to
	// run a tool the model was never offered — defense in depth over projection.
	codeNotOffered = "TOOL_NOT_OFFERED"
)

// Audit outcomes (audit_log.outcome).
const (
	outcomeOK      = "ok"
	outcomeError   = "error"
	outcomeDenied  = "denied"
	outcomeGrantOK = "grant_ok"
)

// Dispatch is the pipeline. It NEVER returns an error (the ToolResult carries
// every failure) and NEVER panics to the caller. Ordering is load-bearing:
// lookup → decode/validate → tier gate → confirm/grant → run →
// recover → audit. `started` is captured FIRST so even fast-fails carry a real
// durationMs. The returned ToolResult has AuditID set when the audit row wrote.
func (r *Registry) Dispatch(ctx context.Context, name string, rawArgs json.RawMessage, tctx *ToolContext) (result ToolResult) {
	started := domain.NowMS()

	// Top-level panic firewall. The handler is recover-wrapped in runHandler, but a
	// panic in Decode / Confirm / ReportProgress / a nil-ctx deref would otherwise
	// crash the agent loop AND skip the audit row. Convert ANY panic into a
	// TOOL_THREW failure and still write the audit record (best-effort — auditing
	// itself is panic-guarded), so dispatch keeps its "never panics to the caller"
	// contract no matter which stage blew up.
	defer func() {
		if rec := recover(); rec != nil {
			res := Fail(codeToolThrew, fmt.Sprintf("%v", rec))
			result = r.audit(ctx, name, rawArgs, tctx, started, outcomeError, res, nil)
		}
	}()

	// 1. Tool lookup. On a miss, add a "did you mean?" hint with the closest
	//    registered name so the model can self-correct a hallucinated/misremembered
	//    tool name on the next round instead of getting a bare "No such tool".
	tool := r.tools[name]
	if tool == nil {
		msg := "No such tool: " + name + "."
		if near := r.closestToolName(name); near != "" && near != name {
			msg += " Did you mean " + near + "?"
		}
		res := Fail(codeUnknownTool, msg, Unrecoverable())
		return r.audit(ctx, name, rawArgs, tctx, started, outcomeError, res, nil)
	}

	// 1b. Read-only projection enforcement (defense in depth). When the caller
	//     supplied an explicit per-turn allowlist (ActiveToolNames non-nil), a tool
	//     outside it was projection-excluded and must NOT be callable, even by a
	//     direct dispatch that bypassed the schema. nil ⇒ unconstrained (all tools).
	if tctx != nil && tctx.ActiveToolNames != nil && !toolOffered(name, tctx.ActiveToolNames) {
		res := Fail(codeNotOffered, fmt.Sprintf(
			"%s is not offered this turn (the active toolset was narrowed); it cannot be invoked now.", name),
			Unrecoverable())
		return r.audit(ctx, name, rawArgs, tctx, started, outcomeDenied, res, nil)
	}

	// 2. Arg validation. Empty args → {} . Run the tool's Decode (replaces Zod);
	//    on success use the PARSED args (defaults/coercion applied). No Decode ⇒
	//    pass through unvalidated. Validation precedes the tier gate: a
	//    malformed call to a high-tier tool returns INVALID_ARGS, not TIER_DENIED.
	tctx.reportProgress(ToolProgress{Phase: ProgressValidating, Message: "validating request"})
	args := rawArgs
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	if tool.Decode != nil {
		parsed, err := tool.Decode(args)
		if err != nil {
			res := Fail(codeInvalidArgs,
				fmt.Sprintf("Invalid arguments for %s: %s", name, err.Error()),
				WithDetails(decodeDetails(err)))
			return r.audit(ctx, name, args, tctx, started, outcomeError, res, nil)
		}
		args = parsed
	}

	// 3. Tier gate (BEFORE confirmation): a tier-denied tool never reaches
	//    the grant/confirm logic.
	decision := safety.Decide(tool.Risk, tctx.Config.Tier)
	if !decision.Allowed {
		reason := decision.Reason
		if reason == "" {
			reason = "denied"
		}
		res := Fail(codeTierDenied, reason, Unrecoverable())
		return r.audit(ctx, name, args, tctx, started, outcomeDenied, res, nil)
	}

	// 4. Confirmation (only when the matrix requires it).
	if decision.NeedsConfirmation {
		if tctx.Actor != domain.ActorMain {
			// Branch A — non-interactive actor: a scoped grant is the only path.
			if grant := r.tryGrant(ctx, tool, name, tctx, started); grant != nil {
				return r.runHandler(ctx, tool, name, args, tctx, started, outcomeGrantOK, grant)
			}
			// No actorId, or no matching grant → blocked.
			res := Fail(codeConfirmRequired, fmt.Sprintf(
				"%s (%s) needs user confirmation and cannot be run by a non-interactive '%s' actor.",
				name, tool.Risk, tctx.Actor), Unrecoverable())
			out := r.audit(ctx, name, args, tctx, started, outcomeDenied, res, nil)
			r.publishDenial(ctx, name, tctx, out.Summary)
			return out
		}

		// Branch B — interactive main actor.
		if !tctx.Config.AutoApprove {
			// autoApprove only removes the prompt for main; the tier gate above
			// already bounded what's permitted.
			tctx.reportProgress(ToolProgress{Phase: ProgressAwaitingApproval, Message: "waiting for approval"})
			approved := false
			if tctx.Confirm != nil {
				// A thrown/errored confirm is treated as a DECLINE, never approval.
				ok, err := tctx.Confirm(ctx, ConfirmRequest{
					ToolName:    name,
					Risk:        tool.Risk,
					Summary:     tool.Description,
					Consequence: tool.Consequence,
					Args:        args,
				})
				approved = err == nil && ok
			}
			if !approved {
				res := Fail(codeUserDeclined, "User declined "+name+".")
				return r.audit(ctx, name, args, tctx, started, outcomeDenied, res, nil)
			}
		}
	}

	// 5. Run (default outcome "ok").
	return r.runHandler(ctx, tool, name, args, tctx, started, outcomeOK, nil)
}

// toolOffered reports whether name is in the per-turn allowlist. The allowlist
// carries internal dotted names (the same identity used for projection), so a
// plain membership test is the faithful check.
func toolOffered(name string, active []string) bool {
	for _, n := range active {
		if n == name {
			return true
		}
	}
	return false
}

// tryGrant attempts the atomic grant consume for a non-interactive actor. Only
// attempted when ActorID is set. Returns the consumed grant or nil.
//
// A storage error is treated as "no grant" (fail-safe: deny rather than approve
// on a flaky DB). The caller then publishes a denial recommending grant.create —
// so on a transient ConsumeGrant failure with a *valid* grant present, the human
// may be nudged to mint a duplicate. Accepted: the alternative (trusting a failed
// read) would let an unauthorized call through, which is the worse failure.
func (r *Registry) tryGrant(ctx context.Context, tool *Tool, name string, tctx *ToolContext, _ int64) *domain.AutomationGrantRecord {
	if tctx.ActorID == "" || tctx.DB == nil {
		return nil
	}
	actorType := domain.AutomationGrantActorType(tctx.Actor) // watcher | timer
	grant, err := tctx.DB.ConsumeGrant(ctx, tctx.ActorID, actorType, name, tool.Risk, domain.NowMS())
	if err != nil {
		return nil
	}
	return grant
}

// runHandler invokes the handler with panic recovery and audits the outcome.
// A grant-authorized handler that FAILS is still audited as "error", not
// "grant_ok" — only a successful grant call is grant_ok. Never panics.
func (r *Registry) runHandler(ctx context.Context, tool *Tool, name string, args json.RawMessage,
	tctx *ToolContext, started int64, okOutcome string, grant *domain.AutomationGrantRecord) (res ToolResult) {

	tctx.reportProgress(ToolProgress{Phase: ProgressRunning, Message: "running"})

	res = func() (out ToolResult) {
		defer func() {
			if rec := recover(); rec != nil {
				// Handlers must never panic to the caller; convert to a recoverable
				// TOOL_THREW failure.
				out = Fail(codeToolThrew, fmt.Sprintf("%v", rec))
			}
		}()
		return tool.Handle(ctx, args, tctx)
	}()

	outcome := okOutcome
	if !res.Ok {
		outcome = outcomeError
	}
	return r.audit(ctx, name, args, tctx, started, outcome, res, grant)
}

// audit writes the two best-effort side-channels (debug log + DB insert), each
// wrapped so a failure can never break the tool call. On a successful DB insert
// the new row id is stamped onto res.AuditID. Returns the (possibly stamped)
// result.
func (r *Registry) audit(ctx context.Context, name string, args json.RawMessage,
	tctx *ToolContext, started int64, outcome string, res ToolResult, grant *domain.AutomationGrantRecord) ToolResult {

	durationMs := domain.NowMS() - started

	// 1. Debug log — full-fidelity, UNTRUNCATED args+result. JSONL event "tool.call".
	func() {
		defer func() { _ = recover() }()
		var errPayload any
		if res.Error != nil {
			errPayload = res.Error
		}
		debuglog.LogDebug(
			debuglog.Config{DebugLog: tctx.Config.DebugLog, LogDir: tctx.Config.LogDir},
			"tool.call",
			map[string]any{
				"tool":       name,
				"actor":      string(tctx.Actor),
				"actorId":    tctx.ActorID,
				"outcome":    outcome,
				"ok":         res.Ok,
				"durationMs": durationMs,
				"summary":    res.Summary,
				"args":       rawAsAny(args),
				"result":     res.Result,
				"error":      errPayload,
			},
		)
	}()

	// 2. DB insert — auditing must never break a tool call.
	func() {
		defer func() { _ = recover() }()
		if tctx.DB == nil {
			return
		}
		rec := domain.AuditRecord{
			ID:         domain.NewID(domain.PrefixAudit),
			Ts:         domain.NowMS(),
			Actor:      tctx.Actor,
			ToolName:   name,
			ArgsJson:   capJSON(safeJSON(args)),
			Outcome:    outcome,
			DurationMs: durationMs,
			Summary:    res.Summary,
		}
		if res.Result != nil {
			rj := capJSON(safeJSON(res.Result))
			rec.ResultJson = &rj
		}
		// Grant provenance is stamped ONLY on a grant_ok row, so a non-grant
		// row never carries a misleading source.
		if outcome == outcomeGrantOK && grant != nil {
			src := grant.Source
			rec.GrantSource = &src
			gid := grant.ID
			rec.GrantID = &gid
		}
		if tctx.RunID != "" {
			rid := tctx.RunID
			rec.RunID = &rid
		}
		id, err := tctx.DB.InsertAudit(ctx, rec)
		if err == nil && id != "" {
			res.AuditID = id
		}
	}()

	return res
}

// Pre-filled defaults for the grant.create recommended action surfaced on a
// denial event. Conservative and clearly bounded — enough to unblock a typical
// supervised session without minting a long-lived standing authority. The human
// can edit both before approving; the values stay within grant.create's own
// caps (maxGrantTTLMs / maxGrantUses).
const (
	defaultGrantTTLMs   = int64(60 * 60 * 1000) // 1 hour
	defaultGrantMaxUses = 10
)

// publishDenial publishes the best-effort "Autonomous action blocked" queue event
// for a non-interactive actor (Branch A). The dedupeKey is intentionally
// TICK-FREE so repeated denials of the same (actor, tool) collapse into one
// count-bumped inbox row; the actorId segment keeps distinct watchers/timers from
// collapsing together. Wrapped so it can never break the call.
//
// The event is SeverityBlocked (not Info) so it crosses the attention notify
// filter and reaches the wake queue with the ⛔ blocked rendering — a blocked
// autonomous action genuinely needs a human to unblock it. When the actor is a
// watcher/timer with a known ActorID, it also carries a RecommendedAction that
// pre-fills grant.create with the exact scope to authorize this one tool, so the
// human can approve in a single step instead of hand-assembling a grant. The
// action is gated on actor == watcher|timer because grant.create's actorType
// only accepts those two — any other actor would produce an invalid suggestion.
func (r *Registry) publishDenial(ctx context.Context, name string, tctx *ToolContext, summary string) {
	defer func() { _ = recover() }()
	if tctx.Queue == nil {
		return
	}
	actorIDSeg := ""
	if tctx.ActorID != "" {
		actorIDSeg = tctx.ActorID + ":"
	}
	var actions []domain.RecommendedAction
	// Only recommend grant.create when a grant could actually authorize this tool:
	// the actor must be a grantable type (watcher/timer — grant.create's actorType
	// enum) with a known ActorID, and the tool must not be ungrantable (e.g.
	// daintree.call). Recommending a grant that grant.create would reject is worse
	// than no recommendation — it's a one-click "unblock" that silently fails.
	if tctx.ActorID != "" && (tctx.Actor == domain.ActorWatcher || tctx.Actor == domain.ActorTimer) &&
		!domain.IsUngrantableTool(name) {
		actions = []domain.RecommendedAction{{
			Label:    fmt.Sprintf("Authorize %s %s to run %s", tctx.Actor, tctx.ActorID, name),
			ToolName: "grant.create",
			Args: map[string]any{
				"actorId":          tctx.ActorID,
				"actorType":        string(tctx.Actor), // "watcher" | "timer"
				"allowedToolNames": []string{name},
				"ttlMs":            defaultGrantTTLMs,
				"maxUses":          defaultGrantMaxUses,
			},
			Risk:                 domain.RiskLocal, // grant.create's registered risk
			RequiresConfirmation: true,             // minting a mutating grant always confirms
		}}
	}
	_, _ = tctx.Queue.Publish(ctx, domain.QueuePublishArgs{
		Source:             domain.SourceSystem,
		Severity:           domain.SeverityBlocked,
		Title:              "Autonomous action blocked: " + name,
		Summary:            summary,
		DedupeKey:          fmt.Sprintf("denied:%s:%s%s", tctx.Actor, actorIDSeg, name),
		RecommendedActions: actions,
	})
}
