// Package app is the single composition root. App.Create
// builds every dependency once in a fixed order — config → store → mcp → queue →
// router → tools registry → skills → agent session → (lazy) scheduler — exposes a
// ToolContext factory, the main AgentSession, and drives both the CLI and the
// (future Bubble Tea) cockpit. Shutdown tears the dependencies down in reverse.
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/daemon"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/queue"
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
	// DocsMCPClientOverride injects a pre-connected low-level docs-MCP client (tests),
	// so the docs tool family can be exercised without hitting the live docs server.
	DocsMCPClientOverride mcp.LowLevelClient
	// BackendOverride injects a fake Daintree backend (tests), bypassing the real
	// HTTP client so unit tests need no live server. nil ⇒ the real client to the
	// hardcoded dev endpoint.
	BackendOverride backend.Backend
	// BuildTools is the tool-registry builder seam. The full tool-family wiring is a
	// separate wave; nil ⇒ DefaultToolBuilder (the always-safe core tools). The
	// builder runs AFTER the registry exists but BEFORE AssertSafe.
	BuildTools ToolBuilder
	// OnSchemaStale, when non-nil, is consulted if the on-disk SQLite database was
	// initialized at an OLDER schema baseline than this build (a storage.SchemaStaleError).
	// It returns true to authorise a DESTRUCTIVE reset (wipe the DB files + rebuild the
	// schema) or false to abort with the stale-schema error. Interactive callers wire a
	// y/N terminal prompt here; non-interactive callers leave it nil, preserving the loud,
	// actionable failure — a script/host path must never silently destroy local state.
	OnSchemaStale func(have, want int) (bool, error)
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
	Config config.AppConfig
	Store  *storage.Store
	MCP    *mcp.Client
	// DocsMCP is the SECOND MCP transport: the public, no-auth Daintree documentation
	// server (docs.search / docs.getPage / docs.getRelatedPages). Always constructed
	// (never nil) and connected in PARALLEL with MCP during the boot splash; it answers
	// "how do I use Daintree" help questions and is fully independent of the primary
	// control-plane MCP. Immutable after Create.
	DocsMCP *mcp.Client
	Queue   *queue.Queue
	// Backend is the native Daintree backend — the assistant turn engine and the
	// server-owned utility tasks. It replaces the direct model provider. Held as an
	// interface so tests can inject a fake (CreateOptions.BackendOverride).
	Backend backend.Backend
	// Router is the legacy DeepSeek model router, retained transitionally only for the
	// ToolContext.Router seam / diagnostics; no assistant turn or utility task uses it.
	Router   *models.Router
	Registry *tools.Registry

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

	// rosterMu guards the startup-context cache: the configured-agents roster (surfaced in
	// message[1] via PromptContext) and the current worktree label (surfaced in the
	// uncached footer via activeWorktreeForFooter since issue #263). Both are fetched once
	// per MCP (re)connect by refreshStartupContext (under the boot/reconnect goroutine) and
	// read on agent/tool goroutines, so — exactly like cfgMu — the write and those reads
	// must be serialized. RWMutex because reads (one per turn-context rebuild / footer
	// build) vastly outnumber writes (one per connect). NEVER nest it with cfgMu:
	// PromptContext takes them sequentially, never one inside the other.
	rosterMu             sync.RWMutex
	cachedAgentIDs       []string
	cachedActiveWorktree string

	// reconcileLedgerOnce guards the boot ledger reconcile (ReconcileLedger) so it runs
	// exactly once per process — on the first successful MCP connect. A mid-session
	// /reconnect must NOT re-run it: it reads the LIVE terminal.list, so a re-run after
	// the user ended a terminal in-session could wrongly expire a run still being worked.
	reconcileLedgerOnce sync.Once

	// docsConnectWG tracks in-flight docs-MCP (re)connect goroutines (connectDocsAsync).
	// Shutdown waits on it AFTER baseCancel so a late applyConnected can't install a docs
	// session after the client is Closed (which would leak the session goroutine).
	docsConnectWG sync.WaitGroup
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
		// A stale on-disk schema is the one open failure with a clean, supported
		// recovery (pre-release policy resets rather than migrates). If the caller
		// supplied an OnSchemaStale handler — an interactive terminal that can ask the
		// human — consult it; on a "yes" wipe the DB files and re-Open onto a fresh
		// schema. Without a handler (scripts / host / non-TTY) the actionable error
		// propagates untouched, so we never silently destroy local state.
		var stale *storage.SchemaStaleError
		if !errors.As(err, &stale) || opts.OnSchemaStale == nil {
			return nil, err
		}
		reset, cbErr := opts.OnSchemaStale(stale.Have, stale.Want)
		if cbErr != nil {
			return nil, cbErr
		}
		if !reset {
			return nil, err // declined → keep the actionable stale-schema error
		}
		if rerr := storage.ResetDB(cfg.DBPath); rerr != nil {
			return nil, fmt.Errorf("reset database: %w", rerr)
		}
		if store, err = storage.Open(cfg.DBPath, nil); err != nil {
			return nil, err
		}
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
	}

	// mcp → docs-mcp → queue → router → registry → skills.
	a.MCP = mcp.New(cfg, mcp.Options{ClientOverride: opts.MCPClientOverride})
	// The Daintree documentation MCP — a public, no-auth, stateless HTTP server that
	// answers "how do I use Daintree" help questions via live doc search. Its endpoint is
	// a fixed product URL (DAINTREE_DOCS_MCP_URL overrides it for dev/test, mirroring the
	// DAINTREE_BACKEND_URL pattern). Anonymous (no bearer) with its own short drift
	// baseline so it never warns that the 60 Daintree control-plane tools are "missing".
	// Connected in parallel with the primary MCP during the boot splash (ConnectMcp).
	docsURL := mcp.DefaultDocsURL
	if v := strings.TrimSpace(os.Getenv("DAINTREE_DOCS_MCP_URL")); v != "" {
		docsURL = v
	}
	a.DocsMCP = mcp.New(cfg, mcp.Options{
		URL:            &docsURL,
		Anonymous:      true,
		DriftBaseline:  mcp.DocsDocumentedToolNames,
		ClientOverride: opts.DocsMCPClientOverride,
	})
	a.Queue = queue.New(queueEventStore{s: store}, domain.NowMS)
	// The native Daintree backend: the assistant turn engine + server-owned utility
	// tasks. The CLI no longer talks to DeepSeek directly — the backend owns the model
	// credentials, prompt assembly, and skill selection.
	//
	// DEVELOPMENT-ONLY: the endpoint is HARDCODED to the single local backend
	// (backend.DefaultBaseURL, http://127.0.0.1:8473) and there is no authentication.
	// The assistant supports exactly this one endpoint for now; a later phase replaces
	// the URL with the production endpoint and adds the real login flow. The only
	// escape hatch is DAINTREE_BACKEND_URL, which exists for local dev + e2e tests (a
	// fake backend server) — it is NOT a product config knob, and the default is always
	// the hardcoded endpoint.
	if opts.BackendOverride != nil {
		a.Backend = opts.BackendOverride
	} else {
		baseURL := backend.DefaultBaseURL
		if v := strings.TrimSpace(os.Getenv("DAINTREE_BACKEND_URL")); v != "" {
			baseURL = v
		}
		dbg := debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir}
		a.Backend = backend.NewClient(backend.ClientConfig{
			BaseURL: baseURL,
			ClientInfo: backend.ClientInfo{
				Name:     "daintree-cli",
				Platform: runtime.GOOS,
			},
			// Surface each transient-failure retry to the session log — otherwise a
			// retried turn leaves no trace and a later log read dead-ends at the last
			// successful tool call (the gap that hid the wild 502).
			OnRetry: func(info backend.RetryInfo) {
				debuglog.LogDebug(dbg, "backend.retry", map[string]any{
					"attempt": info.Attempt,
					"delayMs": info.Delay.Milliseconds(),
					"error":   info.Err.Error(),
				})
			},
		})
	}
	a.Router = models.NewRouter(
		models.RouterConfig{LargeModel: cfg.LargeModel, MediumModel: cfg.MediumModel, SmallModel: cfg.SmallModel},
		models.NewDeepSeekClient(models.DeepSeekConfig{BaseURL: cfg.DeepSeekBaseURL, APIKey: cfg.DeepSeekAPIKey, Offline: cfg.Offline}),
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

	// Skills are SERVER-OWNED: the backend's selector picks and injects runbooks. The
	// CLI no longer loads a local skill catalog (no embedded skill files, no
	// requiredTools validation) — see docs/BACKEND.md.

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

			// Durable compaction checkpoint resume (issue #260). LoadValidCheckpoints
			// returns the parseable slots most-current first (a JSON-corrupt 'latest' is
			// already dropped); SelectResumeCheckpoint then applies the semantic-validity
			// gate and falls back to 'prev' when 'latest' is stale (seq ahead of the delta
			// we just replayed). The accepted compaction depth is recorded for observability
			// — the session continues the depth chain once the compaction-time write path
			// (issue #256, which owns session.go's compactLocked) feeds it back in.
			if cps, cerr := store.LoadValidCheckpoints(); cerr == nil && len(cps) > 0 {
				cp, depth, accepted := agent.SelectResumeCheckpoint(cps, res)
				debuglog.LogDebug(
					debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir},
					"session.checkpoint.resumed",
					map[string]any{
						"slot": cp.Slot, "accepted": accepted, "compactionDepth": depth,
						"lastSeq": cp.LastSeq, "initialSeq": res.InitialSeq,
						"candidates": len(cps), "sessionId": sessionID,
					},
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

	// The turn engine's diagnostic trace seam, routed to the per-session debug log.
	// This restores the trace coverage the backend migration removed: turn.start/end
	// and the backend.respond.* round narration (the successor to the legacy router's
	// model.request/model.response). Wired ONLY when debug logging is on — a nil seam
	// is a true no-op, so the per-round field maps/hashes/previews are never built when
	// the feature is off (the trace must add zero cost to a normal run).
	var traceFn func(event string, fields map[string]any)
	if cfg.DebugLog {
		traceCfg := debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir}
		traceFn = func(event string, fields map[string]any) {
			debuglog.LogDebug(traceCfg, event, fields)
		}
	}

	a.Session = agent.NewSession(agent.SessionDeps{
		Backend:            a.Backend,
		Router:             a.Router,
		Tools:              newToolRunner(a),
		Store:              store,
		MemoryStore:        store,
		MemoryRecaller:     memoryRecallerAdapter{s: store},
		PinnedMemoryLister: pinnedMemoryListerAdapter{s: store},
		// The footer's volatile-state seams (issue #263): the worktree label and the
		// one-time session-ended note. Both are bound App methods so the wiring is testable
		// directly; see activeWorktreeForFooter / sessionEndedWatchersForFooter.
		ActiveWorktreeFunc:   a.activeWorktreeForFooter,
		SessionEndedWatchers: a.sessionEndedWatchersForFooter,
		ArtifactPersister:    store,
		WorkflowRunLister:    store,
		// Per-turn open-terminal inventory (issue #286): a fresh terminal.list +
		// no-output terminal.getStatus snapshot attached to the runtime block so the model
		// sees the live roster as inert data instead of discovering it mid-turn. Best-effort
		// and bounded; reuses the same MCP read adapter as the extraction/id-resolution path.
		OpenTerminalsFetcher: terminalReaderAdapter{c: a.MCP}.FetchOpenTerminals,
		PromptContext:        a.PromptContext(),
		// Live runtime context: pulled every round so a post-construction MCP connect,
		// /permissions tier change, or scheduler start reaches the backend (replaces the
		// removed RefreshRuntimeContext push). a.PromptContext reads live MCP/scheduler state.
		PromptContextFunc:    a.PromptContext,
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
		// Diagnostic trace seam (nil unless debug logging is on — see traceFn above).
		Trace: traceFn,
	})

	return a, nil
}

// debugLogAdapter routes the router's debug trace through the global debug log.
type debugLogAdapter struct{ cfg debuglog.Config }

func (a debugLogAdapter) LogDebug(event string, fields map[string]any) {
	debuglog.LogDebug(a.cfg, event, fields)
}
