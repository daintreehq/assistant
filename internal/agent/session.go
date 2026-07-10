package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
)

// coreToolNames are the essential tools asserted to be registered at boot
// (app.go's AssertRegistered("core tools")). EVERY turn now offers the FULL registry
// — a loaded skill never narrows the toolset (see buildToolFilterLocked) — so this is
// no longer a per-turn projection key; it is just the always-must-exist set. Internal
// dotted names.
var coreToolNames = []string{
	"context.snapshot",
	"fs.read",
	"fs.list",
	"fs.search",
	"queue.digest",
	"daintree.status",
	"tool.search",
	"terminal.read",
	"terminal.extract",
	// terminal.awaitAll is core: waiting for a spawned cohort to finish is a
	// fundamental in-turn orchestration step and the base prompt names it as always
	// available. Read-only, so no confirmation gate. (The full registry is offered every
	// turn now — "core" means asserted-to-exist at boot, not a per-turn projection key.)
	"terminal.awaitAll",
	// terminal.sendCommand is core: talking to a running agent (relaying between
	// agents, answering a question it waits on) is a fundamental operation the base
	// prompt advertises as always available, so it must be asserted-to-exist at boot.
	// The per-call confirmation/tier gate still governs it.
	"terminal.sendCommand",
	// terminal.close is core for the same reason: retiring an agent terminal you spawned
	// is a fundamental cohort-lifecycle operation, and the base prompt tells the model it
	// is "always here — don't tool.search for it", so it must be asserted-to-exist at
	// boot. The per-call confirm/tier gate still governs it. (terminal.kill — permanent
	// delete — stays behind daintree.call.)
	"terminal.close",
	"skill.step.advance",
	"skill.run.get",
	"memory.recall",
	"memory.list",
	"artifact.read",
}

// CoreToolNames returns a copy of the always-offered core tool names.
func CoreToolNames() []string {
	out := make([]string, len(coreToolNames))
	copy(out, coreToolNames)
	return out
}

// ErrTurnInProgress is returned by Send when a turn is already running (the
// single-flight guard). Concurrent sends are a wiring bug — one app, one session.
var ErrTurnInProgress = errors.New("agent: a turn is already in progress")

// Session is the turn engine (was AgentSession). It runs one user/autonomous turn
// to completion, owns the live model history (user/assistant/tool only — no client
// control prefix; skill selection is server-owned), conversation persistence, the
// opaque backend state token, and the event stream.
type Session struct {
	deps SessionDeps

	// mu guards ALL mutable session state below (messages, seq, backendState,
	// inFlight) — the turn goroutine and concurrent UI slash commands
	// (Clear/Compact, Messages, Artifacts) both touch it, so every access
	// is under this lock. Critical sections are kept SHORT: the streaming turn reads
	// an immutable SNAPSHOT of messages under the lock and releases it before the
	// (long) model stream — it never holds the lock across a network call.
	mu       sync.Mutex
	inFlight bool // a turn is running (single-flight guard + mutate-mid-turn gate)

	messages  []models.ChatMessage // live visible history (user/assistant/tool; begins at index 0)
	seq       int                  // next DB seq to write (monotonic)
	events    EventSink
	artifacts *ArtifactStore
	runRef    *RunIDRef

	// backendState is the opaque, server-signed skill-state token returned in the
	// stream's meta event. The CLI NEVER inspects, signs, or mutates it — it stores
	// the latest token and replays it on the next /respond request (a missing token
	// is valid for a new session, and just makes the backend re-run skill selection).
	// Guarded by s.mu (the meta callback writes it; the next round reads it).
	backendState string
	// catalogRevision / promptVersion are the backend's last-reported version markers
	// (informational; surfaced for diagnostics, never sent back). Guarded by s.mu.
	catalogRevision string
	promptVersion   string

	// compactFailures counts CONSECUTIVE small-model auto-compact summary failures
	// (guarded by s.mu). It arms the lossy-truncation fallback once it reaches
	// AutoCompactFailureThreshold AND the history is over the hard ceiling; ANY
	// successful compaction resets it to 0 (a single transient outage must not
	// permanently disable the soft, model-summarized path).
	compactFailures int

	// lastPromptTokens is the provider-reported prompt_tokens for the LARGE (main-thread)
	// tier on the most recent round (guarded by s.mu, stashed in emitUsage). It is the
	// REAL context size — it counts the ~68 tool schemas sent on every request, which the
	// chars/4 estimate is blind to — so maybeAutoCompact gates on it (max'd with the live
	// estimate). Large-tier ONLY, never the cross-tier aggregate: the aggregate folds in
	// small-tier background work, including the auto-compact summary call that runs
	// against the pre-compaction history, which would re-trip the trigger right after a
	// compaction. 0 is the "no provider figure yet" sentinel (DeepSeek never reports 0
	// on a successful call), so the first check (and any during a small-model outage)
	// uses the estimate alone. Zeroed on every history reset (compactLocked/clearLocked/
	// truncateLocked) so a stale pre-reset figure can't re-trip the trigger on the
	// freshly-shrunk history.
	lastPromptTokens int

	// compactionDepth counts how many times this session has compacted (guarded by
	// s.mu). Incremented in compactLocked and embedded in the persisted note prefix so
	// a summary-of-summary chain — each pass re-flattening the prior note — is visible
	// rather than silently degrading detail over a long run. Reset to 0 on /clear (the
	// chain is destroyed). Session-scoped only: it does NOT survive a restart (durable
	// depth tracking is the Wave-3 structured-checkpoint work). See observability.go.
	compactionDepth int

	// toolFailures is the session-cumulative count of failed tool calls keyed by
	// internal tool name (guarded by s.mu, lazy-init). DISTINCT from runToolBatch's
	// per-turn circuit-breaker map: this one accumulates across rounds as a coarse
	// "which tools keep failing" signal, off the audit path. See observability.go.
	toolFailures map[string]int

	// wg tracks detached background work (the post-compaction distill goroutine) so
	// App.Shutdown and tests can DrainBackgroundWork() before the deps it touches
	// (Router/MemoryStore) are torn down. sync.WaitGroup is goroutine-safe — no lock.
	wg sync.WaitGroup

	// draining (guarded by s.mu) is the gate that makes the drain race-free. The turn
	// ctx is NOT derived from bgCtx, so a summary can still succeed while Shutdown is
	// draining; gating wg.Add under the SAME lock that sets draining guarantees an Add
	// never races wg.Wait at counter zero (the WaitGroup Add-after-Wait hazard) and no
	// distill is spawned onto an already-closing Router/Store. Terminal once set.
	draining bool

	// bgCtx is the app-scoped parent for that detached work (from deps.BackgroundCtx;
	// context.Background() when unset). Cancelled on App.Shutdown so a distill call
	// never outlives the closed Router/Store. Immutable after NewSession.
	bgCtx context.Context

	// toolProj memoizes the OpenAITools projection across the iterations of a turn
	// AND across turns. The projection is pure work keyed by the offered toolset
	// (allowedNames). Since skills are server-owned and never narrow the local
	// toolset (buildToolFilterLocked always returns nil — the FULL registry), the
	// cache identity is permanently "unconstrained" and is built ONCE then reused
	// for the whole process. Guarded by s.mu (read in resolveTurnTools) for
	// call-site symmetry. The slices.Equal/unconstrained apparatus is retained as a
	// cheap, correct general key even though only the nil branch is ever taken.
	toolProj toolProjCache

	// pendingDropCount carries RehydrateResult.DroppedRows from NewSession to the
	// first turn. Emitting the info event in NewSession would no-op (no live runID
	// yet, and the AgentEvents hook is wired AFTER Create returns), so the note is
	// deferred to runTurn — after runRef is stamped — and fired exactly once, then
	// zeroed. Single-flight Send serializes turns, so no lock guards it.
	pendingDropCount int

	// resumedNoteShown gates the one-time `# Session note` footer block (live watchers
	// adopted from a prior owner at ownership boot). Set true on the FIRST turn after
	// the titles are read into that turn's footer, so the note surfaces during the
	// first turn (every round of it) and never again — the footer equivalent of the old
	// message[1] consume, minus the RefreshRuntimeContext. Mirrors pendingDropCount:
	// single-flight Send serializes turns, so no lock guards it.
	resumedNoteShown bool

	// pendingInjections buffers messages the human typed WHILE a turn was in flight
	// (InjectPrompt), guarded by s.mu. The turn folds them into the live history at
	// the next tool-iteration boundary (foldInInjections), so the model picks them up
	// "between tasks" — part of the RUNNING turn, not deferred to a fresh one. The UI
	// shows a pending cue while buffered and an inline step once folded in; the
	// daemon's InjectNote uses the same iteration-boundary mechanism for its own notes.
	pendingInjections []string

	// rosterMu guards the cached open-terminal roster below. A DEDICATED mutex, not
	// s.mu: the detached refresher writes the cache while a turn holds s.mu across a long
	// stream snapshot, so coupling the two would make the refresher block on the turn (or
	// vice versa). The critical sections here are tiny (a slice header swap / shallow copy).
	rosterMu sync.Mutex

	// roster is the most recent open-terminal snapshot, served to every round's runtime
	// block WITHOUT blocking the turn. It is refreshed on a detached goroutine
	// (refreshRosterAsync) kicked at each turn's top, so a turn never waits on the
	// terminal.list + getStatus MCP round-trip that used to gate its first model round
	// (up to 5s on a slow MCP, every turn). Cross-turn: a COMPLETED refresh warms the
	// cache for subsequent turns; within a turn, a refresh that lands mid-turn is picked
	// up by the next round (buildRuntimeContext reads it fresh each round). Guarded by
	// rosterMu. Nil until the first refresh completes — a fresh session serves no roster
	// until then (across however many fast single-round turns elapse before the detached
	// fetch returns), which is harmless: the roster is a convenience and the model can
	// still tool-call terminal.list, exactly as before this inventory existed.
	roster []backend.OpenTerminal

	// rosterRefreshing dedupes concurrent refreshers so a slow fetch spanning two turns
	// cannot stack a second one behind it. Guarded by rosterMu.
	rosterRefreshing bool
}

// toolProjCache holds the last OpenAITools projection plus the key that produced
// it. unconstrained tracks the nil-filter (full registry) identity separately
// because slices.Equal treats nil and []string{} as equal, but nil means "all
// tools" while a narrowed set never does.
type toolProjCache struct {
	valid         bool
	unconstrained bool              // allowedNames was nil (the full registry)
	allowedNames  []string          // the filter that produced tools (cache key)
	tools         []models.ChatTool // the cached projection
}

// NewSession builds a Session. The CLI holds NO client-side control prefix — the
// backend owns the system prompt, developer instructions, and skill bodies — so a
// fresh session starts with an EMPTY visible history (index 0). On resume
// (deps.RestoredMessages != nil) the restored working history (user/assistant/tool
// only) is the starting history and seq continues from InitialSeq.
func NewSession(deps SessionDeps) *Session {
	if deps.Events == nil {
		deps.Events = NoopEventSink{}
	}
	if deps.RunRef == nil {
		deps.RunRef = &RunIDRef{}
	}
	s := &Session{
		deps:             deps,
		events:           deps.Events,
		artifacts:        NewArtifactStore(deps.SessionID, deps.ArtifactPersister),
		runRef:           deps.RunRef,
		bgCtx:            deps.BackgroundCtx,
		pendingDropCount: deps.DroppedRehydrateRows,
		// A resumed session replays the persisted opaque token so the backend's
		// skill selector continues where the previous owner left off ("" ⇒ fresh,
		// the backend just re-runs selection).
		backendState: deps.InitialBackendState,
	}
	// Detached distill work parents off the app-scoped context; fall back to
	// Background when unwired (tests) so the goroutine still has a valid parent.
	if s.bgCtx == nil {
		s.bgCtx = context.Background()
	}

	if deps.RestoredMessages != nil {
		// Resume: the restored visible history is the starting point; seq continues.
		s.messages = append([]models.ChatMessage(nil), deps.RestoredMessages...)
		s.seq = deps.InitialSeq
		if s.seq < 0 {
			s.seq = 0
		}
		// On a dup-seq forced fresh start the working history is EMPTY and we resume
		// at maxSeq+1; persist a clear breadcrumb at that collision-free seq so the
		// durable log records the reset boundary and a later resume sees a clean,
		// post-marker history rather than the dirty dup-seq rows again.
		if deps.DirtyFreshStart {
			s.persistMessage(models.TextMessage("system", domain.ClearMarker))
		}
	} else {
		// Fresh session: empty visible history, seq from 0. No control rows.
		s.messages = nil
		s.seq = 0
	}
	return s
}

// Messages returns a read-only copy of the live history (taken under the lock so a
// concurrent turn/command can't tear the slice).
func (s *Session) Messages() []models.ChatMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]models.ChatMessage, len(s.messages))
	copy(out, s.messages)
	return out
}

// InjectNote pushes a "[system event]" user message into the live history (under
// the lock, since the daemon can inject while a turn streams).
func (s *Session) InjectNote(note string) {
	s.pushMessage(models.TextMessage("user", injectNotePrefix+note))
}

// InjectPrompt buffers a message the human typed while a turn is in flight. It is
// folded into the RUNNING turn at the next tool-iteration boundary (foldInInjections)
// — the model sees it "between tasks" rather than after the whole turn ends. Buffered
// (not pushed immediately) so the UI can still retract it before the model consumes
// it. Safe to call from any goroutine.
func (s *Session) InjectPrompt(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingInjections = append(s.pendingInjections, text)
}

// HasPendingInjections reports whether the human has typed a message that is
// buffered but not yet folded into the running turn. A long-running IN-TURN tool
// (terminal.awaitAll) polls this so it can break its wait early and hand control
// back at the next iteration boundary — where foldInInjections delivers the
// message — instead of stranding the user behind a multi-minute block. Safe to
// call from any goroutine.
func (s *Session) HasPendingInjections() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pendingInjections) > 0
}

// RetractPendingInjection removes and returns the most-recently buffered injection
// that has NOT yet been folded in (LIFO — mirrors the cockpit's Esc-retract of a
// typed follow-up). ok is false when nothing is buffered (already folded in, or none
// typed), in which case the caller leaves the running turn alone.
func (s *Session) RetractPendingInjection() (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.pendingInjections)
	if n == 0 {
		return "", false
	}
	text := s.pendingInjections[n-1]
	s.pendingInjections = s.pendingInjections[:n-1]
	return text, true
}

// DiscardPendingInjections drops every buffered-but-not-folded injection. Called on
// Ctrl+C cancel and /clear: a redirect typed mid-turn must not silently fire on a
// later turn once the user abandoned the work it was meant for.
func (s *Session) DiscardPendingInjections() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingInjections = nil
}

// drainPendingInjections atomically takes the buffered injections, appends each to
// the live history as a (prefixed) user message, and returns the RAW texts (for the
// caller to surface to the UI). Empty ⇒ nil. Because the turn re-snapshots history at
// the top of every iteration, a message folded in here is in the very next model call.
func (s *Session) drainPendingInjections() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pendingInjections) == 0 {
		return nil
	}
	drained := s.pendingInjections
	s.pendingInjections = nil
	for _, text := range drained {
		s.pushMessageLocked(models.TextMessage("user", userInterjectPrefix+text))
	}
	return drained
}

// foldInInjections drains any buffered injections into history and emits an
// Interjection event per message so the cockpit can render it inline in the running
// turn. Returns how many were folded in (0 ⇒ nothing pending). Called at every point
// where the turn would proceed to a model call or end, so a mid-turn message is always
// picked up at the next boundary and never stranded.
func (s *Session) foldInInjections() int {
	drained := s.drainPendingInjections()
	for _, text := range drained {
		s.events.Interjection(text)
	}
	return len(drained)
}

// Clear empties the visible history and persists a CLEAR breadcrumb. Returns
// ErrTurnInProgress when a turn is in flight (a mid-turn clear would corrupt the
// streaming snapshot) — do NOT mutate in that case.
func (s *Session) Clear() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight {
		return ErrTurnInProgress
	}
	s.clearLocked()
	return nil
}

// clearLocked performs the clear; caller MUST hold s.mu.
func (s *Session) clearLocked() {
	if len(s.messages) > domain.ControlMessageCount {
		s.messages = s.messages[:domain.ControlMessageCount]
	}
	// History is gone — the auto-compact failure streak is moot; don't let a stale
	// count trip the lossy fallback on the next outage. The stashed real prompt_tokens
	// described the now-discarded history, so zero it too: the next compaction check
	// must fall back to the char estimate until a fresh round reports real usage. The
	// compaction chain is destroyed as well, so the depth tag resets to 0 (a depth that
	// kept climbing past a /clear would be misleading).
	s.compactFailures = 0
	s.lastPromptTokens = 0
	s.compactionDepth = 0
	// Drop the opaque backend skill-state token: /clear is a BRAND-NEW chat, so the
	// server's stateful skill selector must re-decide from scratch. Left set, the next
	// turn replays the pre-clear token and the backend treats the fresh chat as a
	// continuation — it never re-injects the runbook the cleared conversation no longer
	// carries, so the model starts a skill-shaped task (e.g. multi-agent orchestration)
	// with no runbook and, thinking-off, does nothing. Nothing from before /clear may
	// persist, and this token is the one piece that leaked. Clear the durable mirror
	// too, or a post-/clear handover would resurrect the dropped token.
	s.backendState = ""
	if s.deps.BackendStateStore != nil {
		_ = s.deps.BackendStateStore.PutSessionBackendState(s.deps.SessionID, "")
	}
	s.persistMessageLocked(models.TextMessage("system", domain.ClearMarker))
}

// Compact replaces the working history with one "[checkpoint…]" user note,
// persisting a system marker then the note. Returns ErrTurnInProgress when a turn
// is in flight (the interactive /compact path) — the in-turn auto-compact uses
// compactLocked instead.
func (s *Session) Compact(summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight {
		return ErrTurnInProgress
	}
	s.compactLocked(summary)
	return nil
}

// compactLocked performs the compaction; caller MUST hold s.mu. Used by the
// in-turn auto-compact (which already runs under the turn, where inFlight is set,
// so the public guard would reject it).
func (s *Session) compactLocked(summary string) {
	// Any successful compaction (auto OR manual /compact) re-establishes the bound, so
	// the consecutive-failure streak resets here — its single source of truth — rather
	// than only on the auto path. Prevents a stale count from a prior outage tripping
	// the lossy fallback right after the user manually compacted.
	s.compactFailures = 0
	// The stashed real prompt_tokens measured the pre-compaction history (~60K+). After
	// compaction the working history is a single short note, so leaving it set would make
	// the very next maybeAutoCompact see the old over-threshold figure and compact again
	// immediately — a tight, useless re-compaction loop. Zero it so the next check falls
	// back to the char estimate until a fresh round reports the post-compaction usage.
	s.lastPromptTokens = 0
	// Bump the compaction depth and tag the persisted note with it, so a
	// summary-of-summary chain is observable instead of silently flattening detail
	// over a long run (issue #251). The tag rides on the user note; the rehydration
	// boundary keys off the system marker, so this text is free to change.
	s.compactionDepth++
	s.messages = s.messages[:domain.ControlMessageCount]
	s.persistMessageLocked(models.TextMessage("system", compactionMarker))
	note := models.TextMessage("user", compactionNotePrefix(s.compactionDepth)+summary)
	s.messages = append(s.messages, note)
	s.persistMessageLocked(note)
}

// SendOptions tunes a turn. Reserved for future per-turn options; currently empty.
// Every turn — user-driven OR an autonomous watcher wake — runs with the SAME full
// tool capability. (Wake turns were once narrowed to read-only inspection; that
// extra layer is gone — the per-call confirmation/tier gate in Dispatch is the one
// authority on what may mutate, so a wake turn can relay between agents, send
// terminal input, spawn, etc., exactly like a user turn.)
type SendOptions struct {
	// IsWake marks an autonomous watcher-wake turn (the input is a BuildWakePrompt blob,
	// NOT typed by the user). The footer's goal anchor reads it to substitute the active
	// workflow objective for the verbose wake blob (goalAnchorSection). It is a CHANNEL
	// signal set by the wake caller — never inferred from the prompt text — so a user who
	// happens to type the wake prefix still gets their own goal anchored.
	IsWake bool
}

// Send mints a run id, runs one turn, and clears the run ref in finally. It is
// single-flight: a concurrent call returns ErrTurnInProgress. The reply string is
// the model's final answer OR a sentinel string on a model/tool failure (Send
// never returns an error for those — wake reactors prefix-match the sentinels).
// The error return is reserved for the single-flight guard.
func (s *Session) Send(ctx context.Context, userInput string, opts SendOptions) (string, error) {
	s.mu.Lock()
	if s.inFlight {
		s.mu.Unlock()
		return "", ErrTurnInProgress
	}
	s.inFlight = true
	s.mu.Unlock()

	runID := domain.NewID("run_")
	s.runRef.Set(runID)
	defer func() {
		s.runRef.Clear()
		s.mu.Lock()
		s.inFlight = false
		s.mu.Unlock()
	}()

	return s.runTurn(ctx, runID, userInput, opts), nil
}

// recallMemories runs the per-turn BM25 recall seeded by the originating ask, returning
// the rows to inject into the merged memories footer block's `## Relevant` subblock.
// Best-effort and nil-safe: a missing recaller, a blank ask, or a query error all
// yield nil (the footer simply omits the subblock). It NEVER returns an error — a
// recall failure must never break a turn (side-channel reads can't break the loop).
//
// The blank-ask short-circuit is enforced here (not left to the storage layer's
// escapeFTSQuery) so the contract holds for ANY recaller, and a whitespace-only send
// never pays for an adapter→SQLite round-trip that can only return nothing.
func (s *Session) recallMemories(userInput string) []domain.MemoryRecord {
	if s.deps.MemoryRecaller == nil || strings.TrimSpace(userInput) == "" {
		return nil
	}
	rows, err := s.deps.MemoryRecaller.RecallMemories(userInput, relevantMemoriesMaxRows)
	if err != nil {
		return nil
	}
	return rows
}

// runTurn is the core loop (ordering is load-bearing).
func (s *Session) runTurn(ctx context.Context, runID, userInput string, opts SendOptions) (reply string) {
	s.events.Phase(domain.PhaseReceived)

	// Bracket the whole turn in the debug trace: turn.start gives a log reader one
	// entry point per turn and turn.end (a defer that classifies the named `reply`)
	// records the outcome on EVERY exit path — cancel, failure, success — without
	// threading a status through each return. turnID is generated HERE, not in the loop
	// below, so the two bracket events and every backend round share one id.
	turnStartMS := domain.NowMS()
	turnID := domain.NewID("turn_")
	var roundsRun int
	s.traceTurnStart(runID, turnID, userInput, opts.IsWake, len(s.snapshotMessages()))
	defer func() {
		s.traceTurnEnd(runID, turnID, reply, domain.NowMS()-turnStartMS, roundsRun)
	}()

	// Persist the originating prompt as the run's FIRST durable row so /explain can
	// label the run by what prompted it. Emitted before AssistantStart and before
	// the cancel check below, so even an immediately-aborted turn carries a label.
	s.events.TurnPrompt(userInput)

	// One-shot resume-corruption note: if rehydration silently elided corrupt or
	// orphan rows, surface it ONCE, now that a live runID is stamped (the durable
	// sink early-returns without one) and the UI proxy's hook is wired. Emitting in
	// NewSession would no-op on both sinks. Single-flight serializes turns, so the
	// read-and-zero needs no lock.
	if s.pendingDropCount > 0 {
		n := s.pendingDropCount
		s.pendingDropCount = 0
		s.events.Info(fmt.Sprintf("Session resumed: %d malformed or orphan row(s) dropped from saved history.", n))
	}

	// 1. Cancel BEFORE any model work leaves no orphan turn (issue #61 pull-back).
	if ctx.Err() != nil {
		s.events.Phase(domain.PhaseCancelled)
		s.events.AssistantCancelled("")
		return domain.CancelledReply
	}

	// 2. Auto-compact (best-effort).
	s.events.Phase(domain.PhaseAnalyzing)
	s.maybeAutoCompact(ctx, runID)

	// 3. Re-check: a cancel landing in the auto-compact window must ALSO leave no
	//    orphan turn (issue #61).
	if ctx.Err() != nil {
		s.events.Phase(domain.PhaseCancelled)
		s.events.AssistantCancelled("")
		return domain.CancelledReply
	}

	// 3a. Join the splash-time stable discovery before the first request. A fast submit
	// during the handoff can otherwise beat Bubble Tea's bootstrap command and send an
	// avoidably empty project/agent snapshot. The app callback is bounded and fail-open.
	if s.deps.EnsureStartupContext != nil {
		s.deps.EnsureStartupContext(ctx)
	}
	if ctx.Err() != nil {
		s.events.Phase(domain.PhaseCancelled)
		s.events.AssistantCancelled("")
		return domain.CancelledReply
	}

	// 3b. Recall relevant memories ONCE per turn (best-effort), seeded by the
	//     originating ask. The top BM25 hits are injected into every round's merged
	//     memories footer block (`## Relevant` subblock) so distilled, non-pinned facts
	//     resurface automatically. Run HERE — after the cancel re-check, before the loop — so a
	//     pre-loop cancel never pays for it AND the FTS5 query fires exactly once per
	//     turn, not once per model round (composeTurnFooter runs every round). The rows
	//     are cached and threaded through footerContext below. A nil recaller, a blank ask,
	//     or a query error all yield nil: a recall failure must never break the turn.
	recalledMemories := s.recallMemories(userInput)

	// 3c. Resumed-watchers note: surface the one-time heads-up (live watchers adopted
	//     from a prior owner at ownership boot) on the FIRST turn only, then never again
	//     this session — the footer equivalent of the old message[1] consume. Read ONCE
	//     here, gated by the shown flag, into a turn-local so the note rides EVERY round
	//     of this turn (the footer is rebuilt per round) and no later turn. The provider
	//     is scheduler-gated at the app seam (nil on non-interactive paths where the
	//     supervision engines aren't running). Set the flag even when the provider yields
	//     nothing, so a no-watcher first turn doesn't re-probe every later turn.
	var resumedWatchers []string
	if !s.resumedNoteShown {
		s.resumedNoteShown = true
		if s.deps.ResumedWatchers != nil {
			resumedWatchers = s.deps.ResumedWatchers()
		}
	}

	// 3e. Open-terminal inventory: the model sees the live roster as inert runtime data
	//     instead of tool-calling terminal.list mid-turn to discover it. This used to be a
	//     SYNCHRONOUS terminal.list + getStatus MCP round-trip run here, gating the first
	//     model round of EVERY MCP-connected turn on up to 5s of network latency. It is now
	//     served from a cross-turn cache and refreshed on a DETACHED goroutine, so the turn
	//     never blocks on it: kick the refresh now (no-op if one is already in flight), and
	//     each round below reads the freshest cached snapshot (buildRuntimeContext runs per
	//     round, so a refresh that lands mid-turn is picked up on the next round). The one
	//     cost is that a brand-new session serves no roster until its first detached refresh
	//     completes (usually within the first turn, longer if the MCP read is slow) —
	//     harmless: the roster is a convenience, and the model can still tool-call
	//     terminal.list to discover it.
	s.refreshRosterAsync()

	// 3d. An autonomous wake turn carries the verbose [automatic wake-up] blob as its
	//     "goal"; the footer's goal anchor substitutes the active-workflow objective for it
	//     (see goalAnchorSection). This is a CHANNEL signal from the wake caller (SendOptions),
	//     not inferred from the prompt text — so a user who types the wake prefix still gets
	//     their own goal anchored.
	isWake := opts.IsWake

	// 4. Push the user message.
	s.pushMessage(models.TextMessage("user", userInput))

	turn := TurnContext{RunID: runID}

	// One backend turn_id spans the whole tool-call loop (every round shares it so the
	// backend keeps the same skill state and does not re-run selection on a plain
	// continuation round); it is created at the top of runTurn so the turn.start/turn.end
	// trace events carry it too. instructionRevision bumps whenever a mid-turn injection
	// is folded in — a fresh instruction the backend's selector should react to.
	instructionRevision := 0

	failureCounts := make(map[string]int)
	coarseCounts := make(map[string]int)
	stuckNudged := false

	// The agentic loop. `iter` counts model rounds purely to drive the phase display
	// (Analyzing on the first round, Integrating thereafter) — there is deliberately NO
	// per-turn round ceiling. A long-running autonomous workflow (e.g. orchestrating a
	// multi-round game across several agent terminals) legitimately needs many rounds
	// and must be free to keep going; the genuine runaway guard is the per-tool failure
	// breaker in runToolBatch (the same call failing the same way aborts at
	// RepeatFailureAbort), not a blunt round cap. A message the human typed mid-turn
	// (InjectPrompt) is folded into history at the TOP of a round (foldInInjections), so
	// the very next model snapshot includes it — "between tasks" pickup. A fresh user
	// instruction is legitimate new work, so folding one in RESETS the per-turn failure
	// breaker (a redirect must not be aborted by the PRIOR tool's failures) and re-shows
	// the Analyzing phase.
	iter := 0
	resetForInjection := func() {
		iter = 0
		// A folded-in injection is a new active instruction: bump the revision so the
		// backend's selector cadence reacts to it (the same turn_id continues).
		instructionRevision++
		failureCounts = make(map[string]int)
		coarseCounts = make(map[string]int)
		stuckNudged = false
	}
	for {
		// 10a. Cancel check at the top of each round. A cancel always wins over a
		//      pending injection (those are dropped via DiscardPendingInjections on abort).
		if ctx.Err() != nil {
			s.events.Phase(domain.PhaseCancelled)
			s.events.AssistantCancelled("")
			return domain.CancelledReply
		}

		// 10a′. Fold in any message typed during the previous round's stream/tools so
		//       THIS round's snapshot carries it. A fresh instruction resets the breaker.
		if s.foldInInjections() > 0 {
			resetForInjection()
		}

		// 10a″. Per-round auto-compact (best-effort). With no iteration ceiling a single
		//       turn can append unboundedly many rounds, so context must be re-bounded
		//       HERE or a long autonomous workflow would grow history until it hit the
		//       model's hard context limit. Cheap when under the soft threshold (an
		//       estimate + early return); only summarizes when over. Gated on iter>0: the
		//       pre-turn compact (step 2) already covered iter==0, and a fresh injection
		//       above resets iter to 0 — so a just-delivered user message is never
		//       summarized away in the same round it is first folded in. Cancel-safe: a
		//       cancel landing in the summary window leaves no orphan turn (issue #61).
		if iter > 0 {
			s.maybeAutoCompact(ctx, runID)
			if ctx.Err() != nil {
				s.events.Phase(domain.PhaseCancelled)
				s.events.AssistantCancelled("")
				return domain.CancelledReply
			}
		}

		// 5/9. Project the full tool registry for this round. Skills no longer narrow
		//      the toolset (the backend owns skill selection; the CLI always offers the
		//      whole registry), so this is a stable projection — a cache HIT every round.
		//      Computed under s.mu and released before the (long) stream.
		allowedNames, allowedSet, tools, err := s.resolveTurnTools()
		if err != nil {
			// A failure is a WAKE_FAILURE_PREFIX — keep verbatim.
			msg := "Tool projection failed: " + err.Error()
			s.events.Phase(domain.PhaseFailed)
			s.events.Error(msg)
			return msg
		}
		turn.ActiveToolNames = allowedNames

		// 10c. New round.
		if iter == 0 {
			s.events.Phase(domain.PhaseAnalyzing)
		} else {
			s.events.Phase(domain.PhaseIntegrating)
		}
		s.events.AssistantStart()

		// 10d. Stream the backend. Stable discovery travels through request.startup;
		//      input.messages contains visible conversation only. Runtime + per-turn
		//      context travel through request.runtime / request.turn. The backend owns
		//      every system prompt, skill selection, and final assembly, and streams
		//      named SSE events.
		gotToken := false
		// Read an immutable SNAPSHOT of the history under the lock, then stream with the
		// lock RELEASED — the call is long, and a concurrent InjectNote (daemon) or
		// InjectPrompt (user) must be able to append without racing this read.
		bmsgs, cerr := toBackendMessages(s.snapshotMessages())
		if cerr != nil {
			msg := "Conversation could not be encoded for the backend: " + cerr.Error()
			s.events.Phase(domain.PhaseFailed)
			s.events.Error(msg)
			return msg
		}
		btools, terr := toBackendTools(tools)
		if terr != nil {
			msg := "Tool inventory rejected before send: " + terr.Error()
			s.events.Phase(domain.PhaseFailed)
			s.events.Error(msg)
			return msg
		}

		promptContext := s.promptContext()
		if s.deps.CurrentWorktreeFetcher != nil {
			// Assign nil too: a failed live read means "unknown this round", not "reuse
			// the cached splash/reconnect selection as if it were still current".
			worktree := s.currentWorktreeContext(ctx)
			// The read itself can degrade the shared MCP transport. Re-snapshot afterward
			// so this same runtime tail never claims MCP is connected after CallTool tore it
			// down; then preserve the explicit success/none/failure result from this read.
			promptContext = s.promptContext()
			promptContext.Worktree = worktree
		}
		req := backend.RespondRequest{
			Session: backend.RespondSession{
				ID:                  s.deps.SessionID,
				TurnID:              turnID,
				InstructionRevision: instructionRevision,
				Round:               iter,
			},
			Startup: buildStartupContext(promptContext),
			State:   s.backendStatePtr(),
			Input: backend.RespondInput{
				Messages:   bmsgs,
				Tools:      btools,
				ToolChoice: "auto",
			},
			Runtime:    s.buildRuntimeContext(promptContext, s.currentRoster()),
			Turn:       s.buildTurnContext(userInput, isWake, recalledMemories, resumedWatchers),
			Selection:  &backend.Selection{Policy: "new_instruction"},
			Generation: &backend.Generation{ResponseFormat: "text"},
		}

		// One model round per RespondStream — counted for the turn.end summary and used
		// to time first-token latency. roundStartMS is captured AFTER the trace request so
		// it measures the stream itself.
		roundsRun++
		s.traceBackendRequest(runID, turnID, iter, req, bmsgs, btools)
		roundStartMS := domain.NowMS()
		var firstTokenMS int64

		result, serr := s.deps.Backend.RespondStream(ctx, req, backend.StreamCallbacks{
			OnSkillLoaded: func(refs []backend.SkillRef) {
				s.emitSkillLoads(refs)
			},
			OnMeta: func(m backend.StreamMeta) {
				s.applyStreamMeta(m)
				s.traceBackendMeta(runID, turnID, iter, m)
			},
			OnContent: func(tok string) {
				if !gotToken {
					gotToken = true
					firstTokenMS = domain.NowMS()
					s.events.Phase(domain.PhaseGenerating)
				}
				s.events.AssistantToken(tok)
			},
		})
		if serr != nil {
			// A cancel (ctx) always reads as a clean stop, not a model failure.
			if ctx.Err() != nil {
				s.events.Phase(domain.PhaseCancelled)
				s.events.AssistantCancelled("")
				return domain.CancelledReply
			}
			s.traceBackendError(runID, turnID, iter, domain.NowMS()-roundStartMS, serr)
			return s.classifyBackendError(serr)
		}

		firstTokenLatency := int64(0)
		if firstTokenMS > 0 {
			firstTokenLatency = firstTokenMS - roundStartMS
		}
		s.traceBackendDone(runID, turnID, iter, result, domain.NowMS()-roundStartMS, firstTokenLatency)

		calls := backendToolCalls(result.Message.ToolCalls)

		// 10e. Usage — emitted BEFORE appending the assistant message so contextTokens
		//      reflects the prompt actually sent (backend-reported prompt_tokens).
		s.emitBackendUsage(result.Usage, result.Meta.Model)

		// 10f. Append the assistant message (content null on a pure tool-call turn).
		s.pushMessage(backendAssistantMessage(result.Message))

		// 10g. No tool calls ⇒ the model thinks it's done. But if the user slipped a
		//      message in during this round, DON'T seal — fold it in and loop so it is
		//      answered (never strand an injection at the final-answer boundary). The
		//      just-streamed content stays an unsealed intermediate round (exactly like
		//      prose that precedes a tool batch), so no AssistantEnd fires yet.
		if len(calls) == 0 {
			if s.foldInInjections() > 0 {
				resetForInjection()
				continue
			}
			s.events.Phase(domain.PhaseComplete)
			s.events.AssistantEnd(result.Message.Content, "")
			return result.Message.Content
		}

		// 10h. Execute the batch. Announce ALL calls as queued BEFORE sequential
		//      dispatch, then promote each queued→active→done/failed. The round's
		//      finish reason rides along so a parse-failed final call can be diagnosed
		//      as output-cap truncation rather than a model syntax slip.
		if reply, done := s.runToolBatch(ctx, calls, result.FinishReason, turn, allowedSet, failureCounts, coarseCounts, &stuckNudged); done {
			// A cancel always ends the turn. A circuit-breaker abort instead yields to a
			// fresh user instruction if one arrived mid-batch (fold it in + continue, so
			// the model gets the new steer); otherwise the abort stands.
			if reply != domain.CancelledReply && ctx.Err() == nil && s.foldInInjections() > 0 {
				resetForInjection()
				continue
			}
			return reply
		}

		iter++
	}
}

// questionToolName is the one tool that must be invoked ALONE in a batch. It blocks the
// turn on a human decision, so any sibling tool bundled with it would run its (possibly
// side-effecting) work BEFORE the user has answered. When the model bundles it with
// other calls, runToolBatch executes only the FIRST question and stubs the rest as
// recoverable skips so the model re-plans using the answer.
const questionToolName = "user.askMultipleChoice"

// firstQuestionIndex returns the index of the first user.askMultipleChoice call in the
// batch (internal name resolved), or -1 if the batch contains none.
func (s *Session) firstQuestionIndex(calls []models.ToolCallRequest) int {
	for i, call := range calls {
		if s.resolveInternal(call.Function.Name) == questionToolName {
			return i
		}
	}
	return -1
}

// runToolBatch dispatches a batch of tool calls after announcing the whole batch as
// queued. Maximal consecutive runs of parallel-safe calls (no-wait snapshot reads
// that opted in via Tool.Parallelizable — terminal.extract/.json) dispatch
// CONCURRENTLY, collapsing N extraction round-trips into roughly the slowest one,
// with each member's settled result streamed live as it completes; every other call
// runs serially in place, preserving exact ordering and side-effect sequencing. Two
// circuit breakers trip MID-batch (stopping + stubbing the remaining calls the
// instant a runaway is detected, so one giant batch can't fully dispatch first): the
// FINE breaker on identical args, and the COARSE breaker on a tool repeating the same
// UNRECOVERABLE error with varied args. A parallel group folds its tallies in call
// order and applies the same guards at the group boundary. Returns (reply, true)
// when the turn must end (cancel or breaker abort), else ("", false) to continue.
func (s *Session) runToolBatch(ctx context.Context, calls []models.ToolCallRequest, finishReason string, turn TurnContext,
	allowedSet map[string]struct{}, failureCounts, coarseCounts map[string]int, stuckNudged *bool) (string, bool) {

	// Announce the whole batch as queued first.
	batch := make([]BatchedToolCall, 0, len(calls))
	for _, call := range calls {
		internalName := s.resolveInternal(call.Function.Name)
		batch = append(batch, BatchedToolCall{ID: call.ID, Name: internalName, Args: call.Function.Arguments})
	}
	s.events.Phase(domain.PhaseToolQueued)
	s.events.ToolBatch(batch)

	// A multiple-choice question must be asked ALONE: if the model bundled it with other
	// tools, run only the FIRST question and skip every sibling, so no side-effecting tool
	// executes before the user answers. -1 ⇒ no question in this batch (the common path).
	questionIdx := s.firstQuestionIndex(calls)

	var worstFine, worstCoarse *batchRepeat

	for c := 0; c < len(calls); {
		// Cancel BEFORE activating/dispatching the next call or group: a cancel that
		// landed while the PREVIOUS call ran must stop the whole queue here, so no
		// further tool executes after the user hit Escape. The current call AND every
		// remaining one (calls[c:]) get a structurally-valid CANCELLED tool result, so
		// each assistant tool_call still has a matching reply (or DeepSeek 400s on
		// replay).
		if ctx.Err() != nil {
			s.traceCancelledStub(turn.RunID, s.stubCancelledFrom(calls, c))
			s.events.Phase(domain.PhaseCancelled)
			s.events.AssistantCancelled("")
			return domain.CancelledReply, true
		}

		// Gather a maximal run of consecutive parallel-safe calls (no-wait snapshot
		// reads that opted in via Tool.Parallelizable) and dispatch a run of ≥2
		// CONCURRENTLY — each member's result streams live as it settles. Everything
		// else runs on the serial path below, preserving today's exact ordering and
		// side-effect sequencing. A batch carrying a multiple-choice question stays
		// fully serial: every non-question sibling is skipped synthetically, so there
		// is nothing to overlap.
		if questionIdx < 0 {
			if e := s.parallelRunEnd(calls, c, allowedSet); e-c >= 2 {
				s.runParallelGroup(ctx, calls, c, e, turn, failureCounts, coarseCounts, &worstFine, &worstCoarse)
				c = e
				// Apply the serial path's per-call mid-batch guards at the group
				// boundary: a cancel that landed during the group stops the queue now
				// (every member already carries its real result), and a breaker that
				// tripped inside the group aborts before any further dispatch.
				if ctx.Err() != nil {
					s.traceCancelledStub(turn.RunID, s.stubCancelledFrom(calls, c))
					s.events.Phase(domain.PhaseCancelled)
					s.events.AssistantCancelled("")
					return domain.CancelledReply, true
				}
				if reply, done := s.checkBreakerAbort(turn, calls, c, worstFine, worstCoarse); done {
					return reply, true
				}
				continue
			}
		}

		call := calls[c]
		internalName := s.resolveInternal(call.Function.Name)

		// A sibling skipped because a question must be asked alone. Its stub is a
		// SYNTHETIC failure (like a cancel/breaker stub), so it must NOT feed the failure
		// tallies or circuit breakers — else N identical siblings before the question
		// could trip RepeatFailureAbort and kill the turn before the question is even
		// dispatched (or before the model re-plans after the answer).
		questionSkip := questionIdx >= 0 && c != questionIdx

		// Promote queued→active and drive the phase.
		s.events.Phase(domain.PhaseToolRunning)
		s.events.ToolState(call.ID, ToolStateActive)

		startedAt := domain.NowMS()
		var res domain.ToolResult

		// Parse args (catch → recoverable failure, never dispatched). Two distinct
		// failure shapes with different correct recoveries, so they get different
		// error results: a round that hit its output-token cap (finish_reason
		// "length") is amputated mid-args on its FINAL call — the model must
		// re-issue that call AND anything it meant to emit after it — while any
		// other parse failure is a genuine encoding slip the model should just
		// re-encode. Truncated args are never repaired and executed: auto-closing
		// the JSON would run a tool on half its intended input (e.g. spawn an agent
		// with an amputated task prompt).
		//
		// The cap can also sever the stream BEFORE the first argument byte — the
		// function name arrived, the args did not — which the accumulator materializes
		// as an empty "{}" (build() defaults blank args), so it parses cleanly and
		// would otherwise dispatch on empty input. argsEmpty catches that window: an
		// effectively-empty FINAL call in a length round is treated as truncation too.
		// This deliberately also flags a genuinely parameterless final call in a length
		// round as truncated, but that is benign — re-issuing a no-arg call is harmless
		// and the round WAS cut short, so there genuinely IS more to re-emit — and far
		// better than running a required-args tool on {} (which Dispatch would reject
		// anyway, handing the model a misleading "missing required field" instead of the
		// truthful "your response was truncated").
		var parseErr error
		if call.Function.Arguments != "" {
			var probe any
			parseErr = json.Unmarshal([]byte(call.Function.Arguments), &probe)
		}
		trimmedArgs := strings.TrimSpace(call.Function.Arguments)
		argsEmpty := trimmedArgs == "" || trimmedArgs == "{}"

		s.events.ToolCall(ToolCallEvent{ID: call.ID, Name: internalName, Args: call.Function.Arguments, StartedAt: startedAt})

		switch {
		case questionSkip:
			// A question is in this batch and this is NOT it: skip with a recoverable stub
			// so the model asks the question by itself and re-plans using the answer,
			// instead of acting on assumptions before the user has decided.
			res = domain.Fail("QUESTION_BATCH_SKIPPED",
				"Not executed: user.askMultipleChoice must be called by itself. Ask the question alone, then choose your next tool calls using the user's answer.")
			res.Summary = "Skipped — a multiple-choice question must be asked by itself."
			// Short-circuits before Dispatch, so trace it or the skip is invisible in the log.
			s.traceToolGap("tool.question_batch_skipped", turn.RunID, call.ID, internalName, "")
		case (parseErr != nil || argsEmpty) && finishReason == backend.FinishReasonLength && c == len(calls)-1:
			// Output-cap truncation: the stream was cut mid-args (parseErr) or before
			// any argument byte at all (argsEmpty), so only the batch's FINAL call can be
			// affected — earlier siblings parsed and ran normally.
			res = domain.Fail("TOOL_ARGS_TRUNCATED",
				"Your response hit its output-token limit and this call's arguments were cut off mid-generation; nothing ran with the partial arguments. Re-issue this call with complete arguments — and re-issue any calls you intended after it, which were dropped entirely. If you are batching several large calls, issue fewer per response.")
			res.Summary = "Arguments truncated by the output-token limit for " + internalName + "; not executed."
			// Never reaches Dispatch → no tool.call audit event; trace it here or the
			// truncation is invisible in the log.
			s.traceToolGap("tool.args.truncated", turn.RunID, call.ID, internalName, call.Function.Arguments)
		case parseErr != nil:
			res = domain.Fail("INVALID_TOOL_ARGS_JSON",
				"Arguments were not valid JSON ("+parseErr.Error()+"). Re-issue this call with corrected JSON arguments.")
			res.Summary = "Invalid JSON arguments for " + internalName + "; not executed."
			// This rejection never reaches the registry's Dispatch, so it produces no
			// tool.call audit event — trace it here or it is invisible in the log.
			s.traceToolGap("tool.args.invalid", turn.RunID, call.ID, internalName, call.Function.Arguments)
		case allowedSet != nil && !setHas(allowedSet, internalName):
			// Defensive only: allowedSet is now ALWAYS nil (skills never narrow the
			// toolset — every turn offers the full registry; see buildToolFilterLocked),
			// so this branch cannot fire from a loaded skill. It survives purely as a
			// guard in case a future caller ever passes an explicit allow-list; it is NOT
			// a skill capability gate, and a skill must never be the reason a tool is
			// unavailable.
			res = domain.Fail("TOOL_NOT_OFFERED",
				internalName+" is not offered in this turn's tool spec.",
				domain.Unrecoverable())
			res.Summary = internalName + " is not offered in this turn's tool spec."
			// Also short-circuits before Dispatch → trace it so the (dormant today) gate
			// firing is never silent.
			s.traceToolGap("tool.not_offered", turn.RunID, call.ID, internalName, "")
		default:
			res = s.dispatchCall(ctx, call, internalName, turn)
		}

		s.emitToolSettled(ctx, call, internalName, res, domain.NowMS(), questionSkip)

		s.pushMessage(models.ChatMessage{
			Role:          "tool",
			ToolCallID:    call.ID,
			Name:          internalName,
			StringContent: SerializeToolResult(res, s.artifacts),
		})

		// Circuit-breaker bookkeeping: fold this failure into BOTH tallies (see
		// foldBreakerTallies). A question-skip stub is synthetic, not a real tool
		// failure, so it never feeds the breakers.
		if !questionSkip {
			foldBreakerTallies(internalName, call.Function.Arguments, res, failureCounts, coarseCounts, &worstFine, &worstCoarse)
		}

		// Mid-batch cancel: a cancel that landed DURING this call's dispatch stops the
		// queue now. This call already has its real result; stub every remaining
		// undispatched call (calls[c+1:]) so the transcript stays well-formed (each
		// assistant tool_call needs a matching tool result, or DeepSeek 400s on
		// replay).
		if ctx.Err() != nil {
			s.traceCancelledStub(turn.RunID, s.stubCancelledFrom(calls, c+1))
			s.events.Phase(domain.PhaseCancelled)
			s.events.AssistantCancelled("")
			return domain.CancelledReply, true
		}

		// MID-BATCH circuit breaker: abort the instant a runaway is detected, stubbing the
		// undispatched remainder (calls[c+1:]) — so a single huge batch (the model dumping
		// dozens of identical/futile calls in one round) can't fully dispatch before the
		// guard fires. Fine (identical args) at RepeatFailureAbort; coarse (same tool +
		// unrecoverable code, args varied) at CoarseRepeatFailureAbort.
		if reply, done := s.checkBreakerAbort(turn, calls, c+1, worstFine, worstCoarse); done {
			return reply, true
		}

		c++
	}

	// End-of-batch: the one-shot stuck-warning nudge (fine breaker, below the abort
	// threshold — a batch that hit RepeatFailureWarn but not RepeatFailureAbort).
	if worstFine != nil && worstFine.count >= domain.RepeatFailureWarn && !*stuckNudged {
		*stuckNudged = true
		codeSuffix := ""
		if worstFine.res.Error != nil && worstFine.res.Error.Code != "" {
			codeSuffix = " (" + worstFine.res.Error.Code + ")"
		}
		s.traceToolRepeat("tool.repeat.warning", turn.RunID, worstFine.name, worstFine.count, errCodeOf(worstFine.res), worstFine.sig)
		nudge := "[system event]\nYou have called " + worstFine.name + " " + itoa(worstFine.count) +
			" times this turn with the same arguments and it failed the same way each time" + codeSuffix +
			". Repeating the exact same call will keep failing. Read the error, CHANGE the arguments (or use a different tool/approach), or stop and report what's blocking you — do not emit the same arguments again."
		s.pushMessage(models.TextMessage("user", nudge))
		// Surface the stuck loop to the human too. The nudge above only steers the model;
		// without this, a tool repeating the same failure stayed invisible in the footer
		// until the turn either recovered or burned all the way to the abort threshold.
		s.events.Warn(worstFine.name + " keeps failing the same way" + codeSuffix +
			" — nudging the assistant to change approach.")
	}
	return "", false
}

// batchRepeat tracks the most-repeated failing call under one breaker signature in a
// batch — the input to the mid-batch circuit breakers.
type batchRepeat struct {
	name  string
	count int
	sig   string
	res   domain.ToolResult
}

// maxParallelToolDispatch bounds how many parallel-safe calls in one batch dispatch
// concurrently. Sized above the MCP request governor's in-flight cap (4) because an
// extraction's wall-clock is dominated by its backend small-model call, not its MCP
// reads — six extraction model calls genuinely overlap while their short MCP reads
// take turns in the governor queue. Deliberately small: the goal is collapsing a
// handful of per-agent reads into one wait, not a thundering herd.
const maxParallelToolDispatch = 6

// dispatchCall runs one call through the registry with its per-call progress
// plumbing (an in-tool substep emits ToolProgress(callID, msg) tagged so the UI maps
// it to the right activity row). Safe OFF the turn goroutine: Dispatch's
// side-channels (audit DB, debug log) are goroutine-safe, and ToolProgress feeds the
// mutex-guarded UI pump while the durable run-event sink no-ops it. Everything else
// (ToolCall/ToolResult/ToolState, transcript, breaker folds) must stay on the turn
// goroutine.
func (s *Session) dispatchCall(ctx context.Context, call models.ToolCallRequest, internalName string, turn TurnContext) domain.ToolResult {
	argsJSON := call.Function.Arguments
	if argsJSON == "" {
		argsJSON = "{}"
	}
	callTurn := turn
	callTurn.CallID = call.ID
	callTurn.Progress = func(callID string, msg string) {
		if msg == "" {
			return
		}
		s.events.ToolProgress(callID, msg)
	}
	return s.deps.Tools.Dispatch(ctx, internalName, argsJSON, callTurn)
}

// emitToolSettled applies the session-cumulative per-tool failure tally (issue #251 —
// a coarse "which tools keep failing" signal that accumulates across rounds, off the
// audit path) and emits the settled ToolResult + ToolState events for one call. The
// tally counts EVERY failed result (bad-args and not-offered included — a tool the
// model keeps misusing is a real drift signal), EXCEPT a result produced while the
// turn is being cancelled (a user abort tearing down mid-tool is not a tool failure)
// and EXCEPT a synthetic stub that never dispatched (a question-batch skip). MUST run
// on the turn goroutine — the durable run-event sink is not goroutine-safe.
func (s *Session) emitToolSettled(ctx context.Context, call models.ToolCallRequest, internalName string, res domain.ToolResult, endedAt int64, synthetic bool) {
	failCount := 0
	if !res.Ok && ctx.Err() == nil && !synthetic {
		failCount = s.recordToolFailure(internalName)
	}
	s.events.ToolResult(ToolResultEvent{ID: call.ID, Name: internalName, Result: res, EndedAt: endedAt, FailureCount: failCount})
	if res.Ok {
		s.events.ToolState(call.ID, ToolStateDone)
	} else {
		s.events.ToolState(call.ID, ToolStateFailed)
	}
}

// foldBreakerTallies folds one failed result into both circuit-breaker tallies. The
// FINE signature is the CANONICALIZED args + error code (a byte-identical retry, even
// with reordered keys). The COARSE signature strips pagination fields and counts ONLY
// unrecoverable errors — the args-varied futile loop the fine tally misses (the model
// paging a pruned artifact by offset). Ok results are a no-op. Not goroutine-safe
// (mutates the shared maps): call it from the turn goroutine only, in call order, so
// the breaker outcome is deterministic.
func foldBreakerTallies(internalName, rawArgs string, res domain.ToolResult,
	failureCounts, coarseCounts map[string]int, worstFine, worstCoarse **batchRepeat) {

	if res.Ok {
		return
	}
	errCode := ""
	if res.Error != nil {
		errCode = res.Error.Code
	}
	fineSig := failureSignature(internalName, rawArgs, errCode)
	failureCounts[fineSig]++
	if fc := failureCounts[fineSig]; *worstFine == nil || fc > (*worstFine).count {
		*worstFine = &batchRepeat{name: internalName, count: fc, sig: fineSig, res: res}
	}
	if res.Error != nil && !res.Error.Recoverable && errCode != "" {
		coarseSig := coarseFailureSignature(internalName, rawArgs, errCode)
		coarseCounts[coarseSig]++
		if cc := coarseCounts[coarseSig]; *worstCoarse == nil || cc > (*worstCoarse).count {
			*worstCoarse = &batchRepeat{name: internalName, count: cc, sig: coarseSig, res: res}
		}
	}
}

// checkBreakerAbort applies the two mid-batch circuit-breaker thresholds and, on a
// trip, aborts the turn (stubbing calls[from:], the not-yet-dispatched remainder).
// Shared by the serial per-call check and the parallel-group boundary check so the
// two paths can never drift.
func (s *Session) checkBreakerAbort(turn TurnContext, calls []models.ToolCallRequest, from int, worstFine, worstCoarse *batchRepeat) (string, bool) {
	if worstFine != nil && worstFine.count >= domain.RepeatFailureAbort {
		msg := "Stopped: called " + worstFine.name + " " + itoa(worstFine.count) +
			" times this turn with identical arguments, each failing the same way (" + repeatDetail(worstFine.res) +
			"). Tell the user what's blocking and what you tried rather than repeating the call."
		return s.abortForRepeat(turn, calls, from, "tool.repeat.abort", worstFine, msg), true
	}
	if worstCoarse != nil && worstCoarse.count >= domain.CoarseRepeatFailureAbort {
		msg := "Stopped: called " + worstCoarse.name + " " + itoa(worstCoarse.count) +
			" times this turn, each failing with the same unrecoverable error (" + repeatDetail(worstCoarse.res) +
			") despite different arguments. Retrying an unrecoverable error can't succeed — tell the user what's blocking and stop rather than varying the arguments."
		return s.abortForRepeat(turn, calls, from, "tool.repeat.abort.coarse", worstCoarse, msg), true
	}
	return "", false
}

// parallelRunEnd returns the end (exclusive) of the maximal run of parallel-safe
// calls starting at c — the candidate concurrent group.
func (s *Session) parallelRunEnd(calls []models.ToolCallRequest, c int, allowedSet map[string]struct{}) int {
	e := c
	for e < len(calls) && s.callParallelSafe(calls[e], allowedSet) {
		e++
	}
	return e
}

// callParallelSafe reports whether one call may join a concurrent group: its
// arguments parse to a real object (empty/degenerate args stay serial so the
// truncation/invalid-args paths keep today's exact ordering and never race a
// sibling), it carries no `wait` barrier, it survives the (dormant) allow-list gate,
// and the runner classifies the tool as parallel-safe (Tool.Parallelizable + read
// risk — an explicit per-tool opt-in, NOT every read tool: barrier reads like
// terminal.awaitAll are deliberately excluded). A `wait` condition turns even an
// opted-in extract into a BARRIER — it polls until a terminal settles, and a later
// call in the batch may depend on that settle — so a wait-bearing call runs
// serially. Only the production registry adapter implements parallelSafeRunner; a
// test fake that doesn't keeps the fully-serial path, so serial-ordering tests are
// unaffected.
func (s *Session) callParallelSafe(call models.ToolCallRequest, allowedSet map[string]struct{}) bool {
	trimmed := strings.TrimSpace(call.Function.Arguments)
	if trimmed == "" || trimmed == "{}" {
		return false
	}
	var probe struct {
		Wait json.RawMessage `json:"wait"`
	}
	if json.Unmarshal([]byte(call.Function.Arguments), &probe) != nil {
		return false
	}
	if len(probe.Wait) > 0 && strings.TrimSpace(string(probe.Wait)) != "null" {
		return false
	}
	internalName := s.resolveInternal(call.Function.Name)
	if allowedSet != nil && !setHas(allowedSet, internalName) {
		return false
	}
	runner, ok := s.deps.Tools.(parallelSafeRunner)
	return ok && runner.ParallelSafe(internalName)
}

// runParallelGroup dispatches calls[from:to) CONCURRENTLY (bounded by
// maxParallelToolDispatch) and streams each member's settled result the moment it
// completes — the cockpit shows every member live: all spinners appear together, then
// each row flips to done with its OWN true duration while siblings keep running,
// instead of the whole group settling at once with the slowest call's wall-clock.
//
// Threading contract: workers run ONLY dispatchCall (audit/debuglog are goroutine-safe;
// in-tool ToolProgress feeds the mutex-guarded pump and is no-op'd by the durable sink;
// the registry's DispatchObserver.ObserveDispatch also fires on the worker path and MUST
// be goroutine-safe — the workflow-intelligence service guards its state with a mutex).
// EVERY other emission — ToolCall/ToolResult/ToolState events,
// transcript pushes, breaker folds — happens on this (the turn) goroutine: the
// durable RunEventSink and the message/artifact state are not goroutine-safe. Result
// events are emitted in COMPLETION order (that is the point — live settles, keyed by
// call ID so the UI maps each to its row); transcript pushes and breaker folds then
// run in CALL order, so the conversation the backend replays and the breaker outcome
// are deterministic regardless of completion order.
func (s *Session) runParallelGroup(ctx context.Context, calls []models.ToolCallRequest, from, to int, turn TurnContext,
	failureCounts, coarseCounts map[string]int, worstFine, worstCoarse **batchRepeat) {

	n := to - from

	// Announce every member up front, in call order: all spinners appear together. A
	// member queued behind the semaphore is "active" from the user's point of view —
	// its duration honestly includes the queue wait.
	s.events.Phase(domain.PhaseToolRunning)
	names := make([]string, n)
	for i := 0; i < n; i++ {
		call := calls[from+i]
		names[i] = s.resolveInternal(call.Function.Name)
		s.events.ToolState(call.ID, ToolStateActive)
		s.events.ToolCall(ToolCallEvent{ID: call.ID, Name: names[i], Args: call.Function.Arguments, StartedAt: domain.NowMS()})
	}

	type completion struct {
		idx     int
		res     domain.ToolResult
		endedAt int64
	}
	// Buffered to n so a worker can never block on send; the loop below always
	// drains exactly n, so no goroutine or channel outlives this call.
	done := make(chan completion, n)
	sem := make(chan struct{}, maxParallelToolDispatch)
	for i := 0; i < n; i++ {
		go func(i int) {
			sem <- struct{}{}
			defer func() { <-sem }()
			call := calls[from+i]
			// A cancel that landed while earlier slots ran must stop the rest here:
			// honor "no tool executes after Escape" even for members still queued
			// behind the semaphore. The skipped call gets a structurally-valid
			// CANCELLED result so its assistant tool_call still has a matching reply.
			if ctx.Err() != nil {
				done <- completion{i, cancelledToolStub(), domain.NowMS()}
				return
			}
			// Each worker writes only its own send — results meet the turn goroutine
			// exclusively through the channel. endedAt is captured HERE, at dispatch
			// return, so the row's duration is this call's own (not the group's max).
			done <- completion{i, s.dispatchCall(ctx, call, names[i], turn), domain.NowMS()}
		}(i)
	}

	// Drain ALL n completions, emitting each settled result the moment it arrives.
	outs := make([]completion, n)
	for k := 0; k < n; k++ {
		out := <-done
		outs[out.idx] = out
		s.emitToolSettled(ctx, calls[from+out.idx], names[out.idx], out.res, out.endedAt, false)
	}

	// Fold transcript + breaker bookkeeping in CALL order.
	for i := 0; i < n; i++ {
		call := calls[from+i]
		s.pushMessage(models.ChatMessage{
			Role:          "tool",
			ToolCallID:    call.ID,
			Name:          names[i],
			StringContent: SerializeToolResult(outs[i].res, s.artifacts),
		})
		foldBreakerTallies(names[i], call.Function.Arguments, outs[i].res, failureCounts, coarseCounts, worstFine, worstCoarse)
	}
}

// clampRunes truncates s to at most max runes (Unicode code points, matching the
// backend's character-based max_length), so an over-long field can't violate a backend
// schema constraint.
func clampRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// repeatDetail renders a failing result as "code: message" (or its summary) for a
// breaker abort message.
func repeatDetail(res domain.ToolResult) string {
	if res.Error != nil && res.Error.Code != "" {
		return trimSpace(res.Error.Code + ": " + res.Error.Message)
	}
	return res.Summary
}

// abortForRepeat ends a turn on a tripped circuit breaker: it stubs every remaining
// undispatched call (calls[from:]) so each assistant tool_call keeps a matching reply,
// traces the abort, drives the failed phase + error event, and returns the abort
// message. Shared by the fine (identical-args) and coarse (unrecoverable, args-varied)
// breakers.
func (s *Session) abortForRepeat(turn TurnContext, calls []models.ToolCallRequest, from int, traceEvent string, wr *batchRepeat, msg string) string {
	s.stubSkippedFrom(calls, from)
	s.traceToolRepeat(traceEvent, turn.RunID, wr.name, wr.count, errCodeOf(wr.res), wr.sig)
	s.events.Phase(domain.PhaseFailed)
	s.events.Error(msg)
	return msg
}

// stubSkippedFrom pushes a structurally-valid tool result for every call in calls[from:]
// the circuit breaker skipped (never dispatched), so each assistant tool_call keeps a
// matching reply and the transcript replays cleanly. Pushed directly (not through the
// breaker fold), so a skip stub never feeds the tallies.
func (s *Session) stubSkippedFrom(calls []models.ToolCallRequest, from int) {
	for r := from; r < len(calls); r++ {
		pending := calls[r]
		pendingName := s.resolveInternal(pending.Function.Name)
		stub := domain.Fail("SKIPPED_CIRCUIT_BREAKER",
			"Not executed: the circuit breaker stopped the batch after a repeated failure.",
			domain.Unrecoverable())
		stub.Summary = "Skipped — circuit breaker stopped the batch."
		s.pushMessage(models.ChatMessage{
			Role:          "tool",
			ToolCallID:    pending.ID,
			Name:          pendingName,
			StringContent: SerializeToolResult(stub, s.artifacts),
		})
	}
}

// coarsePaginationFields are the volatile paging/cursor args stripped when building the
// coarse failure signature: re-reading the same failing resource while only advancing
// one of these must collapse to a single signature.
var coarsePaginationFields = []string{"offset", "limit", "length", "page", "cursor", "nextOffset", "startOffset"}

// coarseFailureSignature is the pagination-insensitive failure signature for the coarse
// circuit breaker: tool name + error code + the canonicalized args with paging fields
// removed. It collapses an argument-varied futile loop (the model paging a pruned
// artifact by offset) that the exact-args fine signature misses, WITHOUT the
// false-positives a tool+code-only key would cause on distinct missing resources.
// Non-object args fall through to the plain signature.
func coarseFailureSignature(name, rawArgs, errCode string) string {
	stripped := rawArgs
	var m map[string]any
	if json.Unmarshal([]byte(rawArgs), &m) == nil {
		for _, k := range coarsePaginationFields {
			delete(m, k)
		}
		if b, err := json.Marshal(m); err == nil {
			stripped = string(b)
		}
	}
	return failureSignature(name, stripped, errCode)
}

// cancelledToolStub is the structurally-valid result for a call that never executed
// because the turn was cancelled — used both for the undispatched remainder of a
// batch and for a parallel-group member still queued behind the semaphore when the
// cancel landed.
func cancelledToolStub() domain.ToolResult {
	stub := domain.Fail("CANCELLED", "Turn cancelled.", domain.Unrecoverable())
	stub.Summary = "Turn cancelled before this tool was executed."
	return stub
}

// stubCancelledFrom pushes a structurally-valid CANCELLED tool result for every
// call in calls[from:] (none of which executed), so each assistant tool_call keeps
// a matching tool reply and the transcript replays cleanly. Returns the number of
// calls stubbed (for the cancel trace at the call site).
func (s *Session) stubCancelledFrom(calls []models.ToolCallRequest, from int) int {
	stubbed := 0
	for r := from; r < len(calls); r++ {
		pending := calls[r]
		pendingName := s.resolveInternal(pending.Function.Name)
		s.pushMessage(models.ChatMessage{
			Role:          "tool",
			ToolCallID:    pending.ID,
			Name:          pendingName,
			StringContent: SerializeToolResult(cancelledToolStub(), s.artifacts),
		})
		stubbed++
	}
	return stubbed
}

// classifyBackendError maps a backend respond error to its sentinel reply. The
// prefixes are WAKE_FAILURE_PREFIXES — keep them byte-stable. (Cancellation is
// handled at the call site via ctx.Err() before this runs.)
func (s *Session) classifyBackendError(err error) string {
	if errors.Is(err, context.Canceled) {
		s.events.Phase(domain.PhaseCancelled)
		s.events.AssistantCancelled("")
		return domain.CancelledReply
	}
	var be *backend.Error
	if errors.As(err, &be) && be.IsRateLimited() {
		// Upstream/model rate limit: a friendly, byte-stable reply plus a health
		// badge — not the raw provider blob. The badge clears on the next good Usage.
		msg := "Model rate-limited: " + be.Error()
		s.events.Phase(domain.PhaseFailed)
		s.events.ModelRateLimited()
		s.events.Error(msg)
		return msg
	}
	if errors.As(err, &be) && be.IsConnect() {
		// Backend unreachable — the most common local-dev failure. Name it as a
		// connectivity problem with a next step instead of "Model error: dial tcp
		// ...: connection refused" (a connectivity issue mislabeled as a model one,
		// buried in dialer noise). /doctor probes exactly this. The prefix is a
		// registered WAKE_FAILURE_PREFIX (see wake.go) so a wake that fails this way
		// is still treated as a non-result.
		msg := "Can't reach the Daintree assistant backend — is it running? Use /doctor here, or run 'daintree-assistant doctor' from your shell."
		s.events.Phase(domain.PhaseFailed)
		s.events.Error(msg)
		return msg
	}
	msg := "Model error: " + err.Error()
	s.events.Phase(domain.PhaseFailed)
	s.events.Error(msg)
	return msg
}

// backendAssistantMessage builds the local assistant message for a backend result:
// content null on a pure tool-call turn (no prose), else the visible content.
func backendAssistantMessage(msg backend.RespondMessage) models.ChatMessage {
	calls := backendToolCalls(msg.ToolCalls)
	m := models.ChatMessage{Role: "assistant"}
	if msg.Content == "" && len(calls) > 0 {
		m.ContentNull = true
	} else {
		m.StringContent = msg.Content
	}
	if len(calls) > 0 {
		m.ToolCalls = calls
	}
	// Capture the chain-of-thought so it can be replayed (DeepSeek requires it on a
	// tool-call turn's every later request). Empty when thinking is off.
	m.ReasoningContent = msg.ReasoningContent
	return m
}

// backendToolCalls converts backend tool calls to the local ToolCallRequest shape
// the tool dispatcher consumes.
func backendToolCalls(calls []backend.ToolCall) []models.ToolCallRequest {
	if len(calls) == 0 {
		return nil
	}
	out := make([]models.ToolCallRequest, 0, len(calls))
	for _, c := range calls {
		out = append(out, models.ToolCallRequest{
			ID:       c.ID,
			Type:     "function",
			Function: models.ToolCallFunction{Name: c.Function.Name, Arguments: c.Function.Arguments},
		})
	}
	return out
}

// backendStatePtr returns the latest opaque backend state token to replay on the
// next request, or nil for a fresh session (the backend then re-runs selection).
func (s *Session) backendStatePtr() *string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backendState == "" {
		return nil
	}
	st := s.backendState
	return &st
}

// applyStreamMeta stores the refreshed state token + version markers from the
// committed stream attempt. The CLI treats the state token as opaque
// (store-and-replay only). The token is also mirrored to durable storage
// (best-effort) so a DIFFERENT process — the supervisor daemon picking this session
// up after a detach, or the next cockpit after the daemon — replays the same token
// instead of forcing the backend to re-run skill selection from scratch
// mid-conversation. Newly-loaded skills travel through the eager OnSkillLoaded
// callback instead, so their user-visible cue does not wait for model content.
func (s *Session) applyStreamMeta(m backend.StreamMeta) {
	s.mu.Lock()
	s.backendState = m.State
	s.catalogRevision = m.CatalogRevision
	s.promptVersion = m.PromptVersion
	s.mu.Unlock()
	if s.deps.BackendStateStore != nil {
		// Side-channel: a persistence failure must never break the live stream.
		_ = s.deps.BackendStateStore.PutSessionBackendState(s.deps.SessionID, m.State)
	}
}

// emitSkillLoads surfaces newly-loaded runbooks as a dedicated SkillLoaded event,
// which the cockpit folds into the running turn as an inline "Skill loaded" card.
// It is fed by StreamCallbacks.OnSkillLoaded as soon as the SSE meta arrives, without
// waiting for the first model token. Best-effort and informational only; the prelude
// is NEVER replayed into client history.
func (s *Session) emitSkillLoads(refs []backend.SkillRef) {
	if len(refs) == 0 {
		return
	}
	titles := make([]string, 0, len(refs))
	for _, ref := range refs {
		// Prefer the title; fall back to the id. A ref with NEITHER is malformed —
		// skip it rather than surface a bare card.
		label := strings.TrimSpace(ref.Title)
		if label == "" {
			label = strings.TrimSpace(ref.ID)
		}
		if label == "" {
			continue
		}
		titles = append(titles, label)
	}
	if len(titles) == 0 {
		return
	}
	s.events.SkillLoaded(titles)
}

// emitBackendUsage emits the per-round UsageEvent from the backend's reported usage.
// The backend owns model routing, so this is a single-model figure (no per-tier
// rollup). The backend-reported prompt_tokens IS the true context size, so it both
// feeds ContextTokens and is stashed for the next round's auto-compact gate.
func (s *Session) emitBackendUsage(u backend.Usage, model string) {
	if strings.TrimSpace(model) == "" {
		model = "daintree-assistant"
	}
	ev := UsageEvent{
		ContextThreshold: domain.AutoCompactTokenThreshold,
		ContextWindow:    domain.LargeContextWindowTokens,
		CompactionDepth:  s.CompactionDepth(),
		Tier:             string(domain.ModelLarge),
		Model:            model,
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.CachedTokens > 0 {
		ct := u.CachedTokens
		ev.CachedTokens = &ct
		if u.PromptTokens > 0 {
			r := float64(u.CachedTokens) / float64(u.PromptTokens)
			ev.CacheHitRatio = &r
		}
	}
	s.mu.Lock()
	if u.PromptTokens > 0 {
		s.lastPromptTokens = u.PromptTokens
		ev.ContextTokens = u.PromptTokens
	} else {
		ev.ContextTokens = s.estimateTokensLocked()
	}
	s.mu.Unlock()
	s.events.Usage(ev)
}

// promptContext pulls the app's atomic snapshot once per backend round so the stable
// startup value and fresh runtime tail can never observe different halves of a reconnect.
func (s *Session) promptContext() prompts.MainPromptContext {
	pc := s.deps.PromptContext
	if s.deps.PromptContextFunc != nil {
		pc = s.deps.PromptContextFunc()
	}
	return pc
}

const currentWorktreeReadBudget = time.Second

func (s *Session) currentWorktreeContext(ctx context.Context) *prompts.WorktreeContext {
	if s.deps.CurrentWorktreeFetcher == nil {
		return nil
	}
	cctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(currentWorktreeReadBudget, cancel)
	worktree := s.deps.CurrentWorktreeFetcher(cctx)
	timer.Stop()
	cancel()
	return worktree
}

// buildRuntimeContext maps PromptContext onto the backend's structured runtime contract.
// Stable project/agent details ride only request.startup, so they are not duplicated in
// the backend's fresh runtime tail. The context is pulled live
// every round, so a mid-session MCP connect / tier change / scheduler start reaches the
// next request. The
// openTerminals snapshot is read from the cross-turn cache (currentRoster) fresh EACH
// round — never fetched inline — so a detached refresh (step 3e) that lands mid-turn is
// reflected on the next round without ever blocking the turn on an MCP read.
func (s *Session) buildRuntimeContext(pc prompts.MainPromptContext, openTerminals []backend.OpenTerminal) *backend.RuntimeContext {
	rc := &backend.RuntimeContext{
		PermissionTier:  string(pc.Tier),
		SchedulerActive: pc.SchedulerActive,
		Worktree:        buildCurrentWorktreeSnapshot(pc.Worktree),
		OpenTerminals:   openTerminals,
	}
	if pc.MCPConnected || strings.TrimSpace(pc.MCPStatusLine) != "" {
		// When connected the backend builds its status line from transport + tool_count
		// (Status is only consulted when NOT connected), so send all three.
		rc.MCP = &backend.MCPInfo{
			Connected: pc.MCPConnected,
			Transport: pc.MCPTransport,
			ToolCount: pc.MCPToolCount,
			// Clamp to the backend contract's 64-char limit (backend.MCPInfo.status /
			// extensions.py max_length=64). A DISCONNECTED MCP renders a verbose
			// "not connected — <long transport error>" line that can blow past 64 and, left
			// unclamped, 400s the ENTIRE turn on a schema violation (the model never even
			// runs) instead of degrading to a short status the model can report. Truncate so
			// a flaky/expired MCP produces a graceful turn, not a cryptic hard failure.
			Status: clampRunes(pc.MCPStatusLine, 64),
		}
	}
	return rc
}

// buildTurnContext maps the per-turn facts (formerly the prose turn footer) to the
// backend's structured turn block. The backend renders the footer; the CLI sends
// data. Per-round reads (workflow runs, pinned memories) mirror the old footer's
// freshness; recalled memories + resumed watchers are the per-turn snapshot.
func (s *Session) buildTurnContext(goal string, isWake bool, recalled []domain.MemoryRecord, resumedWatchers []string) *backend.TurnContext {
	tc := &backend.TurnContext{
		Goal:            strings.TrimSpace(goal),
		IsWake:          isWake,
		WorkflowRuns:    workflowRunStrings(s.workflowRunsForFooter()),
		AsyncOperations: asyncInvocationStrings(s.asyncInvocationsForFooter()),
		ResumedWatchers: resumedWatchers,
		WorkflowState:   s.workflowDigestsForTurn(),
	}
	pinned := memoryStrings(s.pinnedMemoriesForFooter())
	relevant := memoryStrings(recalled)
	if len(pinned) > 0 || len(relevant) > 0 {
		tc.Memories = &backend.Memories{Pinned: pinned, Relevant: relevant}
	}
	return tc
}

// memoryStrings flattens memory records to single-line strings for the structured
// turn block (the backend joins them). Blank rows are dropped.
func memoryStrings(rows []domain.MemoryRecord) []string {
	out := make([]string, 0, len(rows))
	for _, m := range rows {
		if c := flattenFooterLine(m.Content); c != "" {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// asyncInvocationStrings renders the live async invocations to single-line
// strings for the structured turn block (the backend renders the section). One
// line carries everything the model needs to reference — id, tool, title,
// watched terminals — so it can cancel or discuss the work without re-listing.
func asyncInvocationStrings(rows []domain.AsyncInvocationRecord) []string {
	out := make([]string, 0, len(rows))
	for i := range rows {
		out = append(out, renderAsyncInvocationRow(rows[i]))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// workflowRunStrings renders the open ledger rows to single-line strings for the
// structured turn block, reusing the same compact one-line formatter the footer
// used (minus the leading bullet, since the backend joins them with ", ").
func workflowRunStrings(runs []domain.WorkflowRunRecord) []string {
	out := make([]string, 0, len(runs))
	for i := range runs {
		out = append(out, strings.TrimPrefix(renderWorkflowRunRow(runs[i]), "- "))
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// resolveTurnTools computes the per-iteration tool filter, its membership set, and
// the projected specs under s.mu — a short, in-memory critical section released
// before the (long) model stream. The lock keeps this read consistent with any
// concurrent UI slash command that touches session state. Returns nil
// allowedNames/allowedSet for an unconstrained (full-registry) turn — which, with
// server-owned skills, is ALWAYS the case (skills never narrow the toolset).
func (s *Session) resolveTurnTools() ([]string, map[string]struct{}, []models.ChatTool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	allowedNames := s.buildToolFilterLocked()
	// Preserve nil semantics: an unconstrained turn (nil) offers the FULL registry,
	// and both the tool-not-offered refusal in runToolBatch and the dispatch gate key
	// off allowedSet being nil ⇒ "all tools callable". Materialize the set only when
	// the turn is actually narrowed (never, with server-owned skills — kept for safety).
	var allowedSet map[string]struct{}
	if allowedNames != nil {
		allowedSet = make(map[string]struct{}, len(allowedNames))
		for _, n := range allowedNames {
			allowedSet[n] = struct{}{}
		}
	}
	tools, err := s.projectToolsLocked(allowedNames)
	if err != nil {
		return nil, nil, nil, err
	}
	return allowedNames, allowedSet, tools, nil
}

// buildToolFilterLocked returns the per-turn tool projection. It ALWAYS returns nil
// (the FULL registry): a loaded skill must NEVER limit which tools the model can call.
// Skills are GUIDANCE — their body suggests which tools to focus on and how to use
// them — never a capability gate. Narrowing the toolset to core ∪ requiredTools (the
// old behaviour) silently made legitimate tools un-callable while a skill was loaded
// (e.g. a relay skill couldn't attach a watcher; a watcher skill couldn't read a
// terminal), which is exactly wrong: the right tool for the next step must always be
// reachable. A skill's `requiredTools` is now metadata only (a focus hint + a
// startup sanity check that the named tools exist) — it does not constrain the turn.
// Caller MUST hold s.mu (kept for call-site symmetry; this body reads no shared state).
func (s *Session) buildToolFilterLocked() []string {
	return nil
}

// projectToolsLocked returns the OpenAITools projection for allowedNames,
// reusing the cached projection when the offered toolset is unchanged since the last
// build. allowedNames==nil is the full (unconstrained) registry — a distinct cache
// identity from any narrowed set, tracked via the unconstrained flag because
// slices.Equal treats nil and []string{} as equal. With server-owned skills the
// offered toolset is the full registry on EVERY turn, so in practice the cache is
// populated once (the unconstrained branch) and reused for the process — this skips
// re-projecting every tool spec and rebuilding the registry's wire-name maps each
// round. Caller MUST hold s.mu (it reads/writes the toolProj cache).
//
// On a cache HIT the registry's wire-name alias maps are NOT rebuilt: the
// OpenAITools call that populated the cache for THIS exact projection left them
// correct. There is exactly one agent.Session in the process and OpenAITools is
// only ever called from this turn loop, so no other goroutine overwrites those
// shared maps between here and runToolBatch's ResolveWireName lookups (which run
// later on the same turn goroutine).
func (s *Session) projectToolsLocked(allowedNames []string) ([]models.ChatTool, error) {
	c := &s.toolProj
	unconstrained := allowedNames == nil
	if c.valid && c.unconstrained == unconstrained &&
		(unconstrained || slices.Equal(c.allowedNames, allowedNames)) {
		return c.tools, nil
	}
	tools, err := s.deps.Tools.OpenAITools(allowedNames)
	if err != nil {
		return nil, err
	}
	*c = toolProjCache{
		valid:         true,
		unconstrained: unconstrained,
		// Copy the key so a later in-place mutation of the offered-names slice (it is
		// also handed to turn.ActiveToolNames) can't corrupt the cache identity.
		allowedNames: append([]string(nil), allowedNames...),
		tools:        tools,
	}
	return tools, nil
}

// resolveInternal maps a wire name back to its internal name, falling through to
// the raw name when the registry doesn't resolve it (ResolveWireName ?? raw).
func (s *Session) resolveInternal(wireName string) string {
	if n := s.deps.Tools.ResolveWireName(wireName); n != "" {
		return n
	}
	return wireName
}

// snapshotMessages returns an immutable copy of the live history (under the lock)
// for the streaming model call, so the turn never streams off the mutable slice
// while a concurrent InjectNote appends to it.
func (s *Session) snapshotMessages() []models.ChatMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]models.ChatMessage, len(s.messages))
	copy(out, s.messages)
	return out
}

// workflowRunsForFooter reads the open (non-terminal) ledger rows for this round's
// turn footer, best-effort: a nil lister (the test/feature-off default) or any DB
// error both yield nil, so the footer simply omits the workflow block rather than
// failing the turn it rides on. Bounded by activeWorkflowRunsLimit so the per-round
// read never fetches more rows than the footer can show.
func (s *Session) workflowRunsForFooter() []domain.WorkflowRunRecord {
	if s.deps.WorkflowRunLister == nil {
		return nil
	}
	runs, _ := s.deps.WorkflowRunLister.ListNonTerminalWorkflowRuns(activeWorkflowRunsLimit)
	return runs
}

// asyncInvocationsForFooter reads the live async invocations for this round's
// turn context, best-effort: a nil lister or any DB error yields nil, so the
// block is simply omitted rather than failing the turn. Re-read per round (like
// workflowRunsForFooter) so a completion/cancel surfaces on the very next round.
func (s *Session) asyncInvocationsForFooter() []domain.AsyncInvocationRecord {
	if s.deps.AsyncInvocationLister == nil {
		return nil
	}
	rows, _ := s.deps.AsyncInvocationLister.ListLiveAsyncInvocations()
	if len(rows) > activeAsyncOperationsLimit {
		rows = rows[:activeAsyncOperationsLimit]
	}
	return rows
}

// workflowDigestsForTurn reads the open workflow-graph digests for this round's
// turn context, best-effort: a nil lister (the default, and always when
// DAINTREE_WORKFLOW_INTELLIGENCE is off) or any read failure yields nil, so the
// workflow_state block is simply omitted — the wire stays byte-identical to the
// pre-feature request and a backend without the matching contract never sees
// the field. The lister itself clamps and caps to the wire contract.
func (s *Session) workflowDigestsForTurn() []backend.WorkflowDigest {
	if s.deps.WorkflowDigestLister == nil {
		return nil
	}
	return s.deps.WorkflowDigestLister.WorkflowDigests(backend.MaxWorkflowDigests)
}

// pinnedMemoriesForFooter reads the current pinned project memories for this round's
// footer, best-effort: a nil lister (the test/feature-off default) or any DB error both
// yield nil, so the footer simply omits the pinned subblock rather than failing the turn.
// Read per round (like workflowRunsForFooter, unlike the per-turn recall) so a memory.pin
// landing mid-turn surfaces on the very next round — which is why the migration off
// message[1] needs no RefreshRuntimeContext on pin. Bounded by pinnedMemoriesMaxRows so
// the read never fetches more rows than the subblock can show.
func (s *Session) pinnedMemoriesForFooter() []domain.MemoryRecord {
	if s.deps.PinnedMemoryLister == nil {
		return nil
	}
	rows, _ := s.deps.PinnedMemoryLister.ListPinnedMemories(pinnedMemoriesMaxRows)
	return rows
}

// pushMessage appends to the live history and persists (best-effort), under the
// lock so a concurrent reader (Messages/estimateTokens) never sees a torn slice.
func (s *Session) pushMessage(m models.ChatMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pushMessageLocked(m)
}

// pushMessageLocked appends + persists; caller MUST hold s.mu.
func (s *Session) pushMessageLocked(m models.ChatMessage) {
	s.messages = append(s.messages, m)
	s.persistMessageLocked(m)
}

// persistMessage best-effort writes a conversation row under the lock (it reads +
// bumps s.seq). Used by NewSession (no concurrency yet, but harmless).
func (s *Session) persistMessage(m models.ChatMessage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistMessageLocked(m)
}

// persistMessageLocked writes a conversation row (swallows errors — a DB failure
// must never break a live turn). Caller MUST hold s.mu (it reads/bumps
// s.seq).
func (s *Session) persistMessageLocked(m models.ChatMessage) {
	defer func() { _ = recover() }()
	if s.deps.Store == nil {
		return
	}
	var toolCallsJSON *string
	if len(m.ToolCalls) > 0 {
		if b, err := json.Marshal(m.ToolCalls); err == nil {
			j := string(b)
			toolCallsJSON = &j
		}
	}
	var toolCallID *string
	if m.ToolCallID != "" {
		id := m.ToolCallID
		toolCallID = &id
	}
	// Persist the chain-of-thought so a resumed session replays it verbatim (a
	// tool-call turn missing it 400s once thinking is on). Empty when thinking is off.
	var reasoning *string
	if m.ReasoningContent != "" {
		r := m.ReasoningContent
		reasoning = &r
	}
	rec := domain.ConversationMessageRecord{
		SessionID:        s.deps.SessionID,
		Seq:              s.seq,
		Role:             m.Role,
		Content:          m.ContentToText(),
		ReasoningContent: reasoning,
		ToolCallsJson:    toolCallsJSON,
		ToolCallID:       toolCallID,
	}
	s.seq++
	_, _ = s.deps.Store.InsertMessage(rec)
}

// estimateTokens approximates the conversation size (dependency-free):
// sum of each message's flattened-text length + tool-call argument JSON length,
// divided by CHARS_PER_TOKEN and ceil'd. Approximate by design.
func (s *Session) estimateTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.estimateTokensLocked()
}

// estimateTokensLocked sums message text + tool-call arg JSON; caller MUST hold s.mu.
func (s *Session) estimateTokensLocked() int {
	return estimateMessagesTokens(s.messages)
}

// estimateMessagesTokens approximates the token size of a message slice the same way
// estimateTokensLocked does (text + tool-call arg JSON, / CharsPerToken, ceil'd).
// Lock-free and operates only on the passed slice, so truncateLocked can size a
// prospective retained tail before committing it to s.messages.
func estimateMessagesTokens(msgs []models.ChatMessage) int {
	chars := 0
	for _, m := range msgs {
		chars += charLen(m.ContentToText())
		for _, tc := range m.ToolCalls {
			chars += charLen(tc.Function.Arguments)
		}
	}
	return int(math.Ceil(float64(chars) / float64(domain.CharsPerToken)))
}

// keepValidTail selects the most-recent keepN messages from a working slice and returns
// a copied, model-valid tail bounded by tokenBudget. It is the shared recency-tail logic
// behind BOTH the healthy auto-compact path (which keeps a verbatim tail after the model
// summary) and the lossy truncation fallback. Pure and lock-free — it only reads the
// passed slice and returns a fresh copy, so a caller can size and clean a prospective tail
// before committing it to s.messages. Steps:
//   - take the last keepN messages, then back the start up over any leading tool results
//     so the keepN cut never lands mid tool-batch (which would orphan results whose
//     declaring assistant sits just before the window);
//   - copy, so the cleanup passes and the caller's re-append never alias the s.messages
//     backing array the caller is about to overwrite;
//   - drop orphaned tool results, then an incomplete trailing tool call, exactly as a
//     resume would, so the tail is a valid history DeepSeek won't reject;
//   - shed from the head (oldest first), re-cleaning orphans after each drop, until the
//     estimate is at or under tokenBudget — guaranteeing the bound even when a single
//     retained message is itself larger than the budget.
//
// Returns an empty (non-nil-safe) slice when there is nothing to keep — empty input, a
// non-positive keepN, a non-positive budget, or a tail shed away entirely.
func keepValidTail(msgs []models.ChatMessage, keepN, tokenBudget int) []models.ChatMessage {
	if len(msgs) == 0 || keepN <= 0 || tokenBudget <= 0 {
		return nil
	}
	working := msgs
	if len(working) > keepN {
		// The keepN cut can land mid tool-batch — the window's leading messages would be
		// tool results whose declaring assistant sits just before the window, and the
		// orphan pass below would silently shed them (the WHOLE tail for a single >keepN
		// batch). Back the start up over any leading tool messages so their declaring
		// assistant is included; the budget-shed still trims the whole group if it can't
		// fit. Bounded by one tool-group (or index 0).
		start := len(msgs) - keepN
		for start > 0 && msgs[start].Role == "tool" {
			start--
		}
		working = msgs[start:]
	}
	tail := make([]models.ChatMessage, len(working))
	copy(tail, working)
	// The drop counts are irrelevant here (this path sheds history without surfacing a
	// corruption note); discard them. Order matters: orphan results first, then the
	// incomplete trailing call — matching the resume cleanup order.
	tail, _ = dropOrphanToolResults(tail)
	tail, _ = dropOrphanToolCallTail(tail)
	for len(tail) > 0 && estimateMessagesTokens(tail) >= tokenBudget {
		tail = tail[1:]
		tail, _ = dropOrphanToolResults(tail)
		tail, _ = dropOrphanToolCallTail(tail)
	}
	return tail
}

// distillBackgroundTimeout bounds the detached post-compaction distill call so a
// hung or slow small model can never pin the background goroutine indefinitely — the
// work is best-effort and not load-bearing for the turn.
const distillBackgroundTimeout = 30 * time.Second

// maybeAutoCompact summarizes the conversation with the small model when it has grown
// past the token threshold, replacing the working history with a short note. The
// summary call is on the turn's critical path (its result IS the compacted note), but
// the follow-on distillation is fired on a DETACHED goroutine so the large-model
// stream starts immediately instead of stalling on a second small-model round-trip.
// Best-effort: a successful summary compacts; a SUSTAINED summary outage falls back to
// a no-model lossy truncation so history can't grow unbounded (issue #202).
func (s *Session) maybeAutoCompact(ctx context.Context, runID string) {
	// Build the summary input under the lock (read a stable snapshot), then run the
	// model call OUTSIDE the lock. Runs on the turn goroutine with inFlight set, so
	// it compacts via compactLocked (the public Compact would self-reject).
	s.mu.Lock()
	// Gate on the LARGER of the real provider figure and the live char estimate. The
	// real prompt_tokens from the prior round (lastPromptTokens) is the honest size — it
	// counts the tool schemas the chars/4 estimate is blind to — but it was measured as
	// of the last model call, so content appended SINCE (this round's tool results, a
	// daemon InjectNote) is invisible to it. The char estimate measures the CURRENT
	// history, so taking the max means a large mid-round injection still trips the gate
	// while the real figure governs the steady state. Before the first round, or right
	// after a reset zeroed it, lastPromptTokens is 0 and the estimate alone applies.
	est := s.estimateTokensLocked()
	if s.lastPromptTokens > est {
		est = s.lastPromptTokens
	}
	// Lossless pre-sweep (issue #257): before paying for a small-model summary, run a
	// cheap deterministic pass that dedups byte-identical tool results and collapses
	// already-archived overflow stubs to their artifact placeholder. Only re-estimate
	// when it actually rewrote something — a no-op sweep leaves est untouched. The
	// re-estimate is the honest char count of the swept history (NOT re-maxed with
	// lastPromptTokens, which described the pre-sweep prompt with the now-removed
	// previews/duplicates); the gate just below then skips the summary entirely when the
	// sweep alone dropped the history back under the soft threshold.
	if est > domain.AutoCompactTokenThreshold {
		if n := runPreSweep(s.messages); n > 0 {
			est = s.estimateTokensLocked()
		}
	}
	// Skip when under the soft threshold, OR when there's no real history to
	// summarize (≤1 working message) — UNLESS that lone message is itself over the
	// hard ceiling, in which case the bounded-growth fallback must still get a chance
	// to run rather than letting a single oversized message grow context unbounded.
	if est <= domain.AutoCompactTokenThreshold ||
		(len(s.messages) <= domain.ControlMessageCount+1 && est < domain.AutoCompactHardTruncationThreshold) {
		s.mu.Unlock()
		return
	}
	// Capture a flattened transcript of the about-to-be-discarded history (still under
	// the lock). The backend's checkpoint task OWNS the prompt; the CLI sends only
	// this transcript. Each tool call's name + argument JSON is folded into the text so
	// load-bearing IDs that live ONLY in arguments — e.g. terminal.read
	// {"terminalId":"term_x"} — survive into the checkpoint's ID-preservation pass.
	transcript := ""
	for _, m := range s.messages[domain.ControlMessageCount:] {
		text := m.ContentToText()
		for _, tc := range m.ToolCalls {
			if text != "" {
				text += "\n"
			}
			text += "[tool call " + tc.Function.Name + " " + tc.Function.Arguments + "]"
		}
		if m.Role == "tool" {
			if text == "" {
				text = "[tool result]"
			} else {
				text = "[tool result] " + text
			}
		}
		if text == "" {
			text = "[tool call]"
		}
		transcript += m.Role + ": " + text + "\n"
	}
	s.mu.Unlock()

	// Run the backend's checkpoint task. On an ERROR (not a reply): a cancel is the
	// turn tearing down (don't compact, don't count it — issue #61), a real outage
	// counts toward the bounded-growth truncation fallback (issue #202). A successful
	// result always compacts — even a sparse checkpoint, because validateCheckpoint still
	// mines every load-bearing ID from the transcript into PreservedIDs.
	cp, err := buildCheckpoint(ctx, s.deps.Backend, transcript)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		truncated, failures := s.noteCompactFailureLocked()
		s.mu.Unlock()
		if truncated {
			s.events.Info("Auto-compact fallback: truncated old history (checkpoint unavailable)")
		} else if failures == 1 {
			// maybeAutoCompact runs once per tool-iteration round, so a checkpoint that
			// keeps failing (a real model outage at high context) would re-enter HERE every
			// round and, without this gate, repaint the same skip note on every one —
			// flooding the live footer while a turn is in flight. Surface it ONCE per failure
			// streak (the 0→1 transition); a successful compaction resets the streak, so a
			// later relapse is announced afresh, and the truncation branch above always
			// announces its own (rarer, history-mutating) note.
			s.events.Info("Auto-compact skipped: checkpoint failed")
		}
		return
	}
	summary := renderCheckpoint(cp)

	// Checkpoint in hand: compact IMMEDIATELY so the large-model stream is unblocked,
	// then distill durable facts off the critical path. compactLocked resets the
	// failure streak so one transient outage never permanently disarms the soft path.
	s.mu.Lock()
	// Snapshot the most-recent working messages BEFORE compactLocked reslices history to
	// controls + summary, then re-append them after — so the healthy path keeps a verbatim
	// recent tail instead of collapsing to summary-only. The model summary rounds off the
	// exact references a mid-task orchestrator still needs (terminal/run/watcher/workflow
	// IDs, the active branch, an open grant); the raw tail keeps them intact. The snapshot
	// is taken under this SAME post-summary lock, so any InjectNote that raced in during the
	// (lock-free) model call is included. keepValidTail copies + orphan-cleans, so the
	// re-appended tail never aliases the reslice and is a valid history DeepSeek won't
	// reject. compactLocked still zeroes lastPromptTokens — the tail is small (≤ the budget),
	// so the post-compaction char estimate stays well under the gate and won't re-trip it.
	tail := keepValidTail(s.messages[domain.ControlMessageCount:],
		domain.AutoCompactVerbatimTailMessages, domain.AutoCompactVerbatimTailTokenBudget)
	s.compactLocked(summary)
	for _, m := range tail {
		s.messages = append(s.messages, m)
		s.persistMessageLocked(m)
	}
	s.mu.Unlock()
	s.events.Info("Auto-compacted conversation")

	s.startDistill(runID, transcript)
}

// noteCompactFailureLocked records a failed/empty auto-compact summary and, once
// failures have been SUSTAINED (>= AutoCompactFailureThreshold) AND the history has
// ballooned past the hard ceiling, performs a no-model lossy head-truncation so a
// small-model outage can't grow the conversation without bound. Resets the counter
// after truncating (the bound has been re-established). Caller MUST hold s.mu.
// Returns (truncated, failures): truncated is true iff it head-truncated, and failures is
// the post-increment consecutive-failure count so the caller can surface the skip note
// only on the FIRST failure of a streak instead of every round. After a truncation the
// streak has reset, so failures is reported as 0.
func (s *Session) noteCompactFailureLocked() (truncated bool, failures int) {
	s.compactFailures++
	if s.compactFailures >= domain.AutoCompactFailureThreshold &&
		s.estimateTokensLocked() >= domain.AutoCompactHardTruncationThreshold {
		s.truncateLocked(domain.AutoCompactHardTruncationKeepMessages) // resets compactFailures
		return true, 0
	}
	return false, s.compactFailures
}

// truncateLocked is the no-model lossy fallback for a sustained small-model outage:
// keep the three control messages plus at most the most-recent keepN working
// messages (oldest dropped first), then shed further from the head of that tail until
// the estimate is back under the hard ceiling — guaranteeing the bound even when a
// single retained message is itself enormous. Orphaned tool results and an incomplete
// trailing tool call are cleaned exactly as a resume would, so the retained tail is a
// valid model history DeepSeek won't reject. A compaction marker is persisted (so a
// later resume rebuilds from the truncation boundary) followed by each retained
// message at a fresh monotonic seq. Caller MUST hold s.mu.
func (s *Session) truncateLocked(keepN int) {
	// The bound is being re-established by this truncation, so the failure streak that
	// armed it resets here (the single reset point for the fallback path). The stashed
	// real prompt_tokens described the pre-truncation history; zero it so the next
	// compaction check measures the shrunk tail (via the char estimate) rather than the
	// stale over-ceiling figure.
	s.compactFailures = 0
	s.lastPromptTokens = 0
	// Keep at most keepN recent messages, cleaned and shed back under the hard ceiling.
	// The controls are fixed overhead, so the tail's own budget is the ceiling minus their
	// contribution — keepValidTail then bounds the tail against that. keepValidTail copies
	// before any reslice, so the re-append below never aliases the backing array we are
	// about to overwrite.
	controlTokens := estimateMessagesTokens(s.messages[:domain.ControlMessageCount])
	tail := keepValidTail(s.messages[domain.ControlMessageCount:], keepN, domain.AutoCompactHardTruncationThreshold-controlTokens)
	s.messages = s.messages[:domain.ControlMessageCount]
	s.persistMessageLocked(models.TextMessage("system", compactionMarker))
	for _, m := range tail {
		s.messages = append(s.messages, m)
		s.persistMessageLocked(m)
	}
}

// currentRoster returns the cached open-terminal snapshot under rosterMu — a shallow
// copy so a caller can hold it past the lock while a concurrent refresh swaps the field.
// nil when no refresh has completed yet (a fresh session's cold start).
func (s *Session) currentRoster() []backend.OpenTerminal {
	s.rosterMu.Lock()
	defer s.rosterMu.Unlock()
	if len(s.roster) == 0 {
		return nil
	}
	return append([]backend.OpenTerminal(nil), s.roster...)
}

// WarmOpenTerminals starts the same detached roster refresh normally kicked at turn
// start. App.ConnectMcp calls it during the splash so the first user request can already
// carry terminal metadata; the existing in-flight gate makes repeated connect/bootstrap
// calls harmless.
func (s *Session) WarmOpenTerminals() {
	s.refreshRosterAsync()
}

// refreshRosterAsync starts a detached, best-effort refresh of the cached open-terminal
// roster unless one is already running (deduped via rosterRefreshing), so the turn loop
// NEVER blocks on the terminal.list + getStatus MCP round-trip. The result replaces the
// cache the moment it lands, so the next round's runtime block (rebuilt every round)
// picks it up; the turn itself streams immediately against whatever snapshot the last
// refresh left. Tracked by s.wg + the draining gate exactly like startDistill so
// App.Shutdown joins it before the MCP client it touches is closed. It parents off bgCtx
// (NOT the turn ctx) so a short or cancelled turn's refresh still completes and warms the
// cache for the next turn; the fetcher self-bounds via its own cancel timer, and bgCtx
// must be passed WITHOUT a deadline (mcp.Client tears the connection down on a
// DeadlineExceeded — only a plain cancel is a safe best-effort abort).
func (s *Session) refreshRosterAsync() {
	if s.deps.OpenTerminalsFetcher == nil {
		return
	}
	// Dedupe: at most one refresher in flight. A slow fetch spanning two turns must not
	// stack a second — the next turn simply reuses the in-flight one and reads the cache.
	s.rosterMu.Lock()
	if s.rosterRefreshing {
		s.rosterMu.Unlock()
		return
	}
	s.rosterRefreshing = true
	s.rosterMu.Unlock()

	// Clear the in-flight flag on EVERY exit path (draining bail-out included) so a
	// refusal here can never wedge the dedupe flag true and starve all later refreshes.
	clearInFlight := func() {
		s.rosterMu.Lock()
		s.rosterRefreshing = false
		s.rosterMu.Unlock()
	}

	// Register under s.mu, and ONLY when not draining — the same wg.Add/draining ordering
	// startDistill uses so wg.Add never races DrainBackgroundWork's Wait at counter zero
	// (the turn ctx isn't derived from bgCtx, so this can be reached mid-Shutdown).
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		clearInFlight()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		defer clearInFlight()
		fresh := s.deps.OpenTerminalsFetcher(s.bgCtx)
		// Replace unconditionally (nil included): the fetcher returns nil both for a
		// transient failure AND for a genuinely-empty roster (all terminals closed), and
		// the model must not keep seeing terminals that are gone. This mirrors the old
		// synchronous behaviour (each turn showed exactly its own fetch result); a
		// transient blip simply blanks the roster for a round and self-heals on the next
		// refresh — a false negative the model recovers from, never a stale false positive.
		s.rosterMu.Lock()
		s.roster = fresh
		s.rosterMu.Unlock()
	}()
}

// startDistill mines durable facts from the just-discarded transcript on a DETACHED
// goroutine so the user's turn streams immediately. It parents off the app-scoped
// bgCtx (cancelled on App.Shutdown) with a bounded timeout, and is tracked by s.wg so
// DrainBackgroundWork can join it before the Router/MemoryStore it touches tear down.
// A nil MemoryStore makes distillCompact a no-op, so skip the goroutine entirely.
// runID is captured by value into the detached goroutine and stamped as provenance
// on each distilled memory (empty ⇒ left unstamped).
// IMPORTANT: it must NOT emit events — the prod RunEventSink is single-goroutine
// (turn-only) and unguarded, so an Info call from here would race the streaming turn.
func (s *Session) startDistill(runID, transcript string) {
	if s.deps.MemoryStore == nil {
		return
	}
	bg, cancel := context.WithTimeout(s.bgCtx, distillBackgroundTimeout)
	// Register under s.mu and ONLY when not draining, so wg.Add is ordered against the
	// draining flag that DrainBackgroundWork sets before wg.Wait — closing the
	// Add-after-Wait race (the turn ctx isn't derived from bgCtx, so a summary can
	// succeed mid-Shutdown and reach here even after baseCancel).
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		cancel()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer cancel()
		s.distillCompact(bg, runID, transcript)
	}()
}

// DrainBackgroundWork closes the spawn gate, then blocks until all in-flight detached
// background work (the post-compaction distill goroutine) has finished. App.Shutdown
// calls it before closing the Router/Store those goroutines touch; tests call it to
// make the otherwise-async distill observable. Terminal: once drained, no further
// distill is spawned (the session is being torn down). Safe to call when nothing is
// in flight (a no-op Wait).
func (s *Session) DrainBackgroundWork() {
	s.mu.Lock()
	s.draining = true
	s.mu.Unlock()
	s.wg.Wait()
}

// distillCompact extracts durable facts from a soon-to-be-discarded transcript via
// the backend's memory_distill task and saves the novel ones as source="compact"
// memories. Best-effort by construction: a nil MemoryStore/Backend, an empty
// transcript, a backend error, or any panic yields 0 and never affects compaction. It
// MUST be called with s.mu released (it makes a network call + DB writes).
func (s *Session) distillCompact(ctx context.Context, runID, transcript string) (saved int) {
	defer func() { _ = recover() }()
	if s.deps.MemoryStore == nil || s.deps.Backend == nil {
		return 0
	}
	// Keep the freshest TAIL — durable decisions are most likely near the end of the
	// conversation, while the head is the part most likely already summarized away.
	if r := []rune(transcript); len(r) > domain.DistillTranscriptMaxRunes {
		transcript = string(r[len(r)-domain.DistillTranscriptMaxRunes:])
	}
	if trimSpace(transcript) == "" {
		return 0
	}
	out, err := backend.RunMemoryDistill(ctx, s.deps.Backend, backend.MemoryDistillInput{Transcript: transcript})
	if err != nil {
		return 0
	}
	for _, fact := range out.Facts {
		content := trimSpace(fact.Fact)
		if content == "" {
			continue
		}
		// Route each fact to its kind: semantic (a durable fact) vs episodic (an
		// instructive trajectory trace). An unknown/blank kind defaults to semantic.
		kind := domain.MemoryKindSemantic
		if fact.Kind == string(domain.MemoryKindEpisodic) {
			kind = domain.MemoryKindEpisodic
		}
		exists, exErr := s.deps.MemoryStore.MemoryExists(content)
		if exErr != nil || exists {
			continue
		}
		now := domain.NowMS()
		// Stamp the turn that produced it as provenance; namespace episodic rows to this
		// session (semantic facts carry no sessionId).
		rec := domain.MemoryRecord{
			Content:   content,
			Source:    domain.MemoryCompact,
			Kind:      kind,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if runID != "" {
			rec.RunID = &runID
		}
		if kind == domain.MemoryKindEpisodic && s.deps.SessionID != "" {
			sid := s.deps.SessionID
			rec.SessionID = &sid
		}
		if _, insErr := s.deps.MemoryStore.InsertMemory(rec); insErr == nil {
			saved++
		}
	}
	return saved
}

// Artifacts exposes the session's overflow store so the artifact.read tool family
// can page back through oversized tool results stashed during the turn. The store
// is created in NewSession and lives for the session.
func (s *Session) Artifacts() *ArtifactStore { return s.artifacts }

// --- small helpers ---

func setHas(set map[string]struct{}, k string) bool { _, ok := set[k]; return ok }

func failureSignature(name, rawArgs, errCode string) string {
	b, _ := json.Marshal([]string{name, canonicalJSON(rawArgs), errCode})
	return string(b)
}

// canonicalJSON normalizes a JSON arguments string so two semantically-identical
// calls that differ only in key order or whitespace hash the same: parse then
// re-marshal (Go's encoder sorts object keys). Non-JSON input passes through
// unchanged. This keeps the circuit breaker counting a repeated-same-way failure
// even when the model re-emits the call with reordered keys.
func canonicalJSON(raw string) string {
	if raw == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	b, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return string(b)
}

// trimSpace trims leading/trailing ASCII+unicode whitespace (avoids importing
// strings just for this in the hot path that already uses encoding/json).
func trimSpace(s string) string {
	start := 0
	for start < len(s) && isSpace(s[start]) {
		start++
	}
	end := len(s)
	for end > start && isSpace(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\v' || b == '\f'
}
