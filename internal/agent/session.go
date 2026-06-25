package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
	"github.com/daintreehq/daintree-assistant/internal/skills"
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
	// fundamental in-turn orchestration step, the base prompt names it as always
	// available, and the multi-agent runbook (narrowed to core ∪ requiredTools) leans
	// on it. Read-only, so no confirmation gate. Keeping it core means a skill that
	// forgets to list it can still drive a clean cohort wait.
	"terminal.awaitAll",
	// terminal.sendCommand is core: talking to a running agent (relaying between
	// agents, answering a question it waits on) is a fundamental operation that must
	// stay callable on EVERY turn — including a skill-narrowed one. Without it here,
	// a loaded skill that omits it from requiredTools (e.g. spawn-visible-agent) would
	// silently make relaying impossible, and the base prompt's "always available"
	// claim would be false. The per-call confirmation/tier gate still governs it.
	"terminal.sendCommand",
	// terminal.close is core for the same reason: retiring an agent terminal you spawned
	// is a fundamental cohort-lifecycle operation, and the base prompt tells the model it
	// is "always here — don't tool.search for it". A loaded skill (e.g. the multi-agent
	// orchestration runbook, whose closing step calls it) narrows tools to core ∪
	// requiredTools, so without terminal.close here that very runbook would be told to
	// close terminals with a tool it cannot call. The per-call confirm/tier gate still
	// governs it. (terminal.kill — permanent delete — stays behind daintree.call.)
	"terminal.close",
	"skill.step.advance",
	"skill.run.get",
	"skill.find",
	"skill.load",
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
// to completion, owns the live model history + the three fixed control messages,
// on-demand skill loading (≤3), conversation persistence, and the event stream.
type Session struct {
	deps SessionDeps

	// mu guards ALL mutable session state below (messages, seq, activeSkills,
	// skillBundle, inFlight) — the turn goroutine and concurrent UI slash commands
	// (Clear/Compact/SetSkills, Messages, Artifacts) both touch it, so every access
	// is under this lock. Critical sections are kept SHORT: the streaming turn reads
	// an immutable SNAPSHOT of messages under the lock and releases it before the
	// (long) model stream — it never holds the lock across a network call.
	mu       sync.Mutex
	inFlight bool // a turn is running (single-flight guard + mutate-mid-turn gate)

	messages     []models.ChatMessage // live model history (controls at 0,1,2)
	seq          int                  // next DB seq to write (monotonic)
	activeSkills []string             // loaded skill ids (≤3)
	skillBundle  skills.RenderedSkillBundle
	skillCatalog string // static menu, built once (immutable after NewSession)
	events       EventSink
	artifacts    *ArtifactStore
	runRef       *RunIDRef

	// compactFailures counts CONSECUTIVE small-model auto-compact summary failures
	// (guarded by s.mu). It arms the lossy-truncation fallback once it reaches
	// AutoCompactFailureThreshold AND the history is over the hard ceiling; ANY
	// successful compaction resets it to 0 (a single transient outage must not
	// permanently disable the soft, model-summarized path).
	compactFailures int

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
	// AND across turns. The projection is pure work keyed by the offered
	// toolset (allowedNames); it only changes when skill.find/
	// skill.load mutates the active-skill set, which invalidates it in
	// applySkillBundleLocked. Guarded by s.mu (read in resolveTurnTools, zeroed in
	// applySkillBundleLocked) so the turn loop never races a /skills find slash
	// command, which calls FindSkills off the turn goroutine.
	toolProj toolProjCache

	// pendingDropCount carries RehydrateResult.DroppedRows from NewSession to the
	// first turn. Emitting the info event in NewSession would no-op (no live runID
	// yet, and the AgentEvents hook is wired AFTER Create returns), so the note is
	// deferred to runTurn — after runRef is stamped — and fired exactly once, then
	// zeroed. Single-flight Send serializes turns, so no lock guards it.
	pendingDropCount int

	// pendingInjections buffers messages the human typed WHILE a turn was in flight
	// (InjectPrompt), guarded by s.mu. The turn folds them into the live history at
	// the next tool-iteration boundary (foldInInjections), so the model picks them up
	// "between tasks" — part of the RUNNING turn, not deferred to a fresh one. The UI
	// shows a pending cue while buffered and an inline step once folded in; the
	// daemon's InjectNote uses the same iteration-boundary mechanism for its own notes.
	pendingInjections []string
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

// NewSession builds a Session: the skill catalog + the three control messages.
// On resume (deps.RestoredMessages != nil) the controls are rebuilt fresh but NOT
// re-persisted and the restored working history is appended; seq continues from
// InitialSeq. On a fresh session the controls are persisted.
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
	}
	// Detached distill work parents off the app-scoped context; fall back to
	// Background when unwired (tests) so the goroutine still has a valid parent.
	if s.bgCtx == nil {
		s.bgCtx = context.Background()
	}
	// Static catalog (every available skill's headers), built once.
	s.skillCatalog = prompts.BuildSkillCatalogMessage(toPromptMetadata(deps.SkillCatalog.MetadataForSelection()))
	s.skillBundle = skills.RenderSkillBundle(nil)
	s.activeSkills = s.skillBundle.IDs

	resume := deps.RestoredMessages != nil

	// Control messages at fixed indices.
	control := []models.ChatMessage{
		models.TextMessage("system", prompts.BaseSystemPrompt),
		models.TextMessage("system", s.composeRuntimeContext()),
		models.TextMessage("system", prompts.BuildLoadedSkillsMessage(toPromptBundle(s.skillBundle))),
	}
	s.messages = control

	if resume {
		s.messages = append(s.messages, deps.RestoredMessages...)
		s.seq = deps.InitialSeq
		if s.seq < domain.ControlMessageCount {
			s.seq = domain.ControlMessageCount
		}
		// Controls already exist in the DB — do NOT re-persist.
		// On a dup-seq forced fresh start the working history is EMPTY and we resume
		// at maxSeq+1; persist a clear breadcrumb at that collision-free seq so the
		// durable log records the reset boundary and a later resume sees a clean,
		// post-marker history rather than the dirty dup-seq rows again.
		if deps.DirtyFreshStart {
			s.persistMessage(models.TextMessage("system", domain.ClearMarker))
		}
	} else {
		// Fresh session: persist the three control rows, seq from 0.
		s.seq = 0
		for _, m := range control {
			s.persistMessage(m)
		}
	}
	return s
}

// composeRuntimeContext builds message[1]: the runtime context, then the catalog
// appended (never interleaved) so message[2] stays the loaded-skills slot and the
// "# Runtime context" header stays at the top of [1].
func (s *Session) composeRuntimeContext() string {
	runtime := prompts.BuildRuntimeContextMessage(s.deps.PromptContext)
	if s.skillCatalog != "" {
		return runtime + "\n\n" + s.skillCatalog
	}
	return runtime
}

// RefreshRuntimeContext rewrites ONLY message[1] (re-appending the catalog). The
// cached prefix [0] is untouched; not re-persisted.
func (s *Session) RefreshRuntimeContext(ctx prompts.MainPromptContext) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deps.PromptContext = ctx
	if len(s.messages) > 1 {
		s.messages[1] = models.TextMessage("system", s.composeRuntimeContext())
	}
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

// ActiveSkillIDs returns a copy of the loaded skill ids (under the lock).
func (s *Session) ActiveSkillIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.activeSkills))
	copy(out, s.activeSkills)
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

// Clear truncates the live history to the three controls and persists a CLEAR
// breadcrumb. Loaded skills are left as-is. Returns ErrTurnInProgress when a turn
// is in flight (a mid-turn clear would corrupt the streaming snapshot) — do NOT
// mutate in that case.
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
	// count trip the lossy fallback on the next outage.
	s.compactFailures = 0
	s.persistMessageLocked(models.TextMessage("system", domain.ClearMarker))
}

// Compact keeps the three controls and replaces the working history with one
// "[compacted summary…]" user note, persisting a system marker then the note.
// Returns ErrTurnInProgress when a turn is in flight (the interactive /compact
// path) — the in-turn auto-compact uses compactLocked instead.
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
	s.messages = s.messages[:domain.ControlMessageCount]
	s.persistMessageLocked(models.TextMessage("system", compactionMarker))
	note := models.TextMessage("user", compactedNotePrefix+summary)
	s.messages = append(s.messages, note)
	s.persistMessageLocked(note)
}

// SendOptions tunes a turn. Reserved for future per-turn options; currently empty.
// Every turn — user-driven OR an autonomous watcher wake — runs with the SAME full
// tool capability. (Wake turns were once narrowed to read-only inspection; that
// extra layer is gone — the per-call confirmation/tier gate in Dispatch is the one
// authority on what may mutate, so a wake turn can relay between agents, send
// terminal input, spawn, etc., exactly like a user turn.)
type SendOptions struct{}

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

// runTurn is the core loop (ordering is load-bearing).
func (s *Session) runTurn(ctx context.Context, runID, userInput string, opts SendOptions) string {
	s.events.Phase(domain.PhaseReceived)

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
	s.maybeAutoCompact(ctx)

	// 3. Re-check: a cancel landing in the auto-compact window must ALSO leave no
	//    orphan turn (issue #61).
	if ctx.Err() != nil {
		s.events.Phase(domain.PhaseCancelled)
		s.events.AssistantCancelled("")
		return domain.CancelledReply
	}

	// 4. Push the user message.
	s.pushMessage(models.TextMessage("user", userInput))

	turn := TurnContext{RunID: runID}

	failureCounts := make(map[string]int)
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
		failureCounts = make(map[string]int)
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
			s.maybeAutoCompact(ctx)
			if ctx.Err() != nil {
				s.events.Phase(domain.PhaseCancelled)
				s.events.AssistantCancelled("")
				return domain.CancelledReply
			}
		}

		// 5/9. (Re)compute the tool projection at the START of every iteration. A
		//      skill.find/skill.load run in the PREVIOUS round rewrites the active-skill
		//      set (and message[2]) mid-turn, so a newly-loaded skill's requiredTools
		//      must be offered on the very next model call of the SAME turn — and a turn
		//      that began with a skill already loaded must not be narrowed to a stale
		//      set. Recomputing every iteration is cheap (cache HIT when the skill set
		//      is unchanged) and keeps a single code path. Only message[2]/tools change
		//      here — the cached base prefix [0] stays byte-stable (prompt-cache invariant).
		//
		//      The filter + projection are computed under s.mu (released before the
		//      stream). A /skills find slash command runs OFF the turn goroutine
		//      (independent of single-flight) and can rewrite activeSkills + zero the
		//      cached projection via applySkillBundleLocked, so this read side must be
		//      synchronized with those writes. The cached projection is reused when the
		//      offered toolset is unchanged (the common path — no skill mutation).
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

		// 10d. Stream the large model. Separate <think> from visible text: the router
		//      delivers visible tokens via onToken and the <think> body via
		//      ChatResult.Reasoning (the ThinkFilter handles the split).
		gotToken := false
		// Read an immutable SNAPSHOT of the history under the lock, then stream with
		// the lock RELEASED — the model call is long, and a concurrent InjectNote
		// (daemon) or InjectPrompt (user) must be able to append without racing this read.
		result, serr := s.deps.Router.Stream(ctx, domain.ModelLarge, models.ChatOptions{
			Messages:       s.snapshotMessages(),
			Tools:          tools,
			ToolChoice:     "auto",
			PromptCacheKey: domain.MainPromptCacheKey,
		}, func(tok string) {
			if !gotToken {
				gotToken = true
				s.events.Phase(domain.PhaseGenerating)
			}
			s.events.AssistantToken(tok)
		})
		if serr != nil {
			return s.classifyStreamError(serr)
		}

		// 10e. Usage — computed BEFORE appending the assistant message so
		//      contextTokens reflects the prompt actually sent.
		s.emitUsage()

		// 10f. Append the assistant message (content null on a pure tool-call turn).
		s.pushMessage(s.assistantMessage(result))

		// 10g. No tool calls ⇒ the model thinks it's done. But if the user slipped a
		//      message in during this round, DON'T seal — fold it in and loop so it is
		//      answered (never strand an injection at the final-answer boundary). The
		//      just-streamed content stays an unsealed intermediate round (exactly like
		//      prose that precedes a tool batch), so no AssistantEnd fires yet.
		if len(result.ToolCalls) == 0 {
			if s.foldInInjections() > 0 {
				resetForInjection()
				continue
			}
			s.events.Phase(domain.PhaseComplete)
			s.events.AssistantEnd(result.Content, result.Reasoning)
			return result.Content
		}

		// 10h. Execute the batch. Announce ALL calls as queued BEFORE sequential
		//      dispatch, then promote each queued→active→done/failed.
		if reply, done := s.runToolBatch(ctx, result.ToolCalls, turn, allowedSet, failureCounts, &stuckNudged); done {
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

// runToolBatch dispatches a batch of tool calls sequentially after announcing the
// whole batch as queued. Returns (reply, true) when the turn must end (cancel or
// circuit-breaker abort), else ("", false) to continue the iteration loop.
func (s *Session) runToolBatch(ctx context.Context, calls []models.ToolCallRequest, turn TurnContext,
	allowedSet map[string]struct{}, failureCounts map[string]int, stuckNudged *bool) (string, bool) {

	// Announce the whole batch as queued first.
	batch := make([]BatchedToolCall, 0, len(calls))
	for _, call := range calls {
		internalName := s.resolveInternal(call.Function.Name)
		batch = append(batch, BatchedToolCall{ID: call.ID, Name: internalName, Args: call.Function.Arguments})
	}
	s.events.Phase(domain.PhaseToolQueued)
	s.events.ToolBatch(batch)

	type worst struct {
		name  string
		count int
		res   domain.ToolResult
	}
	var worstRepeat *worst

	for c := 0; c < len(calls); c++ {
		call := calls[c]
		internalName := s.resolveInternal(call.Function.Name)

		// Cancel BEFORE activating/dispatching this call: a cancel that landed while
		// the PREVIOUS call ran must stop the whole queue here, so no further tool
		// executes after the user hit Escape. The current call AND every remaining
		// one (calls[c:]) get a structurally-valid CANCELLED tool result, so each
		// assistant tool_call still has a matching reply (or Fireworks 400s on
		// replay).
		if ctx.Err() != nil {
			s.stubCancelledFrom(calls, c)
			s.events.Phase(domain.PhaseCancelled)
			s.events.AssistantCancelled("")
			return domain.CancelledReply, true
		}

		// Promote queued→active and drive the phase.
		s.events.Phase(domain.PhaseToolRunning)
		s.events.ToolState(call.ID, ToolStateActive)

		startedAt := domain.NowMS()
		var res domain.ToolResult

		// Parse args (catch → INVALID_TOOL_ARGS_JSON, recoverable). The ToolCall
		// event carries the raw args string on parse failure, else the raw string
		// passed through to dispatch.
		parseFailed := false
		if call.Function.Arguments != "" {
			var probe any
			if json.Unmarshal([]byte(call.Function.Arguments), &probe) != nil {
				parseFailed = true
			}
		}

		s.events.ToolCall(ToolCallEvent{ID: call.ID, Name: internalName, Args: call.Function.Arguments, StartedAt: startedAt})

		switch {
		case parseFailed:
			res = domain.Fail("INVALID_TOOL_ARGS_JSON", "Arguments were not valid JSON.")
			res.Summary = "Invalid JSON arguments for " + internalName + "; not executed."
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
		default:
			argsJSON := call.Function.Arguments
			if argsJSON == "" {
				argsJSON = "{}"
			}
			// Per-call progress plumbing: stamp the active call id + a forwarder so an
			// in-tool substep emits ToolProgress(callID, msg) on this turn's sink,
			// tagged so the UI maps it to the right activity row.
			callTurn := turn
			callTurn.CallID = call.ID
			callTurn.Progress = func(callID string, msg string) {
				if msg == "" {
					return
				}
				s.events.ToolProgress(callID, msg)
			}
			res = s.deps.Tools.Dispatch(ctx, internalName, argsJSON, callTurn)
		}

		s.events.ToolResult(ToolResultEvent{ID: call.ID, Name: internalName, Result: res, EndedAt: domain.NowMS()})
		if res.Ok {
			s.events.ToolState(call.ID, ToolStateDone)
		} else {
			s.events.ToolState(call.ID, ToolStateFailed)
		}

		s.pushMessage(models.ChatMessage{
			Role:          "tool",
			ToolCallID:    call.ID,
			Name:          internalName,
			StringContent: SerializeToolResult(res, s.artifacts),
		})

		// Circuit-breaker bookkeeping: signature is the CANONICALIZED argument JSON
		// (key order / whitespace normalized) + the error code, so a semantically
		// identical call failing the SAME way increments the same counter even when
		// the model re-emits it with reordered keys.
		if !res.Ok {
			errCode := ""
			if res.Error != nil {
				errCode = res.Error.Code
			}
			sig := failureSignature(internalName, call.Function.Arguments, errCode)
			failureCounts[sig]++
			count := failureCounts[sig]
			if worstRepeat == nil || count > worstRepeat.count {
				worstRepeat = &worst{name: internalName, count: count, res: res}
			}
		}

		// Mid-batch cancel: a cancel that landed DURING this call's dispatch stops the
		// queue now. This call already has its real result; stub every remaining
		// undispatched call (calls[c+1:]) so the transcript stays well-formed (each
		// assistant tool_call needs a matching tool result, or Fireworks 400s on
		// replay).
		if ctx.Err() != nil {
			s.stubCancelledFrom(calls, c+1)
			s.events.Phase(domain.PhaseCancelled)
			s.events.AssistantCancelled("")
			return domain.CancelledReply, true
		}
	}

	// After every call in the batch has a result, apply the circuit breaker.
	if worstRepeat != nil && worstRepeat.count >= domain.RepeatFailureAbort {
		detail := worstRepeat.res.Summary
		if worstRepeat.res.Error != nil && worstRepeat.res.Error.Code != "" {
			detail = trimSpace(worstRepeat.res.Error.Code + ": " + worstRepeat.res.Error.Message)
		}
		msg := "Stopped: called " + worstRepeat.name + " " + itoa(worstRepeat.count) +
			" times this turn with identical arguments, each failing the same way (" + detail +
			"). Tell the user what's blocking and what you tried rather than repeating the call."
		s.events.Phase(domain.PhaseFailed)
		s.events.Error(msg)
		return msg, true
	}
	if worstRepeat != nil && worstRepeat.count >= domain.RepeatFailureWarn && !*stuckNudged {
		*stuckNudged = true
		codeSuffix := ""
		if worstRepeat.res.Error != nil && worstRepeat.res.Error.Code != "" {
			codeSuffix = " (" + worstRepeat.res.Error.Code + ")"
		}
		nudge := "[system event]\nYou have called " + worstRepeat.name + " " + itoa(worstRepeat.count) +
			" times this turn with the same arguments and it failed the same way each time" + codeSuffix +
			". Repeating the exact same call will keep failing. Read the error, CHANGE the arguments (or use a different tool/approach), or stop and report what's blocking you — do not emit the same arguments again."
		s.pushMessage(models.TextMessage("user", nudge))
		// Surface the stuck loop to the human too. The nudge above only steers the model;
		// without this, a tool repeating the same failure stayed invisible in the footer
		// until the turn either recovered or burned all the way to the abort threshold.
		s.events.Warn(worstRepeat.name + " keeps failing the same way" + codeSuffix +
			" — nudging the assistant to change approach.")
	}
	return "", false
}

// stubCancelledFrom pushes a structurally-valid CANCELLED tool result for every
// call in calls[from:] (none of which executed), so each assistant tool_call keeps
// a matching tool reply and the transcript replays cleanly.
func (s *Session) stubCancelledFrom(calls []models.ToolCallRequest, from int) {
	for r := from; r < len(calls); r++ {
		pending := calls[r]
		pendingName := s.resolveInternal(pending.Function.Name)
		stub := domain.Fail("CANCELLED", "Turn cancelled.", domain.Unrecoverable())
		stub.Summary = "Turn cancelled before this tool was executed."
		s.pushMessage(models.ChatMessage{
			Role:          "tool",
			ToolCallID:    pending.ID,
			Name:          pendingName,
			StringContent: SerializeToolResult(stub, s.artifacts),
		})
	}
}

// classifyStreamError maps a router stream error to its sentinel reply. The
// prefixes are WAKE_FAILURE_PREFIXES — keep them byte-stable.
func (s *Session) classifyStreamError(err error) string {
	var cancelled *models.CancelledError
	if errors.As(err, &cancelled) || errors.Is(err, context.Canceled) {
		s.events.Phase(domain.PhaseCancelled)
		s.events.AssistantCancelled("")
		return domain.CancelledReply
	}
	var unavailable *models.FireworksUnavailableError
	if errors.As(err, &unavailable) {
		msg := "Model unavailable: " + err.Error()
		s.events.Phase(domain.PhaseFailed)
		s.events.Error(msg)
		return msg
	}
	var rateLimited *models.RateLimitedError
	if errors.As(err, &rateLimited) {
		// Retry budget exhausted on a 429: a friendly, byte-stable reply plus a health
		// badge — not the raw provider blob. The badge clears on the next good Usage.
		msg := "Model rate-limited: " + rateLimited.Error()
		s.events.Phase(domain.PhaseFailed)
		s.events.ModelRateLimited()
		s.events.Error(msg)
		return msg
	}
	msg := "Model error: " + err.Error()
	s.events.Phase(domain.PhaseFailed)
	s.events.Error(msg)
	return msg
}

// assistantMessage builds the assistant message for a streamed result: content
// null on a pure tool-call turn (no prose), else the visible content.
func (s *Session) assistantMessage(result models.ChatResult) models.ChatMessage {
	m := models.ChatMessage{Role: "assistant"}
	if result.Content == "" && len(result.ToolCalls) > 0 {
		m.ContentNull = true
	} else {
		m.StringContent = result.Content
	}
	if len(result.ToolCalls) > 0 {
		m.ToolCalls = result.ToolCalls
	}
	return m
}

// emitUsage emits the per-round UsageEvent. It drains the Router-level meter
// (FlushMeter) and sums EVERY model call made since the last round — the large
// thread's stream plus the small-tier background work (skill selection, watcher
// verdicts, extraction, summaries) that previously went unmetered — into one
// aggregate. CachedTokens is nil unless some tier reported it; CostUsd sums the
// known per-tier costs (a partial total beats showing nothing) and is nil only
// when no tier had a known rate, so the UI shows "no data" not a misleading
// $0.000. Tier/Model stay the large-tier display rollup for the footer label.
func (s *Session) emitUsage() {
	tiers := s.deps.Router.FlushMeter()
	ev := UsageEvent{
		ContextTokens:    s.estimateTokens(),
		ContextThreshold: domain.AutoCompactTokenThreshold,
		ContextWindow:    domain.LargeContextWindowTokens,
		Tier:             string(domain.ModelLarge),
		Model:            models.BareModelID(s.deps.Router.ModelFor(domain.ModelLarge)),
		Tiers:            tiers,
	}
	var (
		anyCost     bool
		costTotal   float64
		anyCached   bool
		cachedTotal int
	)
	for _, t := range tiers {
		ev.PromptTokens += t.PromptTokens
		ev.CompletionTokens += t.CompletionTokens
		ev.TotalTokens += t.TotalTokens
		if t.CostUsd != nil {
			anyCost = true
			costTotal += *t.CostUsd
		}
		if t.CachedTokens != nil {
			anyCached = true
			cachedTotal += *t.CachedTokens
		}
	}
	if anyCached {
		ev.CachedTokens = &cachedTotal
	}
	if anyCost {
		ev.CostUsd = &costTotal
	}
	s.events.Usage(ev)
}

// resolveTurnTools computes the per-iteration tool filter, its membership set, and
// the projected specs under s.mu — a short, in-memory critical section released
// before the (long) model stream. Holding the lock serializes this read against
// applySkillBundleLocked's writes (activeSkills + the cached projection), which a
// /skills find slash command can trigger off the turn goroutine. Returns nil
// allowedNames/allowedSet for an unconstrained (full-registry) turn.
func (s *Session) resolveTurnTools() ([]string, map[string]struct{}, []models.ChatTool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	allowedNames := s.buildToolFilterLocked()
	// Preserve nil semantics: an unconstrained turn (nil) offers the FULL registry,
	// and both the tool-not-offered refusal in runToolBatch and the dispatch gate key
	// off allowedSet being nil ⇒ "all tools callable". Materialize the set only when
	// the turn is actually narrowed (a skill is loaded).
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
// slices.Equal treats nil and []string{} as equal. The cache is invalidated only
// when skill.find/skill.load rewrites the active set (applySkillBundleLocked), so
// across a turn's iterations (and across turns) with a stable skill set this skips
// re-projecting every tool spec and rebuilding the registry's wire-name maps.
// Caller MUST hold s.mu (it reads/writes the toolProj cache, which a concurrent
// skill mutation zeroes under the lock).
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
	rec := domain.ConversationMessageRecord{
		SessionID:     s.deps.SessionID,
		Seq:           s.seq,
		Role:          m.Role,
		Content:       m.ContentToText(),
		ToolCallsJson: toolCallsJSON,
		ToolCallID:    toolCallID,
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
func (s *Session) maybeAutoCompact(ctx context.Context) {
	// Build the summary input under the lock (read a stable snapshot), then run the
	// model call OUTSIDE the lock. Runs on the turn goroutine with inFlight set, so
	// it compacts via compactLocked (the public Compact would self-reject).
	s.mu.Lock()
	est := s.estimateTokensLocked()
	// Skip when under the soft threshold, OR when there's no real history to
	// summarize (≤1 working message) — UNLESS that lone message is itself over the
	// hard ceiling, in which case the bounded-growth fallback must still get a chance
	// to run rather than letting a single oversized message grow context unbounded.
	if est <= domain.AutoCompactTokenThreshold ||
		(len(s.messages) <= domain.ControlMessageCount+1 && est < domain.AutoCompactHardTruncationThreshold) {
		s.mu.Unlock()
		return
	}
	// Flatten multimodal history to text (the small model is text-only; an image
	// turn would otherwise trip the vision tier gate and silently fail every
	// auto-compact, growing history unbounded).
	summaryMsgs := []models.ChatMessage{
		models.TextMessage("system", "Summarize the conversation below in 2-3 sentences: the current goals, key decisions made, and any pending work. Be concise and factual."),
	}
	// Capture a flattened transcript from the SAME snapshot (still under the lock) so
	// the distillation pass can mine the about-to-be-discarded history after the lock
	// is released for the model calls.
	transcript := ""
	for _, m := range s.messages[domain.ControlMessageCount:] {
		text := m.ContentToText()
		summaryMsgs = append(summaryMsgs, models.TextMessage(m.Role, text))
		if text == "" {
			text = "[tool call]"
		}
		transcript += m.Role + ": " + text + "\n"
	}
	s.mu.Unlock()

	result, err := s.deps.Router.Chat(ctx, domain.ModelSmall, models.ChatOptions{Messages: summaryMsgs})
	summary := ""
	if err == nil {
		summary = trimSpace(result.Content)
	}
	if err != nil || summary == "" {
		// A user cancel landing mid-summary is the turn tearing down, NOT a provider
		// outage — don't count it toward the fallback and don't mutate history while
		// the turn is aborting (issue #61: leave no orphan state).
		if ctx.Err() != nil {
			return
		}
		s.mu.Lock()
		truncated := s.noteCompactFailureLocked()
		s.mu.Unlock()
		switch {
		case truncated:
			s.events.Info("Auto-compact fallback: truncated old history (summary unavailable)")
		case err != nil:
			s.events.Info("Auto-compact skipped: summary failed")
		default:
			s.events.Info("Auto-compact skipped: empty summary")
		}
		return
	}

	// Summary in hand: compact IMMEDIATELY so the large-model stream is unblocked,
	// then distill durable facts off the critical path. compactLocked resets the
	// failure streak so one transient outage never permanently disarms the soft path.
	s.mu.Lock()
	s.compactLocked(summary)
	s.mu.Unlock()
	s.events.Info("Auto-compacted conversation")

	s.startDistill(transcript)
}

// noteCompactFailureLocked records a failed/empty auto-compact summary and, once
// failures have been SUSTAINED (>= AutoCompactFailureThreshold) AND the history has
// ballooned past the hard ceiling, performs a no-model lossy head-truncation so a
// small-model outage can't grow the conversation without bound. Resets the counter
// after truncating (the bound has been re-established). Caller MUST hold s.mu.
// Returns true iff it truncated.
func (s *Session) noteCompactFailureLocked() (truncated bool) {
	s.compactFailures++
	if s.compactFailures >= domain.AutoCompactFailureThreshold &&
		s.estimateTokensLocked() >= domain.AutoCompactHardTruncationThreshold {
		s.truncateLocked(domain.AutoCompactHardTruncationKeepMessages) // resets compactFailures
		return true
	}
	return false
}

// truncateLocked is the no-model lossy fallback for a sustained small-model outage:
// keep the three control messages plus at most the most-recent keepN working
// messages (oldest dropped first), then shed further from the head of that tail until
// the estimate is back under the hard ceiling — guaranteeing the bound even when a
// single retained message is itself enormous. Orphaned tool results and an incomplete
// trailing tool call are cleaned exactly as a resume would, so the retained tail is a
// valid model history Fireworks won't reject. A compaction marker is persisted (so a
// later resume rebuilds from the truncation boundary) followed by each retained
// message at a fresh monotonic seq. Caller MUST hold s.mu.
func (s *Session) truncateLocked(keepN int) {
	// The bound is being re-established by this truncation, so the failure streak that
	// armed it resets here (the single reset point for the fallback path).
	s.compactFailures = 0
	working := s.messages[domain.ControlMessageCount:]
	if len(working) > keepN {
		working = working[len(working)-keepN:]
	}
	// Copy before any reslice of s.messages so the cleanup passes (and the re-append
	// below) never alias the backing array we are about to overwrite.
	tail := make([]models.ChatMessage, len(working))
	copy(tail, working)
	// The drop counts are irrelevant on this lossy-truncation path (it intentionally
	// sheds history without surfacing a corruption note); discard them.
	tail, _ = dropOrphanToolResults(tail)
	tail, _ = dropOrphanToolCallTail(tail)
	// Shed from the head until under the hard ceiling, re-cleaning orphans after each
	// drop so the tail stays a valid history. The controls are fixed, so only their
	// contribution is constant; estimateMessagesTokens is summed fresh on the tail.
	controlTokens := estimateMessagesTokens(s.messages[:domain.ControlMessageCount])
	for len(tail) > 0 && controlTokens+estimateMessagesTokens(tail) >= domain.AutoCompactHardTruncationThreshold {
		tail = tail[1:]
		tail, _ = dropOrphanToolResults(tail)
		tail, _ = dropOrphanToolCallTail(tail)
	}
	s.messages = s.messages[:domain.ControlMessageCount]
	s.persistMessageLocked(models.TextMessage("system", compactionMarker))
	for _, m := range tail {
		s.messages = append(s.messages, m)
		s.persistMessageLocked(m)
	}
}

// startDistill mines durable facts from the just-discarded transcript on a DETACHED
// goroutine so the user's turn streams immediately. It parents off the app-scoped
// bgCtx (cancelled on App.Shutdown) with a bounded timeout, and is tracked by s.wg so
// DrainBackgroundWork can join it before the Router/MemoryStore it touches tear down.
// A nil MemoryStore makes distillCompact a no-op, so skip the goroutine entirely.
// IMPORTANT: it must NOT emit events — the prod RunEventSink is single-goroutine
// (turn-only) and unguarded, so an Info call from here would race the streaming turn.
func (s *Session) startDistill(transcript string) {
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
		s.distillCompact(bg, transcript)
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

// distillTranscriptMaxRunes caps the transcript fed to the distillation model — the
// small model needs only enough context to extract facts, not the full summary input.
const distillTranscriptMaxRunes = 8000

// distillCompact extracts durable facts from a soon-to-be-discarded transcript via a
// single small-model call and saves the novel ones as source="compact" memories.
// Best-effort by construction: a nil MemoryStore, an empty transcript, a model error,
// an unparseable reply, or any panic yields 0 and never affects compaction. It MUST be
// called with s.mu released (it makes a network call + DB writes).
func (s *Session) distillCompact(ctx context.Context, transcript string) (saved int) {
	defer func() { _ = recover() }()
	if s.deps.MemoryStore == nil {
		return 0
	}
	// Keep the freshest TAIL — durable decisions are most likely near the end of the
	// conversation, while the head is the part most likely already summarized away.
	if r := []rune(transcript); len(r) > distillTranscriptMaxRunes {
		transcript = string(r[len(r)-distillTranscriptMaxRunes:])
	}
	if trimSpace(transcript) == "" {
		return 0
	}
	maxTok := 600
	result, err := s.deps.Router.Chat(ctx, domain.ModelSmall, models.ChatOptions{
		Messages: []models.ChatMessage{
			models.TextMessage("system", prompts.DistillSystemPrompt),
			models.TextMessage("user", transcript),
		},
		MaxTokens: &maxTok,
	})
	if err != nil {
		return 0
	}
	for _, fact := range prompts.ParseDistilledFacts(result.Content) {
		exists, exErr := s.deps.MemoryStore.MemoryExists(fact)
		if exErr != nil || exists {
			continue
		}
		now := domain.NowMS()
		if _, insErr := s.deps.MemoryStore.InsertMemory(domain.MemoryRecord{
			Content:   fact,
			Source:    domain.MemoryCompact,
			CreatedAt: now,
			UpdatedAt: now,
		}); insErr == nil {
			saved++
		}
	}
	return saved
}

// Artifacts exposes the session's overflow store so the artifact.read tool family
// can page back through oversized tool results stashed during the turn. The store
// is created in NewSession and lives for the session.
func (s *Session) Artifacts() *ArtifactStore { return s.artifacts }

// FindSkills runs the skill.find engine. On a selector error/cancel it
// leaves the loaded set unchanged. New ids merge FIRST so they survive the cap of
// 3 (an explicit/new load evicts the lowest-priority prior skill).
// FindSkills is invoked by the skill.find TOOL on the turn goroutine, so it does
// NOT reject mid-turn — it merely takes the lock for its short mutation. The
// selector call runs OUTSIDE the lock (it's a model call).
func (s *Session) FindSkills(ctx context.Context, query string) skills.SkillFindResult {
	selection, err := s.deps.SkillSelector.Select(ctx, s.deps.SkillCatalog.MetadataForSelection(), query)
	if err != nil {
		return skills.SkillFindResult{Ok: false, Matched: false, Query: query, Reason: "skill selector unavailable", ActiveSkillIDs: s.ActiveSkillIDs()}
	}
	if ctx.Err() != nil {
		// Don't mutate the live skill set with an abandoned result.
		return skills.SkillFindResult{Ok: false, Matched: false, Query: query, Reason: "cancelled", ActiveSkillIDs: s.ActiveSkillIDs()}
	}
	newlyKnown := s.resolveKnownIDs(selection.SkillIDs)

	s.mu.Lock()
	merged := s.resolveKnownIDs(append(append([]string{}, selection.SkillIDs...), s.activeSkills...))
	s.applySkillBundleLocked(s.deps.SkillCatalog.GetMany(merged))
	active := s.activeSkillIDsLocked()
	s.mu.Unlock()

	s.logSelection(query, selection, newlyKnown)

	selected := make([]skills.SelectedSkill, 0, len(newlyKnown))
	for _, sk := range s.deps.SkillCatalog.GetMany(newlyKnown) {
		selected = append(selected, skills.SelectedSkill{ID: sk.ID, Title: sk.Title, Summary: sk.Summary})
	}
	return skills.SkillFindResult{
		Ok: true, Matched: len(selected) > 0, Query: query,
		Reason: selection.Reason, Confidence: selection.Confidence,
		Selected: selected, ActiveSkillIDs: active,
	}
}

// LoadAdditionalSkills is the skill.load TOOL path (turn goroutine): it merges new
// ids FIRST (so an explicit load evicts the lowest-priority prior skill rather than
// being dropped), applies under the lock, and returns the active set. It does NOT
// reject mid-turn (the tool runs as part of the turn).
func (s *Session) LoadAdditionalSkills(ids []string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := s.resolveKnownIDs(append(append([]string{}, ids...), s.activeSkills...))
	s.applySkillBundleLocked(s.deps.SkillCatalog.GetMany(merged))
	return s.activeSkillIDsLocked()
}

// SetSkills replaces the loaded set with the resolved-known ids. It is a UI-command
// path (e.g. /skills load|clear), so it returns ErrTurnInProgress when a turn is in
// flight rather than racing the streaming snapshot.
func (s *Session) SetSkills(ids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inFlight {
		return ErrTurnInProgress
	}
	s.applySkillBundleLocked(s.deps.SkillCatalog.GetMany(s.resolveKnownIDs(ids)))
	return nil
}

// activeSkillIDsLocked returns a copy of the loaded ids; caller MUST hold s.mu.
func (s *Session) activeSkillIDsLocked() []string {
	out := make([]string, len(s.activeSkills))
	copy(out, s.activeSkills)
	return out
}

// resolveKnownIDs = unique(ids).filter(has).slice(0,3) — filter BEFORE the cap so
// a hallucinated id can't evict a valid one.
func (s *Session) resolveKnownIDs(ids []string) []string {
	seen := make(map[string]struct{})
	var out []string
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		if s.deps.SkillCatalog.Has(id) {
			out = append(out, id)
			if len(out) == 3 {
				break
			}
		}
	}
	return out
}

// applySkillBundleLocked re-renders the bundle, updates active ids, and rewrites
// message[2]. Caller MUST hold s.mu.
func (s *Session) applySkillBundleLocked(sks []skills.Skill) {
	s.skillBundle = skills.RenderSkillBundle(sks)
	s.activeSkills = s.skillBundle.IDs
	if len(s.messages) > 2 {
		s.messages[2] = models.TextMessage("system", prompts.BuildLoadedSkillsMessage(toPromptBundle(s.skillBundle)))
	}
	// The active-skill set drives buildToolFilter, so the memoized projection is now
	// stale — drop it so the next iteration rebuilds against the new toolset.
	s.toolProj = toolProjCache{}
}

// logSelection best-effort records what the QUERY resolved (newlyKnown, NOT the
// merged set), with userInput sliced to 1000 chars.
func (s *Session) logSelection(userInput string, selection skills.SkillSelection, selectedIDs []string) {
	defer func() { _ = recover() }()
	if s.deps.Store == nil {
		return
	}
	idsJSON, _ := json.Marshal(selectedIDs)
	taskType := selection.TaskType
	reason := selection.Reason
	rec := domain.SkillSelectionLogRecord{
		SessionID:            s.deps.SessionID,
		UserInput:            sliceChars(userInput, 1000),
		SelectedSkillIdsJson: string(idsJSON),
		Confidence:           selection.Confidence,
	}
	if taskType != "" {
		rec.TaskType = &taskType
	}
	if reason != "" {
		rec.Reason = &reason
	}
	_, _ = s.deps.Store.InsertSkillSelection(rec)
}

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

// toPromptMetadata / toPromptBundle adapt the skills package shapes to the prompts
// package shapes (the prompt builders own their own slim view types).
func toPromptMetadata(md []skills.SkillMetadata) []prompts.SkillMetadata {
	out := make([]prompts.SkillMetadata, 0, len(md))
	for _, m := range md {
		out = append(out, prompts.SkillMetadata{ID: m.ID, Summary: m.Summary, WhenToUse: m.WhenToUse})
	}
	return out
}

func toPromptBundle(b skills.RenderedSkillBundle) prompts.RenderedSkillBundle {
	items := make([]prompts.LoadedSkill, 0, len(b.Items))
	for _, sk := range b.Items {
		items = append(items, prompts.LoadedSkill{ID: sk.ID, Version: sk.Version, Title: sk.Title, Body: sk.Body})
	}
	return prompts.RenderedSkillBundle{Items: items}
}
