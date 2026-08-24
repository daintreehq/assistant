// Package app is the single composition root. App.Create
// builds every dependency once in a fixed order — config → store → mcp → queue →
// backend → tools registry → runbooks → agent session → (lazy) scheduler — exposes a
// ToolContext factory, the main AgentSession, and drives both the CLI and the
// (future Bubble Tea) attached session. Shutdown tears the dependencies down in reverse.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/asyncwork"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/costledger"
	"github.com/daintreehq/assistant/internal/daemon"
	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/models"
	"github.com/daintreehq/assistant/internal/prompts"
	"github.com/daintreehq/assistant/internal/queue"
	"github.com/daintreehq/assistant/internal/redact"
	"github.com/daintreehq/assistant/internal/storage"
	"github.com/daintreehq/assistant/internal/tools"
	"github.com/daintreehq/assistant/internal/tools/scratchx"
	"github.com/daintreehq/assistant/internal/tools/terminalobs"
	"github.com/daintreehq/assistant/internal/workflowgraph"
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
	// surfaces (attached session, line REPL); nil elsewhere (one-shot, host), so the tool
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
	// wake turns continue the SAME conversation the last attached session ran. A missing
	// pointer still mints fresh. Ignored when SessionID is set explicitly.
	ResumeCurrentSession bool
	// PinnedRunbookIDs names backend runbooks every turn of this session must load
	// (`--runbook`, or `runbooks` on daintree.session.open). Deliberately NOT a config
	// value: it has no environment source and is a per-session execution control like
	// Timeout, so routing it through config.AppConfig would also leak it into the
	// supervisor's wake turns, which nobody asked to pin.
	//
	// Normalized on the way in. Naming pins here does NOT negotiate them — the caller
	// must run App.PreparePinnedRunbooks before the first turn, which is where an
	// unsupported backend or a mistyped id becomes a loud failure.
	PinnedRunbookIDs []string
}

// ToolBuilder returns the tools to register on the App's registry. It is a SEAM:
// the tool-family wiring wave registers the real families here; until then the
// default builds the safe core set so the module stays green. The App passes
// itself so a builder can reach config/store/mcp/queue/backend.
type ToolBuilder func(a *App) ([]*tools.Tool, error)

// App is the composition root. Fields are effectively read-only after Create
// except Config.Tier (mutated by /permissions, under cfgMu), Hooks (merged by
// SetHooks), and scheduler (set lazily by StartScheduler).
type App struct {
	Config config.AppConfig
	Store  *storage.Store
	MCP    *mcp.Client
	Queue  *queue.Queue
	// Backend is the native Daintree backend — the assistant turn engine and the
	// server-owned utility tasks. It replaces the direct model provider. Held as an
	// interface so tests can inject a fake (CreateOptions.BackendOverride).
	//
	// It is ALWAYS the *backend.Swappable below. Consumers capture this value and keep
	// it for the App's lifetime; replacing the live client is a delegate swap, so nothing
	// downstream has to be re-wired or can go on holding a dead endpoint.
	Backend  backend.Backend
	Registry *tools.Registry

	// CostLedger accumulates what this process has spent on the caller's own upstream
	// key — turns and utility tasks alike. It is wired into the backend client's OnCost
	// hook and deliberately OUTLIVES that client, so anything that rebuilds the client
	// mid-session cannot reset the bill to zero. Surfaced by `/cost` and `/doctor`.
	CostLedger *costledger.Ledger

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
	// unless DAINTREE_WORKFLOW_INTELLIGENCE=0). See workflowintel.go for the four seams
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

	// cfgMu guards Config — today only Tier (/permissions) is mutated at runtime.
	// buildContext copies the WHOLE Config and PromptContext reads it on agent/tool
	// goroutines, so every runtime write must be serialized against them or the race
	// detector flags a torn read. Any future runtime-mutable field belongs under this
	// lock too.
	cfgMu sync.RWMutex

	// switchMu serializes a WHOLE endpoint switch. cfgMu guards one config access and
	// Swappable guards one delegate access, but a switch is three writes (delegate,
	// config, disk) and nothing made them one transaction: two concurrent switches could
	// interleave and leave the live delegate on one endpoint, App.Config on another, and
	// the stored preference on a third, permanently. Held across resolve → gate → swap →
	// publish → persist. NEVER nest cfgMu outside this.
	switchMu sync.Mutex

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

	// display is the surface's live render geometry, published by the front end that
	// owns the terminal — today only the attached session, which measures at boot and on every
	// resize. An atomic POINTER, not two counters: the pair must move together, so a
	// turn building its runtime context reads one coherent geometry rather than a new
	// column count against a stale content width. It stays nil everywhere the reply is
	// not wrapped by us (the line REPL streams raw tokens the host wraps itself, a
	// piped one-shot, the stdio host, the headless daemon) — the backend is told
	// "unknown" and picks its own default rather than being handed a fabricated 80x24.
	display atomic.Pointer[prompts.DisplayContext]

	// backendCaps is the last capability descriptor a backend answered with, cached
	// because it is fetched once at boot and then read per turn. nil until a handshake
	// succeeds, and a capability read fails CLOSED (see backendAcceptsDisplayContext).
	//
	// It is PINNED TO THE ENDPOINT that answered, because the delegate can be swapped
	// underneath a fetch that is still in flight: a slow boot handshake completing after
	// a swap would otherwise publish the OLD deployment's answer as if it described the
	// new one, and a gate opened on that lie sends a field the new backend rejects —
	// 422ing every subsequent turn, permanently. Readers compare the pin against the live
	// endpoint, so a mismatched answer is simply not believed.
	backendCaps atomic.Pointer[backendCapsSnapshot]

	// pinnedRunbookIDs are the runbooks the LAUNCH named (`--runbook`, or `runbooks` on
	// daintree.session.open) and every turn of this session must load. Immutable after
	// Create: argv and the session-open arguments are both session-constant, so there
	// is no writer and callers get copies (see PinnedRunbookIDs). Nil on every ordinary
	// launch, which is what keeps the capability preflight off the normal boot path.
	pinnedRunbookIDs []string

	// reconcileLedgerMu/done guard the boot ledger reconcile. `done` is committed only
	// after terminal.list returned a parseable inventory; launch cancellation or a transient
	// read failure stays retryable by the normal bootstrap. A completed attempt never reruns.
	reconcileLedgerMu   sync.Mutex
	reconcileLedgerDone bool
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

// SetDisplaySize publishes the front end's live render geometry: `columns` is the
// terminal, `contentWidth` the measure the assistant's own text is wrapped at (the
// attached session's content width, which is narrower than the terminal and capped). Called
// from the UI's Update loop on every resize while turns read it on their own
// goroutines, hence the atomic pointer.
//
// A non-positive contentWidth CLEARS the geometry rather than publishing a nonsense
// one: "we can't measure this surface" and "the reply renders in 0 columns" are
// different claims, and only the first is ever true.
func (a *App) SetDisplaySize(columns, contentWidth int) {
	if contentWidth <= 0 {
		a.display.Store(nil)
		return
	}
	if columns < 0 {
		columns = 0
	}
	// Text cannot render wider than the window it renders in. A caller that reports
	// otherwise measured one of the two wrong, and the terminal is the value to trust —
	// the content width is derived from it. (columns == 0 means the window itself was
	// not measured, which is a different, permitted state.)
	if columns > 0 && contentWidth > columns {
		contentWidth = columns
	}
	a.display.Store(&prompts.DisplayContext{Columns: columns, ContentWidth: contentWidth})
}

// DisplaySize returns the last published geometry, or nil when no front end has
// measured a terminal. The returned pointer addresses an immutable value — callers
// must never mutate it in place; publish a new one instead.
func (a *App) DisplaySize() *prompts.DisplayContext { return a.display.Load() }

// backendCapsSnapshot is one capability descriptor together with the endpoint that
// answered it. The pairing is the point: a descriptor alone cannot say WHICH backend
// it describes, and this session's backend client can be replaced mid-flight.
type backendCapsSnapshot struct {
	baseURL string
	caps    backend.Capabilities
}

// BackendCapabilities fetches the live backend's capability descriptor and caches it
// for the per-turn readers. Every capability fetch should come through here so the
// cache reflects the backend actually in use; a failed fetch leaves the previous
// answer alone and returns the error, since "we could not ask" is not evidence that
// anything changed.
//
// The endpoint is read BEFORE the call and stored with the answer, so a descriptor that
// arrives after an endpoint change is filed under the endpoint it actually came from and
// the readers below discard it.
func (a *App) BackendCapabilities(ctx context.Context) (backend.Capabilities, error) {
	if a.Backend == nil {
		return backend.Capabilities{}, errors.New("no backend client")
	}
	// Capture the delegate ONCE. Reading BaseURL() and Capabilities() through two
	// separate atomic loads of a Swappable would let a /backend switch land between
	// them, and the answer from the NEW endpoint would then be filed under the OLD
	// one — the exact confusion the pin exists to prevent, arrived at from the other
	// direction. Pinning the delegate makes "the endpoint that answered" literally true.
	client := a.Backend
	if sw, ok := client.(*backend.Swappable); ok {
		client = sw.Current()
	}
	asked := client.BaseURL()
	caps, err := client.Capabilities(ctx)
	if err != nil {
		return backend.Capabilities{}, err
	}
	a.backendCaps.Store(&backendCapsSnapshot{baseURL: asked, caps: caps})
	return caps, nil
}

// backendAcceptsDisplayContext reports whether it is SAFE to attach the terminal
// geometry to a request. Fails closed on an unknown backend, and on one whose cached
// answer came from a DIFFERENT endpoint than the one about to be called: `runtime` is
// validated with extra="forbid", so guessing wrong costs the entire turn, while
// withholding the block costs only the width-aware wording of one reply.
func (a *App) backendAcceptsDisplayContext() bool {
	snap := a.backendCaps.Load()
	if snap == nil || a.Backend == nil {
		return false
	}
	return snap.baseURL == a.Backend.BaseURL() && snap.caps.Respond.DisplayContext
}

// backendContextCompaction reports whether it is safe to ACCEPT a compacted context
// block from the endpoint about to be called, and hands back the descriptor it was
// judged against (the block's byte cap rides on it).
//
// Same endpoint-pinned, fail-closed shape as the two gates above — but for a different
// reason, and the difference is worth stating. Those two protect a request: guessing
// wrong there 422s the whole turn. This one protects HISTORY. A block is applied by
// splicing it over a span of the client's own conversation, using integers the server
// chose; a deployment whose contract this client has not verified could name a span
// meaning something else, and the damage would be silent and permanent. So the check is
// ReplayCompatible (the whole advertised contract), not merely `enabled` — and an
// unknown backend, a stale answer from a different endpoint, or a single field this
// client does not recognise all read as "not supported", which is simply today's
// behaviour: full history, every round.
//
// KNOWN LIMIT, and deliberate. It reads only what an EXPLICIT handshake left behind —
// the attached session's boot fetch, /doctor, /routing, or a pinned-runbook negotiation. A classic
// REPL, a one-shot, the embedded host, the MCP server and the supervisor daemon perform
// none of those, so compaction stays off there, and it stays off after a /backend switch
// until something negotiates with the new endpoint. That is invisible and costs only
// prompt savings, which is why it is tolerable while every deployment ships
// context_compaction disabled — but it does mean the feature is not yet live on the
// long-running surfaces it was built for.
//
// Fixing it means an explicit negotiation step per surface, NOT a boot-time fetch here:
// TestPreparePinnedRunbooksMakesNoCallWithoutPins pins the cost contract that an ordinary
// launch adds no round trip, and a detached warm-up also races the first turn and any
// concurrent switch. That is its own change, with its own decisions to make.
func (a *App) backendContextCompaction() (backend.ContextCompactionCaps, bool) {
	snap := a.backendCaps.Load()
	if snap == nil || a.Backend == nil || snap.baseURL != a.Backend.BaseURL() {
		return backend.ContextCompactionCaps{}, false
	}
	if !snap.caps.ContextCompaction.ReplayCompatible() {
		return backend.ContextCompactionCaps{}, false
	}
	return *snap.caps.ContextCompaction, true
}

// Tier returns the current permission tier under the read lock.
func (a *App) Tier() domain.Tier {
	a.cfgMu.RLock()
	defer a.cfgMu.RUnlock()
	return a.Config.Tier
}

// snapshotConfig returns a consistent copy of Config under the read lock, so a
// caller building a per-turn ToolContext can't observe a torn Tier write.
// SnapshotConfig returns a consistent copy of the resolved config. EVERY whole-Config
// read outside this package must come through here: Config is an exported struct whose
// Tier is mutated at runtime, so a direct `a.Config` copy races SetTier.
func (a *App) SnapshotConfig() config.AppConfig { return a.snapshotConfig() }

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

	// Register this process's real credentials for redaction, FIRST — before anything can
	// log. Shape matching alone is not enough for either: an account credential need
	// match no known pattern, and the Daintree MCP token has no fixed format, while both
	// are among the most expensive values in the process to leak (one funds model calls,
	// the other authorises system-tier Daintree actions for its validity window).
	// cfg.APIKey is normally EMPTY — RegisterSecret ignores that — and registering it
	// unconditionally means the rare install that exports DAINTREE_API_KEY is protected
	// without a second code path. Registration is additive, so an MCP reconnect adds the
	// new value without un-protecting the old one, which is correct: a log written
	// before the refresh still contains it.
	redact.RegisterSecret(cfg.APIKey)
	redact.RegisterSecret(cfg.McpToken)

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
		// Normalized once, here, so every consumer (the preflight, the Session, the
		// MCP facts) sees the same trimmed, deduplicated, order-preserving list.
		pinnedRunbookIDs: NormalizePinnedRunbookIDs(opts.PinnedRunbookIDs),
		// A fresh, empty scratch workspace for this session. Built before the tool
		// registry so the scratch.* family can capture the concrete store directly.
		scratchStore: scratchx.NewStore(),
		terminalObs:  terminalobs.NewMemory(),
	}

	// mcp → queue → router → registry → runbooks.
	a.MCP = mcp.New(cfg, mcp.Options{ClientOverride: opts.MCPClientOverride})
	a.Queue = queue.New(queueEventStore{s: store}, domain.NowMS)
	// The native Daintree backend: the assistant turn engine + server-owned utility
	// tasks. The CLI no longer talks to DeepSeek directly — the backend owns the model
	// credentials, prompt assembly, and runbook selection.
	//
	// The endpoint comes from the resolved config (internal/config) — app.Create never
	// reads the environment itself, so every entry point (attached session, one-shot, host,
	// supervisor daemon) is configured identically. There is no credential to resolve
	// on the normal path: the backend holds its own upstream key and serves a request
	// with no Authorization header. cfg.APIKey is set only when DAINTREE_API_KEY named
	// one, and then it overrides the backend's own for this session's calls.
	//
	// The client is wrapped in a backend.Swappable and that wrapper — never the raw
	// client — is what every consumer captures, so Session, the watcher engine, the
	// async coordinator and the workflow layer can never end up holding a stale
	// endpoint. Nothing swaps today; the wrapper is kept because account sign-in is
	// being built next and re-authenticating in place is a delegate swap through this
	// seam rather than a re-wiring of every consumer. Test overrides are wrapped too,
	// so there is one code path.
	// Built before the client, which captures its Record method as the OnCost hook.
	a.CostLedger = costledger.New()
	if opts.BackendOverride != nil {
		a.Backend = backend.NewSwappable(opts.BackendOverride)
	} else {
		a.Backend = backend.NewSwappable(backend.NewClient(backendClientConfig(cfg, a.CostLedger)))
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
		// persisted live invocations a prior owner (attached session or supervisor daemon)
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

	// Runbooks are SERVER-OWNED: the backend's selector picks and injects runbooks. The
	// CLI no longer loads a local runbook catalog (no embedded runbook files, no
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
		// handed over between processes (attached session ↔ supervisor daemon) keeps the
		// backend's runbook-selection cadence. Seeded only on a genuine resume: a
		// fresh session id has no persisted token.
		BackendStateStore:   store,
		InitialBackendState: loadPersistedBackendState(store, sessionID, resumedSession),
		// Caller-named runbooks + the gate that decides whether they may reach the wire.
		// The gate is a func, not a bool, because the capability answer is pinned to the
		// endpoint that gave it and the delegate behind Backend can be replaced: reading
		// it per round is what keeps a swapped endpoint from being sent a field it
		// forbids. Nil pins make both inert.
		PinnedRunbookIDs:               a.PinnedRunbookIDs(),
		BackendAcceptsPinnedRunbookIDs: a.backendAcceptsPinnedRunbookIDs,
		BackendContextCompaction:       a.backendContextCompaction,
		// Live async futures for the turn context's async-operations block, re-read
		// every round so the model sees (and never re-issues) its in-flight work.
		AsyncInvocationLister: store,
		// Open workflow-graph digests for the turn context's workflow_state block
		// (nil when DAINTREE_WORKFLOW_INTELLIGENCE=0 — the wire then stays
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
// the backend re-runs runbook selection.
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
// autonomous wake turns. Interactive paths (attached session, line REPL, host) call
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
