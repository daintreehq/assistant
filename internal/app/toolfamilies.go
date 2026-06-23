package app

import (
	"context"
	"encoding/json"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/models"
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

/* ----------------------------- Router adapters --------------------------- */

// contextRouterAdapter maps *models.Router onto contextx.Router (terminal.summarize
// runs router.Chat("small", …)). maxTokens <= 0 means "no explicit cap".
type contextRouterAdapter struct{ router *models.Router }

func (r contextRouterAdapter) Chat(ctx context.Context, tier domain.ModelTier, messages []contextx.ChatMessage, maxTokens int) (contextx.ChatResult, error) {
	// maxTokens <= 0 means "no explicit output cap": leave max_tokens unset so the
	// provider emits the WHOLE summary instead of truncating mid-sentence. The input
	// tail is already bounded, so this can't run away (same pattern as the uncapped
	// compaction summary). A positive value still caps the output.
	var capPtr *int
	if maxTokens > 0 {
		capPtr = &maxTokens
	}
	res, err := r.router.Chat(ctx, tier, models.ChatOptions{Messages: toModelMessages(roleContents(messages)), MaxTokens: capPtr})
	if err != nil {
		return contextx.ChatResult{}, err
	}
	return contextx.ChatResult{Content: res.Content, FinishReason: res.FinishReason}, nil
}

// extractionRouterAdapter maps *models.Router onto extractionx.Router (Chat for
// text extraction + JSON for structured extraction).
type extractionRouterAdapter struct{ router *models.Router }

func (r extractionRouterAdapter) Chat(ctx context.Context, tier domain.ModelTier, messages []extractionx.ChatMessage, maxTokens int) (extractionx.ChatResult, error) {
	rc := make([]roleContent, 0, len(messages))
	for _, m := range messages {
		rc = append(rc, roleContent{role: m.Role, content: m.Content})
	}
	res, err := r.router.Chat(ctx, tier, models.ChatOptions{Messages: toModelMessages(rc), MaxTokens: &maxTokens})
	if err != nil {
		return extractionx.ChatResult{}, err
	}
	return extractionx.ChatResult{Content: res.Content, FinishReason: res.FinishReason}, nil
}

func (r extractionRouterAdapter) JSON(ctx context.Context, tier domain.ModelTier, messages []extractionx.ChatMessage, maxTokens int) (any, error) {
	rc := make([]roleContent, 0, len(messages))
	for _, m := range messages {
		rc = append(rc, roleContent{role: m.Role, content: m.Content})
	}
	raw, err := r.router.JSON(ctx, tier, models.ChatOptions{Messages: toModelMessages(rc), MaxTokens: &maxTokens})
	if err != nil {
		return nil, err
	}
	// The family emits {"result": <value>}; mirror that contract by returning the
	// parsed result value (or the raw object when there's no result wrapper).
	var wrapper map[string]any
	if json.Unmarshal([]byte(raw), &wrapper) == nil {
		if v, ok := wrapper["result"]; ok {
			return v, nil
		}
		return wrapper, nil
	}
	var any2 any
	_ = json.Unmarshal([]byte(raw), &any2)
	return any2, nil
}

// roleContent is the shared role/content pair the family ChatMessage slices map to
// before becoming models.ChatMessage.
type roleContent struct{ role, content string }

func roleContents(in []contextx.ChatMessage) []roleContent {
	out := make([]roleContent, 0, len(in))
	for _, m := range in {
		out = append(out, roleContent{role: m.Role, content: m.Content})
	}
	return out
}

func toModelMessages(in []roleContent) []models.ChatMessage {
	out := make([]models.ChatMessage, 0, len(in))
	for _, m := range in {
		out = append(out, models.TextMessage(m.role, m.content))
	}
	return out
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

// artifactStoreAdapter resolves the session's overflow store LAZILY. The tool
// builder runs before a.Session is constructed (registry → AssertSafe precede the
// session in Create), so capturing a.Session.Artifacts() at build time would deref
// a nil session. Reading it per-call defers to the live session; an absent session
// yields a miss (the handler then returns ARTIFACT_UNAVAILABLE/NOT_FOUND).
type artifactStoreAdapter struct{ app *App }

func (a artifactStoreAdapter) Get(id string) (string, bool) {
	if a.app.Session == nil {
		return "", false
	}
	store := a.app.Session.Artifacts()
	if store == nil {
		return "", false
	}
	return store.Get(id)
}

// loadSkills / findSkills resolve the session lazily for the same reason the
// artifact store does — the builder runs before the session exists.
func (a *App) loadSkills(ids []string) []string {
	if a.Session == nil {
		return nil
	}
	return a.Session.LoadAdditionalSkills(ids)
}

// skillSourceAdapter maps *skills.SkillRegistry onto skill.SkillSource (the
// read-only library skill.load reads).
type skillSourceAdapter struct{ app *App }

func (a skillSourceAdapter) Get(id string) (*skill.SkillInfo, bool) {
	sk, ok := a.app.Skills.Get(id)
	if !ok {
		return nil, false
	}
	return &skill.SkillInfo{ID: sk.ID, Title: sk.Title, Summary: sk.Summary}, true
}

// skillFindResultFrom maps the session's skills.SkillFindResult onto the family's
// skill.SkillFindResult (structurally identical; declared separately so the family
// stays storage/session-agnostic).
func (a *App) skillFind(ctx context.Context, query string) (skill.SkillFindResult, error) {
	if a.Session == nil {
		return skill.SkillFindResult{OK: false, Matched: false, Reason: "session unavailable"}, nil
	}
	r := a.Session.FindSkills(ctx, query)
	selected := make([]skill.SkillInfo, 0, len(r.Selected))
	for _, s := range r.Selected {
		selected = append(selected, skill.SkillInfo{ID: s.ID, Title: s.Title, Summary: s.Summary})
	}
	return skill.SkillFindResult{
		OK: r.Ok, Matched: r.Matched, Selected: selected,
		Reason: r.Reason, ActiveSkillIDs: r.ActiveSkillIDs,
	}, nil
}
