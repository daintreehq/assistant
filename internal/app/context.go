package app

import (
	"context"
	"encoding/json"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
	"github.com/daintreehq/daintree-assistant/internal/storage"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// PromptContext builds the dynamic MainPromptContext from the live MCP status +
// config + scheduler state. It is re-read on every connect/reconnect
// and on /permissions changes so message[1] stays current.
func (a *App) PromptContext() prompts.MainPromptContext {
	// Snapshot Config under cfgMu so a concurrent SetTier (/permissions) can't tear
	// the Tier read while a turn is rebuilding its runtime context.
	cfg := a.snapshotConfig()
	st := a.MCP.Status()
	schedulerActive := a.scheduler != nil
	// Read the startup cache (configured-agents roster) under its own lock, SEQUENTIALLY —
	// snapshotConfig already released cfgMu, so rosterMu is never nested inside cfgMu.
	// refreshStartupContext (the sole writer) populated it on the last (re)connect; this is
	// a plain field read, no MCP round-trip. The active-worktree label is NO LONGER read
	// here: it moved to the uncached footer (issue #263), reached via SessionDeps.ActiveWorktreeFunc.
	a.rosterMu.RLock()
	agentIDs := a.cachedAgentIDs
	a.rosterMu.RUnlock()
	return prompts.MainPromptContext{
		Tier:                cfg.Tier,
		ProjectPath:         cfg.ProjectPath,
		ProjectID:           cfg.ProjectID,
		MCPConnected:        st.Connected,
		MCPStatusLine:       mcpStatusLine(st),
		MCPTransport:        st.Transport,
		MCPToolCount:        st.ToolCount,
		ConfiguredAgentIDs:  agentIDs,
		SchedulerActive:     schedulerActive,
		ProjectInstructions: cfg.ProjectInstructions,
	}
}

// Send runs one interactive user turn through the session. It is the interactive entry
// point (cockpit/REPL user turns route through here); autonomous wake turns and one-shot
// runs call Session.Send directly. The one-time session-ended-watchers note that this
// wrapper used to consume now lives in the uncached footer, surfaced once by the Session
// itself (sessionEndedNoteShown) — so Send is a thin pass-through with no post-turn work.
func (a *App) Send(ctx context.Context, userInput string, opts agent.SendOptions) (string, error) {
	return a.Session.Send(ctx, userInput, opts)
}

// activeWorktreeForFooter returns the current active-worktree label for the footer's
// `# Active worktree` section (SessionDeps.ActiveWorktreeFunc). It reads the startup cache
// under rosterMu — the same field PromptContext used to surface in message[1] — so a
// mid-session switch (refreshStartupContext on reconnect) shows up on the next round
// without rewriting the cached runtime context (issue #263).
func (a *App) activeWorktreeForFooter() string {
	a.rosterMu.RLock()
	defer a.rosterMu.RUnlock()
	return a.cachedActiveWorktree
}

// sessionEndedWatchersForFooter returns the carried-over titles of watchers a prior
// session left running (the store recorded them at open-time), but ONLY while the
// scheduler is active — the interactive path where the offer to re-create them is
// meaningful. The footer's one-time `# Session note` reads it once, on the first turn, via
// SessionDeps.SessionEndedWatchers. a.scheduler is nil until StartScheduler runs, which
// precedes the first interactive turn — so an interactive run returns the titles and a
// one-shot/non-interactive run (no scheduler) returns nil. Reads a.scheduler the same
// lock-free way PromptContext/buildContext do (StartScheduler happens-before turn one).
func (a *App) sessionEndedWatchersForFooter() []string {
	if a.scheduler == nil {
		return nil
	}
	return a.Store.SessionEndedWatchers()
}

// mcpStatusLine renders the connected/not-connected one-liner.
func mcpStatusLine(st mcp.Status) string {
	if st.Connected {
		count := "?"
		if st.ToolCount != nil {
			count = itoa(*st.ToolCount)
		}
		return "connected (" + st.Transport + ", " + count + " tools)"
	}
	reason := st.Error
	if reason == "" {
		reason = "no url/token"
	}
	return "not connected — " + reason
}

// buildContext is the ToolContext factory. actor gates the confirm
// branch: only the interactive "main" actor can prompt; non-interactive actors
// (watcher/timer/workflow) auto-decline and rely on automation grants. The Confirm
// / Log closures read a.hooks LIVE so SetHooks partial updates take effect without
// rebuilding the session.
func (a *App) buildContext(actor domain.ToolActor, actorID string) *tools.ToolContext {
	// Read hooks through snapshotHooks (RLock) — the UI's Update loop may call
	// SetHooks concurrently with these closures running on agent/tool goroutines (#7).
	confirm := func(ctx context.Context, req tools.ConfirmRequest) (bool, error) {
		if actor != domain.ActorMain {
			return false, nil
		}
		fn := a.snapshotHooks().Confirm
		if fn == nil {
			return false, nil
		}
		return fn(ctx, req)
	}
	log := func(msg string) {
		if fn := a.snapshotHooks().Log; fn != nil {
			fn(msg)
		}
	}
	// Snapshot Config under cfgMu so the per-turn tool context can't capture a torn
	// Tier write from a concurrent /permissions change.
	cfg := a.snapshotConfig()
	return &tools.ToolContext{
		Config:       cfg,
		MCP:          mcpToolAdapter{c: a.MCP},
		DB:           storeToolAdapter{s: a.Store},
		Queue:        a.Queue,
		Router:       a.Router,
		ProjectPath:  cfg.ProjectPath,
		Actor:        actor,
		Confirm:      confirm,
		Log:          log,
		SessionID:    a.SessionID,
		ActorID:      actorID,
		DaemonActive: func() bool { return a.scheduler != nil },
		// Only the interactive MAIN turn can be interrupted by a typed message — a
		// watcher/timer/workflow await has no human folding messages into IT. Read
		// a.Session LIVE (it is built after buildContext's closures are defined, and
		// this runs per tool call, long after).
		InjectionsPending: func() bool {
			return actor == domain.ActorMain && a.Session != nil && a.Session.HasPendingInjections()
		},
	}
}

// --- eventProxy: stable session sink delegating to the live UI hook ---

// eventProxy is the stable EventSink the session holds; every method delegates to
// a.hooks.AgentEvents (read live), so SetHooks can swap the UI/console/JSON sink
// without rebuilding the session and dropping history.
type eventProxy struct{ app *App }

func (p *eventProxy) sink() agent.EventSink {
	// Read through the RLock-guarded snapshot — the session emits events on its own
	// goroutine while the UI may swap the sink via SetHooks (#7 data race).
	if s := p.app.snapshotHooks().AgentEvents; s != nil {
		return s
	}
	return agent.NoopEventSink{}
}

func (p *eventProxy) Phase(ph domain.RunPhase)               { p.sink().Phase(ph) }
func (p *eventProxy) AssistantStart()                        { p.sink().AssistantStart() }
func (p *eventProxy) AssistantToken(t string)                { p.sink().AssistantToken(t) }
func (p *eventProxy) AssistantEnd(c, r string)               { p.sink().AssistantEnd(c, r) }
func (p *eventProxy) AssistantCancelled(c string)            { p.sink().AssistantCancelled(c) }
func (p *eventProxy) Interjection(t string)                  { p.sink().Interjection(t) }
func (p *eventProxy) SkillLoaded(t []string)                 { p.sink().SkillLoaded(t) }
func (p *eventProxy) ToolBatch(b []agent.BatchedToolCall)    { p.sink().ToolBatch(b) }
func (p *eventProxy) ToolState(id string, s agent.ToolState) { p.sink().ToolState(id, s) }
func (p *eventProxy) ToolProgress(id, msg string)            { p.sink().ToolProgress(id, msg) }
func (p *eventProxy) ToolCall(ev agent.ToolCallEvent)        { p.sink().ToolCall(ev) }
func (p *eventProxy) ToolResult(ev agent.ToolResultEvent)    { p.sink().ToolResult(ev) }
func (p *eventProxy) Error(m string)                         { p.sink().Error(m) }
func (p *eventProxy) Warn(m string)                          { p.sink().Warn(m) }
func (p *eventProxy) Info(m string)                          { p.sink().Info(m) }
func (p *eventProxy) Usage(ev agent.UsageEvent)              { p.sink().Usage(ev) }
func (p *eventProxy) TurnPrompt(in string)                   { p.sink().TurnPrompt(in) }
func (p *eventProxy) ModelRateLimited()                      { p.sink().ModelRateLimited() }

// --- toolRunner: the agent.ToolRunner over *tools.Registry + buildContext ---

// toolRunner adapts the concrete *tools.Registry to the agent.ToolRunner seam: it
// projects tools to OpenAI specs (converting the tools.ChatTool shape to the
// models.ChatTool shape), resolves wire names, and builds the per-call ToolContext
// (stamped with the turn's run id + allowed names).
type toolRunner struct {
	app *App
}

func newToolRunner(a *App) *toolRunner { return &toolRunner{app: a} }

func (t *toolRunner) OpenAITools(filterNames []string) ([]models.ChatTool, error) {
	specs, err := t.app.Registry.OpenAITools(filterNames)
	if err != nil {
		return nil, err
	}
	out := make([]models.ChatTool, 0, len(specs))
	for _, sp := range specs {
		// Parameters are already canonical schema bytes (json.RawMessage) — forward
		// them verbatim, no re-marshal round-trip.
		out = append(out, models.ChatTool{
			Type: sp.Type,
			Function: models.ChatToolFunc{
				Name:        sp.Function.Name,
				Description: sp.Function.Description,
				Parameters:  sp.Function.Parameters,
			},
		})
	}
	return out, nil
}

func (t *toolRunner) ResolveWireName(wireName string) string {
	return t.app.Registry.ResolveWireName(wireName)
}

// Dispatch builds the per-call ToolContext from the turn and runs the call. It
// NEVER returns an error — every failure is a domain.ToolResult.
func (t *toolRunner) Dispatch(ctx context.Context, name, argsJSON string, turn agent.TurnContext) domain.ToolResult {
	tctx := t.app.buildContext(domain.ActorMain, "")
	tctx.RunID = turn.RunID
	tctx.ActiveToolNames = turn.ActiveToolNames
	// Liveness: forward the registry's in-tool progress beats out to the session's
	// sink, tagged with this call's id so the UI maps them to the right activity row.
	// The registry emits the standard
	// validating→awaiting_approval→running phases; long handlers add substeps.
	tctx.ToolCallID = turn.CallID
	if turn.Progress != nil {
		callID := turn.CallID
		tctx.ReportProgress = func(p tools.ToolProgress) {
			turn.Progress(callID, p.Message)
		}
	}
	return t.app.Registry.Dispatch(ctx, name, json.RawMessage(argsJSON), tctx)
}

// --- storeToolAdapter: tools.Store over *storage.Store ---

// storeToolAdapter adapts the concrete *storage.Store (no-ctx, record-returning
// methods) onto the narrow tools.Store seam the dispatch pipeline calls
// (ctx-carrying, id-returning audit insert + grant consume).
type storeToolAdapter struct{ s *storage.Store }

func (a storeToolAdapter) InsertAudit(_ context.Context, rec domain.AuditRecord) (string, error) {
	out, err := a.s.InsertAudit(rec)
	if err != nil {
		return "", err
	}
	return out.ID, nil
}

func (a storeToolAdapter) ConsumeGrant(_ context.Context, actorID string, actorType domain.AutomationGrantActorType,
	toolName string, riskClass domain.RiskClass, now int64) (*domain.AutomationGrantRecord, error) {
	return a.s.ConsumeGrant(actorID, actorType, toolName, riskClass, now)
}

// --- mcpToolAdapter: tools.MCPClient over *mcp.Client ---

// mcpToolAdapter adapts the concrete mcp.Client (CallTool with CallOptions →
// CallResult) onto the narrow tools.MCPClient seam daintree.call forwards to.
type mcpToolAdapter struct{ c *mcp.Client }

func (m mcpToolAdapter) Connected() bool { return m.c.IsConnected() }

func (m mcpToolAdapter) CallTool(ctx context.Context, name string, args map[string]any) (tools.MCPCallResult, error) {
	res, err := m.c.CallTool(ctx, name, args, mcp.CallOptions{})
	if err != nil {
		return tools.MCPCallResult{}, err
	}
	return tools.MCPCallResult{
		Text:              res.Text,
		StructuredContent: res.StructuredContent,
		IsError:           res.IsError,
	}, nil
}
