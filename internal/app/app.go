// Package app is the single composition root. App.Create
// builds every dependency once in a fixed order — config → store → mcp → queue →
// backend → tools registry → skills → agent session → (lazy) scheduler — exposes a
// ToolContext factory, the main AgentSession, and drives both the CLI and the
// (future Bubble Tea) cockpit. Shutdown tears the dependencies down in reverse.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/asyncwork"
	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/daemon"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/prompts"
	"github.com/daintreehq/daintree-assistant/internal/queue"
	"github.com/daintreehq/daintree-assistant/internal/storage"
	"github.com/daintreehq/daintree-assistant/internal/tools"
	"github.com/daintreehq/daintree-assistant/internal/tools/mcpx"
	"github.com/daintreehq/daintree-assistant/internal/tools/scratchx"
	"github.com/daintreehq/daintree-assistant/internal/tools/terminalobs"
	"github.com/daintreehq/daintree-assistant/internal/workflowgraph"
)

// AppHooks are the swappable UI/REPL callbacks. SetHooks merges partial updates
// so the live session never rebuilds (which would drop history) — the closures in
// buildContext read these live.
type AppHooks struct {
	// Confirm approves a mutating action for the interactive main actor. A returned
	// error is treated as a DECLINE.
	Confirm func(ctx context.Context, req tools.ConfirmRequest) (bool, error)
	// AskChoice presents a multiple-choice question to the interactive main actor and
	// blocks until they answer (or the turn is cancelled). Wired only by interactive
	// surfaces (cockpit, classic REPL); nil elsewhere (one-shot, host), so the tool
	// reports that the runtime can't ask.
	AskChoice func(ctx context.Context, req tools.AskChoiceRequest) (tools.AskChoiceAnswer, error)
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
	// It returns true to authorise a reset (move the DB files aside as a backup +
	// rebuild the schema) or false to abort with the stale-schema error. Interactive
	// callers wire a terminal notice/prompt here; non-interactive callers leave it nil,
	// preserving the loud, actionable failure — a script/host path must never silently
	// destroy local state.
	OnSchemaStale func(have, want int) (bool, error)
	// OnSchemaReset, when non-nil, is invoked after an authorised stale-schema reset
	// has safely moved the old database aside, with the backup's main-file path (""
	// when there was no file to back up). Informational only — interactive callers
	// use it to tell the user where their previous state went.
	OnSchemaReset func(backupPath string)
	// DispatchActor overrides the actor identity the Session's tool dispatches run
	// under. Zero ⇒ ActorMain (every interactive path). The supervisor daemon sets
	// ActorWake so its unattended wake turns take dispatch's grant-or-blocked
	// branch (the actor id is then domain.WakeActorID).
	DispatchActor domain.ToolActor
	// ResumeCurrentSession (with an empty SessionID) resumes the project's
	// durable current-session pointer (runtime_state) instead of minting a fresh
	// session — the supervisor daemon's continuity mechanism: its autonomous
	// wake turns continue the SAME conversation the last cockpit ran. A missing
	// pointer still mints fresh. Ignored when SessionID is set explicitly.
	ResumeCurrentSession bool
}

// ToolBuilder returns the tools to register on the App's registry. It is a SEAM:
// the tool-family wiring wave registers the real families here; until then the
// default builds the safe core set so the module stays green. The App passes
// itself so a builder can reach config/store/mcp/queue/backend.
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
	Backend  backend.Backend
	Registry *tools.Registry

	SessionID string
	Session   *agent.Session

	runRef    *agent.RunIDRef
	scheduler *daemon.Scheduler

	// asyncCoordinator owns the runtime's async tool futures (terminal.run.async /
	// terminal.await.async): the 1s poll loop, sibling coalescing, and the
	// completion publish into the attention queue. Constructed always (the asyncx
	// tools capture it); STARTED only alongside the scheduler on interactive
	// paths, so a one-shot run cleanly rejects async work instead of stranding it.
	asyncCoordinator *asyncwork.Coordinator

	// workflowService is the workflow-intelligence graph layer (nil unless
	// DAINTREE_WORKFLOW_INTELLIGENCE=1). See workflowintel.go for the four seams
	// it feeds (graph tools, dispatch observer, async sink, turn digests).
	workflowService *workflowgraph.Service

	// scratchStore is the session-scoped, pure in-memory scratch workspace the
	// scratch.* tools drive. Initialized once per App (== once per session) before
	// the tool registry is built, and discarded with the App — nothing here persists
	// or leaks across sessions. Held on App (not Session) because the tool builder
	// runs before a.Session exists; the family captures this concrete store directly.
	scratchStore *scratchx.Store

	// terminalObs is the session-scoped settle memory (internal/tools/terminalobs):
	// the send wrappers (terminal.sendCommand / copyTree.injectToTerminal /
	// terminal.run.async) WRITE input-injection marks, the in-turn waits
	// (terminal.awaitAll / terminal.extract wait:{}) WRITE working observations and
	// READ the seenWorking seed — so a re-await of a since-finished agent settles on
	// its first poll instead of burning the 20s settle grace re-collecting evidence
	// a previous wait already had. In-memory, discarded with the App.
	terminalObs *terminalobs.Memory

	// ownership is the owner-boot reconciliation summary (what this process adopted
	// when it took the project DB over): resumed watchers/async, unpublished
	// completions, carried-over inbox. Immutable after Create; feeds the one-time
	// resumed-watchers session note and the attach-time UI surfaces.
	ownership storage.OwnershipSummary

	// dispatchActor / dispatchActorID are the actor identity the Session's tool
	// dispatches run under — ActorMain/"" for every interactive process, and
	// ActorWake/WakeActorID in the headless supervisor daemon so mutating tools
	// route through the grant-or-blocked branch instead of a confirm prompt no
	// human is present to answer. Immutable after Create.
	dispatchActor   domain.ToolActor
	dispatchActorID string

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

	// startupMu guards the typed Daintree snapshot fetched during the splash: project,
	// full effective agent catalog + toolbar state, and Daintree's current worktree.
	// refreshStartupContext publishes all three atomically after concurrent reads, so a
	// backend round sees either the previous complete snapshot or the new one, never a
	// half-refreshed mix. Project/agents feed the cacheable pre-conversation startup block;
	// worktree stays in the fresh tail. NEVER nest startupMu with cfgMu.
	// mcpLifecycleMu serializes the entire primary MCP connect/reconnect + discovery
	// publication. A fast first turn joins this gate instead of sampling `Connected=false`
	// while the splash handshake is still in flight and racing ahead with empty context.
	mcpLifecycleMu          sync.Mutex
	startupConnectAttempted bool // guarded by mcpLifecycleMu; cancelled launch attempts do not set it
	startupRefreshMu        sync.Mutex
	startupMu               sync.RWMutex
	cachedProject           *prompts.ProjectContext
	cachedAgents            *prompts.AgentRosterContext
	cachedWorktree          *prompts.WorktreeContext
	startupReady            bool
	startupGeneration       uint64

	// reconcileLedgerMu/done guard the boot ledger reconcile. `done` is committed only
	// after terminal.list returned a parseable inventory; launch cancellation or a transient
	// read failure stays retryable by the normal bootstrap. A completed attempt never reruns.
	reconcileLedgerMu   sync.Mutex
	reconcileLedgerDone bool

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
		// human — consult it; on a "yes" move the DB files aside as a timestamped
		// backup and re-Open onto a fresh schema. Without a handler (scripts / host /
		// non-TTY) the actionable error propagates untouched, so we never silently
		// destroy local state.
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
		// Even an authorised reset never DELETES state: the old database (plus
		// WAL/SHM) is moved into a timestamped backup directory beside it as ONE
		// all-or-nothing unit. If that backup fails, refuse to reset — and let
		// BackupDB's error state the on-disk truth, which differs per branch: on a
		// clean failure every file was rolled back to its original location, while
		// a failed rollback reports exactly which files remain in the backup dir.
		// Claiming "left untouched" here unconditionally would be a lie in the
		// second case, so the wrapper only says what is always true.
		backupPath, berr := storage.BackupDB(cfg.DBPath, stale.Have)
		if berr != nil {
			return nil, fmt.Errorf(
				"refusing to reset the stale database — backing it up failed: %w", berr)
		}
		if opts.OnSchemaReset != nil {
			opts.OnSchemaReset(backupPath)
		}
		if store, err = storage.Open(cfg.DBPath, nil); err != nil {
			return nil, err
		}
	}

	// Owner-boot reconciliation. The caller (cli/daemon runtime) holds the project
	// owner lock before Create, so this process is the DB's only owner: adopt the
	// live supervision a prior owner left behind (watchers keep running, async
	// futures keep polling, the inbox carries over) and clear only the dead spawn
	// sagas. The summary feeds the one-time resumed-watchers session note.
	ownership, err := store.BeginOwnership(domain.NowMS())
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("ownership boot: %w", err)
	}
	// Adopted watchers carry a PERSISTED `subscribed` latch whose subscription is
	// per-PROCESS in-memory state (mcp.Client.subs starts empty here), so it must be
	// cleared at the ownership boundary or an adopted watcher believes in a push
	// channel that will never deliver — and widens its poll to 30s on the strength
	// of it. Best-effort: never blocks boot. See daemon.ClearAdoptedSubscriptionLatches.
	_ = daemon.ClearAdoptedSubscriptionLatches(store)

	sessionID := opts.SessionID
	resumedSession := opts.SessionID != ""
	if sessionID == "" && opts.ResumeCurrentSession {
		// The supervisor daemon continues the project's current conversation: read
		// the durable pointer the last interactive owner wrote. Best-effort — a
		// missing/unreadable pointer just mints fresh below.
		if v, err := store.GetRuntimeState(storage.RuntimeKeyCurrentSession); err == nil && v != "" {
			sessionID = v
			resumedSession = true
		}
	}
	if sessionID == "" {
		sessionID = domain.NewID("ses_")
	}

	// Non-main dispatch actors carry the well-known wake actor id (grants are
	// keyed to it); the interactive default stays main/"".
	dispatchActor := opts.DispatchActor
	if dispatchActor == "" {
		dispatchActor = domain.ActorMain
	}
	dispatchActorID := ""
	if dispatchActor == domain.ActorWake {
		dispatchActorID = domain.WakeActorID
	}

	baseCtx, baseCancel := context.WithCancel(context.Background())
	a := &App{
		Config:          cfg,
		Store:           store,
		SessionID:       sessionID,
		runRef:          &agent.RunIDRef{},
		hooks:           opts.Hooks,
		baseCtx:         baseCtx,
		baseCancel:      baseCancel,
		InitialTier:     cfg.Tier,
		ownership:       ownership,
		dispatchActor:   dispatchActor,
		dispatchActorID: dispatchActorID,
		// A fresh, empty scratch workspace for this session. Built before the tool
		// registry so the scratch.* family can capture the concrete store directly.
		scratchStore: scratchx.NewStore(),
		terminalObs:  terminalobs.NewMemory(),
	}

	// mcp → docs-mcp → queue → router → registry → skills.
	//
	// The primary client's drift baseline is INJECTED from the typed-wrapper denylist
	// rather than hand-listed inside internal/mcp: those are the raw host tool names
	// this build actually depends on, so a missing one is a real breakage, and the
	// list cannot go stale because a wrapper cannot exist without its entry. (The
	// wiring has to happen here because internal/mcp importing internal/tools/mcpx
	// would be an import cycle.) Retry classification is NOT listed anywhere — it is
	// read live from the annotations the host already ships on tools/list.
	a.MCP = mcp.New(cfg, mcp.Options{
		ClientOverride: opts.MCPClientOverride,
		DriftBaseline:  mcpx.WrappedMCPToolNames(),
	})
	// The Daintree documentation MCP — a public, no-auth, stateless HTTP server that
	// answers "how do I use Daintree" help questions via live doc search. Its endpoint is
	// a fixed product URL (DAINTREE_DOCS_MCP_URL overrides it for dev/test, mirroring the
	// DAINTREE_BACKEND_URL pattern). Anonymous (no bearer) with its own short drift
	// baseline so it never warns that the Daintree control-plane tools are "missing".
	// It also passes that list as ReadOnlyFallback: this third-party server is not
	// guaranteed to send MCP annotations, and its three tools are pure documentation
	// reads that would otherwise lose auto-retry. The fallback applies only where the
	// server declared nothing — any annotations it does send win outright.
	// Connected in parallel with the primary MCP during the boot splash (ConnectMcp).
	docsURL := mcp.DefaultDocsURL
	if v := strings.TrimSpace(os.Getenv("DAINTREE_DOCS_MCP_URL")); v != "" {
		docsURL = v
	}
	a.DocsMCP = mcp.New(cfg, mcp.Options{
		URL:              &docsURL,
		Anonymous:        true,
		DriftBaseline:    mcp.DocsToolNames,
		ReadOnlyFallback: mcp.DocsToolNames,
		ClientOverride:   opts.DocsMCPClientOverride,
	})
	a.Queue = queue.New(queueEventStore{s: store}, domain.NowMS)
	// The native Daintree backend: the assistant turn engine + server-owned utility
	// tasks. The CLI no longer talks to DeepSeek directly — the backend owns the model
	// credentials, prompt assembly, and skill selection.
	//
	// Endpoint + API key are resolved by config.LoadConfig (DAINTREE_BACKEND_URL
	// env escape hatch → persisted /login credentials → production default), and
	// the key is paired to the persisted endpoint so a dev/test override never
	// receives the real key. The first-run login flow and /login live in
	// internal/cli; here the resolved values are simply wired into the client.
	if opts.BackendOverride != nil {
		a.Backend = opts.BackendOverride
	} else {
		dbg := debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir}
		clientCfg := backend.ClientConfig{
			BaseURL: cfg.BackendURL,
			APIKey:  cfg.BackendAPIKey,
			ClientInfo: backend.ClientInfo{
				Name:     "daintree-cli",
				Platform: runtime.GOOS,
			},
			// Surface each transient-failure retry to the session log — otherwise a
			// retried turn leaves no trace and a later log read dead-ends at the last
			// successful tool call (the gap that hid the wild 502).
			OnRetry: func(info backend.RetryInfo) {
				debuglog.LogDebug(dbg, "backend.retry", map[string]any{
					"op":          info.Op,
					"attempt":     info.Attempt,
					"maxAttempts": info.MaxAttempts,
					"delayMs":     info.Delay.Milliseconds(),
					"error":       info.Err.Error(),
				})
			},
		}
		// Every utility-task round trip lands in the session log. Without this the
		// tasks were the one backend surface the trace couldn't see — a /compact's
		// checkpoint + memory_distill calls left literally zero log lines (observed
		// 2026-07-12), so compaction archaeology was impossible. Wired ONLY when debug
		// logging is on (same rule as the Session trace seam): the hook re-serializes
		// the task input to measure it, and that must cost nothing in a normal run.
		if cfg.DebugLog {
			clientCfg.OnTask = func(info backend.TaskTraceInfo) {
				fields := map[string]any{
					"task":        info.Task,
					"durationMs":  info.Duration.Milliseconds(),
					"inputBytes":  info.InputBytes,
					"outputBytes": info.OutputBytes,
					"ok":          info.Err == nil,
				}
				if info.Err != nil {
					fields["error"] = info.Err.Error()
				}
				debuglog.LogDebug(dbg, "backend.task", fields)
			}
		}
		a.Backend = backend.NewClient(clientCfg)
	}
	// The async coordinator is built BEFORE the tool registry (the asyncx family
	// captures it) and started later, alongside the scheduler (StartScheduler).
	// Its Notify hook pushes a fresh completion to the scheduler's delivery path
	// immediately; a.scheduler is set before the coordinator ever starts ticking,
	// and NotifyNow serializes with the tick's own notify.
	asyncTrace := func(event string, fields map[string]any) {
		debuglog.LogDebug(debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir}, event, fields)
	}

	// Workflow-intelligence service (nil when the flag is off). Built BEFORE the
	// coordinator so async completions can route back to graph nodes, and before
	// the registry so the tool builder can capture it; its registry-dependent
	// closures deref a.Registry lazily (see workflowintel.go).
	a.workflowService, err = a.buildWorkflowService()
	if err != nil {
		baseCancel()
		_ = store.Close()
		return nil, err
	}
	var workflowSink asyncwork.WorkflowSink
	if a.workflowService != nil {
		workflowSink = a.workflowService
	}

	a.asyncCoordinator = asyncwork.New(asyncwork.Deps{
		Reader:       asyncStatusReaderAdapter{r: terminalReaderAdapter{c: a.MCP}},
		Queue:        a.Queue,
		Store:        store,
		WorkflowSink: workflowSink,
		// Ownership-boot adoption: when the coordinator starts it re-tracks the
		// persisted live invocations a prior owner (cockpit or supervisor daemon)
		// left polling, and retries any finalized-but-unpublished completion.
		AdoptLister: store,
		Notify: func() {
			if s := a.scheduler; s != nil {
				s.NotifyNow()
			}
		},
		Trace: asyncTrace,
	})

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

	// Workflow-intelligence dispatch observer: project completed tool calls into
	// graph evidence/resource links. Installed AFTER the registry is fully built
	// and asserted; a pure after-the-fact side-channel (panic-guarded in the
	// registry) that can never alter dispatch behaviour.
	if a.workflowService != nil {
		a.Registry.SetDispatchObserver(workflowObserverAdapter{svc: a.workflowService})
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
		Tools:              newToolRunner(a),
		Store:              store,
		MemoryStore:        store,
		MemoryRecaller:     memoryRecallerAdapter{s: store},
		PinnedMemoryLister: pinnedMemoryListerAdapter{s: store},
		// One-time resumed-watchers note. Worktree data now travels through the typed
		// request.runtime.worktree field; activeWorktreeForFooter remains available to
		// workflow intelligence but is no longer a Session dependency.
		ResumedWatchers:   a.resumedWatchersForFooter,
		ArtifactPersister: store,
		WorkflowRunLister: store,
		// Durable mirror + seed for the opaque backend state token, so a session
		// handed over between processes (cockpit ↔ supervisor daemon) keeps the
		// backend's skill-selection cadence. Seeded only on a genuine resume: a
		// fresh session id has no persisted token.
		BackendStateStore:   store,
		InitialBackendState: loadPersistedBackendState(store, sessionID, resumedSession),
		// Live async futures for the turn context's async-operations block, re-read
		// every round so the model sees (and never re-issues) its in-flight work.
		AsyncInvocationLister: store,
		// Open workflow-graph digests for the turn context's workflow_state block
		// (nil unless DAINTREE_WORKFLOW_INTELLIGENCE=1 — the wire then stays
		// byte-identical to the pre-feature request).
		WorkflowDigestLister: a.workflowDigestLister(),
		// Per-turn open-terminal inventory (issue #286): a fresh terminal.list +
		// no-output terminal.getStatus snapshot attached to the runtime block so the model
		// sees the live roster as inert data instead of discovering it mid-turn. Best-effort
		// and bounded; reuses the same MCP read adapter as the extraction/id-resolution path.
		OpenTerminalsFetcher:   terminalReaderAdapter{c: a.MCP}.FetchOpenTerminals,
		PromptContext:          a.PromptContext(),
		EnsureStartupContext:   a.ensureStartupForTurn,
		CurrentWorktreeFetcher: a.refreshCurrentWorktree,
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

// loadPersistedBackendState loads the durable backend state token for a RESUMED
// session (resumed == the caller passed an explicit session id, i.e. it intends
// to continue an existing transcript). A fresh session id never has a token, so
// the read is skipped — and a stale token from an unrelated prior session can
// never leak into a new conversation. Best-effort: any read failure just means
// the backend re-runs skill selection.
func loadPersistedBackendState(store *storage.Store, sessionID string, resumed bool) string {
	if !resumed {
		return ""
	}
	token, err := store.GetSessionBackendState(sessionID)
	if err != nil {
		return ""
	}
	return token
}

// Ownership returns the owner-boot reconciliation summary captured at Create.
func (a *App) Ownership() storage.OwnershipSummary { return a.ownership }

// turnActor returns the actor identity the Session's tool dispatches run under
// (immutable after Create — see CreateOptions.DispatchActor).
func (a *App) turnActor() (domain.ToolActor, string) {
	return a.dispatchActor, a.dispatchActorID
}

// AdoptAsCurrentSession durably marks this App's session as the project's
// current conversation — the one a detached supervisor daemon continues with
// autonomous wake turns. Interactive paths (cockpit, classic REPL, host) call
// it once after Create; one-shot and doctor runs deliberately do NOT, so a
// script probe never hijacks the conversation the daemon is supervising.
func (a *App) AdoptAsCurrentSession() {
	// Best-effort: continuity is a convenience, never worth failing boot over.
	_ = a.Store.PutRuntimeState(storage.RuntimeKeyCurrentSession, a.SessionID)
}

// DetachedActivity is what the supervisor daemon accomplished while no
// assistant was attached, persisted in runtime_state and consumed by the next
// attach for the "while you were away" notice.
type DetachedActivity struct {
	WakeTurns        int    `json:"wakeTurns"`
	LastWakeAtMs     int64  `json:"lastWakeAtMs"`
	LastReplyPreview string `json:"lastReplyPreview"`
}

// detachedReplyPreviewMax bounds the stored reply preview (a notice line, not
// a transcript).
const detachedReplyPreviewMax = 300

// RecordDetachedWake accumulates one successful autonomous wake turn into the
// durable detached-activity record (the supervisor daemon calls this after
// each wake). Best-effort — never disturbs the wake path.
func (a *App) RecordDetachedWake(reply string) {
	act := DetachedActivity{}
	if raw, err := a.Store.GetRuntimeState(storage.RuntimeKeyDetachedActivity); err == nil && raw != "" {
		_ = json.Unmarshal([]byte(raw), &act)
	}
	act.WakeTurns++
	act.LastWakeAtMs = domain.NowMS()
	preview := strings.TrimSpace(reply)
	if idx := strings.IndexByte(preview, '\n'); idx >= 0 {
		preview = preview[:idx]
	}
	if len(preview) > detachedReplyPreviewMax {
		preview = preview[:detachedReplyPreviewMax] + "…"
	}
	act.LastReplyPreview = preview
	if b, err := json.Marshal(act); err == nil {
		_ = a.Store.PutRuntimeState(storage.RuntimeKeyDetachedActivity, string(b))
	}
}

// AttachSummaryLines composes the one-time "while you were away" notice for an
// attaching assistant: the daemon's detached wake activity (consumed — read
// then cleared, so it prints at most once) plus what this process adopted at
// ownership boot. Empty when there is nothing worth saying.
func (a *App) AttachSummaryLines() []string {
	var lines []string
	if raw, err := a.Store.GetRuntimeState(storage.RuntimeKeyDetachedActivity); err == nil && raw != "" {
		var act DetachedActivity
		if json.Unmarshal([]byte(raw), &act) == nil && act.WakeTurns > 0 {
			plural := "s"
			if act.WakeTurns == 1 {
				plural = ""
			}
			line := fmt.Sprintf("While you were away: the supervisor handled %d event%s", act.WakeTurns, plural)
			if act.LastReplyPreview != "" {
				line += " — " + act.LastReplyPreview
			}
			lines = append(lines, line)
		}
		_ = a.Store.DeleteRuntimeState(storage.RuntimeKeyDetachedActivity)
	}
	own := a.ownership
	if n := len(own.ResumedWatcherTitles); n > 0 || own.ResumedAsyncCount > 0 {
		parts := []string{}
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%d watcher(s)", n))
		}
		if own.ResumedAsyncCount > 0 {
			parts = append(parts, fmt.Sprintf("%d async operation(s)", own.ResumedAsyncCount))
		}
		lines = append(lines, "Resumed supervision of "+strings.Join(parts, " and ")+" from the previous session.")
	}
	if own.OpenAttentionCount > 0 {
		lines = append(lines, fmt.Sprintf("%d attention item(s) carried over — see /inbox.", own.OpenAttentionCount))
	}
	return lines
}
