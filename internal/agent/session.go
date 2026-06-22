package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
	"github.com/daintreehq/daintree-assistant/internal/skills"
)

// CORE_TOOL_NAMES — always offered to the model regardless of loaded skills. The
// union with loaded skills' requiredTools forms the per-turn projection. Internal
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
	"skill.step.advance",
	"skill.run.get",
	"skill.find",
	"skill.load",
	"memory.recall",
	"memory.list",
	"artifact.read",
}

// SkillContextMutatingTools are read-risk tools withheld on read-only wake turns
// (they mutate the loaded-skill set). The ToolRunner adapter uses this set when
// building ReadOnlyToolNames so both sides share one source of truth.
var SkillContextMutatingTools = map[string]struct{}{
	"skill.find": {},
	"skill.load": {},
}

// IsSkillContextMutating reports whether a tool mutates the loaded-skill set (and
// so is withheld on a read-only wake turn).
func IsSkillContextMutating(name string) bool {
	_, ok := SkillContextMutatingTools[name]
	return ok
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
		deps:      deps,
		events:    deps.Events,
		artifacts: NewArtifactStore(),
		runRef:    deps.RunRef,
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
	s.messages = s.messages[:domain.ControlMessageCount]
	s.persistMessageLocked(models.TextMessage("system", compactionMarker))
	note := models.TextMessage("user", compactedNotePrefix+summary)
	s.messages = append(s.messages, note)
	s.persistMessageLocked(note)
}

// SendOptions tunes a turn. ReadOnly marks an autonomous wake turn (read-risk
// tools only, enforced at dispatch).
type SendOptions struct {
	ReadOnly bool
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

// runTurn is the core loop (ordering is load-bearing).
func (s *Session) runTurn(ctx context.Context, runID, userInput string, opts SendOptions) string {
	s.events.Phase(domain.PhaseReceived)

	// Persist the originating prompt as the run's FIRST durable row so /explain can
	// label the run by what prompted it. Emitted before AssistantStart and before
	// the cancel check below, so even an immediately-aborted turn carries a label.
	s.events.TurnPrompt(userInput)

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

	turn := TurnContext{RunID: runID, ReadOnly: opts.ReadOnly}

	failureCounts := make(map[string]int)
	stuckNudged := false

	for i := 0; i < domain.MaxToolIterations; i++ {
		// 10a. Cancel check at the top of each round.
		if ctx.Err() != nil {
			s.events.Phase(domain.PhaseCancelled)
			s.events.AssistantCancelled("")
			return domain.CancelledReply
		}

		// 5/9. (Re)compute the tool projection at the START of every iteration. A
		//      skill.find/skill.load run in the PREVIOUS round rewrites the active-skill
		//      set (and message[2]) mid-turn, so a newly-loaded skill's requiredTools
		//      must be offered on the very next model call of the SAME turn — and a turn
		//      that began with a skill already loaded must not be narrowed to a stale
		//      set. Read-only wake turns withhold the skill-mutating tools, so their
		//      projection can't change; recomputing is still correct (it's stable) and
		//      keeps a single code path. Only message[2]/tools change here — the cached
		//      base prefix [0] stays byte-stable (prompt-cache invariant).
		var allowedNames []string
		if opts.ReadOnly {
			allowedNames = s.deps.Tools.ReadOnlyToolNames()
		} else {
			allowedNames = s.buildToolFilter()
		}
		// Preserve nil semantics: buildToolFilter returns nil for an unconstrained
		// turn (the FULL registry), and BOTH the read-only-turn refusal in
		// runToolBatch and the dispatch projection gate key off allowedSet/
		// ActiveToolNames being nil ⇒ "all tools callable". Only materialize the set
		// (and the allowlist) when the turn is actually narrowed.
		var allowedSet map[string]struct{}
		if allowedNames != nil {
			allowedSet = make(map[string]struct{}, len(allowedNames))
			for _, n := range allowedNames {
				allowedSet[n] = struct{}{}
			}
		}
		turn.ActiveToolNames = allowedNames

		// Project tools (a failure is a WAKE_FAILURE_PREFIX — keep verbatim).
		tools, err := s.deps.Tools.OpenAITools(allowedNames)
		if err != nil {
			msg := "Tool projection failed: " + err.Error()
			s.events.Phase(domain.PhaseFailed)
			s.events.Error(msg)
			return msg
		}

		// 10b. New round.
		if i == 0 {
			s.events.Phase(domain.PhaseAnalyzing)
		} else {
			s.events.Phase(domain.PhaseIntegrating)
		}
		s.events.AssistantStart()

		// 10c. Stream the large model. Separate <think> from visible text: the router
		//      delivers visible tokens via onToken and the <think> body via
		//      ChatResult.Reasoning (the ThinkFilter handles the split).
		gotToken := false
		// Read an immutable SNAPSHOT of the history under the lock, then stream with
		// the lock RELEASED — the model call is long, and a concurrent InjectNote
		// (daemon) must be able to append without racing this read (Fix: session race).
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

		// 10d. Usage — computed BEFORE appending the assistant message so
		//      contextTokens reflects the prompt actually sent.
		s.emitUsage()

		// 10e. Append the assistant message (content null on a pure tool-call turn).
		s.pushMessage(s.assistantMessage(result))

		// 10f. No tool calls ⇒ final answer.
		if len(result.ToolCalls) == 0 {
			s.events.Phase(domain.PhaseComplete)
			s.events.AssistantEnd(result.Content, result.Reasoning)
			return result.Content
		}

		// 10g. Execute the batch. Announce ALL calls as queued BEFORE sequential
		//      dispatch, then promote each queued→active→done/failed.
		if reply, done := s.runToolBatch(ctx, result.ToolCalls, turn, allowedSet, failureCounts, &stuckNudged); done {
			return reply
		}
	}

	// 11. Fell out of the loop.
	msg := "Reached the tool-iteration limit without a final answer."
	s.events.Phase(domain.PhaseFailed)
	s.events.Error(msg)
	return msg
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
			// Read-only-turn refusal, double-gated (the list filter alone is
			// insufficient — ResolveWireName can fall through to a raw name).
			res = domain.Fail("READ_ONLY_TURN",
				"Mutating tools are disabled on autonomous wake-up turns; only read-only inspection is allowed.",
				domain.Unrecoverable())
			res.Summary = internalName + " is not available on an autonomous read-only turn."
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

// buildToolFilter returns the per-turn tool projection. No active skills ⇒ nil
// (the FULL registry — an unconstrained turn must not be starved of tools).
// Else: unique(core ∪ skills.requiredTools).
func (s *Session) buildToolFilter() []string {
	if len(s.activeSkills) == 0 {
		return nil
	}
	seen := make(map[string]struct{})
	var out []string
	add := func(n string) {
		if _, ok := seen[n]; !ok {
			seen[n] = struct{}{}
			out = append(out, n)
		}
	}
	for _, n := range coreToolNames {
		add(n)
	}
	for _, sk := range s.deps.SkillCatalog.GetMany(s.activeSkills) {
		for _, t := range sk.RequiredTools {
			add(t)
		}
	}
	return out
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
	chars := 0
	for _, m := range s.messages {
		chars += charLen(m.ContentToText())
		for _, tc := range m.ToolCalls {
			chars += charLen(tc.Function.Arguments)
		}
	}
	return int(math.Ceil(float64(chars) / float64(domain.CharsPerToken)))
}

// maybeAutoCompact summarizes the conversation with the small model when it has
// grown past the token threshold, replacing the working history with a short note.
// Best-effort: any failure leaves the conversation untouched and the turn proceeds.
func (s *Session) maybeAutoCompact(ctx context.Context) {
	// Build the summary input under the lock (read a stable snapshot), then run the
	// model call OUTSIDE the lock. Runs on the turn goroutine with inFlight set, so
	// it compacts via compactLocked (the public Compact would self-reject).
	s.mu.Lock()
	if s.estimateTokensLocked() <= domain.AutoCompactTokenThreshold || len(s.messages) <= domain.ControlMessageCount+1 {
		s.mu.Unlock()
		return // under threshold, or no real history
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
	if err != nil {
		s.events.Info("Auto-compact skipped: summary failed")
		return
	}
	summary := trimSpace(result.Content)
	if summary == "" {
		s.events.Info("Auto-compact skipped: empty summary")
		return
	}
	// Distill durable facts from the transcript BEFORE compaction discards it
	// (best-effort, outside the lock; a nil MemoryStore skips it with no model call).
	saved := s.distillCompact(ctx, transcript)
	s.mu.Lock()
	s.compactLocked(summary)
	s.mu.Unlock()
	if saved > 0 {
		s.events.Info("Distilled " + itoa(saved) + " " + pluralMemory(saved) + " before compacting")
	}
	s.events.Info("Auto-compacted conversation")
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

// pluralMemory renders the singular/plural noun for a saved-memory count.
func pluralMemory(n int) string {
	if n == 1 {
		return "memory"
	}
	return "memories"
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
