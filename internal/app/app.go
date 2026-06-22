// Package app is the single composition root. App.Create
// builds every dependency once in a fixed order — config → store → mcp → queue →
// router → tools registry → skills → agent session → (lazy) scheduler — exposes a
// ToolContext factory, the main AgentSession, and drives both the CLI and the
// (future Bubble Tea) cockpit. Shutdown tears the dependencies down in reverse.
package app

import (
	"context"
	"strings"
	"sync"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/daemon"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/queue"
	"github.com/daintreehq/daintree-assistant/internal/skills"
	"github.com/daintreehq/daintree-assistant/internal/storage"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// AppHooks are the swappable UI/REPL callbacks. SetHooks merges partial updates
// so the live session never rebuilds (which would drop history) — the closures in
// buildContext read these live.
type AppHooks struct {
	// Confirm approves a mutating action for the interactive main actor. A returned
	// error is treated as a DECLINE.
	Confirm func(ctx context.Context, req tools.ConfirmRequest) (bool, error)
	// Log emits an out-of-band line to the user.
	Log func(msg string)
	// AgentEvents is the live UI/console/JSON sink the session proxies to.
	AgentEvents agent.EventSink
}

// CreateOptions is the App.Create input.
type CreateOptions struct {
	Overrides config.ConfigOverrides
	Hooks     AppHooks
	// SessionID overrides the generated session id (resume / tests). "" ⇒ a fresh
	// "ses_"+8hex id.
	SessionID string
	// MCPClientOverride injects a pre-connected low-level MCP client (tests).
	MCPClientOverride mcp.LowLevelClient
	// BuildTools is the tool-registry builder seam. The full tool-family wiring is a
	// separate wave; nil ⇒ DefaultToolBuilder (the always-safe core tools). The
	// builder runs AFTER the registry exists but BEFORE AssertSafe.
	BuildTools ToolBuilder
}

// ToolBuilder returns the tools to register on the App's registry. It is a SEAM:
// the tool-family wiring wave registers the real families here; until then the
// default builds the safe core set so the module stays green. The App passes
// itself so a builder can reach config/store/mcp/queue/router.
type ToolBuilder func(a *App) ([]*tools.Tool, error)

// App is the composition root. Fields are effectively read-only after Create
// except Config.Tier (mutated by /permissions), Hooks (merged by SetHooks), and
// scheduler (set lazily by StartScheduler).
type App struct {
	Config   config.AppConfig
	Store    *storage.Store
	MCP      *mcp.Client
	Queue    *queue.Queue
	Router   *models.Router
	Registry *tools.Registry
	Skills   *skills.SkillRegistry

	SessionID string
	Session   *agent.Session

	runRef    *agent.RunIDRef
	scheduler *daemon.Scheduler

	// baseCtx is the APP-SCOPED background context for detached work that outlives a
	// single turn (e.g. terminal.extract.async) but must NOT outlive the App. It is
	// cancelled in Shutdown so a closing assistant tears down its background jobs
	// instead of leaking goroutines into closed MCP/Router/Store deps. It is NOT the
	// per-turn ctx (that would cancel the background job the instant the turn ends).
	baseCtx    context.Context
	baseCancel context.CancelFunc

	// hooks are read by agent/tool goroutines (the buildContext confirm/log closures
	// and the eventProxy sink) while SetHooks is called from the UI's Update loop, so
	// they are guarded by hooksMu (#7 data race). Snapshot under RLock; replace under
	// Lock. The hooks struct holds only func/interface values, so a snapshot copy is
	// cheap and the closures read a consistent set.
	hooksMu sync.RWMutex
	hooks   AppHooks

	// cfgMu guards Config — specifically Config.Tier, the one field mutated at
	// runtime (/permissions). buildContext copies the whole Config and PromptContext
	// reads Tier on agent/tool goroutines, so SetTier (write) and those reads must be
	// serialized or the race detector flags a torn read of the mutated field.
	cfgMu sync.RWMutex

	// InitialTier is the boot-time tier resolved from env/overrides/DEFAULTS, captured
	// once in Create and NEVER mutated by SetTier. /permissions compares the live tier
	// against it to warn that a narrowed/broadened tier is session-only and reverts next
	// launch. Immutable after Create, so it needs no lock.
	InitialTier domain.Tier
}

// snapshotHooks returns a consistent copy of the current hooks under the read lock.
func (a *App) snapshotHooks() AppHooks {
	a.hooksMu.RLock()
	defer a.hooksMu.RUnlock()
	return a.hooks
}

// SetTier updates the permission tier under cfgMu. Callers refresh the runtime
// prompt context afterwards (PromptContext takes its own read lock, so this must
// not hold cfgMu across that call).
func (a *App) SetTier(t domain.Tier) {
	a.cfgMu.Lock()
	a.Config.Tier = t
	a.cfgMu.Unlock()
}

// Tier returns the current permission tier under the read lock.
func (a *App) Tier() domain.Tier {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.Config.Tier
}

// snapshotConfig returns a consistent copy of Config under the read lock, so a
// caller building a per-turn ToolContext can't observe a torn Tier write.
func (a *App) snapshotConfig() config.AppConfig {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.Config
}

// Create builds the App in the canonical construction order. A failure at any
// stage closes whatever was already opened and returns the error.
func Create(opts CreateOptions) (*App, error) {
	cfg, err := config.LoadConfig(opts.Overrides)
	if err != nil {
		return nil, err
	}

	store, err := storage.Open(cfg.DBPath, nil)
	if err != nil {
		return nil, err
	}

	sessionID := opts.SessionID
	if sessionID == "" {
		sessionID = domain.NewID("ses_")
	}

	baseCtx, baseCancel := context.WithCancel(context.Background())
	a := &App{
		Config:      cfg,
		Store:       store,
		SessionID:   sessionID,
		runRef:      &agent.RunIDRef{},
		hooks:       opts.Hooks,
		baseCtx:     baseCtx,
		baseCancel:  baseCancel,
		InitialTier: cfg.Tier,
	}

	// mcp → queue → router → registry → skills.
	a.MCP = mcp.New(cfg, mcp.Options{ClientOverride: opts.MCPClientOverride})
	a.Queue = queue.New(queueEventStore{s: store}, domain.NowMS)
	a.Router = models.NewRouter(
		models.RouterConfig{LargeModel: cfg.LargeModel, MediumModel: cfg.MediumModel, SmallModel: cfg.SmallModel},
		models.NewFireworksClient(models.FireworksConfig{BaseURL: cfg.FireworksBaseURL, APIKey: cfg.FireworksAPIKey, Offline: cfg.Offline}),
		debugLogAdapter{cfg: debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir}},
	)

	a.Registry = tools.NewRegistry()
	build := opts.BuildTools
	if build == nil {
		build = DefaultToolBuilder
	}
	built, err := build(a)
	if err != nil {
		baseCancel()
		_ = store.Close()
		return nil, err
	}
	if err := a.Registry.RegisterAll(built...); err != nil {
		baseCancel()
		_ = store.Close()
		return nil, err
	}
	// Hard no-file-edit gate at construction (tools-core invariant). A forbidden
	// tool name aborts boot.
	if err := a.Registry.AssertSafe(); err != nil {
		baseCancel()
		_ = store.Close()
		return nil, err
	}

	initial, err := skills.LoadSkills()
	if err != nil {
		baseCancel()
		_ = store.Close()
		return nil, err
	}
	skillReg, err := skills.NewRegistry(initial)
	if err != nil {
		baseCancel()
		_ = store.Close()
		return nil, err
	}
	a.Skills = skillReg

	// Boot-time validation: every skill's declared requiredTools must exist in the
	// registry. A missing (skillID, tool) pair would otherwise boot clean and then
	// silently vanish from OpenAITools when that skill is loaded — the model is
	// starved of a tool it was promised, with no signal. Surface it LOUDLY (debug
	// log + the Log hook when present). It is a wiring bug in a pre-release codebase,
	// so we don't hard-fail boot, but it must never pass unnoticed.
	if missing := skills.ValidateRequiredTools(a.Skills.All(), a.Registry); len(missing) > 0 {
		pairs := make([]string, 0, len(missing))
		for _, m := range missing {
			pairs = append(pairs, m.SkillID+" → "+m.Tool)
		}
		warning := "skill requiredTools missing from the registry (the model will be starved of these): " + strings.Join(pairs, ", ")
		debuglog.LogDebug(
			debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir},
			"skill.required_tools_missing",
			map[string]any{"missing": pairs},
		)
		if a.hooks.Log != nil {
			a.hooks.Log("⚠ " + warning)
		}
	}

	// Resume prior conversation if this session id has history.
	var restored []models.ChatMessage
	initialSeq := 0
	dirtyFreshStart := false
	if rows, lerr := store.ListMessages(sessionID); lerr == nil {
		if res, ok := agent.RehydrateSession(rows); ok {
			restored = res.RestoredMessages
			initialSeq = res.InitialSeq
			dirtyFreshStart = res.DirtyFreshStart
		}
	}

	// The durable run-event sink + the live UI proxy, fanned out so a DB-write
	// failure can never break the UI stream.
	events := agent.NewMultiSink(
		agent.NewRunEventSink(store, a.runRef),
		&eventProxy{app: a},
	)

	a.Session = agent.NewSession(agent.SessionDeps{
		Router:           a.Router,
		Tools:            newToolRunner(a),
		SkillSelector:    skillSelectorAdapter{router: a.Router},
		SkillCatalog:     skillReg,
		Store:            store,
		MemoryStore:      store,
		PromptContext:    a.PromptContext(),
		SessionID:        sessionID,
		RestoredMessages: restored,
		InitialSeq:       initialSeq,
		DirtyFreshStart:  dirtyFreshStart,
		Events:           events,
		RunRef:           a.runRef,
	})

	return a, nil
}

// debugLogAdapter routes the router's debug trace through the global debug log.
type debugLogAdapter struct{ cfg debuglog.Config }

func (a debugLogAdapter) LogDebug(event string, fields map[string]any) {
	debuglog.LogDebug(a.cfg, event, fields)
}
