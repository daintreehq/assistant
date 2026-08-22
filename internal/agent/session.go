package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
	"github.com/daintreehq/assistant/internal/prompts"
	"github.com/daintreehq/assistant/internal/waitbudget"
)

// coreToolNames are the essential tools asserted to be registered at boot
// (app.go's AssertRegistered("core tools")). EVERY turn now offers the FULL registry
// — a loaded skill never narrows the toolset (see buildToolFilterLocked) — so this is
// no longer a per-turn projection key; it is just the always-must-exist set. Internal
// dotted names.
//
// It is ALSO the floor the autonomous wake prompt is pinned against
// (TestBuildWakePromptNamesOnlyCoreTools): a wake turn must be able to call every tool
// its prompt TELLS IT TO CALL, and it may run with no relevant skill active. That pin is
// LOCAL —
// nothing transports this list to the backend, so it does not itself constrain the
// backend's tool projection; it is the CLI-side declaration such a floor would adopt.
var coreToolNames = []string{
	"context.snapshot",
	"fs.read",
	"fs.list",
	"fs.search",
	"queue.digest",
	// queue.resolve is core: the autonomous wake prompt tells the reactor to clear a
	// handled inbox item with it — the watcher branch's hygiene line, the async
	// completion guidance, and the daemon's unattended note all name it literally. A
	// wake must work with NO relevant skill active, so the prompt cannot lean on a
	// skill to reintroduce it; without it a handled item keeps the attention badge lit
	// until some other path resolves or clears it. RiskLocal, so every tier allows it and
	// it needs no confirmation.
	"queue.resolve",
	"daintree.status",
	"tool.search",
	// tool.schema is core alongside tool.search: the discovery tools' note now
	// tells the model to look up an argument shape with it, so it must never be
	// possible to ship a build where that pointer names an absent tool.
	"tool.schema",
	"terminal.read",
	// terminal.summarize is core: it is the DEFAULT the wake prompt names for reading a
	// finished agent's output (raw scrollback may be garbled, repainted TUI output), in
	// both the watcher
	// and the async branch. It is the first read an autonomous wake is told to reach for,
	// and that wake must work with no relevant skill active. RiskRead, so no confirmation
	// gate.
	"terminal.summarize",
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
	// pinWarningsSeen latches the pin-failure warning codes already surfaced, so a
	// multi-round turn (and every later turn of the session) reports each cause ONCE.
	// The condition is a property of the pin list, which is session-constant: repeating
	// "that id is unknown" on every round would bury the tool activity the operator is
	// actually reading. Lazily allocated and guarded by s.mu. Deliberately NOT reset by
	// /clear or compaction — the pins survive both, so the warning is still stale news.
	pinWarningsSeen map[string]struct{}

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

	// rosterFetchedAt is when the fetch behind the cached roster STARTED (zero ⇒ never
	// fetched). It exists so a round can refuse to serve a snapshot it cannot claim is
	// live: currentRosterForRound omits any roster older than rosterSnapshotMaxAge
	// rather than assert it as fact (issue #334). Stamped from the start of the ACCEPTED
	// fetch attempt, not from its commit, so a slow fetch arrives already carrying its
	// true age — terminal.list captures membership at the head of the fetcher, and
	// dating the snapshot from the commit would understate its age by the whole
	// round-trip, the unsafe direction under a serving cap. A discarded (gen-mismatched)
	// attempt never stamps, and a local prune (pruneRosterTerminals) deliberately does
	// not re-stamp: dropping a confirmed-closed id makes the snapshot safer but does not
	// recapture the entries that remain. Guarded by rosterMu.
	rosterFetchedAt time.Time

	// rosterRefreshing dedupes concurrent refreshers so a slow fetch spanning two turns
	// cannot stack a second one behind it. Guarded by rosterMu.
	rosterRefreshing bool

	// rosterRefreshDone is closed when the in-flight refresh reaches its FINAL outcome —
	// a gen-stable commit, or a definitive abandonment — and is nil when none is in
	// flight. The gen-mismatch discard loop inside refreshRosterAsync must NOT close it
	// per attempt: a discarded attempt decides nothing, and releasing a waiter on it
	// would hand back the very pre-mutation cache the discard exists to reject. Round 0
	// waits on it (bounded by rosterRoundZeroGrace) so the turn-start refresh already in
	// flight gets a moment to land before the request asserts a roster — the race behind
	// issue #334. Guarded by rosterMu.
	rosterRefreshDone chan struct{}

	// rosterGen is bumped by every LOCAL roster mutation (a terminal.close pruning ids,
	// a spawn adding terminals) so an in-flight detached fetch that STARTED before the
	// mutation cannot land afterwards and resurrect state the session itself just
	// changed — the exact failure seen 2026-07-11 (ses_d33fa2d8): the turn-start fetch
	// listed 6 terminals, the turn then closed 5, the stale fetch result sat in the
	// cache for the next turn and the model reported the close "didn't stick". The
	// refresher snapshots rosterGen before fetching and discards (and refetches) on
	// mismatch. Guarded by rosterMu.
	rosterGen uint64

	// rosterTombstones maps every terminal id this session has CONFIRMED closed (a
	// terminal.close result's `closed` list) to the tombstone's expiry. rosterGen only
	// guards against fetches that started BEFORE a local mutation; it cannot help when
	// the fetch starts after the mutation but reads a server that hasn't caught up —
	// Daintree acks terminal.close before the teardown reaches terminal.list, so the
	// reconciliation fetch kicked right after a close can read a pre-close list and
	// resurrect the ids the prune just removed (seen 2026-07-11, ses_a9e0a6ef: the
	// kicked fetch landed 2ms after the close acks and re-committed all 6 closed
	// terminals, which the next turn's round 0 then served to the model). Every fetch
	// commit filters against this set, so a confirmed close stays closed regardless of
	// server-side lag. Tombstones EXPIRE (rosterTombstoneTTL) rather than live for the
	// session: close moves a terminal to Daintree's TRASH (terminal.kill is the
	// permanent delete), so the same id can legitimately reappear if a human restores
	// it — the TTL is orders of magnitude above the observed settle lag yet lets a
	// restored terminal surface on the first refresh after expiry. Lazily allocated;
	// expired entries are deleted at each fetch commit (dropTombstoned), bounding the
	// map by RECENT closes. Guarded by rosterMu.
	rosterTombstones map[string]time.Time

	// worktreeMu guards the cached current-worktree snapshot below — the same
	// dedicated-mutex reasoning as rosterMu (the detached refresher must never block
	// on a turn holding s.mu, and vice versa; critical sections are pointer swaps).
	worktreeMu sync.Mutex

	// worktreeSnap is the most recent completed CurrentWorktreeFetcher result, served
	// to every round's runtime block WITHOUT an inline MCP read (the roster pattern).
	// nil is a faithful cache of a FAILED read ("unknown", exactly what the old
	// synchronous path injected when its 1s budget expired) — never a stale prior
	// selection masquerading as current. Guarded by worktreeMu.
	worktreeSnap *prompts.WorktreeContext

	// worktreeFetchedAt is when the last refresh COMPLETED (zero ⇒ never fetched).
	// Consulted-at-send-time staleness beyond worktreeSnapshotTTL triggers a detached
	// refresh; the round itself always proceeds on the cached value. Guarded by
	// worktreeMu.
	worktreeFetchedAt time.Time

	// worktreeRefreshing dedupes concurrent worktree refreshers (single-flight),
	// mirroring rosterRefreshing. Guarded by worktreeMu.
	worktreeRefreshing bool

	// worktreeRefreshDone is closed when the in-flight refresh lands (nil when none
	// is in flight). It exists ONLY for the first-ever fetch: a cold cache waits up
	// to worktreeFirstFetchGrace on it so the very first round usually still carries
	// a worktree, without ever blocking on a slow MCP. Guarded by worktreeMu.
	worktreeRefreshDone chan struct{}

	// worktreeGraceElapsed latches true once a cold-cache consult has paid the FULL
	// worktreeFirstFetchGrace without the fetch landing. From then on, consults that
	// still find a cold cache (the same slow first fetch, later rounds) proceed
	// immediately — the grace is a one-shot first-round courtesy, never a per-round
	// tax while a degraded MCP read (seconds) is in flight. A grace wait that the
	// fetch DID beat does not latch (nothing was wasted, and the cache is warm
	// anyway). Guarded by worktreeMu.
	worktreeGraceElapsed bool
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
	backendValid  bool
	backendTools  []backend.Tool // cached validation + backend wire projection
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
// that has NOT yet been folded in (LIFO — mirrors the attached session's Esc-retract of a
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
// Interjection event per message so the attached session can render it inline in the running
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

// DropBackendState forgets the opaque backend state token, for use when the session
// changes which BACKEND it is talking to.
//
// The token is server-SIGNED and endpoint-specific. Replaying one issued by the deployed
// backend to a local one (or the reverse) hands it a token it cannot verify, so the next
// turn after an endpoint switch fails on a token the user has no way to see, and keeps
// failing until /clear. The conversation itself is fine and must survive: only the
// server-side selection state is endpoint-bound, and dropping it just makes the new
// backend re-run skill selection from scratch, which is exactly right for a backend that
// has never seen this conversation.
//
// The durable mirror goes with it, for the same reason /clear clears it: a handover to
// the supervisor daemon would otherwise resurrect the token this just dropped.
//
// Returns ErrTurnInProgress when a turn is in flight. That is the load-bearing half: a
// turn is multi-round, and swapping the endpoint between rounds would send the next
// round to a backend that cannot read the state token the previous one signed. Every
// surface that can switch endpoints goes through here, so the guard cannot be bypassed
// by a caller that forgot about it.
func (s *Session) DropBackendState() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight {
		return ErrTurnInProgress
	}
	s.backendState = ""
	if s.deps.BackendStateStore != nil {
		_ = s.deps.BackendStateStore.PutSessionBackendState(s.deps.SessionID, "")
	}
	return nil
}

// Compact replaces the working history with one "[checkpoint…]" user note plus a
// small verbatim tail of the most-recent messages, persisting a system marker then
// the note. The tail mirrors the healthy auto-compact path (same keepValidTail
// budget): the checkpoint rounds off the exact references a mid-task orchestrator
// still needs — terminal/watcher/workflow IDs, the active branch, an open grant —
// while the raw tail keeps them intact, so a manual /compact no longer loses MORE
// than an automatic one. Returns ErrTurnInProgress when a turn is in flight (the
// interactive /compact path) — the in-turn auto-compact uses compactLocked instead.
func (s *Session) Compact(summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight {
		return ErrTurnInProgress
	}
	// Snapshot BEFORE compactLocked reslices; keepValidTail copies + orphan-cleans, so
	// the re-appended tail never aliases the reslice and is a valid history.
	tail := keepValidTail(s.messages[domain.ControlMessageCount:],
		domain.AutoCompactVerbatimTailMessages, domain.AutoCompactVerbatimTailTokenBudget)
	s.compactLocked(summary)
	for _, m := range tail {
		s.messages = append(s.messages, m)
		s.persistMessageLocked(m)
	}
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
	// The per-turn cumulative foreground-wait budget (reset per USER TURN, not per
	// model round) rides the turn context into every tool dispatch (values only —
	// cancellation semantics are untouched). Blocking waits inside tools — today
	// terminal.awaitAll's poll sleeps — draw down this one shared allowance; when it
	// is gone a wait returns immediately with budgetExhausted so the model hands the
	// remaining supervision to the async path (watchers/queue) instead of chaining
	// foreground waits across rounds indefinitely. Mid-turn injections deliberately
	// do NOT reset it: an injected message extends the same foreground occupation.
	ctx = waitbudget.With(ctx, waitbudget.New(waitbudget.TurnBudget))

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
	//     never performs the MCP read inline: kick the refresh now (no-op if one is already
	//     in flight), and each round below reads the freshest cached snapshot
	//     (buildRuntimeContext runs per round, so a refresh that lands mid-turn is picked up
	//     on the next round). The one cost is that a brand-new session serves no roster until
	//     its first detached refresh completes (usually within the first turn, longer if the
	//     MCP read is slow) — harmless: the roster is a convenience, and the model can still
	//     tool-call terminal.list to discover it.
	//
	//     Round 0 alone may WAIT a bounded moment on the refresh kicked here before it
	//     builds its request (currentRosterForRound) — never on the MCP call itself, only
	//     on the completion signal of work already in flight, capped at
	//     rosterRoundZeroGrace. Without that, round 0 systematically outraced this kick and
	//     served the PREVIOUS snapshot as live fact (issue #334: a boot-warm roster 21s old
	//     named a terminal the human had since closed in the Daintree UI; the refresh that
	//     would have blanked it landed 15ms too late, and the model both reported the dead
	//     id and scheduled a 60-minute durable timer against it).
	s.refreshRosterAsync()

	// 3e′. Current-worktree snapshot: same cached/detached pattern as the roster
	//      (issue: the per-round synchronous worktree.getCurrent read sat on every
	//      round's first-byte path). Warm it at turn start — TTL-gated, so a turn
	//      arriving seconds after the last fetch kicks nothing — and each round below
	//      reads the freshest cached snapshot (currentWorktreeContext).
	s.maybeRefreshWorktreeAsync()

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
		btools, terr := s.toBackendToolsCached(tools)
		if terr != nil {
			msg := "Tool inventory rejected before send: " + terr.Error()
			s.events.Phase(domain.PhaseFailed)
			s.events.Error(msg)
			return msg
		}

		promptContext := s.promptContext()
		if s.deps.CurrentWorktreeFetcher != nil {
			// Served from the cross-turn cache (assign nil too: a failed cached read
			// means "unknown this round", not "reuse the splash/reconnect selection as
			// if it were still current"). The fetch itself runs DETACHED — kicked here
			// when the cache has passed its TTL — so, unlike the old inline read, it
			// can neither block this round nor degrade the shared MCP transport
			// mid-round (no post-read promptContext re-snapshot needed anymore).
			promptContext.Worktree = s.currentWorktreeContext(ctx)
		}
		// Served from the same cross-turn cache, under a policy that would rather omit the
		// roster than assert a stale one (issue #334). Round 0 passes true so it — and only
		// it — may wait out the turn-start refresh kicked above; later rounds read whatever
		// is cached, so a close settled MID-turn reaches the very next round without waiting
		// on anything (observeRosterMutation patches the cache synchronously). That prune is
		// monotonic but does NOT re-date the snapshot, so in a turn long enough to age the
		// cache past rosterSnapshotMaxAge the next round carries no roster rather than the
		// pruned one — the closed ids still never reappear, and the kicked refresh restores
		// the list a round or two later.
		openTerminals := s.currentRosterForRound(ctx, iter == 0)
		// Both consults above can park for up to their grace, so re-check cancellation
		// before committing to a backend round — the same shape as the post-compact check.
		if ctx.Err() != nil {
			s.events.Phase(domain.PhaseCancelled)
			s.events.AssistantCancelled("")
			return domain.CancelledReply
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
			Runtime:    s.buildRuntimeContext(promptContext, openTerminals),
			Turn:       s.buildTurnContext(userInput, isWake, recalledMemories, resumedWatchers),
			Selection:  s.selectionForRound(),
			Generation: &backend.Generation{ResponseFormat: "text"},
		}

		// One model round per RespondStream — counted for the turn.end summary and used
		// to time first-token latency. roundStartMS is captured AFTER the trace request so
		// it measures the stream itself.
		roundsRun++
		s.traceBackendRequest(runID, turnID, iter, req, bmsgs, btools)
		roundStartMS := domain.NowMS()
		var firstTokenMS int64

		// thinkingShown latches the once-per-round Analyzing/Integrating → Thinking
		// flip. All stream callbacks run synchronously on the reader goroutine, so
		// plain bools (like gotToken) are race-free. The flip is suppressed once a
		// visible token has arrived: Generating is the more specific state and must
		// never regress to Thinking on a trailing reasoning fragment.
		// retryNoticeShown latches the once-per-round retry cue (see OnRetry below), and
		// retryCount tallies the attempts behind it for the trace — a round's wall clock
		// is unreadable without knowing how many attempts it covers.
		// Race-free for the same reason as thinkingShown: RespondStream runs its retry
		// loop and its stream callbacks synchronously on THIS goroutine.
		retryNoticeShown := false
		retryCount := 0
		thinkingShown := false
		markThinking := func() {
			if thinkingShown || gotToken {
				return
			}
			thinkingShown = true
			s.events.Phase(domain.PhaseThinking)
		}

		result, serr := s.deps.Backend.RespondStream(ctx, req, backend.StreamCallbacks{
			OnRawMeta: func(m backend.StreamMeta) {
				s.traceBackendRawMeta(runID, turnID, iter, m)
			},
			OnRetry: func(info backend.RetryInfo) {
				retryCount++
				// The retry budget now rides out a backend restart (~a minute of wall
				// clock), so say so ONCE per round. Without any cue the host shows
				// an unchanged spinner and a retried turn is indistinguishable from a
				// hang; but a note is a STANDALONE transcript cell appended after the
				// active turn, so one per attempt would stack up to nine cells that
				// commit out of order with the answer they preceded and crowd the
				// streaming text out of the live footer. Per-attempt detail stays in
				// the debug log (`backend.retry`), where archaeology wants it anyway.
				if retryNoticeShown {
					return
				}
				retryNoticeShown = true
				s.events.Warn(retryNotice(info))
			},
			OnSkillLoaded: func(refs []backend.SkillRef) {
				if s.emitSkillLoads(refs) {
					s.traceBackendSkillCue(runID, turnID, iter, refs)
				}
			},
			OnMeta: func(m backend.StreamMeta) {
				s.applyStreamMeta(m)
				// The committed round's skill outcome. Emitted here rather than inside
				// applyStreamMeta so that function stays about state persistence, and
				// emitted unconditionally so a consumer sees the active set on a round
				// that loaded nothing — the eager OnSkillLoaded cue above fires only on a
				// delta, and can report a load the committed attempt did not repeat.
				s.events.SkillDecision(skillDecisionFrom(m.Skills))
				s.traceBackendMeta(runID, turnID, iter, m)
			},
			OnStatus: func(st backend.StreamStatus) {
				// The backend emits status ONCE, phase "thinking", the instant
				// chain-of-thought begins (backend/sse.go contract). Map only that
				// value; any unknown/future phase keeps the current UI phase — a
				// conservative posture so a new backend status can never blank or
				// scramble the attached session's liveness line.
				if st.Phase == "thinking" {
					markThinking()
				}
			},
			OnReasoning: func(string) {
				// Reasoning deltas are a pure LIVENESS signal: the first one flips
				// the phase so the footer shows the model is working instead of
				// sitting on "Analyzing request" through a long thinking stretch.
				// The text itself is deliberately dropped — chain-of-thought is
				// NEVER surfaced to the user (no AssistantToken, no event carries
				// it); the parser still accumulates it for the final message.
				markThinking()
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
			s.traceBackendError(runID, turnID, iter, domain.NowMS()-roundStartMS, retryCount, result.Transport, serr)
			return s.classifyBackendError(serr)
		}

		firstTokenLatency := int64(0)
		if firstTokenMS > 0 {
			firstTokenLatency = firstTokenMS - roundStartMS
		}
		s.traceBackendDone(runID, turnID, iter, result, domain.NowMS()-roundStartMS, firstTokenLatency, retryCount)

		calls := backendToolCalls(result.Message.ToolCalls)

		// 10e. Usage — emitted BEFORE appending the assistant message so contextTokens
		//      reflects the prompt actually sent (backend-reported prompt_tokens).
		s.emitBackendUsage(result.Usage, result.Meta.Model)

		// 10e-bis. Server-side compaction, if the backend sent a block.
		//
		//      The position is load-bearing at both ends. AFTER emitBackendUsage,
		//      because that call stashes this round's reported prompt_tokens and the
		//      splice must be the last writer — the figure describes the PRE-splice
		//      prompt, and left standing against a history the block just shrank it
		//      would trip maybeAutoCompact into compacting again on top of the
		//      server's work. BEFORE the assistant message is appended, because that
		//      is the coordinate space the span was measured in: the reply now in hand
		//      is excluded from it by contract, and appending onto the spliced array
		//      below leaves it exactly where the raw tail already sits.
		//
		//      The next round of THIS turn therefore sends block + tail, which is
		//      where a tool loop's prompt cost actually accumulates. Best-effort
		//      throughout: a refused block leaves full history and the turn carries
		//      on, so the only trace of it is the debug log.
		if result.Compaction != nil {
			applied, reason := s.applyServerCompaction(result.Compaction, len(bmsgs), turnID)
			s.traceServerCompaction(runID, turnID, iter, result.Compaction, len(bmsgs), applied, reason)
		}

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
// queued. Two kinds of consecutive runs dispatch CONCURRENTLY, with each member's
// settled result streamed live as it completes: maximal runs of parallel-safe calls
// (no-wait snapshot reads that opted in via Tool.Parallelizable —
// terminal.extract/.json), and bounded homogeneous-mutation cohorts (consecutive
// SAME-NAME pre-authorized mutating calls that opted in via Tool.ParallelHomogeneous
// — the agentTask.spawnForEdits fan-out, collapsing N ~5s launches into roughly the
// slowest wave; see mutationRunEnd for the authorization/independence bar). Every
// other call runs serially in place, preserving exact ordering and side-effect
// sequencing. Two
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
	s.events.ToolBatch(redactBatchedToolCalls(batch))

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
		// reads that opted in via Tool.Parallelizable) — or, failing that, a bounded
		// homogeneous-mutation cohort (consecutive SAME-NAME pre-authorized mutating
		// calls, e.g. a spawn fan-out; see mutationRunEnd) — and dispatch a run of ≥2
		// CONCURRENTLY — each member's result streams live as it settles. Everything
		// else runs on the serial path below, preserving today's exact ordering and
		// side-effect sequencing. A batch carrying a multiple-choice question stays
		// fully serial: every non-question sibling is skipped synthetically, so there
		// is nothing to overlap. The two groupings never mix in one group: reads
		// require RiskRead, mutation cohorts require a same-name mutating tool.
		if questionIdx < 0 {
			e := s.parallelRunEnd(calls, c, allowedSet)
			if e-c < 2 {
				e = s.mutationRunEnd(calls, c, allowedSet)
			}
			if e-c >= 2 {
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

		s.events.ToolCall(redactToolCallEvent(ToolCallEvent{ID: call.ID, Name: internalName, Args: call.Function.Arguments, StartedAt: startedAt}))

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
	// Keep the cached roster consistent with this call's own terminal mutation
	// (close prunes ids; spawns kick a refresh) — a synthetic stub never dispatched,
	// so it mutated nothing. Runs before the events so the very next round's runtime
	// block already reflects the change.
	if !synthetic {
		s.observeRosterMutation(internalName, call.Function.Arguments, res)
	}
	failCount := 0
	if !res.Ok && ctx.Err() == nil && !synthetic {
		failCount = s.recordToolFailure(internalName)
	}
	s.events.ToolResult(redactToolResultEvent(ToolResultEvent{ID: call.ID, Name: internalName, Result: res, EndedAt: endedAt, FailureCount: failCount}))
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

// callBatchable applies the call-level checks shared by BOTH concurrent groupings:
// the arguments parse to a real object (empty/degenerate args stay serial so the
// truncation/invalid-args paths keep today's exact ordering and never race a
// sibling), the call carries no `wait` barrier (a `wait` condition turns even an
// opted-in call into a BARRIER — it polls until a terminal settles, and a later
// call in the batch may depend on that settle), and it survives the (dormant)
// allow-list gate.
func (s *Session) callBatchable(call models.ToolCallRequest, allowedSet map[string]struct{}) bool {
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
	if allowedSet != nil && !setHas(allowedSet, s.resolveInternal(call.Function.Name)) {
		return false
	}
	return true
}

// callParallelSafe reports whether one call may join a concurrent READ group: it
// passes the shared batchable checks (callBatchable) and the runner classifies the
// tool as parallel-safe (Tool.Parallelizable + read risk — an explicit per-tool
// opt-in, NOT every read tool: barrier reads like terminal.awaitAll are
// deliberately excluded). Only the production registry adapter implements
// parallelSafeRunner; a test fake that doesn't keeps the fully-serial path, so
// serial-ordering tests are unaffected.
func (s *Session) callParallelSafe(call models.ToolCallRequest, allowedSet map[string]struct{}) bool {
	if !s.callBatchable(call, allowedSet) {
		return false
	}
	runner, ok := s.deps.Tools.(parallelSafeRunner)
	return ok && runner.ParallelSafe(s.resolveInternal(call.Function.Name))
}

// maxParallelMutationDispatch bounds a homogeneous-mutation cohort. Matched to the
// MCP request governor's in-flight cap (4): unlike extraction (whose wall-clock is a
// backend model call), a spawn IS an MCP mutation, so members beyond the governor's
// cap would only queue at the wire while inflating the cohort the breakers can't see
// into. The bound also serves as the breaker/cancel cadence: a longer run dispatches
// as successive ≤4-call cohorts with the mid-batch guards re-applied between waves,
// so a runaway 50-spawn batch is stoppable, never one giant unstoppable group.
const maxParallelMutationDispatch = 4

// mutationRunEnd returns the end (exclusive) of the bounded homogeneous-mutation
// cohort starting at c — consecutive calls of the SAME internal tool name whose tool
// opted in via Tool.ParallelHomogeneous AND is pre-authorized to run without any
// confirmation/grant interaction (interactive main + auto-approve + tier allows —
// the runner's ParallelMutationSafe; anything else keeps today's serial path). Two
// independence guards end the cohort early at the offending call: a call whose
// canonicalized args byte-match an earlier member (a generic backstop — the
// per-tool keys below carry the real identity semantics), and a call sharing ANY
// ParallelConflictKey dimension with an earlier member (a normalized shared target
// or collision-prone identity — e.g. two edit-mode spawns into one worktree, or
// two spawns whose launch names collide and could cross-bind on reconcile). The
// offender never overlaps the member it conflicts with: it dispatches only after
// this cohort fully settles (in the next cohort or serially — the caller re-enters
// at e). The caller forms a concurrent group only when e-c ≥ 2; a lone qualifying
// call (e == c+1) falls back to the serial path.
func (s *Session) mutationRunEnd(calls []models.ToolCallRequest, c int, allowedSet map[string]struct{}) int {
	runner, ok := s.deps.Tools.(parallelMutationRunner)
	if !ok || c >= len(calls) {
		return c
	}
	name := s.resolveInternal(calls[c].Function.Name)
	if !runner.ParallelMutationSafe(name) {
		return c
	}
	seenSigs := make(map[string]struct{}, maxParallelMutationDispatch)
	seenKeys := make(map[string]struct{}, maxParallelMutationDispatch)
	e := c
	for e < len(calls) && e-c < maxParallelMutationDispatch {
		call := calls[e]
		if s.resolveInternal(call.Function.Name) != name || !s.callBatchable(call, allowedSet) {
			break
		}
		keys, keyOK := runner.ParallelConflictKey(name, json.RawMessage(call.Function.Arguments))
		if !keyOK {
			break
		}
		sig := canonicalJSON(call.Function.Arguments)
		if _, dup := seenSigs[sig]; dup {
			break
		}
		clash := false
		for _, k := range keys {
			if k == "" {
				continue
			}
			if _, hit := seenKeys[k]; hit {
				clash = true
				break
			}
		}
		if clash {
			break
		}
		for _, k := range keys {
			if k != "" {
				seenKeys[k] = struct{}{}
			}
		}
		seenSigs[sig] = struct{}{}
		e++
	}
	return e
}

// runParallelGroup dispatches calls[from:to) CONCURRENTLY (bounded by
// maxParallelToolDispatch) and streams each member's settled result the moment it
// completes — the host shows every member live: all spinners appear together, then
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
		s.events.ToolCall(redactToolCallEvent(ToolCallEvent{ID: call.ID, Name: names[i], Args: call.Function.Arguments, StartedAt: domain.NowMS()}))
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

// retryNotice renders the one-line cue shown when a transient backend failure starts
// being replayed. It fires ONCE per round (on the first retry), so it summarises the
// whole budget rather than counting attempts: the cause in the user's terms, the
// first wait, and how many attempts are coming — enough for a minute-long chain to
// read as deliberate patience rather than a stall.
func retryNotice(info backend.RetryInfo) string {
	cause := "Backend error"
	var be *backend.Error
	switch {
	case errors.As(info.Err, &be) && be.IsConnect():
		cause = "Can't reach the backend"
	case errors.As(info.Err, &be) && be.IsRateLimited():
		cause = "Model rate-limited"
	}
	return fmt.Sprintf("%s — retrying in %s (up to %d attempts)",
		cause, formatRetryDelay(info.Delay), info.MaxAttempts)
}

// formatRetryDelay renders a backoff wait for humans: milliseconds below a second,
// one decimal through the jittered ramp (a 1.4s wait shown as "1s" reads as a wrong
// countdown), and whole seconds for the steady-state 10–15s poll.
func formatRetryDelay(d time.Duration) string {
	switch {
	case d < time.Second:
		return strconv.FormatInt(d.Milliseconds(), 10) + "ms"
	case d < 10*time.Second:
		// 'g' with -1 precision drops a trailing zero, so 2s stays "2s" and only a
		// genuinely fractional wait grows a decimal.
		return strconv.FormatFloat(d.Round(100*time.Millisecond).Seconds(), 'g', -1, 64) + "s"
	default:
		return strconv.Itoa(int(d.Round(time.Second)/time.Second)) + "s"
	}
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
	if errors.As(err, &be) {
		if msg := upstreamFailureAdvice(be); msg != "" {
			s.events.Phase(domain.PhaseFailed)
			s.events.Error(msg)
			return msg
		}
	}
	msg := "Model error: " + err.Error()
	s.events.Phase(domain.PhaseFailed)
	s.events.Error(msg)
	return msg
}

// upstreamFailureAdvice renders the user-facing reply for the backend's upstream-failure
// taxonomy, or "" when the error is not one of those codes (leaving the generic
// "Model error:" fallback in place).
//
// Every one of these used to arrive as the same 502 `upstream_error` and read as the
// same "Model error: backend: http 502 upstream_error: …", which is the least useful
// possible answer to five genuinely different questions. The three account codes in
// particular are the ones a tester can actually fix — and the fixes are mutually
// exclusive, so a message that names the wrong one is worse than none: telling someone
// whose balance ran out to re-enter their key gets a good key replaced.
//
// Each returned string starts with a registered WAKE_FAILURE_PREFIX (wake.go). That
// registration is load-bearing, not bookkeeping: an unregistered prefix would let the
// supervisor's unattended wake mistake a failed turn for a real answer and record the
// work as summarized.
func upstreamFailureAdvice(be *backend.Error) string {
	switch be.Code {
	case backend.CodeProviderInvalidAPIKey:
		return "Model unavailable: OpenRouter rejected your API key. Replace or rotate it, then run /login."
	case backend.CodeProviderInsufficientCredit:
		// The most likely failure of the lot, and the easiest to fix — so it gets the
		// place to go, not just the diagnosis.
		return "Model unavailable: your OpenRouter account is out of credit. Add credits at https://openrouter.ai/credits, then try again."
	case backend.CodeProviderKeyForbidden:
		// Deliberately does NOT suggest /login. The key is recognised and funded; a
		// fresh sign-in with the same key changes nothing.
		return "Model unavailable: the credential funding this turn isn't permitted to use this model. Its model permissions, spend limit or guardrails are blocking it."
	case backend.CodeUpstreamNoCompliantProvider:
		return "Model unavailable: no OpenRouter endpoint matched your routing policy. Run /routing to see it — a stricter privacy mode or a narrow endpoint list can empty the pool, and it is never relaxed automatically to find a route."
	case backend.CodeUpstreamUnavailable:
		// Deliberately does NOT say "did not recover while retrying". This code is
		// retryable, but a turn can reach here without any replay having happened — a
		// one-shot context, an exhausted elapsed budget, or visible tokens already
		// streamed all skip the retry loop. Asserting a retry we may not have made
		// would be a small lie in the one message a user reads to decide what to do.
		return "Model unavailable: OpenRouter is having trouble reaching a provider for this model. Nothing is wrong with your account — try again shortly."
	}
	// Both reportable codes carry the same next step and OPPOSITE directions of fault,
	// so they get the same request id and different sentences. Attributing a provider's
	// malformed reply to a Daintree bug would send someone hunting through our code for
	// something that is not there.
	if be.IsReportable() {
		var msg string
		if be.Code == backend.CodeUpstreamRequestRejected {
			msg = "Model error: OpenRouter rejected the request Daintree built, which is a Daintree bug rather than a problem with your account. Please report it"
		} else {
			msg = "Model error: OpenRouter's reply could not be parsed — usually a provider or compatibility problem, not your account. Please report it"
		}
		if be.RequestID != "" {
			msg += " with request id " + be.RequestID
		}
		return msg + " (run 'daintree-assistant support-bundle' for redacted diagnostics)."
	}
	return ""
}

// backendAssistantMessage builds the local assistant message for a backend result:
// content null on a pure tool-call turn (no prose), else the visible content.
// BackendAssistantMessage is the exported form of backendAssistantMessage, shared
// with internal/subagent for the same reason ToBackendMessages is: the null-content
// and reasoning_content rules are a wire contract, not a detail worth duplicating.
func BackendAssistantMessage(msg backend.RespondMessage) models.ChatMessage {
	return backendAssistantMessage(msg)
}

// BackendToolCalls is the exported form of backendToolCalls (see
// BackendAssistantMessage).
func BackendToolCalls(calls []backend.ToolCall) []models.ToolCallRequest {
	return backendToolCalls(calls)
}

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
// up after a detach, or the next attached session after the daemon — replays the same token
// instead of forcing the backend to re-run skill selection from scratch
// mid-conversation. The round's skill outcome (meta.Skills) is deliberately NOT retained
// as session state: it is reported per round as it arrives (the caller emits
// SkillDecision from the same OnMeta callback) and logged by the debug trace, so there is
// nothing for a later round to read back.
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
	s.reportPinnedSkillWarnings(m.Warnings)
}

// pinnedSkillWarnings maps the backend's pin-refusal codes to what an operator needs
// told. It is an ALLOWLIST, and that is the whole design: backend skill loading is
// deliberately invisible in this CLI — no card, no cue, no /skills — because the delta
// a load card showed was misleading about what was retained, capped, or auto-paired.
// A pin refusal is the one narrow carve-out, because the operator asked for a specific
// runbook by name and a `--skill` that quietly did nothing is the exact failure the flag
// exists to prevent. Every other warning code the backend may add stays diagnostic-only
// (the debug trace and --json already carry the raw list); widening this map would
// resurrect the surfacing that was removed on purpose.
var pinnedSkillWarnings = map[string]string{
	"unknown_skill_id_ignored":    "the backend did not recognise a pinned skill id and ignored it — run `daintree-assistant --list-skills` to see what it can load",
	"pinned_skill_not_executable": "a pinned skill exists but is not executable in this session's profile, so it was not loaded",
	"pinned_skill_over_cap":       "a pinned skill did not fit the backend's active-skill limit and was dropped",
}

// reportPinnedSkillWarnings surfaces the pin-refusal codes from a COMMITTED meta event,
// once per cause per session.
//
// Fed from applyStreamMeta (the retry-safe OnMeta path) rather than OnRawMeta on
// purpose: a retried attempt re-emits its meta, and a warning shown twice for one
// refusal reads as two refusals.
//
// It also stays silent when nothing was pinned. The backend can only raise these codes
// in response to selection.pinned_skill_ids, but a client that reports a pin failure to
// someone who never pinned anything is reporting a bug it cannot explain.
func (s *Session) reportPinnedSkillWarnings(warnings []string) {
	if len(warnings) == 0 || len(s.deps.PinnedSkillIDs) == 0 {
		return
	}
	for _, code := range warnings {
		msg, ok := pinnedSkillWarnings[code]
		if !ok {
			continue
		}
		s.mu.Lock()
		if s.pinWarningsSeen == nil {
			s.pinWarningsSeen = make(map[string]struct{}, len(pinnedSkillWarnings))
		}
		_, dup := s.pinWarningsSeen[code]
		if !dup {
			s.pinWarningsSeen[code] = struct{}{}
		}
		s.mu.Unlock()
		if dup {
			continue
		}
		s.events.Warn(msg + " (" + code + ")")
	}
}

// selectionForRound builds the request's selection block.
//
// With no pins it returns exactly what it always did, so the wire bytes of an ordinary
// turn are unchanged by this feature.
//
// With pins it attaches them only while the live endpoint still advertises the
// capability. app.PreparePinnedSkills already proved that before the first turn, so a
// closed gate here means the endpoint CHANGED underneath us — in practice `/backend`,
// which swaps the client in place and deliberately does no network work, leaving the
// cached capability answer pinned to the endpoint that is no longer being called.
//
// Sending the field anyway would 422 the entire turn against the new endpoint. Dropping
// it and saying so once keeps the turn alive and keeps the operator from reading an
// unpinned run as a pinned one. Deliberately not a turn failure: the switch was a
// deliberate act and the conversation should survive it.
func (s *Session) selectionForRound() *backend.Selection {
	sel := &backend.Selection{Policy: "new_instruction"}
	if len(s.deps.PinnedSkillIDs) == 0 {
		return sel
	}
	if s.deps.BackendAcceptsPinnedSkillIDs == nil || !s.deps.BackendAcceptsPinnedSkillIDs() {
		s.reportPinGateClosed()
		return sel
	}
	sel.PinnedSkillIDs = append([]string(nil), s.deps.PinnedSkillIDs...)
	return sel
}

// pinGateClosedCode is a CLIENT-side pseudo-code, kept in the same one-per-session
// ledger as the backend's own so the two cannot each report the same silent unpinning.
const pinGateClosedCode = "pinned_skills_withheld"

func (s *Session) reportPinGateClosed() {
	s.mu.Lock()
	if s.pinWarningsSeen == nil {
		s.pinWarningsSeen = make(map[string]struct{}, len(pinnedSkillWarnings)+1)
	}
	_, dup := s.pinWarningsSeen[pinGateClosedCode]
	if !dup {
		s.pinWarningsSeen[pinGateClosedCode] = struct{}{}
	}
	s.mu.Unlock()
	if dup {
		return
	}
	// Names the CAUSE, not just the effect. "This backend does not support pinning"
	// would be a guess and usually a wrong one: the pins were negotiated successfully at
	// launch, so what changed is which endpoint is being called.
	s.events.Warn("the backend endpoint changed after --skill was negotiated, so this turn ran WITHOUT the pinned runbooks; " +
		"restart with --skill against the new endpoint to pin them there (" + pinGateClosedCode + ")")
}

// skillLabels renders skill refs to display labels, preferring the title and falling
// back to the id. A ref with NEITHER is malformed — dropped rather than surfaced as a
// blank row.
func skillLabels(refs []backend.SkillRef) []string {
	if len(refs) == 0 {
		return nil
	}
	labels := make([]string, 0, len(refs))
	for _, ref := range refs {
		label := strings.TrimSpace(ref.Title)
		if label == "" {
			label = strings.TrimSpace(ref.ID)
		}
		if label == "" {
			continue
		}
		labels = append(labels, label)
	}
	return labels
}

// skillDecisionFrom projects the committed stream meta's skills block onto the
// backend-independent event DTO the sinks consume.
//
// Refs are copied VERBATIM — same order, no title fallback, no dropping of malformed
// entries — because this event is the machine-facing record of what the backend actually
// decided, and laundering it would hide exactly the selector bug a consumer is looking
// for. (skillLabels' cosmetic fallback stays on the human-readable SkillLoaded path.)
// Both slices are allocated even when empty so they marshal as [] rather than null.
//
// Selector.Usage and the vestigial Prelude are intentionally dropped; see
// SkillSelectorOutcome for why usage does not belong on this seam.
func skillDecisionFrom(b backend.SkillsBlock) SkillDecisionEvent {
	return SkillDecisionEvent{
		Active:      copySkillRefs(b.Active),
		NewlyLoaded: copySkillRefs(b.NewlyLoaded),
		Selector: SkillSelectorOutcome{
			Ran:        b.Selector.Ran,
			Degraded:   b.Selector.Degraded,
			TaskType:   b.Selector.TaskType,
			Confidence: b.Selector.Confidence,
			Reason:     b.Selector.Reason,
		},
	}
}

// copySkillRefs converts wire refs to event refs, always returning a non-nil slice.
func copySkillRefs(refs []backend.SkillRef) []SkillRef {
	out := make([]SkillRef, 0, len(refs))
	for _, ref := range refs {
		out = append(out, SkillRef{ID: ref.ID, Title: ref.Title})
	}
	return out
}

// emitSkillLoads surfaces newly-loaded runbooks as a dedicated SkillLoaded event, fed
// by StreamCallbacks.OnSkillLoaded as soon as the SSE meta arrives. Its distinct value is
// TIMING — it is the only skill signal available before the upstream model connects, so a
// trace separates selection latency from generation. It is NOT authoritative: it fires per
// attempt on a delta, so a retried round can report a load the committed attempt never
// repeated. SkillDecision (from the committed meta) is the record a consumer asserts on.
// It is a
// DIAGNOSTIC/AUTOMATION signal only — the durable run log, the --json stream, and the
// debug trace consume it; nothing folds it into the live conversation, and the one place
// it reaches a human is an explicit `/explain <run>` replay of that run's timeline.
//
// In an /explain replay these rows are SUPERSEDED by the committed skill:decision rows
// whenever the run recorded any; they still render for runs from before that event
// existed, which would otherwise lose their skill story entirely.
//
// It used to draw an inline "Skill loaded" card, and there used to be a /skills command.
// Both were removed: backend skill selection is prompt-assembly machinery, not a decision
// the user takes or can reverse, so there is no affordance to attach the information to.
// The card named only the NewlyLoaded delta (never what was retained, dropped by the cap,
// or paired in as a domain foundation), so across rounds it read as the assistant changing
// its mind while hiding what it changed from — and once selection became a ~10ms
// in-process classifier it no longer explained a wait, which was its original job.
//
// The "skill" VOCABULARY — a visible "Skill loaded" event, the /skills command — is
// deliberately left free for user-authored *assistant* skills, which are intent-driven
// and will want it. Selector tuning reads the debug trace (backend.respond.meta logs the
// active and newly-loaded sets per round), not the product UI.
func (s *Session) emitSkillLoads(refs []backend.SkillRef) bool {
	titles := skillLabels(refs)
	if len(titles) == 0 {
		return false
	}
	s.events.SkillLoaded(titles)
	return true
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

const (
	// worktreeSnapshotTTL is how old the cached current-worktree snapshot may be, at
	// the moment a round consults it, before a detached refresh is kicked. A worktree
	// switch therefore reaches the model within ~one TTL + one round — plenty for a
	// human-scale action ("switch to the fix branch") — while a multi-round turn stops
	// paying a per-round MCP read on its first-byte path.
	worktreeSnapshotTTL = 15 * time.Second

	// worktreeFirstFetchGrace is how long a COLD cache (never fetched) waits for the
	// just-kicked first refresh before proceeding without worktree context. Chosen
	// over proceeding immediately: the old synchronous path gave the read a 1s inline
	// budget, so the first round of a session virtually always carried a worktree —
	// proceeding at once would regress that to "first round never has one". 250ms
	// keeps that hit rate for a healthy MCP (the read is a single local round-trip,
	// typically tens of ms) while capping the worst case at a quarter of the old
	// budget; a slow/hung MCP degrades to exactly what budget-expiry produced before:
	// no worktree this round, self-healing on a later round via the cache.
	worktreeFirstFetchGrace = 250 * time.Millisecond
)

// currentWorktreeContext returns the cached current-worktree snapshot for this round,
// kicking a detached refresh when the cache is older than worktreeSnapshotTTL (the
// open-terminal roster pattern — the round itself NEVER blocks on the MCP read). The
// one exception is a cold cache (never fetched): the FIRST such consult waits up to
// worktreeFirstFetchGrace for the just-kicked refresh so the session's first round
// usually still carries a worktree (see the constant's comment). If that grace fully
// elapses with the fetch still in flight, worktreeGraceElapsed latches and every
// later cold consult proceeds immediately — a slow first fetch costs one grace for
// the whole session, not one per round. ctx bounds only that short grace wait, never
// the fetch itself (which is detached and self-bounded).
//
// NOTE deliberately not implemented: marking a >60s-old snapshot as stale in the
// injected context. The snapshot travels the backend's strict typed wire contract
// (backend.CurrentWorktreeSnapshot — internal/backend/contracts.go, mirrored by the
// server's extensions.py schema with extra="forbid"), which has no staleness field and
// no free-text slot to carry one; overloading the 64-rune Status enum would corrupt a
// semantic field the backend renders. Adding a field means a coordinated wire change
// in the backend package another agent owns. The TTL keeps ordinary staleness ≤ ~15s
// + one fetch anyway, well under that 60s bar.
func (s *Session) currentWorktreeContext(ctx context.Context) *prompts.WorktreeContext {
	if s.deps.CurrentWorktreeFetcher == nil {
		return nil
	}
	s.maybeRefreshWorktreeAsync()

	s.worktreeMu.Lock()
	cold := s.worktreeFetchedAt.IsZero()
	done := s.worktreeRefreshDone
	graceSpent := s.worktreeGraceElapsed
	s.worktreeMu.Unlock()

	if cold && done != nil && !graceSpent {
		t := time.NewTimer(worktreeFirstFetchGrace)
		select {
		case <-done:
		case <-t.C:
			// The full grace was paid and the fetch is STILL in flight: latch, so
			// the remaining rounds of this slow first fetch don't each pay it again.
			s.worktreeMu.Lock()
			s.worktreeGraceElapsed = true
			s.worktreeMu.Unlock()
		case <-ctx.Done():
		}
		t.Stop()
	}

	s.worktreeMu.Lock()
	defer s.worktreeMu.Unlock()
	// The snapshot is replaced whole by the refresher and treated as read-only by
	// every consumer, so sharing the pointer is safe (no copy needed).
	return s.worktreeSnap
}

// maybeRefreshWorktreeAsync starts a detached, best-effort refresh of the cached
// current-worktree snapshot when that cache is stale (older than worktreeSnapshotTTL)
// or was never filled, and only when no refresh is already in flight — the exact
// refreshRosterAsync shape: single-flight dedupe, wg.Add gated under s.mu with the
// draining flag (so Shutdown's drain never races the Add), and parented off bgCtx
// WITHOUT a deadline (the fetcher self-bounds via its own cancel timer; a ctx
// deadline would make mcp.Client tear down the shared transport). The result —
// nil for a failed read included — replaces the cache the moment it lands, so the
// next round's runtime block picks it up without ever blocking a turn. The TTL gate
// lives here rather than at the call sites so the per-round consult and the
// turn/splash warm calls share one policy: a turn starting seconds after the last
// fetch re-reads nothing.
//
// The staleness test and the in-flight claim MUST hold worktreeMu across BOTH, and
// that is the whole reason they sit in one function. Reading staleness under an
// earlier, separate hold left a window: an in-flight fetch could land in the gap —
// re-dating worktreeFetchedAt and clearing worktreeRefreshing — so the caller that
// had just read "stale" then won the claim and kicked a SECOND fetch for the same
// turn, the per-round re-read this cache exists to prevent. It surfaced as a rare
// -race failure of TestCurrentWorktreeCachedSnapshotServesEveryBackendRound (turn
// start kicks the fetch, round 0 consults the still-cold cache, the fetch lands
// between that consult's two locks).
func (s *Session) maybeRefreshWorktreeAsync() {
	if s.deps.CurrentWorktreeFetcher == nil {
		return
	}
	s.worktreeMu.Lock()
	if s.worktreeRefreshing {
		s.worktreeMu.Unlock()
		return
	}
	if !s.worktreeFetchedAt.IsZero() && time.Since(s.worktreeFetchedAt) <= worktreeSnapshotTTL {
		s.worktreeMu.Unlock()
		return
	}
	s.worktreeRefreshing = true
	done := make(chan struct{})
	s.worktreeRefreshDone = done
	s.worktreeMu.Unlock()

	// Clear the in-flight state on EVERY exit path (draining bail-out included) so a
	// refusal can never wedge the dedupe flag true, and close(done) so a cold-cache
	// grace waiter is released rather than sleeping out its full grace.
	abandon := func() {
		s.worktreeMu.Lock()
		s.worktreeRefreshing = false
		s.worktreeRefreshDone = nil
		s.worktreeMu.Unlock()
		close(done)
	}

	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		abandon()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		fresh := s.deps.CurrentWorktreeFetcher(s.bgCtx)
		// Replace unconditionally (nil included): a failed live read means "unknown
		// this round", never "reuse the cached splash/reconnect selection as if it
		// were still current" — the same posture the old synchronous path enforced.
		// The failure is still cached under the TTL (a broken MCP is retried every
		// ~15s, not hammered every round) and self-heals on a later refresh.
		s.worktreeMu.Lock()
		s.worktreeSnap = fresh
		s.worktreeFetchedAt = time.Now()
		s.worktreeRefreshing = false
		s.worktreeRefreshDone = nil
		s.worktreeMu.Unlock()
		close(done)
	}()
}

// buildRuntimeContext maps PromptContext onto the backend's structured runtime contract.
// Stable project/agent details ride only request.startup, so they are not duplicated in
// the backend's fresh runtime tail. The context is pulled live
// every round, so a mid-session MCP connect / tier change / scheduler start reaches the
// next request. The
// openTerminals snapshot is read from the cross-turn cache (currentRosterForRound) fresh
// EACH round — never fetched inline — so a detached refresh (step 3e) that lands mid-turn
// is reflected on the next round without ever blocking the turn on an MCP read. A nil
// slice is a deliberate value, not a gap: the caller withholds any snapshot it cannot
// claim is live, and the backend simply renders no open-terminals block.
func (s *Session) buildRuntimeContext(pc prompts.MainPromptContext, openTerminals []backend.OpenTerminal) *backend.RuntimeContext {
	rc := &backend.RuntimeContext{
		PermissionTier:  string(pc.Tier),
		MCPServers:      buildMCPServers(pc.MCPServers),
		SchedulerActive: pc.SchedulerActive,
		Worktree:        buildCurrentWorktreeSnapshot(pc.Worktree),
		OpenTerminals:   openTerminals,
	}
	if d := pc.Display; d != nil {
		// Live like the rest of this block: the attached session republishes on every resize, so
		// a window dragged narrower mid-session reaches the very next round.
		rc.Display = backend.NewDisplayInfo(d.Columns, d.ContentWidth)
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

// buildMCPServers maps the configured integration surface onto the backend contract.
// The endpoint leads the description because that is the fact the block exists to carry:
// the backend renders "- <name>: <description>", so this reads
// "- daintree: endpoint http://127.0.0.1:45454/mcp — Daintree control plane …" and the
// model can name the server it is driving instead of guessing at a plausible localhost
// URL (ses_8cb40b4e). Status/transport/tool-count are deliberately left unset here — this
// block is cached ahead of the tool schemas, so only session-stable facts belong in it;
// the live link state rides rc.MCP. Fields are clamped to the backend's strict
// max_lengths (name 128, description 4096) so an odd endpoint can never 400 the turn.
func buildMCPServers(servers []prompts.MCPServerContext) []backend.MCPServer {
	var out []backend.MCPServer
	for _, s := range servers {
		name := clampRunes(strings.TrimSpace(s.Name), 128)
		url := strings.TrimSpace(s.URL)
		if name == "" || url == "" {
			continue
		}
		desc := "endpoint " + url
		if d := strings.TrimSpace(s.Description); d != "" {
			desc += " — " + d
		}
		out = append(out, backend.MCPServer{Name: name, Description: clampRunes(desc, 4096)})
	}
	return out
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

// toBackendToolsCached validates and converts the stable model-tool projection once.
// projectToolsLocked replaces the whole cache whenever its key changes, naturally
// invalidating this secondary representation. With the full registry stable for the
// process, later model rounds reuse the same immutable backend slice.
func (s *Session) toBackendToolsCached(tools []models.ChatTool) ([]backend.Tool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := &s.toolProj
	if c.backendValid {
		return c.backendTools, nil
	}
	btools, err := ToBackendTools(tools)
	if err != nil {
		return nil, err
	}
	c.backendTools = btools
	c.backendValid = true
	return btools, nil
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
	rec := s.conversationRecord(m, s.seq)
	s.seq++
	_, _ = s.deps.Store.InsertMessage(rec)
}

// conversationRecord projects a chat message into its durable row at the given seq.
// Split out of persistMessageLocked so the grouped compaction write (which stamps
// several rows before committing any of them) cannot drift from the single-row path —
// the reasoning and name rules below are subtle enough that two copies would.
func (s *Session) conversationRecord(m models.ChatMessage, seq int) domain.ConversationMessageRecord {
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
	// Persist the wire `name` for the compacted context block ONLY, so it rehydrates as
	// a boundary the backend still recognises. Scoped the same way the wire encoder is,
	// and for the same reason: a tool result's Name is the tool's internal name, kept
	// for local bookkeeping, and it has no business in the durable transcript.
	var name *string
	if isCompactionBlockMessage(m) {
		n := m.Name
		name = &n
	}
	return domain.ConversationMessageRecord{
		SessionID:        s.deps.SessionID,
		Seq:              seq,
		Role:             m.Role,
		Content:          m.ContentToText(),
		Name:             name,
		ReasoningContent: reasoning,
		ToolCallsJson:    toolCallsJSON,
		ToolCallID:       toolCallID,
	}
}

// EstimateTokens exposes the working-history size estimate to surfaces that report
// context size — e.g. /compact's "~412k → ~9k tokens" before/after line. Same chars/4
// heuristic as the auto-compact gate; approximate by design.
func (s *Session) EstimateTokens() int { return s.estimateTokens() }

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
	// this transcript. FlattenTranscript folds tool-call names + argument JSON in so
	// load-bearing IDs that live ONLY in arguments — e.g. terminal.read
	// {"terminalId":"term_x"} — survive into the checkpoint's ID-preservation pass.
	transcript := FlattenTranscript(s.messages[domain.ControlMessageCount:])
	s.mu.Unlock()

	// Run the backend's checkpoint task. On an ERROR (not a reply): a cancel is the
	// turn tearing down (don't compact, don't count it — issue #61), a real outage
	// counts toward the bounded-growth truncation fallback (issue #202). A successful
	// result always compacts — even a sparse checkpoint, because validateCheckpoint still
	// mines every load-bearing ID from the transcript into PreservedIDs.
	cp, err := BuildCheckpoint(ctx, s.deps.Backend, transcript)
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
	// Escape hatch: archive the full flattened transcript as a durable artifact and
	// point the note at it, so anything the checkpoint rounded off stays recoverable
	// via artifact.read instead of being lost forever. Best-effort ("" appends nothing).
	summary = AppendTranscriptBreadcrumb(summary, s.ArchiveCompactionTranscript(transcript))

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

// currentRoster returns the RAW cached open-terminal snapshot under rosterMu — a
// shallow copy so a caller can hold it past the lock while a concurrent refresh swaps
// the field. nil when no refresh has completed yet (a fresh session's cold start).
// It applies no freshness policy: the model-facing read is currentRosterForRound.
func (s *Session) currentRoster() []backend.OpenTerminal {
	s.rosterMu.Lock()
	defer s.rosterMu.Unlock()
	if len(s.roster) == 0 {
		return nil
	}
	return append([]backend.OpenTerminal(nil), s.roster...)
}

// rosterRoundZeroGrace bounds how long ROUND 0 waits on an already-in-flight roster
// refresh before building its request. It is a wait on a completion signal, never on
// an MCP call: the fetch is detached and self-bounded either way, and every other
// round reads the cache without waiting at all.
//
// The trigger is deliberately "a refresh is in flight", NOT the sibling worktree
// cache's "the cache was never fetched" (currentWorktreeContext). Issue #334's
// failure was a WARM cache — 21s old, a terminal since closed in the Daintree UI —
// losing a race to the turn-start refresh by 15ms; a cold-cache trigger would not
// have fired at all.
//
// 250ms matches worktreeFirstFetchGrace: enough for a healthy MCP (the fetch is two
// round-trips, typically tens of ms — the issue's own trace missed by 15ms), and a
// twentieth of the 5s synchronous gate this cache replaced. Unlike the worktree's
// grace there is NO latch suppressing it after one timeout, because the two graces
// have different blast radii: the worktree's fires on every consult while the cache
// is cold, so an unbounded first fetch would tax every round of every turn, whereas
// this one fires only at iter==0 — at most once per turn (plus once per mid-turn
// injection, each of which is itself a fresh user instruction that deserves a fresh
// roster). Latching it would leave every later turn's round 0 back where #334 found
// it: asserting an unverified snapshot, or dropping the roster outright for the rest
// of the session.
//
// A var, not a const, ONLY so tests can widen it (withRosterGrace): at 250ms "waited"
// and "did not wait" are one scheduler hiccup apart, so asserting either way against
// the real value measures machine load rather than behaviour. Production never writes
// it, and the swap is unsynchronized — tests that widen it must not use t.Parallel().
var rosterRoundZeroGrace = 250 * time.Millisecond

const (
	// rosterSnapshotMaxAge is how old a cached roster may be and still be served to the
	// model as live state. Past it the round carries NO roster rather than a snapshot it
	// cannot vouch for — the "missing roster is a safe false negative, stale roster is an
	// unsafe false positive" posture the refresher already takes on a failed fetch (see
	// refreshRosterAsync's commit comment), extended from "this fetch failed" to "this
	// snapshot is too old to assert". The model recovers from an absent roster by
	// tool-calling terminal.list; it does not recover from a confidently-wrong one — in
	// #334 it named a dead terminal to the human and armed a 60-minute durable timer
	// against it.
	//
	// 15s mirrors worktreeSnapshotTTL and sits ~two orders of magnitude above a healthy
	// refresh, so it never bites in normal operation: the turn-start kick plus the
	// round-0 grace keep the served snapshot sub-second whenever the MCP is answering.
	// It is the backstop for when it is not — and it independently rejects #334's 21s
	// boot-warm roster even if the grace expires.
	rosterSnapshotMaxAge = 15 * time.Second
)

// currentRosterForRound is the MODEL-FACING roster read: the cached snapshot if it can
// still be claimed live, otherwise nothing.
//
// roundZero rounds first wait up to rosterRoundZeroGrace on any in-flight refresh (the
// one runTurn kicks at the top of the turn), so the round usually builds its request on
// truth fetched for THIS turn instead of outracing it — issue #334. The wait ends early
// on the refresh landing or on ctx cancellation, and never blocks on the MCP read
// itself, which stays detached and parented off bgCtx.
//
// Every round then applies the age cap: a snapshot older than rosterSnapshotMaxAge is
// omitted and a detached refresh is kicked, so the next round self-heals rather than the
// turn going roster-blind for good. Deliberately NOT done here: reporting the age to the
// model instead of withholding the roster. The block travels the backend's strict typed
// wire contract (backend.OpenTerminal / RuntimeContext, mirrored server-side with
// extra="forbid") which has no staleness field and no free-text slot to carry one, so an
// age stamp means a lockstep backend contract change — the same reason the sibling
// worktree snapshot carries none.
func (s *Session) currentRosterForRound(ctx context.Context, roundZero bool) []backend.OpenTerminal {
	if roundZero {
		s.rosterMu.Lock()
		done := s.rosterRefreshDone
		s.rosterMu.Unlock()
		if done != nil {
			t := time.NewTimer(rosterRoundZeroGrace)
			select {
			case <-done: // the refresh reached its final outcome — read what it committed
			case <-t.C: // still unresolved: fall through to the age cap on what we hold
			case <-ctx.Done():
			}
			t.Stop()
		}
	}

	s.rosterMu.Lock()
	// A zero rosterFetchedAt means no fetch has ever committed, so anything in the cache
	// was seeded (tests) rather than observed — treat it as unservable, not as ageless.
	live := !s.rosterFetchedAt.IsZero() && time.Since(s.rosterFetchedAt) <= rosterSnapshotMaxAge
	var out []backend.OpenTerminal
	if live && len(s.roster) > 0 {
		out = append([]backend.OpenTerminal(nil), s.roster...)
	}
	// Only worth a kick when something was actually withheld: an empty cache is already
	// the value we serve, and refreshRosterAsync is single-flight anyway.
	withheld := !live && len(s.roster) > 0
	s.rosterMu.Unlock()

	if withheld {
		s.refreshRosterAsync()
	}
	return out
}

// observeRosterMutation keeps the cached open-terminal roster consistent with the
// session's OWN terminal mutations the moment their tool results settle, instead of
// waiting for the next turn's detached refresh (which the round-0 request always
// outraces). terminal.close prunes exactly the ids the result reports closed — a
// synchronous, MCP-free cache patch, honoured even on a PARTIAL failure (the ids in
// the failure's details DID close). Terminal-opening tools can't be patched in
// locally (the roster entry needs live agent state), so they just invalidate any
// in-flight fetch and kick a fresh one. Every path bumps rosterGen (via
// invalidateRosterAndRefresh / pruneRosterTerminals) so a fetch that started before
// the mutation can never land over it (see the rosterGen field doc).
//
// Scope: this observes SESSION-dispatched tools only (both dispatch paths funnel
// through emitToolSettled — wake turns included, since they run Session.Send). A
// daemon timer's direct safe-tool action bypasses it, but those cannot mutate
// terminals today; the roster also self-heals on the next turn-start refresh.
func (s *Session) observeRosterMutation(internalName, rawArgs string, res domain.ToolResult) {
	switch internalName {
	case "terminal.close":
		// Ok and partial-failure results both carry the faithfully-closed ids. Prune
		// what we know (which also bumps rosterGen AND tombstones the ids — the kicked
		// refresh below can read a server that hasn't processed the close yet, and the
		// tombstones stop that laggy snapshot from resurrecting them); an unrecognized
		// payload shape (contract drift) still invalidates so a pre-close fetch can't
		// land over the close. Either way exactly ONE gen bump — a fetch mid-close
		// retries once, and the refresh kick reconciles the cache with live truth.
		if ids := closedTerminalIDs(res); len(ids) > 0 {
			s.pruneRosterTerminals(ids)
			s.refreshRosterAsync()
		} else {
			s.invalidateRosterAndRefresh()
		}
	case "terminal.moveToWorktree":
		// A move rewrites each cached row's worktreeId, so the cache is stale either
		// way — but the ids we asked for are NOT the full set that changed: Daintree
		// moves a whole tab group together, so panes we never named can travel with
		// the ones we did. Nothing short of a re-read knows which, so invalidate
		// rather than patch. Invalidate on FAILURE too: a partial batch already
		// applied some moves, and even a fully-failed one may have been accepted by
		// Daintree before the link dropped.
		//
		// Known residual staleness, deliberately not solved here: the kicked refresh can
		// read a Daintree that has not yet applied the move and commit those pre-move
		// worktreeIds under the new generation, so the roster can show the OLD worktree
		// for a round or two. There is no tombstone equivalent to close's — the rows
		// still exist, only one field is wrong — and patching the named ids would still
		// miss the tab-group passengers a re-read exists to discover. Tolerable because
		// nothing acts on the roster's worktreeId: the follow-up instruction uses the
		// destination the caller passed, not the cached row.
		s.invalidateRosterAndRefresh()
	case "agentTask.spawnForEdits", "workflow.startWorkOnIssue", "recipe.run", "worktree.createWithRecipe":
		// Invalidate on FAILURE too: a spawn can fail ambiguously with the launch
		// already accepted by Daintree (saga `ambiguous`), or after Daintree opened a
		// diagnostic terminal — a pre-launch fetch committing afterwards would then
		// hide a terminal that really exists. A spare roster read on the rare failed
		// spawn is cheap; a stale false negative is not.
		s.invalidateRosterAndRefresh()
	case "daintree.call", "daintree.invoke":
		// Both invokers can reach unwrapped terminal mutations (terminal.new,
		// terminal.inject; terminal.kill through the raw hatch only — the wrapped ones
		// are denylisted and redirected). Only the inner action name tells; failures
		// never ran, so only successes invalidate. daintree.invoke spells that name
		// `action` rather than `name`, which is why rawCallInnerName reads both: a
		// target-aware invoke that opened a terminal must invalidate the roster for
		// exactly the reason the raw call does, and keying only on `name` would have
		// left every dynamic terminal mutation silently stale.
		// Best-effort by design: no result ids are extracted, so no tombstones — a
		// laggy post-kill fetch can transiently resurrect the terminal until the next
		// refresh. Acceptable for a rarely-used escape hatch; the wrapped close path
		// (the only one the model is steered to) gets the full tombstone guarantee.
		if res.Ok && strings.HasPrefix(strings.ToLower(rawCallInnerName(rawArgs)), "terminal.") {
			s.invalidateRosterAndRefresh()
		}
	}
}

// invalidateRosterAndRefresh bumps rosterGen — discarding any in-flight pre-mutation
// fetch — and kicks a detached refresh to reconcile the cache with live truth.
func (s *Session) invalidateRosterAndRefresh() {
	s.rosterMu.Lock()
	s.rosterGen++
	s.rosterMu.Unlock()
	s.refreshRosterAsync()
}

// rawCallInnerName extracts the inner MCP action name from either invoker's
// arguments: daintree.call spells it `name` (`{"name":"terminal.kill",...}`) and
// daintree.invoke spells it `action` (`{"action":"terminal.new",...}`). Exactly one
// is ever present, so reading both keeps this a single predicate rather than a
// per-invoker branch the next invoker would forget to extend. Empty on any parse
// failure — the caller then treats the call as non-roster.
func rawCallInnerName(rawArgs string) string {
	var a struct {
		Name   string `json:"name"`
		Action string `json:"action"`
	}
	if json.Unmarshal([]byte(rawArgs), &a) != nil {
		return ""
	}
	if a.Name != "" {
		return strings.TrimSpace(a.Name)
	}
	return strings.TrimSpace(a.Action)
}

// pruneRosterTerminals drops the given terminal ids from the cached roster, bumps
// rosterGen so a concurrent pre-mutation fetch is discarded rather than committed,
// and tombstones the ids so no LATER fetch can resurrect them either (the server
// acks a close before its terminal.list reflects it — see the rosterTombstones doc).
// Together those make the prune monotonic: a confirmed-closed id never reappears.
func (s *Session) pruneRosterTerminals(ids []string) {
	drop := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if id != "" {
			drop[id] = struct{}{}
		}
	}
	if len(drop) == 0 {
		return
	}
	s.rosterMu.Lock()
	defer s.rosterMu.Unlock()
	s.rosterGen++
	if s.rosterTombstones == nil {
		s.rosterTombstones = make(map[string]time.Time, len(drop))
	}
	expiry := time.Now().Add(rosterTombstoneTTL)
	for id := range drop {
		s.rosterTombstones[id] = expiry
	}
	if len(s.roster) == 0 {
		return
	}
	kept := make([]backend.OpenTerminal, 0, len(s.roster))
	for _, t := range s.roster {
		if _, gone := drop[t.ID]; !gone {
			kept = append(kept, t)
		}
	}
	s.roster = kept
}

// rosterTombstoneTTL bounds how long a confirmed-closed terminal id suppresses
// fetched roster entries. It only needs to outlive the server's close→list settle
// lag (observed: milliseconds — see the rosterTombstones field doc); it must NOT be
// unbounded, because close TRASHES the terminal rather than deleting it, so a human
// restore can legitimately bring the same id back.
const rosterTombstoneTTL = 30 * time.Second

// dropTombstoned returns fetched minus any terminals whose ids carry a live
// (unexpired) tombstone, deleting expired tombstones as it goes so the map stays
// bounded by recent closes. The common cases allocate nothing: no tombstones, an
// empty fetch, or a fetch with no tombstoned entries all return the input as-is.
// Caller must hold rosterMu (tombstones is the guarded map, and it is mutated here).
func dropTombstoned(fetched []backend.OpenTerminal, tombstones map[string]time.Time, now time.Time) []backend.OpenTerminal {
	if len(tombstones) == 0 {
		return fetched
	}
	for id, expiry := range tombstones {
		if !now.Before(expiry) { // expired at now >= expiry, not only strictly after
			delete(tombstones, id)
		}
	}
	if len(tombstones) == 0 || len(fetched) == 0 {
		return fetched
	}
	dead := 0
	for _, t := range fetched {
		if _, gone := tombstones[t.ID]; gone {
			dead++
		}
	}
	if dead == 0 {
		return fetched
	}
	kept := make([]backend.OpenTerminal, 0, len(fetched)-dead)
	for _, t := range fetched {
		if _, gone := tombstones[t.ID]; !gone {
			kept = append(kept, t)
		}
	}
	return kept
}

// closedTerminalIDs extracts the faithfully-closed terminal ids from a terminal.close
// result: Result["closed"] on success, Error.Details["closed"] on a partial failure
// (those ids DID close before the batch broke). Tolerates []any for any payload that
// crossed a JSON boundary. Nil when the shape is unrecognized — the caller then
// leaves the cache to the detached refresh.
func closedTerminalIDs(res domain.ToolResult) []string {
	var payload any
	if res.Ok {
		payload = res.Result
	} else if res.Error != nil {
		payload = res.Error.Details
	}
	m, ok := payload.(map[string]any)
	if !ok {
		return nil
	}
	switch v := m["closed"].(type) {
	case []string:
		// Drop empties here too (not just the []any branch): a malformed all-empty
		// list must yield len 0 so the caller takes the invalidate path (gen bump) —
		// pruneRosterTerminals would silently no-op on it without bumping.
		out := make([]string, 0, len(v))
		for _, id := range v {
			if id != "" {
				out = append(out, id)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// WarmOpenTerminals starts the same detached roster refresh normally kicked at turn
// start. App.ConnectMcp calls it during the splash so the first user request can already
// carry terminal metadata; the existing in-flight gate makes repeated connect/bootstrap
// calls harmless. It warms the current-worktree cache too — same motivation (a fast
// first submit should find both cross-turn caches already filling), same single-flight
// + TTL gates making repeated calls free.
func (s *Session) WarmOpenTerminals() {
	s.refreshRosterAsync()
	s.maybeRefreshWorktreeAsync()
}

// refreshRosterAsync starts a detached, best-effort refresh of the cached open-terminal
// roster unless one is already running (deduped via rosterRefreshing), so the turn loop
// NEVER performs the terminal.list + getStatus MCP round-trip inline. The result replaces
// the cache the moment it lands, so the next round's runtime block (rebuilt every round)
// picks it up; the turn itself streams against whatever snapshot the last refresh left —
// except that round 0 may wait up to rosterRoundZeroGrace on rosterRefreshDone, this
// refresher's completion signal, rather than assert a snapshot the refresh is about to
// contradict (issue #334). Tracked by s.wg + the draining gate exactly like startDistill so
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
	// Published with the flag so a deduped kick and a round-0 waiter observe the SAME
	// channel: whoever waits is waiting on this refresher's final outcome.
	done := make(chan struct{})
	s.rosterRefreshDone = done
	s.rosterMu.Unlock()

	// Clear the in-flight flag on EVERY exit path (draining bail-out included) so a
	// refusal here can never wedge the dedupe flag true and starve all later refreshes.
	// The channel identity check keeps this honest even if it somehow ran after a NEWER
	// refresher claimed the flag: it may only retract its own publication.
	clearInFlight := func() {
		s.rosterMu.Lock()
		s.rosterRefreshing = false
		if s.rosterRefreshDone == done {
			s.rosterRefreshDone = nil
		}
		s.rosterMu.Unlock()
	}

	// Register under s.mu, and ONLY when not draining — the same wg.Add/draining ordering
	// startDistill uses so wg.Add never races DrainBackgroundWork's Wait at counter zero
	// (the turn ctx isn't derived from bgCtx, so this can be reached mid-Shutdown).
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		clearInFlight()
		// A refusal is still a final outcome: release any waiter rather than make it
		// sleep out the full grace for a refresh that will never run.
		close(done)
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		// Runs after the commit/abandon defer below (LIFO) and before wg.Done, so a
		// waiter released here always observes the settled cache, and a drain that
		// joins this goroutine cannot leave one parked. Exactly one close: this
		// goroutine and the draining bail-out above are mutually exclusive, and the
		// gen-mismatch loop never closes per attempt.
		defer close(done)
		// Safety net only (a fetcher panic): the normal exit clears the flag INSIDE
		// the commit critical section (see why at the bottom of the loop) and must
		// then leave it ALONE — an unconditional deferred clear would run after that
		// commit, racing a newer refresher that has since claimed the flag for itself
		// and stomping its ownership (two refreshers could then run concurrently).
		committed := false
		defer func() {
			if !committed {
				clearInFlight()
			}
		}()
		// Loop until a fetch lands with rosterGen unchanged: a fetch that raced a local
		// roster mutation (gen moved while it was in flight) may predate that mutation,
		// so committing it would resurrect terminals the session just closed (or hide
		// ones it just spawned) — discard it and refetch; the refetch starts after the
		// mutation, so it carries post-mutation truth. The loop needs no artificial cap:
		// each extra iteration requires a FRESH gen bump during the previous fetch, so
		// total iterations are bounded by the number of actual local mutations — and a
		// capped give-up would silently drop the raced mutation's refresh (its kick was
		// deduped against THIS refresher, so nobody else would service it).
		for {
			s.rosterMu.Lock()
			startGen := s.rosterGen
			s.rosterMu.Unlock()
			// Dates the snapshot from the moment the fetch was ISSUED — see the
			// rosterFetchedAt field doc. Per-attempt, so only the accepted attempt's
			// stamp is ever committed; a discarded one leaves the cache's age alone.
			attemptStartedAt := time.Now()
			fresh := s.deps.OpenTerminalsFetcher(s.bgCtx)
			// Replace unconditionally (nil included): the fetcher returns nil both for a
			// transient failure AND for a genuinely-empty roster (all terminals closed), and
			// the model must not keep seeing terminals that are gone. This mirrors the old
			// synchronous behaviour (each turn showed exactly its own fetch result); a
			// transient blip simply blanks the roster for a round and self-heals on the next
			// refresh — a false negative the model recovers from, never a stale false positive.
			// The critical section is a closure with a DEFERRED unlock so that a panic
			// inside it releases rosterMu during unwinding — the safety-net defer above
			// then locks it to clear the flag, which would self-deadlock otherwise.
			if func() bool {
				s.rosterMu.Lock()
				defer s.rosterMu.Unlock()
				if s.rosterGen != startGen {
					return false
				}
				// Drop confirmed-closed ids the server may still be listing: a fetch that
				// started AFTER a close (gen matches) can still carry a PRE-close server
				// snapshot, because Daintree acks the close before terminal.list reflects
				// it. Without this filter that snapshot resurrects the closed terminals
				// until the next natural refresh (see the rosterTombstones field doc).
				s.roster = dropTombstoned(fresh, s.rosterTombstones, time.Now())
				s.rosterFetchedAt = attemptStartedAt
				// Retire the single-flight flag ATOMICALLY with the commit. Clearing it
				// later (the deferred path) would open a window where a mutation settles
				// after the commit, its refresh kick dedupes against this already-decided
				// refresher, and the mutation is never serviced — a lost refresh. The
				// done channel is retracted here too but CLOSED by the deferred close
				// above, so a waiter released by it always reads this committed state.
				s.rosterRefreshing = false
				s.rosterRefreshDone = nil
				committed = true
				return true
			}() {
				return
			}
		}
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
