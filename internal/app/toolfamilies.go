package app

import (
	"context"
	"encoding/json"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/storage"

	"github.com/daintreehq/daintree-assistant/internal/tools/agenttaskx"
	"github.com/daintreehq/daintree-assistant/internal/tools/auditx"
	"github.com/daintreehq/daintree-assistant/internal/tools/contextx"
	"github.com/daintreehq/daintree-assistant/internal/tools/extractionx"
	"github.com/daintreehq/daintree-assistant/internal/tools/mcpx"
	"github.com/daintreehq/daintree-assistant/internal/tools/skill"
)

// This file holds the small integrator adapters that bridge each tool family's
// consumer-defined Deps interfaces onto the App's already-built concrete providers
// (Store, MCP client, Router, Queue, agent session). The families were each built
// in isolation against narrow consumer interfaces; many of those interface methods
// carry a context.Context the concrete storage layer omits, or use the family's own
// locally-declared result/option structs. Each adapter here normalizes the one
// difference (ctx-strip, now-stamp, struct-shape) and forwards to the concrete
// provider. Kept here (not in tools.go) so the builder reads as pure wiring.

/* ----------------------------- MCP adapters ------------------------------ */

// agentTaskMCPAdapter maps *mcp.Client onto agenttaskx.MCPClient (its own
// MCPCallResult envelope). agentTask reaches agent.launch + terminal.list.
type agentTaskMCPAdapter struct{ c *mcp.Client }

func (m agentTaskMCPAdapter) Connected() bool { return m.c.IsConnected() }

func (m agentTaskMCPAdapter) CallTool(ctx context.Context, name string, args map[string]any) (agenttaskx.MCPCallResult, error) {
	res, err := m.c.CallTool(ctx, name, args, mcp.CallOptions{})
	if err != nil {
		return agenttaskx.MCPCallResult{}, err
	}
	return agenttaskx.MCPCallResult{Text: res.Text, StructuredContent: res.StructuredContent, IsError: res.IsError}, nil
}

// contextMCPAdapter maps *mcp.Client onto contextx.MCPClient (adds Status()).
type contextMCPAdapter struct{ c *mcp.Client }

func (m contextMCPAdapter) Connected() bool { return m.c.IsConnected() }

func (m contextMCPAdapter) Status() contextx.MCPStatus {
	st := m.c.Status()
	return contextx.MCPStatus{Connected: st.Connected, Transport: st.Transport, ToolCount: st.ToolCount, Error: st.Error}
}

func (m contextMCPAdapter) CallTool(ctx context.Context, name string, args map[string]any) (contextx.MCPCallResult, error) {
	res, err := m.c.CallTool(ctx, name, args, mcp.CallOptions{})
	if err != nil {
		return contextx.MCPCallResult{}, err
	}
	return contextx.MCPCallResult{Text: res.Text, StructuredContent: res.StructuredContent, IsError: res.IsError}, nil
}

// mcpxMCPAdapter maps *mcp.Client onto mcpx.MCPClient (adds Status() + ListTools()).
type mcpxMCPAdapter struct{ c *mcp.Client }

func (m mcpxMCPAdapter) Connected() bool { return m.c.IsConnected() }

func (m mcpxMCPAdapter) Status() mcpx.MCPStatus {
	st := m.c.Status()
	return mcpx.MCPStatus{Connected: st.Connected, Transport: st.Transport, ToolCount: st.ToolCount, Error: st.Error}
}

func (m mcpxMCPAdapter) CallTool(ctx context.Context, name string, args map[string]any) (mcpx.MCPCallResult, error) {
	res, err := m.c.CallTool(ctx, name, args, mcp.CallOptions{})
	if err != nil {
		return mcpx.MCPCallResult{}, err
	}
	return mcpx.MCPCallResult{Text: res.Text, StructuredContent: res.StructuredContent, IsError: res.IsError}, nil
}

func (m mcpxMCPAdapter) ListTools(ctx context.Context, force bool) ([]mcpx.MCPToolInfo, error) {
	infos, err := m.c.ListTools(ctx, force)
	if err != nil {
		return nil, err
	}
	out := make([]mcpx.MCPToolInfo, 0, len(infos))
	for _, i := range infos {
		out = append(out, mcpx.MCPToolInfo{Name: i.Name, Description: i.Description})
	}
	return out, nil
}

/* ----------------------------- Backend task adapters --------------------- */

// contextRouterAdapter maps the backend onto contextx.Router: terminal.summarize
// runs the server-owned terminal_summarize.v1 task with the structured purpose+tail.
type contextRouterAdapter struct{ tasks backend.TaskRunner }

func (r contextRouterAdapter) Summarize(ctx context.Context, purpose, tail string) (string, error) {
	out, err := backend.RunTerminalSummarize(ctx, r.tasks, backend.TerminalSummarizeInput{Purpose: purpose, Tail: tail})
	if err != nil {
		return "", err
	}
	return out.Text, nil
}

// extractionRouterAdapter maps the backend onto extractionx.Router: text/json
// extraction, the pass/fail verdict, and the shared yes/no terminal judge — each a
// server-owned task (the CLI sends structured data only).
type extractionRouterAdapter struct{ tasks backend.TaskRunner }

func (r extractionRouterAdapter) ExtractText(ctx context.Context, instruction string, terminalIDs []string, tail string) (string, bool, error) {
	out, err := backend.RunTerminalExtractText(ctx, r.tasks, backend.TerminalExtractTextInput{
		Instruction: instruction, TerminalIDs: terminalIDs, Tail: tail,
	})
	if err != nil {
		return "", false, err
	}
	// The backend task does not report output truncation (its tail is already bounded).
	return out.Text, false, nil
}

func (r extractionRouterAdapter) ExtractJSON(ctx context.Context, instruction string, terminalIDs []string, tail string, schema map[string]any) (any, error) {
	out, err := backend.RunTerminalExtractJSON(ctx, r.tasks, backend.TerminalExtractJSONInput{
		Instruction: instruction, TerminalIDs: terminalIDs, Tail: tail,
	}, schema)
	if err != nil {
		return nil, err
	}
	var v any
	if len(out.Result) > 0 {
		_ = json.Unmarshal(out.Result, &v)
	}
	return v, nil
}

func (r extractionRouterAdapter) Verdict(ctx context.Context, result, condition string) (bool, string, error) {
	out, err := backend.RunExtractionVerdict(ctx, r.tasks, backend.ExtractionVerdictInput{Result: result, Condition: condition})
	if err != nil {
		return false, "", err
	}
	return out.Passed, out.Reason, nil
}

// Judge routes the extract settle gate's finished-confirmation through the SAME
// shared terminal_judge.v1 task the watcher uses, so the extract tool and the
// watcher ask the identical question of the same server-owned judge.
func (r extractionRouterAdapter) Judge(ctx context.Context, in extractionx.JudgeInput) (domain.ModelJudgeAnswer, error) {
	return judgeTerminal(ctx, r.tasks, in.Goal, in.Question, backend.TerminalState{
		AgentState:    in.AgentState,
		RuntimeStatus: in.RuntimeStatus,
		WaitingReason: in.WaitingReason,
		LastOutputAt:  in.LastOutputAt,
	}, in.Tail)
}

// judgeTerminal runs the shared yes/no terminal judge (terminal_judge.v1) and maps
// the verdict onto the local domain shape. Used by both the watcher and the extract
// settle gate so they agree on what "finished" means.
func judgeTerminal(ctx context.Context, tasks backend.TaskRunner, goal, question string, state backend.TerminalState, tail string) (domain.ModelJudgeAnswer, error) {
	out, err := backend.RunTerminalJudge(ctx, tasks, backend.TerminalJudgeInput{
		Goal: goal, Question: question, TerminalState: state, Tail: tail,
	})
	if err != nil {
		return domain.ModelJudgeAnswer{}, err
	}
	return domain.ModelJudgeAnswer{Reason: out.Reason, Confidence: out.Confidence, Matched: out.Matched}, nil
}

/* ----------------------------- Queue adapters ---------------------------- */

// contextQueueAdapter maps *queue.Queue onto contextx.Queue (context.snapshot reads
// the open inbox). Digest drops the ctx/error — a snapshot read is best-effort and
// must never throw, so a query failure degrades to an empty digest.
type contextQueueAdapter struct{ app *App }

func (q contextQueueAdapter) Digest(opts domain.QueueDigestOptions) []domain.QueueEvent {
	events, err := q.app.Queue.Digest(context.Background(), opts)
	if err != nil {
		return nil
	}
	return events
}

func (q contextQueueAdapter) Format(events []domain.QueueEvent) string {
	return q.app.Queue.Format(events)
}

/* ----------------------------- Store adapters ---------------------------- */

// auditStoreAdapter maps *storage.Store onto auditx.AuditStore, translating the
// family's locally-declared AuditFilters to the field-identical storage shape.
type auditStoreAdapter struct{ s *storage.Store }

func (a auditStoreAdapter) QueryAudit(f auditx.AuditFilters) ([]domain.AuditRecord, error) {
	return a.s.QueryAudit(storage.AuditFilters{
		Actor: f.Actor, ToolName: f.ToolName, Outcome: f.Outcome,
		TsFrom: f.TsFrom, TsTo: f.TsTo, Limit: f.Limit,
	})
}

// grantStoreAdapter maps *storage.Store onto grant.Store: strips ctx and supplies
// the now-stamp the storage revoke/list methods take (0 ⇒ store fills it).
type grantStoreAdapter struct{ s *storage.Store }

func (a grantStoreAdapter) InsertGrant(_ context.Context, rec domain.AutomationGrantRecord) (string, error) {
	out, err := a.s.InsertGrant(rec)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

func (a grantStoreAdapter) ListGrants(_ context.Context, actorID string) ([]domain.AutomationGrantRecord, error) {
	return a.s.ListGrants(actorID, domain.NowMS())
}

func (a grantStoreAdapter) RevokeGrant(_ context.Context, id string) (bool, error) {
	return a.s.RevokeGrant(id, domain.NowMS())
}

// mcpwrapWatcherStoreAdapter maps *storage.Store onto mcpwrap.WatcherStore (the
// workflow.startWorkOnIssue supervisor-watcher attach). Strips ctx; InsertWatcher
// returns the persisted record (its id back-links the workflow ledger row).
type mcpwrapWatcherStoreAdapter struct{ s *storage.Store }

func (a mcpwrapWatcherStoreAdapter) InsertWatcher(_ context.Context, rec domain.WatcherRecord) (domain.WatcherRecord, error) {
	return a.s.InsertWatcher(rec)
}

func (a mcpwrapWatcherStoreAdapter) ListWatchers(_ context.Context, status string) ([]domain.WatcherRecord, error) {
	return a.s.ListWatchers(status)
}

// mcpwrapWorkflowStoreAdapter maps *storage.Store onto mcpwrap.WorkflowStore (the
// workflow.startWorkOnIssue durable-ledger insert). Strips ctx; InsertWorkflowRun
// projects the persisted record down to its id; UpdateWorkflowRun forwards the
// allowlisted patch directly.
type mcpwrapWorkflowStoreAdapter struct{ s *storage.Store }

func (a mcpwrapWorkflowStoreAdapter) InsertWorkflowRun(_ context.Context, rec domain.WorkflowRunRecord) (string, error) {
	out, err := a.s.InsertWorkflowRun(rec)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

func (a mcpwrapWorkflowStoreAdapter) UpdateWorkflowRun(_ context.Context, id string, patch map[string]any) error {
	return a.s.UpdateWorkflowRun(id, patch)
}

/* ----------------------------- Skill adapters ---------------------------- */

// artifactStoreAdapter resolves an overflow artifact, hot cache first then the
// durable table. It reads the session's in-memory store LAZILY: the tool builder
// runs before a.Session is constructed (registry → AssertSafe precede the session in
// Create), so capturing a.Session.Artifacts() at build time would deref a nil
// session. On a cache miss — an id evicted from the bounded 64-entry store, or a
// stub rehydrated from a prior (now-restarted) session — it falls back to the
// artifacts table, so a persisted truncation stub stays readable across eviction and
// restart. Only a genuinely unknown/pruned id reaches the handler as a miss
// (ARTIFACT_NOT_FOUND).
type artifactStoreAdapter struct{ app *App }

func (a artifactStoreAdapter) Get(id string) (string, bool) {
	if a.app.Session != nil {
		if store := a.app.Session.Artifacts(); store != nil {
			if v, ok := store.Get(id); ok {
				return v, true
			}
		}
	}
	if a.app.Store != nil {
		return a.app.Store.GetArtifact(id)
	}
	return "", false
}

// checkSkillStepConsistency is the decision-correctness judge wired into
// skill.step.advance: it asks the backend's skill_step_consistency.v1 task whether a
// recorded step advance looks like a mistake (a bad jump, a regression, an impossible
// sequence, a premature finish) and returns the {reason,confidence,matched} verdict —
// matched=true ⇒ the advance is FLAGGED as likely-wrong. A backend error returns the
// fallback verdict AND the error, so the caller (observeStepAdvance) logs checkOk=false
// truthfully rather than masking a failed check as a passing one.
func (a *App) checkSkillStepConsistency(ctx context.Context, in skill.ConsistencyCheckInput) (domain.ModelJudgeAnswer, error) {
	fallback := domain.ModelJudgeAnswer{
		Reason:     "Could not evaluate step consistency.",
		Confidence: 0.3,
		Matched:    false,
	}
	requestedNext := ""
	if in.NextStep != nil {
		requestedNext = itoa(*in.NextStep)
	}
	out, err := backend.RunSkillStepConsistency(ctx, a.Backend, backend.SkillStepConsistencyInput{
		SkillID:           in.SkillID,
		CompletedStep:     in.CompletedStep,
		Status:            string(in.StepStatus),
		RequestedNext:     requestedNext,
		CurrentStepBefore: in.PrevCurrentStep,
		CurrentStepAfter:  in.NextCurrentStep,
		RunStatusBefore:   string(in.PrevRunStatus),
		RunStatusAfter:    string(in.NextRunStatus),
		Notes:             in.Notes,
		ProgressBefore:    stepsToMaps(in.BeforeSteps),
		ProgressAfter:     stepsToMaps(in.AfterSteps),
	})
	if err != nil {
		return fallback, err
	}
	return domain.ModelJudgeAnswer{Reason: out.Reason, Confidence: out.Confidence, Matched: out.Matched}, nil
}

// stepsToMaps renders a SkillStepProgress[] as the []map[string]any the backend task
// input carries (a JSON round-trip preserves the progress shape); a marshal failure
// drops that step rather than failing the check.
func stepsToMaps(steps []domain.SkillStepProgress) []map[string]any {
	out := make([]map[string]any, 0, len(steps))
	for _, s := range steps {
		b, err := json.Marshal(s)
		if err != nil {
			continue
		}
		var m map[string]any
		if json.Unmarshal(b, &m) == nil {
			out = append(out, m)
		}
	}
	return out
}
