package app

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
	"github.com/daintreehq/daintree-assistant/internal/skills"
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
	return prompts.MainPromptContext{
		Tier:                 cfg.Tier,
		ProjectPath:          cfg.ProjectPath,
		ProjectID:            cfg.ProjectID,
		MCPConnected:         st.Connected,
		MCPStatusLine:        mcpStatusLine(st),
		LargeModel:           cfg.LargeModel,
		SmallModel:           cfg.SmallModel,
		SchedulerActive:      schedulerActive,
		ProjectInstructions:  cfg.ProjectInstructions,
		PinnedMemories:       a.pinnedMemoriesBlock(),
		SessionEndedWatchers: a.sessionEndedNote(schedulerActive),
	}
}

// sessionEndedNote returns the carried-over titles of watchers cancelled when the
// prior session ended, but ONLY while the scheduler is active (the interactive path
// where watchers matter) and the one-time NOTE has not yet been consumed by the
// first turn. Returns nil otherwise, so message[1] omits the NOTE. Reads the flag +
// slice under noteMu since a turn goroutine may consume concurrently.
func (a *App) sessionEndedNote(schedulerActive bool) []string {
	if !schedulerActive {
		return nil
	}
	a.noteMu.Lock()
	defer a.noteMu.Unlock()
	if a.sessionEndedNoteConsumed || len(a.sessionEndedWatchers) == 0 {
		return nil
	}
	return a.sessionEndedWatchers
}

// ConsumeSessionEndedNote marks the one-time session-ended-watchers NOTE consumed
// and refreshes message[1] to strip it, so it surfaces on the first interactive turn
// and never again this session. Idempotent and cheap after the first call (no-op when
// already consumed or when there was no NOTE to begin with). Exported so the embedded
// host (which drives Session.Send directly, not App.Send) can consume it after its
// first prompt turn too.
func (a *App) ConsumeSessionEndedNote() {
	a.noteMu.Lock()
	if a.sessionEndedNoteConsumed || len(a.sessionEndedWatchers) == 0 {
		a.noteMu.Unlock()
		return
	}
	a.sessionEndedNoteConsumed = true
	a.noteMu.Unlock()
	// Rebuild message[1] without the NOTE. PromptContext now returns nil for the
	// titles (consumed flag is set), so this strips it.
	a.Session.RefreshRuntimeContext(a.PromptContext())
}

// Send runs one interactive user turn through the session, then consumes the
// one-time session-ended-watchers NOTE so it surfaces during exactly the first turn
// and is stripped from message[1] afterwards. Interactive callers (cockpit/REPL user
// turns) route through here instead of Session.Send directly; autonomous wake turns
// and one-shot runs call Session.Send and never trip the consume (one-shot never
// starts the scheduler, so the NOTE is never seeded there).
func (a *App) Send(ctx context.Context, userInput string, opts agent.SendOptions) (string, error) {
	reply, err := a.Session.Send(ctx, userInput, opts)
	a.ConsumeSessionEndedNote()
	return reply, err
}

// pinnedMemoriesMax caps how many pinned memories are injected (pinned-first order,
// so the most recently pinned win when the operator exceeds it).
const pinnedMemoriesMax = 20

// pinnedMemoriesBlockMaxBytes bounds the rendered pinned-memory block, mirroring the
// DAINTREE.md project-instructions ceiling (16 KiB) — generous, since pins are short.
const pinnedMemoriesBlockMaxBytes = 16384

// pinnedMemoriesBlock renders pinned project memories as "- content" lines for
// message[1] (consumed by prompts.BuildRuntimeContextMessage). Best-effort: a nil
// Store, a query error, or no pins yields "" (the block is simply omitted). The total
// is bounded to pinnedMemoriesBlockMaxBytes and never emits a partial trailing line.
func (a *App) pinnedMemoriesBlock() string {
	if a.Store == nil {
		return ""
	}
	limit := pinnedMemoriesMax
	rows, err := a.Store.ListMemories(storage.MemoryListOptions{PinnedOnly: true, Limit: &limit})
	if err != nil || len(rows) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range rows {
		// Flatten embedded newlines so one memory is exactly one "- fact" line — a raw
		// "\n" would otherwise break the list (or inject a stray heading) into the
		// system message.
		content := strings.ReplaceAll(m.Content, "\r\n", " ")
		content = strings.ReplaceAll(content, "\n", " ")
		content = strings.ReplaceAll(content, "\r", " ")
		line := "- " + strings.TrimSpace(content)
		// +1 for the joining newline; skip a line that would overflow the cap (continue,
		// not break, so a single oversized pin can't suppress the shorter ones after it).
		if b.Len()+len(line)+1 > pinnedMemoriesBlockMaxBytes {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	return b.String()
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
func (p *eventProxy) ToolBatch(b []agent.BatchedToolCall)    { p.sink().ToolBatch(b) }
func (p *eventProxy) ToolState(id string, s agent.ToolState) { p.sink().ToolState(id, s) }
func (p *eventProxy) ToolProgress(id, msg string)            { p.sink().ToolProgress(id, msg) }
func (p *eventProxy) ToolCall(ev agent.ToolCallEvent)        { p.sink().ToolCall(ev) }
func (p *eventProxy) ToolResult(ev agent.ToolResultEvent)    { p.sink().ToolResult(ev) }
func (p *eventProxy) Error(m string)                         { p.sink().Error(m) }
func (p *eventProxy) Info(m string)                          { p.sink().Info(m) }
func (p *eventProxy) Usage(ev agent.UsageEvent)              { p.sink().Usage(ev) }
func (p *eventProxy) TurnPrompt(in string)                   { p.sink().TurnPrompt(in) }
func (p *eventProxy) ModelRateLimited()                      { p.sink().ModelRateLimited() }

// --- toolRunner: the agent.ToolRunner over *tools.Registry + buildContext ---

// toolRunner adapts the concrete *tools.Registry to the agent.ToolRunner seam: it
// projects tools to OpenAI specs (converting the tools.ChatTool shape to the
// models.ChatTool shape), resolves wire names, lists the read-only set, and builds
// the per-call ToolContext (stamped with the turn's run id + allowed names).
type toolRunner struct {
	app *App
	// roNamesOnce/roNames memoize the read-only-turn tool set. It is derived purely
	// from the registry, which is append-only after startup, so it is computed once
	// and reused (watcher/timer wake turns call it repeatedly).
	roNamesOnce sync.Once
	roNames     []string
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

// ReadOnlyToolNames returns read-risk tools minus the skill-context-mutating ones
// (skill.find/skill.load) — the autonomous-wake-turn set (agent.SessionDeps). The
// set is computed once (the registry is append-only after startup) and a defensive
// copy is returned so a caller can't mutate the memoized slice.
func (t *toolRunner) ReadOnlyToolNames() []string {
	t.roNamesOnce.Do(func() {
		for _, tool := range t.app.Registry.List() {
			if tool.Risk != domain.RiskRead {
				continue
			}
			if agent.IsSkillContextMutating(tool.Name) {
				continue
			}
			t.roNames = append(t.roNames, tool.Name)
		}
	})
	if t.roNames == nil {
		return nil
	}
	return append([]string(nil), t.roNames...)
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

// --- skillSelectorAdapter: agent.SkillSelector over skills.SelectSkills ---

type skillSelectorAdapter struct{ router *models.Router }

func (s skillSelectorAdapter) Select(ctx context.Context, candidates []skills.SkillMetadata, query string) (skills.SkillSelection, error) {
	return skills.SelectSkills(ctx, jsonRouterAdapter{router: s.router}, candidates, query)
}

// jsonRouterAdapter bridges skills.JSONRouter (req+out) onto models.Router.JSON
// (ChatOptions → JSON string). It maps the selector messages into ChatOptions,
// pins temperature/maxTokens, calls the small tier, and unmarshals into out.
type jsonRouterAdapter struct{ router *models.Router }

func (j jsonRouterAdapter) JSON(ctx context.Context, tier domain.ModelTier, req skills.JSONRequest, out any) error {
	msgs := make([]models.ChatMessage, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, models.TextMessage(m.Role, m.Content))
	}
	temp := req.Temperature
	maxTok := req.MaxTokens
	raw, err := j.router.JSON(ctx, tier, models.ChatOptions{
		Messages:    msgs,
		Temperature: &temp,
		MaxTokens:   &maxTok,
	})
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(raw), out)
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
