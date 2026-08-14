package app

import (
	"context"
	"encoding/json"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/models"
	"github.com/daintreehq/assistant/internal/prompts"
	"github.com/daintreehq/assistant/internal/safety"
	"github.com/daintreehq/assistant/internal/storage"
	"github.com/daintreehq/assistant/internal/tools"
)

// PromptContext combines live runtime state with the atomic splash-time Daintree
// snapshot. The session splits it into the cacheable request.startup value (project,
// agents, project instructions) and the fresh runtime/turn tail (tier, MCP, scheduler,
// worktree).
func (a *App) PromptContext() prompts.MainPromptContext {
	// Snapshot Config under cfgMu so a concurrent SetTier (/permissions) can't tear
	// the Tier read while a turn is rebuilding its runtime context.
	cfg := a.snapshotConfig()
	st := a.MCP.Status()
	schedulerActive := a.scheduler != nil
	// Snapshot the cached pointers under startupMu after releasing cfgMu. Values are
	// immutable after publication; copy the structs/slice headers so the returned context
	// cannot alias a later cache replacement.
	a.startupMu.RLock()
	var project *prompts.ProjectContext
	if a.cachedProject != nil {
		copy := *a.cachedProject
		project = &copy
	}
	var agents *prompts.AgentRosterContext
	if a.cachedAgents != nil {
		copy := *a.cachedAgents
		copy.Agents = append([]prompts.AgentContext(nil), a.cachedAgents.Agents...)
		agents = &copy
	}
	var worktree *prompts.WorktreeContext
	if a.cachedWorktree != nil {
		copy := *a.cachedWorktree
		worktree = &copy
	}
	a.startupMu.RUnlock()
	if project == nil && (cfg.ProjectID != "" || cfg.ProjectPath != "") {
		project = &prompts.ProjectContext{ID: cfg.ProjectID, Path: cfg.ProjectPath}
	}
	if project != nil && project.ID == "" {
		project.ID = cfg.ProjectID
	}
	if project != nil && project.Path == "" {
		project.Path = cfg.ProjectPath
	}
	return prompts.MainPromptContext{
		Tier:                cfg.Tier,
		ProjectPath:         cfg.ProjectPath,
		ProjectID:           cfg.ProjectID,
		Project:             project,
		AgentRoster:         agents,
		Worktree:            worktree,
		MCPConnected:        st.Connected,
		MCPStatusLine:       mcpStatusLine(st),
		MCPTransport:        st.Transport,
		MCPToolCount:        st.ToolCount,
		MCPServers:          a.mcpServerContexts(st),
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

// activeWorktreeForFooter is the compact label workflow intelligence records alongside
// its event inputs. Assistant requests use the typed snapshot in request.runtime instead.
func (a *App) activeWorktreeForFooter() string {
	a.startupMu.RLock()
	defer a.startupMu.RUnlock()
	return worktreeLabel(a.cachedWorktree)
}

// resumedWatchersForFooter returns the titles of live watchers this process adopted
// from a prior owner at ownership boot, but ONLY while the scheduler is active — the
// paths where those watchers are actually being supervised again. The footer's
// one-time `# Session note` reads it once, on the first turn, via
// SessionDeps.ResumedWatchers. a.scheduler is nil until StartScheduler runs, which
// precedes the first interactive turn — so an interactive/daemon run returns the
// titles and a one-shot run (no scheduler, nothing resumed) returns nil. Reads
// a.scheduler the same lock-free way PromptContext/buildContext do (StartScheduler
// happens-before turn one).
func (a *App) resumedWatchersForFooter() []string {
	if a.scheduler == nil {
		return nil
	}
	return a.ownership.ResumedWatcherTitles
}

// mcpServerContexts lists the MCP servers this process is wired to, each with its
// endpoint, so the model can answer "which Daintree are you actually talking to?"
// without guessing (ses_8cb40b4e). The primary status is passed in so PromptContext
// keeps its single a.MCP.Status() read.
//
// Endpoint-only by design: the backend renders this list as a session-stable system
// block, so it must NOT carry connected/transport/toolCount — those change mid-session
// (a reconnect flips transport, a dropped link flips connected) and would re-encode the
// ~18k tool schemas behind it on every change. The live status
// already rides the volatile runtime tail. An unconfigured server (no URL — no Daintree
// in the environment) contributes no entry rather than a blank one.
func (a *App) mcpServerContexts(primary mcp.Status) []prompts.MCPServerContext {
	var out []prompts.MCPServerContext
	add := func(name, rawURL, description string) {
		url := mcp.SanitizeURL(rawURL)
		if url == "" {
			return
		}
		out = append(out, prompts.MCPServerContext{Name: name, URL: url, Description: description})
	}
	add("daintree", primary.URL, "Daintree control plane (terminals, agents, worktrees, actions)")
	return out
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
	// AskChoice is wired ONLY for the interactive main actor and left nil otherwise, so
	// user.askMultipleChoice reports QUESTION_NOT_INTERACTIVE on a nil check for a
	// watcher/timer/workflow. When the main actor runs in a surface with no question hook
	// (one-shot, host), the live hook is nil and the closure returns ErrNoAskChoiceHook,
	// which the handler maps to QUESTION_UNAVAILABLE.
	var askChoice func(context.Context, tools.AskChoiceRequest) (tools.AskChoiceAnswer, error)
	if actor == domain.ActorMain {
		askChoice = func(ctx context.Context, req tools.AskChoiceRequest) (tools.AskChoiceAnswer, error) {
			fn := a.snapshotHooks().AskChoice
			if fn == nil {
				return tools.AskChoiceAnswer{}, tools.ErrNoAskChoiceHook
			}
			return fn(ctx, req)
		}
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
		ProjectPath:  cfg.ProjectPath,
		Actor:        actor,
		Confirm:      confirm,
		AskChoice:    askChoice,
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
//
// The actor is the App's configured turn actor: ActorMain ("") for every
// interactive process, ActorWake/WakeActorID for the headless supervisor daemon
// — where the human is away, so dispatch's non-interactive branch (grant or
// blocked-denial) must gate mutations instead of a confirm prompt nobody can
// answer.
func (t *toolRunner) Dispatch(ctx context.Context, name, argsJSON string, turn agent.TurnContext) domain.ToolResult {
	// In-turn tool dispatch is the user-facing path: every MCP call made under
	// this ctx — including the awaitAll/extract poll loops inside handlers —
	// rides the Interactive traffic class, so background polls can never occupy
	// all governor capacity while the turn's calls queue.
	ctx = mcp.WithPriority(ctx, mcp.PriorityInteractive)
	actor, actorID := t.app.turnActor()
	tctx := t.app.buildContext(actor, actorID)
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

// ParallelSafe reports whether the named tool may run concurrently with other calls
// in the same batch. It is an EXPLICIT per-tool opt-in (Tool.Parallelizable), NOT a
// blanket "read risk ⇒ concurrent" rule: some read-only tools are barriers with an
// ordering dependency (terminal.awaitAll must settle BEFORE a following extract
// reads), so only tools individually verified as independent snapshot reads (e.g.
// terminal.extract / .json) set the flag. Double-gated on RiskRead as a safety net so
// a mutating tool can never be parallelized even if it mistakenly sets the flag.
// Unknown names ⇒ false (dispatch serially). Satisfies the agent's optional
// parallelSafeRunner capability.
func (t *toolRunner) ParallelSafe(name string) bool {
	tool := t.app.Registry.Get(name)
	return tool != nil && tool.Parallelizable && tool.Risk == domain.RiskRead
}

// ParallelMutationSafe reports whether the named MUTATING tool may dispatch
// concurrently with consecutive same-name batch siblings (the spawn fan-out).
// The bar is deliberately higher than ParallelSafe: beyond the per-tool
// ParallelHomogeneous opt-in, every member must be ALREADY fully authorized at
// grouping time — interactive main actor, tier allows the risk, and auto-approve
// removing the prompt. Anything that would reach dispatch's confirmation or
// grant branch stays serial: the cockpit holds exactly ONE pending approval
// (concurrent Confirm calls would overwrite each other's resolve channels), and
// which concurrent call consumes a bounded grant's last use must never be
// scheduling-dependent. The same dispatch pipeline still runs per call — this
// gate only decides grouping, never authorization itself. Satisfies the agent's
// optional parallelMutationRunner capability.
func (t *toolRunner) ParallelMutationSafe(name string) bool {
	tool := t.app.Registry.Get(name)
	if tool == nil || !tool.ParallelHomogeneous || tool.Risk == domain.RiskRead {
		return false
	}
	actor, _ := t.app.turnActor()
	if actor != domain.ActorMain {
		return false
	}
	// Snapshot under cfgMu so a concurrent /permissions tier change can't tear the
	// read; the SAME snapshot rule dispatch itself uses (buildContext).
	cfg := t.app.snapshotConfig()
	decision := safety.Decide(tool.Risk, cfg.Tier)
	if !decision.Allowed {
		return false
	}
	return !decision.NeedsConfirmation || cfg.AutoApprove
}

// ParallelConflictKey resolves a call's independence classification for a
// homogeneous-mutation cohort from the tool's own ParallelConflictKey (nil ⇒
// freely independent). Unknown tools never join a cohort.
func (t *toolRunner) ParallelConflictKey(name string, args json.RawMessage) ([]string, bool) {
	tool := t.app.Registry.Get(name)
	if tool == nil {
		return nil, false
	}
	if tool.ParallelConflictKey == nil {
		return nil, true
	}
	return tool.ParallelConflictKey(args)
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
