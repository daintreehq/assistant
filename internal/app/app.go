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
	"github.com/daintreehq/daintree-assistant/internal/tools/scratchx"
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

	// scratchStore is the session-scoped, pure in-memory scratch workspace the
	// scratch.* tools drive. Initialized once per App (== once per session) before
	// the tool registry is built, and discarded with the App — nothing here persists
	// or leaks across sessions. Held on App (not Session) because the tool builder
	// runs before a.Session exists; the family captures this concrete store directly.
	scratchStore *scratchx.Store

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

	// noteMu guards the one-time session-ended-watchers carryover. The carryover is set
	// once in Create (from the store's open-time sweep) and surfaces as a NOTE in
	// message[1] from the first scheduler-active PromptContext until the first
	// interactive turn consumes it (sessionEndedNoteConsumed). PromptContext reads it on
	// agent/tool goroutines while Send (on the turn goroutine) consumes it, so both the
	// slice and the flag must be serialized.
	noteMu                   sync.Mutex
	sessionEndedWatchers     []string
	sessionEndedNoteConsumed bool

	// rosterMu guards the startup-context cache: the configured-agents roster and the
	// current worktree label, both surfaced in message[1]. They are fetched once per
	// MCP (re)connect by refreshStartupContext (under the boot/reconnect goroutine) and
	// read by PromptContext on agent/tool goroutines, so — exactly like cfgMu — the
	// write and those reads must be serialized. RWMutex because reads (one per turn-
	// context rebuild) vastly outnumber writes (one per connect). NEVER nest it with
	// cfgMu: PromptContext takes them sequentially, never one inside the other.
	rosterMu             sync.RWMutex
	cachedAgentIDs       []string
	cachedActiveWorktree string
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
		// A fresh, empty scratch workspace for this session. Built before the tool
		// registry so the scratch.* family can capture the concrete store directly.
		scratchStore: scratchx.NewStore(),
		// Watchers a prior session left running were cancelled by store.Open's sweep;
		// carry their titles so the first scheduler-active runtime context surfaces a
		// one-time NOTE offering to re-create them.
		sessionEndedWatchers: store.SessionEndedWatchers(),
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
	// Hard core-tool drift gate: agent.coreToolNames is a hand-maintained list
	// offered to the model on EVERY turn. If a rename or removal drops one of those
	// names from the registry it would boot clean and then silently vanish from the
	// per-turn projection — the model starved of a core tool with no signal. Catch
	// the drift loudly at construction, like AssertSafe.
	if err := a.Registry.AssertRegistered("core tools", agent.CoreToolNames()); err != nil {
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

	// Boot-time validation: every skill's declared requiredTools must name a tool that
	// exists in the registry. requiredTools no longer narrows the toolset (skills never
	// limit what the model can call — the full registry is offered every turn), but it
	// is still a focus hint, and a skill that points the model at a NON-EXISTENT tool
	// name is a documentation bug. Surface it LOUDLY (debug log + the Log hook when
	// present). It is a wiring bug in a pre-release codebase, so we don't hard-fail
	// boot, but it must never pass unnoticed.
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
	droppedRows := 0
	if rows, lerr := store.ListMessages(sessionID); lerr == nil {
		if res, ok := agent.RehydrateSession(rows); ok {
			restored = res.RestoredMessages
			initialSeq = res.InitialSeq
			dirtyFreshStart = res.DirtyFreshStart
			droppedRows = res.DroppedRows
			// Rehydration silently elides corrupt/orphan rows to keep the resume valid.
			// A non-zero count is observable evidence of upstream corruption: record it
			// at boot (the session also emits one info event on the first resumed turn).
			if droppedRows > 0 {
				debuglog.LogDebug(
					debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir},
					"session.rehydrate.dropped",
					map[string]any{"count": droppedRows, "sessionId": sessionID},
				)
			}
		}
	}

	// The durable run-event sink + the live UI proxy, fanned out so a DB-write
	// failure can never break the UI stream.
	events := agent.NewMultiSink(
		agent.NewRunEventSink(store, a.runRef),
		&eventProxy{app: a},
	)

	a.Session = agent.NewSession(agent.SessionDeps{
		Router:               a.Router,
		Tools:                newToolRunner(a),
		SkillSelector:        skillSelectorAdapter{router: a.Router},
		SkillCatalog:         skillReg,
		Store:                store,
		MemoryStore:          store,
		ArtifactPersister:    store,
		PromptContext:        a.PromptContext(),
		SessionID:            sessionID,
		RestoredMessages:     restored,
		InitialSeq:           initialSeq,
		DirtyFreshStart:      dirtyFreshStart,
		DroppedRehydrateRows: droppedRows,
		Events:               events,
		RunRef:               a.runRef,
		// App-scoped parent for the detached post-compaction distill goroutine —
		// cancelled in Shutdown (via baseCancel) so it never touches a closed
		// Router/Store; DrainBackgroundWork joins it there before they close.
		BackgroundCtx: a.baseCtx,
	})

	return a, nil
}

// debugLogAdapter routes the router's debug trace through the global debug log.
type debugLogAdapter struct{ cfg debuglog.Config }

func (a debugLogAdapter) LogDebug(event string, fields map[string]any) {
	debuglog.LogDebug(a.cfg, event, fields)
}
