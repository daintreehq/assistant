// Package asyncwork is the runtime owner of asynchronous tool invocations —
// the durable futures behind terminal.run.async / terminal.await.async. A tool
// handler performs the initial side effect, persists an async_invocations row,
// registers it here, and returns an immediate "accepted" result; from then on
// the Coordinator owns the lifecycle: it polls the watched terminals every
// second with the cheap pure-FSM settle policy (domain.SettleAgentFSM — the
// same policy terminal.awaitAll applies in-turn, NO output reads, NO model
// calls), holds a short settle grace so sibling invocations from the same turn
// coalesce, publishes ONE attention-queue event per settled group, and nudges
// the notifier so the wake fires immediately instead of on the next scheduler
// tick.
//
// Two invariants carried over from the rest of the runtime:
//   - Completion NEVER lands as a late tool result for the original call — the
//     queue event (and the autonomous wake it triggers) is the only delivery
//     channel, so the model transcript stays structurally valid.
//   - PROJECT-scoped, owner-supervised: async futures survive process
//     boundaries. Whichever process owns the project DB (an open cockpit or
//     the detached supervisor daemon) runs a coordinator, and Start ADOPTS the
//     persisted live rows — rebuilding the poll state from the row itself —
//     and retries any completion a prior owner finalized but died before
//     publishing (a terminal row with a NULL queueEventId). The pure-FSM poll
//     is idempotent, so re-adoption simply re-polls to settle; the stable
//     group DedupeKey makes a publish retry idempotent at the queue.
package asyncwork

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
)

// Defaults. The poll tick is deliberately 1s (not the scheduler's 3s): an async
// completion should reach the wake path promptly, and the per-tick cost is one
// batched no-output terminal.getStatus across ALL watched terminals — cheap.
const (
	DefaultTickMS        int64 = 1000
	DefaultSettleGraceMS int64 = 2500

	// runAsyncNeverWorkedGraceMS is the spawn-grace override for terminal.run.async.
	// The shared FinishSettleGraceMS (20s) models "a fast agent finished between
	// polls right after SPAWN"; run.async instead sends a command to an agent that
	// is usually ALREADY 'waiting', so a never-seen-working idle 20s later far more
	// often means the command hasn't been picked up than that it finished. A longer
	// window keeps a genuinely-instant command completing in ~a minute while making
	// a false "finished before it started" much less likely; the outcome is also
	// annotated when a settle never observed a working phase.
	runAsyncNeverWorkedGraceMS int64 = 60_000

	// Read-failure backoff: after readBackoffAfter consecutive failed status
	// reads, only attempt a read every readBackoffEvery ticks (≈5s) until one
	// succeeds — a downed MCP must not be hammered every second. Deadlines do
	// NOT enforce while reads are failing (see readsHealthy): expiring blind
	// would abandon work that may have finished during the blackout.
	readBackoffAfter = 5
	readBackoffEvery = 5

	// rosterCheckMinIntervalMS rate-limits the terminal.list absence check: a
	// terminal missing from getStatus is condemned as gone ONLY when the live
	// roster confirms it (the authoritative inventory), and that second read runs
	// at most this often — never every tick.
	rosterCheckMinIntervalMS int64 = 10_000
)

// TerminalStatus is one watched terminal's FSM snapshot for one poll.
type TerminalStatus struct {
	AgentState    string
	WaitingReason string // "question" | "prompt" | "" (meaningful only while waiting)
	ExitCode      *int
}

// StatusReadResult is the outcome of one batched status read. OK is true on a
// clean read; a terminal missing from ByID is NOT proof of exit — absence is
// only trusted once the live roster (ListTerminals) confirms it.
type StatusReadResult struct {
	OK   bool
	ByID map[string]TerminalStatus
}

// StatusReader is the coordinator's read-only terminal seam: one cheap
// no-output terminal.getStatus per tick, plus the rate-limited terminal.list
// roster read that authoritatively confirms a terminal is gone. Consumer-
// defined so the package compiles in isolation; the app layer adapts the
// shared MCP read adapter.
type StatusReader interface {
	Connected() bool
	ReadStatuses(ctx context.Context, terminalIDs []string) StatusReadResult
	// ListTerminals enumerates the live roster; ok=false on an unreadable
	// roster, in which case absence stays unproven and polling continues.
	ListTerminals(ctx context.Context) (ids []string, ok bool)
}

// Queue is the slice of the attention queue completions publish to.
type Queue interface {
	Publish(ctx context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error)
}

// Store is the async-invocation persistence seam. Every live-row write goes
// through the claim so a concurrent async.cancel can never be overwritten (the
// ClaimDueTimer / ClaimDueWatcher discipline). StampAsyncQueueEvents back-links
// a whole published group to its queue event in ONE statement — atomic, so a
// crash can never leave a group HALF-stamped (a partial stamp would make the
// adoption retry publish a subset under a different dedupe key = a duplicate).
type Store interface {
	ClaimLiveAsyncInvocation(id string, patch map[string]any) (bool, error)
	UpdateAsyncInvocation(id string, patch map[string]any) error
	StampAsyncQueueEvents(ids []string, eventID string) error
}

// AdoptLister enumerates persisted invocations at ownership boot: the live
// rows to re-poll and the finalized-but-unpublished rows whose completion
// event must be retried. Optional (nil ⇒ no adoption — unit tests and any
// context without a durable store).
type AdoptLister interface {
	ListLiveAsyncInvocations() ([]domain.AsyncInvocationRecord, error)
	ListUnpublishedAsyncInvocations() ([]domain.AsyncInvocationRecord, error)
}

// WorkflowSink routes a settled async invocation back to the
// workflow-intelligence graph layer (evidence + node transition on the graph
// that spawned it). Optional; nil ⇒ no-op. Implementations must be fast local
// writes — the coordinator calls them synchronously on the tick goroutine,
// panic-guarded, AFTER the completion event published (so a sink failure can
// never lose a wake).
type WorkflowSink interface {
	NoteAsyncSettled(asyncID, finalStatus, summary, queueEventID string)
}

// Deps wires the Coordinator.
type Deps struct {
	Reader StatusReader
	Queue  Queue
	Store  Store
	// WorkflowSink links settled invocations to the workflow graph layer
	// (nil ⇒ disabled).
	WorkflowSink WorkflowSink
	// AdoptLister enables ownership-boot adoption: Start re-tracks the persisted
	// live invocations and queues publish retries for finalized-but-unpublished
	// ones. nil ⇒ Start begins empty (tests / no durable store).
	AdoptLister AdoptLister
	// Notify pushes freshly-published attention events to the wake path
	// immediately (the scheduler's NotifyNow) instead of waiting up to a full
	// scheduler tick. nil ⇒ the next scheduler tick delivers (correct, slower).
	Notify func()
	// Trace is the diagnostic seam (debug log); nil ⇒ no-op.
	Trace func(event string, fields map[string]any)
	// TickMS / SettleGraceMS override the defaults (tests).
	TickMS        int64
	SettleGraceMS int64
	// Now seams the clock; nil ⇒ domain.NowMS.
	Now func() int64
}

// termState is the per-terminal poll memory: the seenWorking latch (a later
// "waiting" is a genuine working→idle transition, not a pre-start prompt) and
// the settled outcome (nil until settled).
type termState struct {
	seenWorking bool
	outcome     *domain.AsyncTerminalOutcome
}

// tracked is one live invocation's in-memory poll state. Not persisted as-is:
// a new owner REBUILDS it from the row at adoption (terminal ids from
// terminalIdsJson, settled outcomes from outcomesJson; the seenWorking latch
// restarts false, which only makes the FSM more conservative). Mutated only by
// the single tick goroutine; the map holding it is guarded by Coordinator.mu
// for Register/Deregister.
type tracked struct {
	rec         domain.AsyncInvocationRecord
	terminalIDs []string
	perTerminal map[string]*termState
	// graceMS is the never-seen-working settle grace for this invocation's tool
	// (run.async waits longer — see runAsyncNeverWorkedGraceMS).
	graceMS int64
	// settleAt is the coalescing deadline once every terminal settled (or the
	// hard deadline hit); 0 while still polling.
	settleAt int64
	// finalized marks a member whose terminal DB transition already committed but
	// whose completion event publish failed — the next tick retries ONLY the
	// publish (never the claim, which would lose to its own prior write).
	finalized   bool
	finalStatus domain.AsyncStatus
	// retryOnly marks an ADOPTED finalized-but-unpublished row. Retry entries
	// publish as their OWN unit, never merged with live same-group siblings: the
	// retried event must reproduce the ORIGINAL member set (same dedupe key) so
	// a publish that DID land before the crash dedupes instead of duplicating.
	retryOnly bool
}

// Coordinator owns every live async invocation. One pass per tick; no-overlap
// guarded; Drain waits for the in-flight pass (mirrors daemon.Scheduler).
type Coordinator struct {
	deps  Deps
	tick  int64
	grace int64
	now   func() int64

	// mu guards the active map (Register/Deregister vs the tick snapshot), the
	// read-failure counters, and the clear generation. Lock order where both are
	// held: stateMu → mu.
	mu           sync.Mutex
	active       map[string]*tracked
	readFailures int
	tickCount    int64
	// generation bumps on CancelAll (/clear): an in-flight pass re-checks it
	// right before Queue.Publish so a completion finalized JUST before the clear
	// can never wake the cleared conversation (the publish is the one side
	// effect the claim guard cannot retract).
	generation int64

	// lastRosterCheckAt rate-limits the terminal.list absence check. Touched
	// only on the tick goroutine.
	lastRosterCheckAt int64

	// Lifecycle state (the scheduler's stop/drain discipline).
	stateMu  sync.Mutex
	running  bool
	stopped  bool
	started  bool
	current  chan struct{}
	loopDone chan struct{}
	cancel   context.CancelFunc
	ticker   *time.Ticker
	nudge    chan struct{}
}

// New builds a Coordinator.
func New(deps Deps) *Coordinator {
	tick := deps.TickMS
	if tick <= 0 {
		tick = DefaultTickMS
	}
	grace := deps.SettleGraceMS
	if grace <= 0 {
		grace = DefaultSettleGraceMS
	}
	now := deps.Now
	if now == nil {
		now = domain.NowMS
	}
	return &Coordinator{
		deps:   deps,
		tick:   tick,
		grace:  grace,
		now:    now,
		active: make(map[string]*tracked),
		nudge:  make(chan struct{}, 1),
	}
}

// Start begins ticking. Idempotent; the loop ends on Stop (context cancel).
// Before the loop launches it ADOPTS the persisted invocations (when an
// AdoptLister is wired): live rows re-enter the poll set and finalized-but-
// unpublished rows queue an immediate publish retry — the ownership-boot half
// of project-scoped async futures.
func (c *Coordinator) Start(parent context.Context) {
	c.stateMu.Lock()
	if c.ticker != nil {
		c.stateMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	// Coordinator polls are maintenance traffic: every MCP call made under the
	// loop ctx (ticks and nudged polls) is Background class, so the 1s status
	// poll queues behind user-facing calls instead of crowding them out.
	ctx = mcp.WithPriority(ctx, mcp.PriorityBackground)
	c.cancel = cancel
	c.stopped = false
	c.started = true
	c.ticker = time.NewTicker(time.Duration(c.tick) * time.Millisecond)
	c.loopDone = make(chan struct{})
	ticker := c.ticker
	loopDone := c.loopDone
	c.stateMu.Unlock()

	c.adoptFromStore(c.now())

	go func() {
		defer close(loopDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				c.Tick(ctx, c.now())
			case <-c.nudge:
				// A fresh registration: poll NOW so the first status read (and a
				// possibly already-finished terminal) is picked up without waiting
				// out the tick interval.
				c.Tick(ctx, c.now())
			}
		}
	}()
}

// Stop stops the ticker and cancels the tick context. Does NOT wait — Drain.
func (c *Coordinator) Stop() {
	c.stateMu.Lock()
	c.stopped = true
	c.started = false
	if c.ticker != nil {
		c.ticker.Stop()
		c.ticker = nil
	}
	if c.cancel != nil {
		c.cancel()
	}
	c.stateMu.Unlock()
}

// Drain waits for the loop goroutine AND any in-flight pass to finish. Call
// after Stop, before tearing down deps.
func (c *Coordinator) Drain() {
	c.stateMu.Lock()
	loopDone := c.loopDone
	c.stateMu.Unlock()
	if loopDone != nil {
		<-loopDone
	}
	c.stateMu.Lock()
	done := c.current
	c.stateMu.Unlock()
	if done != nil {
		<-done
	}
}

// Started reports whether the coordinator is running (registrations allowed).
func (c *Coordinator) Started() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.started
}

// Register adopts a persisted invocation into the live poll set. The caller has
// already inserted the row and performed the initial side effect. Fails when
// the coordinator is not running (a one-shot/non-interactive session has no
// foreground poll loop, so accepting the work would silently strand it). The
// started check and the map insert happen under ONE stateMu hold so a
// concurrent Stop can never slip between them and strand a registered-but-
// never-polled invocation (lock order: stateMu → mu).
func (c *Coordinator) Register(rec domain.AsyncInvocationRecord, terminalIDs []string) error {
	if len(terminalIDs) == 0 {
		return fmt.Errorf("async invocation %s has no terminals to watch", rec.ID)
	}
	per := make(map[string]*termState, len(terminalIDs))
	ids := make([]string, len(terminalIDs))
	copy(ids, terminalIDs)
	for _, id := range ids {
		per[id] = &termState{}
	}
	entry := &tracked{rec: rec, terminalIDs: ids, perTerminal: per, graceMS: graceFor(rec.ToolName)}

	c.stateMu.Lock()
	if !c.started {
		c.stateMu.Unlock()
		return fmt.Errorf("the async coordinator is not running in this session")
	}
	c.mu.Lock()
	c.active[rec.ID] = entry
	c.mu.Unlock()
	c.stateMu.Unlock()

	c.trace("async.registered", map[string]any{
		"asyncId": rec.ID, "tool": rec.ToolName, "group": rec.GroupID,
		"terminals": ids, "expiresAt": rec.ExpiresAt, "graceMs": entry.graceMS,
	})
	// Kick an immediate pass (non-blocking: a pending nudge already covers it).
	select {
	case c.nudge <- struct{}{}:
	default:
	}
	return nil
}

// graceFor picks the never-seen-working settle grace per tool (see the
// runAsyncNeverWorkedGraceMS rationale).
func graceFor(toolName string) int64 {
	if toolName == "terminal.run.async" {
		return runAsyncNeverWorkedGraceMS
	}
	return domain.FinishSettleGraceMS
}

// adoptFromStore is the ownership-boot half of project-scoped async futures:
// re-track every persisted live invocation and queue a publish retry for every
// finalized-but-unpublished one. Idempotent per row (keyed map insert). All
// failures degrade to "that row stays un-adopted until the next owner" — a
// boot must never fail because one row is unreadable.
func (c *Coordinator) adoptFromStore(now int64) {
	if c.deps.AdoptLister == nil {
		return
	}
	live, err := c.deps.AdoptLister.ListLiveAsyncInvocations()
	if err != nil {
		c.trace("async.adopt_failed", map[string]any{"stage": "live", "error": err.Error()})
		live = nil
	}
	adopted := 0
	var orphanedStarts []string
	for _, rec := range live {
		// A run.async row still in 'starting' means the prior owner died BETWEEN
		// persisting the intent and (confirming) the terminal send — the command
		// may never have run. Supervising it as normal work could publish a clean
		// "finished" for a command that never executed, so cancel it and surface
		// an attention item asking for verification instead. (await.async rows in
		// 'starting' are just a read-only watch that never registered — adopting
		// those is safe and correct.)
		if rec.Status == domain.AsyncStarting && rec.ToolName == "terminal.run.async" {
			if ok, _ := c.deps.Store.ClaimLiveAsyncInvocation(rec.ID, map[string]any{
				"status": string(domain.AsyncCancelled), "endedReason": "orphaned_startup", "finishedAt": now,
			}); ok {
				orphanedStarts = append(orphanedStarts, fmt.Sprintf("%s %q", rec.ID, rec.Title))
			}
			continue
		}
		entry := trackedFromRecord(rec, now, c.grace)
		if entry == nil {
			// Unparseable terminal list: the row can never settle by polling. Cancel
			// it under the claim (excluded from the unpublished retry set) so it
			// doesn't sit live forever.
			_, _ = c.deps.Store.ClaimLiveAsyncInvocation(rec.ID, map[string]any{
				"status": string(domain.AsyncCancelled), "endedReason": "corrupt_row", "finishedAt": now,
			})
			c.trace("async.adopt_corrupt", map[string]any{"asyncId": rec.ID})
			continue
		}
		c.mu.Lock()
		c.active[rec.ID] = entry
		c.mu.Unlock()
		adopted++
	}
	if len(orphanedStarts) > 0 {
		// SourceAsyncTool so the wake filter fires: the model can read the
		// terminal and tell the user whether the command actually ran.
		_, _ = c.deps.Queue.Publish(context.Background(), domain.QueuePublishArgs{
			Source:   domain.SourceAsyncTool,
			Severity: domain.SeverityAttention,
			Title:    "Async command may not have started",
			Summary: "The previous assistant exited while starting: " + strings.Join(orphanedStarts, "; ") +
				". The command may or may not have been sent — check the terminal before re-running it.",
			DedupeKey: "async-orphaned:" + strings.Join(orphanedStarts, "+"),
		})
	}
	retries := 0
	if unpub, uerr := c.deps.AdoptLister.ListUnpublishedAsyncInvocations(); uerr != nil {
		c.trace("async.adopt_failed", map[string]any{"stage": "unpublished", "error": uerr.Error()})
	} else {
		for _, rec := range unpub {
			entry := trackedFromRecord(rec, now, c.grace)
			if entry == nil {
				continue // no terminals to summarize; nothing meaningful to publish
			}
			// The DB transition already committed under the prior owner; only the
			// publish is owed. Marking finalized + due-now routes it through the
			// existing publish-retry path; the stable group DedupeKey keeps a retry
			// of an event that DID land (crash between publish and stamp) a dedupe
			// hit instead of a duplicate wake. retryOnly keeps the retried member
			// set byte-identical to the original publish — a live same-group
			// sibling settling now must not widen it into a NEW dedupe key.
			entry.finalized = true
			entry.retryOnly = true
			entry.finalStatus = rec.Status
			entry.settleAt = now
			c.mu.Lock()
			c.active[rec.ID] = entry
			c.mu.Unlock()
			retries++
		}
	}
	if adopted > 0 || retries > 0 {
		c.trace("async.adopted", map[string]any{"live": adopted, "publishRetries": retries})
		select {
		case c.nudge <- struct{}{}:
		default:
		}
	}
}

// trackedFromRecord rebuilds an invocation's poll state from its persisted
// row: watched terminals from terminalIdsJson, already-settled outcomes from
// outcomesJson (never re-polled), and the coalescing deadline for rows a prior
// owner already moved to settling. The seenWorking latch restarts false —
// strictly more conservative (a waiting agent needs the grace to count as
// finished). nil when the row has no parseable terminals.
func trackedFromRecord(rec domain.AsyncInvocationRecord, now int64, grace int64) *tracked {
	ids := parseJSONIDs(rec.TerminalIdsJson)
	if len(ids) == 0 {
		return nil
	}
	per := make(map[string]*termState, len(ids))
	restored := parseOutcomes(rec.OutcomesJson)
	for _, id := range ids {
		st := &termState{}
		if o, ok := restored[id]; ok {
			oc := o
			st.outcome = &oc
		}
		per[id] = st
	}
	entry := &tracked{rec: rec, terminalIDs: ids, perTerminal: per, graceMS: graceFor(rec.ToolName)}
	if rec.Status == domain.AsyncSettling {
		entry.settleAt = now + grace
	}
	return entry
}

// Deregister drops an invocation from the live poll set (async.cancel). The
// caller owns the DB transition; the claim-guard on every coordinator write
// makes the race benign in the other direction too.
func (c *Coordinator) Deregister(id string) {
	c.mu.Lock()
	delete(c.active, id)
	c.mu.Unlock()
}

// CancelAll drops EVERY tracked invocation and bumps the clear generation —
// /clear's coordinator half. The generation bump is what closes the in-flight
// window: a pass that already finalized a group re-checks the generation
// before publishing and drops the event instead of waking the cleared
// conversation (the rows stay terminal in the ledger; no wake is correct —
// /clear explicitly dropped that work).
func (c *Coordinator) CancelAll() {
	c.mu.Lock()
	c.generation++
	c.active = make(map[string]*tracked)
	c.mu.Unlock()
}

// currentGeneration reads the clear generation (see CancelAll).
func (c *Coordinator) currentGeneration() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.generation
}

// ActiveCount reports how many invocations are being polled (UI/status).
func (c *Coordinator) ActiveCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.active)
}

// Tick runs one pass. Safe to call directly in tests; the no-overlap guard
// lives here (mirrors Scheduler.Tick). The idle fast path skips the whole
// scaffolding (drain handle, panic guard) when nothing is tracked — the common
// state for sessions that never start async work.
func (c *Coordinator) Tick(ctx context.Context, now int64) {
	if c.ActiveCount() == 0 {
		return
	}
	c.stateMu.Lock()
	if c.running || c.stopped {
		c.stateMu.Unlock()
		return
	}
	c.running = true
	done := make(chan struct{})
	c.current = done
	c.stateMu.Unlock()

	defer close(done)
	defer func() {
		c.stateMu.Lock()
		c.running = false
		c.stateMu.Unlock()
	}()
	// A panic in the pass must not kill async supervision for the session; the
	// next tick still runs. Best-effort surface, publish itself panic-guarded.
	defer func() {
		if r := recover(); r != nil {
			func() {
				defer func() { _ = recover() }()
				_, _ = c.deps.Queue.Publish(ctx, domain.QueuePublishArgs{
					Source:    domain.SourceSystem,
					Severity:  domain.SeverityError,
					Title:     "Async supervision error",
					Summary:   fmt.Sprintf("An async coordinator pass panicked and was recovered: %v", r),
					DedupeKey: "async-tick-panic",
				})
			}()
		}
	}()

	c.runPass(ctx, now)
}

// runPass is one unguarded pass: poll → settle → expire → publish due groups.
func (c *Coordinator) runPass(ctx context.Context, now int64) {
	c.mu.Lock()
	c.tickCount++
	tickCount := c.tickCount
	entries := make([]*tracked, 0, len(c.active))
	for _, t := range c.active {
		entries = append(entries, t)
	}
	c.mu.Unlock()
	if len(entries) == 0 {
		return
	}
	// Deterministic order (map iteration is random): grouped publishes and tests
	// both want stable composition. The ID tie-break matters for correctness,
	// not just tidiness: the group dedupe key is the JOINED MEMBER IDS, so a
	// crash-retry must reproduce the original ordering byte-for-byte even when
	// members share a CreatedAt millisecond.
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].rec.CreatedAt != entries[j].rec.CreatedAt {
			return entries[i].rec.CreatedAt < entries[j].rec.CreatedAt
		}
		return entries[i].rec.ID < entries[j].rec.ID
	})

	var polling []*tracked
	for _, t := range entries {
		if t.settleAt == 0 {
			polling = append(polling, t)
		}
	}

	// One batched, no-output status read across every still-polling terminal —
	// one MCP call per tick regardless of how many invocations are live.
	if len(polling) > 0 && c.shouldRead(tickCount) {
		idSet := make(map[string]struct{})
		var ids []string
		for _, t := range polling {
			for _, id := range t.terminalIDs {
				if _, ok := idSet[id]; !ok {
					idSet[id] = struct{}{}
					ids = append(ids, id)
				}
			}
		}
		res := c.deps.Reader.ReadStatuses(ctx, ids)
		if !res.OK {
			c.noteReadFailure()
		} else {
			c.noteReadSuccess()
			gone := c.confirmGone(ctx, now, ids, res)
			for _, t := range polling {
				c.feedStatuses(t, res, gone, now)
			}
		}
	}

	// Settle / expire transitions. The deadline is enforced ONLY while MCP is
	// reachable: expiring blind would mark work "timed out" that may have
	// finished cleanly while the credentials were down — the one outcome the
	// persistent supervisor exists to prevent. While disconnected the invocation
	// stays live (blocked, not abandoned); on reconnect the very next read either
	// settles it honestly or the deadline fires with evidence behind it.
	for _, t := range polling {
		switch {
		case t.allSettled():
			c.enterSettling(t, now)
		case now >= t.rec.ExpiresAt && c.deps.Reader.Connected() && c.readsHealthy():
			// Deadline: unsettled terminals are recorded as still working and the
			// invocation settles as expired.
			for _, st := range t.perTerminal {
				if st.outcome == nil {
					st.outcome = &domain.AsyncTerminalOutcome{
						Status: domain.SettleStatusWorking,
						Reason: "still working when the deadline passed",
					}
				}
			}
			c.enterSettling(t, now)
		}
	}

	// Publish every due settling group; one wake nudge per pass no matter how
	// many groups settled together (the notifier drains ALL fresh events at
	// once, so per-group nudges would just be redundant digest queries).
	if c.publishDueGroups(ctx, now) && c.deps.Notify != nil {
		func() {
			defer func() { _ = recover() }()
			c.deps.Notify()
		}()
	}
}

// confirmGone resolves which watched terminals are AUTHORITATIVELY gone. A
// terminal missing from the batched getStatus is only a hint — the response
// may be partial, and with cross-invocation batching another invocation's
// terminals being present proves nothing about this one. The live roster
// (terminal.list) is the authority: a missing terminal is condemned only when
// a fresh roster read (rate-limited to one per rosterCheckMinIntervalMS)
// confirms it is not listed. Unproven absence keeps polling — worst case the
// deadline reports it.
func (c *Coordinator) confirmGone(ctx context.Context, now int64, watched []string, res StatusReadResult) map[string]bool {
	var missing []string
	for _, id := range watched {
		if _, ok := res.ByID[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if now-c.lastRosterCheckAt < rosterCheckMinIntervalMS {
		return nil
	}
	c.lastRosterCheckAt = now
	live, ok := c.deps.Reader.ListTerminals(ctx)
	if !ok {
		return nil // unreadable roster — absence stays unproven
	}
	liveSet := make(map[string]struct{}, len(live))
	for _, id := range live {
		liveSet[id] = struct{}{}
	}
	gone := make(map[string]bool)
	for _, id := range missing {
		if _, alive := liveSet[id]; !alive {
			gone[id] = true
		}
	}
	if len(gone) > 0 {
		c.trace("async.terminals_gone", map[string]any{"terminals": keys(gone)})
	}
	return gone
}

// keys returns a map's keys (trace rendering).
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// readsHealthy reports whether the LAST status read succeeded (readFailures
// zero). The deadline may only expire an invocation when we can actually see
// the terminals — Connected() alone can be true while every read fails, and
// expiring blind would mark work "timed out" that may have finished during the
// blackout (the exact abandonment the supervisor exists to prevent).
func (c *Coordinator) readsHealthy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readFailures == 0
}

// shouldRead applies the read-failure backoff (guarded by mu).
func (c *Coordinator) shouldRead(tickCount int64) bool {
	if !c.deps.Reader.Connected() {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readFailures < readBackoffAfter {
		return true
	}
	return tickCount%readBackoffEvery == 0
}

func (c *Coordinator) noteReadFailure() {
	c.mu.Lock()
	c.readFailures++
	n := c.readFailures
	c.mu.Unlock()
	if n == readBackoffAfter {
		c.trace("async.read.backoff", map[string]any{"consecutiveFailures": n})
	}
}

func (c *Coordinator) noteReadSuccess() {
	c.mu.Lock()
	c.readFailures = 0
	c.mu.Unlock()
}

// feedStatuses folds one status read into an invocation's per-terminal FSM
// state. The settle verdict is the shared domain.SettleAgentFSM (awaitAll's
// policy); absence is roster-confirmed by the caller (gone), never inferred
// from the batched response shape; and a terminal missing without roster proof
// simply skips the tick. A finished verdict that never observed a working
// phase is annotated so the wake nudges verification instead of blind trust.
func (c *Coordinator) feedStatuses(t *tracked, res StatusReadResult, gone map[string]bool, now int64) {
	for _, id := range t.terminalIDs {
		st := t.perTerminal[id]
		if st == nil || st.outcome != nil {
			continue // already settled — never re-poll
		}
		entry, present := res.ByID[id]
		agentState, waitingReason := "", ""
		var exitCode *int
		switch {
		case present:
			agentState, waitingReason, exitCode = entry.AgentState, entry.WaitingReason, entry.ExitCode
		case gone[id]:
			agentState = string(domain.AgentExited)
		default:
			continue // missing without roster proof — a partial read, not an exit
		}
		if agentState == string(domain.AgentWorking) {
			st.seenWorking = true
		}

		v := domain.SettleAgentFSM(agentState, waitingReason, exitCode, st.seenWorking,
			now-t.rec.CreatedAt, t.graceMS)
		if !v.Settled {
			continue
		}
		o := &domain.AsyncTerminalOutcome{Status: v.Status, ExitCode: exitCode}
		switch {
		case gone[id]:
			o.Reason = "terminal is gone (closed or exited)"
		case v.Status == domain.SettleStatusFailed && exitCode != nil:
			o.Reason = fmt.Sprintf("exited with code %d", *exitCode)
		case v.Status == domain.SettleStatusQuestion:
			o.Reason = "asking a question"
		case v.Status == domain.SettleStatusFinished && !st.seenWorking && agentState == string(domain.AgentWaiting):
			// Settled purely on the spawn grace: honest uncertainty travels with the
			// outcome so the wake reads it as "verify", not gospel.
			o.Reason = "idle past the grace without an observed working phase — verify the output"
		}
		st.outcome = o
	}
}

// allSettled reports whether every watched terminal has an outcome.
func (t *tracked) allSettled() bool {
	for _, st := range t.perTerminal {
		if st.outcome == nil {
			return false
		}
	}
	return len(t.perTerminal) > 0
}

// outcomes snapshots the per-terminal outcome ledger.
func (t *tracked) outcomes() map[string]domain.AsyncTerminalOutcome {
	out := make(map[string]domain.AsyncTerminalOutcome, len(t.perTerminal))
	for id, st := range t.perTerminal {
		if st.outcome != nil {
			out[id] = *st.outcome
		}
	}
	return out
}

// invocationFailed reports whether any watched terminal failed.
func invocationFailed(t *tracked) bool {
	for _, st := range t.perTerminal {
		if st.outcome != nil && st.outcome.Status == domain.SettleStatusFailed {
			return true
		}
	}
	return false
}

// invocationExpired reports whether the invocation settled via the deadline —
// derivable because ONLY the expiry branch records a SettleStatusWorking
// outcome (one source of truth; no parallel flag to drift).
func invocationExpired(t *tracked) bool {
	for _, st := range t.perTerminal {
		if st.outcome != nil && st.outcome.Status == domain.SettleStatusWorking {
			return true
		}
	}
	return false
}

// finalStatusFor derives the invocation's terminal status from its outcomes.
func finalStatusFor(t *tracked) domain.AsyncStatus {
	switch {
	case invocationFailed(t):
		return domain.AsyncFailed
	case invocationExpired(t):
		return domain.AsyncExpired
	default:
		return domain.AsyncSucceeded
	}
}

// enterSettling moves an invocation into the coalescing window and persists the
// transition under the live-claim. The two failure modes are DIFFERENT races:
// a clean lost claim (!ok, no error) means async.cancel won — drop without
// publishing; a storage ERROR is transient — keep the invocation polling-state
// (outcomes are latched, so the next tick retries this transition) rather than
// stranding a live DB row nothing owns.
func (c *Coordinator) enterSettling(t *tracked, now int64) {
	patch := map[string]any{
		"status":       string(domain.AsyncSettling),
		"outcomesJson": mustJSON(t.outcomes()),
	}
	ok, err := c.deps.Store.ClaimLiveAsyncInvocation(t.rec.ID, patch)
	if err != nil {
		c.trace("async.claim_retry", map[string]any{"asyncId": t.rec.ID, "stage": "settling", "error": err.Error()})
		return
	}
	if !ok {
		// Cancelled underneath — stop tracking; never publish a completion for
		// work the user explicitly stopped following.
		c.Deregister(t.rec.ID)
		c.trace("async.claim_lost", map[string]any{"asyncId": t.rec.ID, "stage": "settling"})
		return
	}
	t.settleAt = now + c.grace
	c.trace("async.settled", map[string]any{
		"asyncId": t.rec.ID, "expired": invocationExpired(t), "settleAt": t.settleAt,
		"outcomes": t.outcomes(),
	})
}

// publishDueGroups publishes one grouped completion event per due group and
// finalizes its invocations. Returns whether anything was published (the
// caller sends ONE wake nudge per pass).
func (c *Coordinator) publishDueGroups(ctx context.Context, now int64) bool {
	c.mu.Lock()
	gen := c.generation
	byGroup := make(map[string][]*tracked)
	groupHasPolling := make(map[string]bool)
	for _, t := range c.active {
		if t.settleAt == 0 {
			groupHasPolling[t.rec.GroupID] = true
			continue
		}
		key := t.rec.GroupID
		if t.retryOnly {
			// Adopted publish-retries are their own publish unit (see retryOnly):
			// a live sibling settling later must NOT widen the retried member set.
			key = t.rec.GroupID + "\x00retry"
		}
		byGroup[key] = append(byGroup[key], t)
	}
	c.mu.Unlock()

	published := false
	for group, members := range byGroup {
		// A group is due when its earliest settleAt passed. The no-polling fast
		// path applies ONLY to self-grouped invocations (GroupID == its own id —
		// no run id): a RUN-scoped group must wait out the full grace even when
		// nothing else is polling yet, because a same-turn sibling may not have
		// REGISTERED yet (sequential dispatch + a confirmation prompt can sit
		// between two async starters in one batch) — publishing early would split
		// the turn's completions into separate wakes.
		selfGrouped := len(members) == 1 && members[0].rec.GroupID == members[0].rec.ID
		due := (selfGrouped && !groupHasPolling[group]) || members[0].retryOnly
		for _, t := range members {
			if now >= t.settleAt {
				due = true
				break
			}
		}
		if !due {
			continue
		}
		sort.Slice(members, func(i, j int) bool {
			if members[i].rec.CreatedAt != members[j].rec.CreatedAt {
				return members[i].rec.CreatedAt < members[j].rec.CreatedAt
			}
			return members[i].rec.ID < members[j].rec.ID
		})
		if c.publishGroup(ctx, now, gen, group, members) {
			published = true
		}
	}
	return published
}

// publishGroup finalizes each settled member under the live-claim FIRST, then
// emits ONE attention event covering the members that survived, then stamps
// the event id. Finalize-before-publish is the race-correct order: an
// async.cancel that wins any member's claim simply drops that member from the
// event (never a completion wake for cancelled work), and a publish failure
// after finalize leaves the members marked `finalized` so the next tick
// retries ONLY the publish — the completion is never silently lost.
func (c *Coordinator) publishGroup(ctx context.Context, now int64, gen int64, group string, members []*tracked) bool {
	// 1. Finalize. Claim errors keep the member settling (retry next tick);
	//    clean lost claims drop it (cancel won); already-finalized members (a
	//    prior publish failure) pass straight through.
	var final []*tracked
	for _, t := range members {
		if t.finalized {
			final = append(final, t)
			continue
		}
		status := finalStatusFor(t)
		ok, err := c.deps.Store.ClaimLiveAsyncInvocation(t.rec.ID, map[string]any{
			"status":       string(status),
			"finishedAt":   now,
			"outcomesJson": mustJSON(t.outcomes()),
		})
		if err != nil {
			c.trace("async.claim_retry", map[string]any{"asyncId": t.rec.ID, "stage": "finalize", "error": err.Error()})
			continue
		}
		if !ok {
			c.Deregister(t.rec.ID)
			c.trace("async.claim_lost", map[string]any{"asyncId": t.rec.ID, "stage": "finalize"})
			continue
		}
		t.finalized = true
		t.finalStatus = status
		final = append(final, t)
	}
	if len(final) == 0 {
		return false
	}

	// 2. Publish one event over the finalized members.
	ids := make([]string, 0, len(final))
	evidence := make([]string, 0, len(final))
	anyFailed, anyExpired, anyQuestion := false, false, false
	for _, t := range final {
		ids = append(ids, t.rec.ID)
		line, failed, question := summarizeInvocation(t)
		evidence = append(evidence, line)
		anyFailed = anyFailed || failed
		anyQuestion = anyQuestion || question
		anyExpired = anyExpired || invocationExpired(t)
	}
	title, summary := composeGroupEvent(final, anyFailed, anyExpired, anyQuestion)
	target := &domain.EventTarget{AsyncInvocationID: final[0].rec.ID}
	if len(final) == 1 && len(final[0].terminalIDs) == 1 {
		target.TerminalID = final[0].terminalIDs[0]
	}

	// /clear guard: the publish is the one side effect the claim discipline
	// cannot retract, so re-check the clear generation captured when this pass
	// snapshotted. A clear that landed after the finalize claims drops the event
	// here — the ledger keeps the terminal rows, but the cleared conversation is
	// never woken for work it no longer owns.
	if c.currentGeneration() != gen {
		c.trace("async.publish_dropped_cleared", map[string]any{"group": group, "asyncIds": ids})
		return false
	}

	ev, err := c.deps.Queue.Publish(ctx, domain.QueuePublishArgs{
		Source:        domain.SourceAsyncTool,
		Severity:      domain.SeverityAttention,
		Title:         title,
		Summary:       summary,
		Target:        target,
		Evidence:      evidence,
		EpistemicKind: domain.EpistemicObserved,
		DedupeKey:     "async:" + strings.Join(ids, "+"),
	})
	if err != nil {
		// The queue is the ONLY delivery channel; the members stay tracked as
		// finalized so the next tick retries the publish.
		c.trace("async.publish_failed", map[string]any{"group": group, "asyncIds": ids, "error": err.Error()})
		return false
	}

	// 3. Stamp the event back-link on the WHOLE group in one atomic statement
	//    (the rows are terminal now), then drop the members from the poll set.
	//    Atomicity matters for crash-safety: a half-stamped group would make the
	//    next owner's adoption retry a SUBSET under a different dedupe key — a
	//    duplicate wake. All-or-nothing keeps the retry byte-identical.
	if uerr := c.deps.Store.StampAsyncQueueEvents(ids, ev.ID); uerr != nil {
		c.trace("async.event_stamp_failed", map[string]any{"asyncIds": ids, "error": uerr.Error()})
	}
	for _, t := range final {
		c.Deregister(t.rec.ID)
	}

	// 4. Workflow-graph back-link (best-effort, AFTER the publish so a sink
	//    failure can never lose the wake). One call per member: each invocation
	//    may belong to a different graph node.
	if c.deps.WorkflowSink != nil {
		func() {
			defer func() { _ = recover() }()
			for _, t := range final {
				line, _, _ := summarizeInvocation(t)
				c.deps.WorkflowSink.NoteAsyncSettled(t.rec.ID, string(t.finalStatus), line, ev.ID)
			}
		}()
	}

	c.trace("async.published", map[string]any{
		"group": group, "asyncIds": ids, "eventId": ev.ID, "title": title,
	})
	return true
}

// summarizeInvocation renders one member's evidence line:
//
//	asy_1a2b "npm test": terminal-abc: finished
//	asy_9f00 "lint": terminal-def: failed — exited with code 1
//
// and reports whether it failed / left a question pending.
func summarizeInvocation(t *tracked) (line string, failed, question bool) {
	parts := make([]string, 0, len(t.terminalIDs))
	for _, id := range t.terminalIDs {
		st := t.perTerminal[id]
		if st == nil || st.outcome == nil {
			parts = append(parts, id+": still working")
			continue
		}
		p := id + ": " + st.outcome.Status
		if st.outcome.Reason != "" {
			p += " — " + st.outcome.Reason
		}
		switch st.outcome.Status {
		case domain.SettleStatusFailed:
			failed = true
		case domain.SettleStatusQuestion:
			question = true
		}
		parts = append(parts, p)
	}
	return t.rec.ID + " \"" + t.rec.Title + "\": " + strings.Join(parts, "; "), failed, question
}

// composeGroupEvent renders the event title + summary for a settled group. The
// wake prompt renders "- <Title>: <Summary>", so both must be self-contained.
func composeGroupEvent(members []*tracked, anyFailed, anyExpired, anyQuestion bool) (title, summary string) {
	outcomeWord := "finished"
	switch {
	case anyFailed:
		outcomeWord = "finished with failures"
	case anyExpired:
		outcomeWord = "timed out"
	case anyQuestion:
		outcomeWord = "finished (a question is waiting)"
	}

	if len(members) == 1 {
		t := members[0]
		title = "Async " + outcomeWord + ": " + t.rec.Title
		line, _, _ := summarizeInvocation(t)
		summary = line
		return title, summary
	}

	title = fmt.Sprintf("%d async operations %s", len(members), outcomeWord)
	lines := make([]string, 0, len(members))
	for _, t := range members {
		line, _, _ := summarizeInvocation(t)
		lines = append(lines, line)
	}
	summary = strings.Join(lines, " | ")
	return title, summary
}

// trace emits a diagnostic event through the nil-safe seam.
func (c *Coordinator) trace(event string, fields map[string]any) {
	if c.deps.Trace == nil {
		return
	}
	defer func() { _ = recover() }()
	c.deps.Trace(event, fields)
}
