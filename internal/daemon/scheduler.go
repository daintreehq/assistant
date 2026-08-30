package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
)

// SchedulerDeps wires the scheduler to its dependencies.
type SchedulerDeps struct {
	Store    Store
	Queue    Queue
	Registry Registry
	// CtxFor builds a CheckContext for a non-interactive actor ("watcher"/"timer").
	// It supplies the MCP/model/projectPath for that actor. ctx is the scheduler's
	// tick context (cancellation).
	CtxFor func(ctx context.Context, actor domain.ToolActor, actorID string) *CheckContext
	// TickMS overrides the tick interval (defaults to SchedulerTickMS).
	TickMS int64
	// OnAttention is called with newly-created attention+ events after each tick.
	OnAttention func(events []domain.QueueEvent)
	// OnTimerFired is called once per timer that fires, AFTER its payload has been
	// dispatched and its outcome published.
	//
	// It exists because a fired timer is otherwise invisible. OnAttention only carries
	// attention-and-above, and a call_safe_tool that SUCCEEDS publishes at info — so
	// the one signal saying "the thing you scheduled has happened" reached nothing. It
	// is deliberately just an id: a host uses it to invalidate and re-read, rather than
	// receiving a payload it would have to keep in sync with the snapshot.
	//
	// Called on a scheduler goroutine; implementations must not block.
	OnTimerFired func(timerID string)
	// ResourceUpdates is the MCP client's resource-update wake channel (each value
	// is a changed resource URI). When a subscribed terminal's agent state pushes a
	// transition, the loop nudges active terminal watchers due and ticks immediately
	// instead of waiting the next interval. nil disables the fast path (poll-only).
	ResourceUpdates <-chan string
}

// Scheduler is the in-process daemon. One pass per tick: fire due timers, run due
// watchers, delivering attention as each job settles (with an end-of-pass backstop
// notify). No-overlap guarded; Drain waits for the in-flight tick.
type Scheduler struct {
	deps   SchedulerDeps
	tickMs int64

	mu          sync.Mutex
	onAttention func(events []domain.QueueEvent)

	// notifyMu serializes notify() between the per-job settles, the end-of-tick
	// backstop, and NotifyNow (the async coordinator's post-publish push). Two
	// concurrent notify passes could both digest the same fresh event before either
	// marks it notified, delivering one completion twice — the lock makes delivery
	// exactly-once.
	notifyMu sync.Mutex

	// notifyReqMu guards the requestNotify coalescing state. A burst of
	// near-simultaneous requests (several jobs settling together, NotifyNow pushes)
	// collapses into one active runner plus at most one pending re-run — see
	// requestNotify.
	notifyReqMu   sync.Mutex
	notifyActive  bool
	notifyPending bool

	// running guards against overlapping ticks; current is the in-flight tick
	// goroutine's done channel that Drain awaits. A skipped tick must NOT replace
	// current (else Drain returns before the real tick releases MCP/Store).
	stateMu sync.Mutex
	running bool
	current chan struct{}
	// stopped latches once Stop() has run. The Start loop re-checks it under
	// stateMu immediately before installing a tick so a ticker event already
	// selected when Stop() fires can't launch a fresh pass against the canceled
	// ctx after Drain() has already returned.
	stopped bool
	// loopDone closes when the Start loop goroutine exits. Drain awaits BOTH the
	// in-flight tick AND the loop, so a tick the loop is about to launch (selected
	// but not yet started) is covered too — the loop can't have exited until it
	// stops launching ticks.
	loopDone chan struct{}
	// notifyWG tracks the overflow notify re-run goroutine (see spawnNotifyRerun).
	// It is Add()ed under stateMu and only while !stopped, so once Stop() has
	// latched no new runner can appear and Drain's Wait can never miss one —
	// closing the hole where an orphan pass outlived Drain() and hit a Store the
	// caller had already closed.
	notifyWG sync.WaitGroup

	cancel context.CancelFunc
	ticker *time.Ticker
}

// NewScheduler builds a Scheduler.
func NewScheduler(deps SchedulerDeps) *Scheduler {
	tickMs := deps.TickMS
	if tickMs <= 0 {
		tickMs = SchedulerTickMS
	}
	return &Scheduler{deps: deps, tickMs: tickMs, onAttention: deps.OnAttention}
}

// SetOnAttention replaces the attention callback (a UI remount rebinds a fresh one).
func (s *Scheduler) SetOnAttention(cb func(events []domain.QueueEvent)) {
	s.mu.Lock()
	s.onAttention = cb
	s.mu.Unlock()
}

// Start begins ticking. Idempotent. The loop ends on Stop (context cancel). There
// is no "don't keep the process alive" concern — the goroutine's lifecycle is
// owned by Stop/context cancellation.
func (s *Scheduler) Start(parent context.Context) {
	s.stateMu.Lock()
	if s.ticker != nil {
		s.stateMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	// Watcher ticks are maintenance traffic: every MCP call made under the loop
	// ctx (ticks, resource wakes, check dispatches) is Background class, so a
	// tick fan-out queues behind user-facing calls instead of crowding them out.
	ctx = mcp.WithPriority(ctx, mcp.PriorityBackground)
	s.cancel = cancel
	s.stopped = false
	s.ticker = time.NewTicker(time.Duration(s.tickMs) * time.Millisecond)
	s.loopDone = make(chan struct{})
	ticker := s.ticker
	loopDone := s.loopDone
	s.stateMu.Unlock()

	resourceUpdates := s.deps.ResourceUpdates
	go func() {
		defer close(loopDone)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				// The no-overlap + stop re-check now live inside Tick (it returns
				// early without touching `current` when a tick is in flight or the
				// scheduler has stopped), so a ticker event selected just as Stop()
				// fires can't launch a fresh pass against the canceled ctx.
				s.Tick(ctx, domain.NowMS())
			case uri, ok := <-resourceUpdates:
				// A subscribed agent-state resource changed: re-check NOW rather than
				// waiting up to a full interval. Coalesce a burst (drain the rest of the
				// buffer) into one pass — the individual URI doesn't matter, the wake
				// just nudges active terminal watchers due. A closed channel (nilled) is
				// never selected again, so the loop falls back to ticker-only.
				if !ok {
					resourceUpdates = nil
					continue
				}
				_ = uri
				drainResourceUpdates(resourceUpdates)
				s.onResourceWake(ctx)
			}
		}
	}()
}

// Stop stops the ticker and cancels the tick context. Does NOT wait — call Drain.
// It latches `stopped` under the lock so a tick already selected by the loop's
// select (but not yet started) sees the stop and skips, rather than running a
// pass against the just-canceled ctx after Drain() has returned.
func (s *Scheduler) Stop() {
	s.stateMu.Lock()
	s.stopped = true
	if s.ticker != nil {
		s.ticker.Stop()
		s.ticker = nil
	}
	if s.cancel != nil {
		s.cancel()
	}
	s.stateMu.Unlock()
}

// Drain waits for the scheduler to quiesce: BOTH the Start loop goroutine to exit
// AND any in-flight tick to finish. Awaiting the loop closes the stop-race window
// — once the loop has exited it can never launch another tick, and `stopped` plus
// the in-tick re-check guarantee a just-selected ticker event won't start a pass
// after Stop(). Call after Stop, before tearing down deps.
func (s *Scheduler) Drain() {
	s.stateMu.Lock()
	loopDone := s.loopDone
	s.stateMu.Unlock()
	if loopDone != nil {
		<-loopDone
	}
	// The loop has exited; capture and await whatever tick (if any) it last
	// launched. Re-read under the lock — the final tick may have installed
	// `current` after we snapshotted loopDone.
	s.stateMu.Lock()
	done := s.current
	s.stateMu.Unlock()
	if done != nil {
		<-done
	}
	// Finally await any overflow notify re-run (spawnNotifyRerun). Stop() has
	// already latched `stopped` under stateMu, so no further runner can be added
	// and this Wait is the last thing standing between Drain and a safe teardown
	// of Queue/Store.
	s.notifyWG.Wait()
}

// Tick runs one scheduler pass. Safe to call directly in tests and the no-overlap
// guard lives HERE (not in Start) so direct callers can't double-run either. It
// returns immediately (without touching `current`, the handle Drain awaits) when
// a pass is already in flight or the scheduler has stopped; otherwise it installs
// the drain handle, runs the pass, and clears `running`.
func (s *Scheduler) Tick(ctx context.Context, now int64) {
	s.stateMu.Lock()
	// Stop re-check: a ticker event already selected when Stop() fired must not
	// launch a pass against the canceled ctx after Drain() returned.
	if s.running || s.stopped {
		s.stateMu.Unlock()
		return
	}
	s.running = true
	done := make(chan struct{})
	s.current = done
	s.stateMu.Unlock()

	defer close(done)
	defer func() {
		s.stateMu.Lock()
		s.running = false
		s.stateMu.Unlock()
	}()
	// A panic in the pass BODY itself — DueTimers/DueWatchers, the job fan-out, or
	// notify()/MarkNotified — sits OUTSIDE the per-item recovers and would otherwise
	// unwind out of the daemon goroutine and SILENTLY KILL ALL SUPERVISION for the rest
	// of the session (no more timers fire, no watcher ever completes). Recover here so the
	// next tick still runs, and best-effort surface it as an attention event (the publish
	// is itself guarded so a panic inside Queue/Store can't re-escape and kill the loop).
	defer func() {
		if r := recover(); r != nil {
			func() {
				defer func() { _ = recover() }()
				_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
					Source:    domain.SourceSystem,
					Severity:  domain.SeverityError,
					Title:     "Supervision error",
					Summary:   fmt.Sprintf("A background scheduler tick panicked and was recovered: %v", r),
					DedupeKey: "daemon-tick-panic",
				})
			}()
		}
	}()

	s.runPass(ctx, now)
}

// onResourceWake handles a resource-update notification: it brings every active
// terminal watcher forward (nextCheckAt = now) and runs one immediate pass, so a
// pushed agent-state transition is reflected without waiting the tick interval. It
// nudges ALL terminal watchers rather than mapping the URI to a specific one —
// notifications are infrequent (one per transition) and a watcher re-check is cheap
// and idempotent, so this avoids a stateful URI→watcher index. The Tick no-overlap
// guard means a wake colliding with an in-flight pass is dropped; the nudged
// nextCheckAt persists, so the next tick still picks it up (bounded by the interval).
func (s *Scheduler) onResourceWake(ctx context.Context) {
	now := domain.NowMS()
	if watchers, err := s.deps.Store.ListWatchers("active"); err == nil {
		for _, w := range watchers {
			// Only terminal watchers subscribe; a not-yet-due one is pulled forward so
			// the immediate pass's DueWatchers(now) returns it.
			if w.Kind == "terminal" && w.NextCheckAt > now {
				_ = s.deps.Store.UpdateWatcher(w.ID, map[string]any{"nextCheckAt": now})
			}
		}
	}
	s.Tick(ctx, now)
}

// drainResourceUpdates empties the buffered wake channel without blocking, so a
// burst of transitions coalesces into the single pass onResourceWake already ran.
func drainResourceUpdates(ch <-chan string) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// runPass is the unguarded body of one tick: fire due timers, run due watchers,
// delivering attention AS EACH JOB SETTLES — each item isolated so one failure
// can't starve the others or skip delivery. Due timers and due watchers run as
// BOUNDED, INDEPENDENT jobs (through a fixed worker pool with per-item deadlines)
// so a slow timer/watcher/MCP-read/model-judge can't block later items or delay
// notification delivery. Job EXECUTION is capped at tickJobConcurrency: an
// unbounded fan-out let ten due watchers land ten simultaneous MCP read bursts on
// Daintree in the same instant — the pool keeps the tick's aggregate pressure flat
// no matter how many items come due together. Each job triggers requestNotify()
// when it settles (so a fast timer's wake is never parked behind a wedged watcher
// burning its 120s deadline), and one backstop notify() still runs after every job
// settles so the pass can never end with undelivered attention. The whole pass
// stays no-overlap (the guard is in Tick).
func (s *Scheduler) runPass(ctx context.Context, now int64) {
	timers, _ := s.deps.Store.DueTimers(now)
	watchers, _ := s.deps.Store.DueWatchers(now)

	// A fixed worker set bounds BOTH execution and goroutine count. The previous
	// semaphore capped active work but still created one waiting goroutine per due
	// row, making a large catch-up backlog allocate in proportion to history.
	pool := newTickJobPool(ctx, len(timers)+len(watchers), s.requestNotify)

	for _, t := range timers {
		rec := t
		pool.Submit(func(jctx context.Context) {
			// fireTimer's inner handling covers payload execution; reschedule/publish
			// run outside it, so the pool's recovery keeps a panic there from taking
			// down the pass. The per-item deadline bounds a slow call_safe_tool dispatch.
			s.fireTimer(jctx, rec, now)
		})
	}

	// ONE tick-shared batched status read covering every due terminal watcher's
	// targets, threaded into each check via CheckContext.PrefetchedStatuses — N
	// due watchers cost one terminal.getStatus instead of N (the async
	// coordinator's proven batch pattern, applied across watchers). Launched
	// AFTER the timer jobs so a slow status read can never delay a due reminder
	// (timers don't depend on it and run concurrently in the pool).
	prefetched, prefetchedAt := s.prefetchWatcherStatuses(ctx, watchers)

	for _, w := range watchers {
		rec := w
		pool.Submit(func(jctx context.Context) {
			switch rec.Kind {
			case "terminal":
				cctx := s.deps.CtxFor(jctx, domain.ActorWatcher, rec.ID)
				cctx.PrefetchedStatuses = prefetched
				cctx.PrefetchedStatusesAt = prefetchedAt
				RunTerminalWatcherCheck(cctx, rec)
			case "pr_state":
				cctx := s.deps.CtxFor(jctx, domain.ActorWatcher, rec.ID)
				RunPrWatcherCheck(cctx, rec)
			default:
				// Fail closed: a misrouted unknown kind would silently reschedule
				// forever (false supervision).
				_ = s.deps.Store.UpdateWatcher(rec.ID, map[string]any{"status": "error", "lastCheckedAt": now})
			}
		})
	}

	// Backstop: after every job settles, one final delivery pass. Per-job
	// requestNotify already delivered the common cases as they happened; this
	// catches anything that slipped past it (a panicking job's published error, a
	// publish racing the last in-flight Digest) so the pass can never end with
	// undelivered attention. A single slow/hung job is bounded by its per-item
	// deadline, so this can at worst wait a few deadline rounds under the pool — it
	// never blocks indefinitely on one item.
	pool.CloseAndWait()
	s.notify()
}

// tickJobPool executes a pass through a fixed number of workers. The buffered
// queue is sized to this pass's known due set: it retains one small function value
// per row but never one goroutine stack per row. Each worker applies the deadline
// when an item actually starts, preserving the old queued-job budget semantics.
type tickJobPool struct {
	ctx       context.Context
	jobs      chan func(context.Context)
	onSettled func()
	workers   sync.WaitGroup
}

func newTickJobPool(ctx context.Context, capacity int, onSettled func()) *tickJobPool {
	p := &tickJobPool{
		ctx:       ctx,
		onSettled: onSettled,
	}
	if capacity <= 0 {
		return p
	}
	p.jobs = make(chan func(context.Context), capacity)
	workers := min(capacity, tickJobConcurrency)
	for i := 0; i < workers; i++ {
		p.workers.Add(1)
		go p.work()
	}
	return p
}

func (p *tickJobPool) Submit(job func(context.Context)) {
	if p.jobs == nil {
		return
	}
	p.jobs <- job
}

func (p *tickJobPool) CloseAndWait() {
	if p.jobs != nil {
		close(p.jobs)
	}
	p.workers.Wait()
}

func (p *tickJobPool) work() {
	defer p.workers.Done()
	for job := range p.jobs {
		if p.ctx.Err() != nil {
			continue // shutdown: leave queued durable rows due for the next owner
		}
		settled := func() (ok bool) {
			// Isolate a panicking job without killing this worker or starving the
			// remainder of the queue. As before, a panic skips the per-item notify;
			// the end-of-pass backstop still delivers anything it published.
			defer func() { _ = recover() }()
			// The per-item budget stays a real context.WithTimeout, deliberately —
			// unlike prefetchBudget below, which is cancel-based.
			//
			// Two forces pull in opposite directions here. This ctx is threaded into
			// every watcher/timer MCP read (CtxFor → CheckContext.Ctx → CallRead →
			// CallTool), where a DeadlineExceeded used to reach the client's degrade
			// path and tear the process-wide connection down over ONE over-budget
			// item. That is now handled at the source: mcp's degrade gate keys on
			// callerDone (any finished CALLER context), so a deadline that came from
			// this budget no longer degrades. A cancel-based timer here would be
			// redundant belt-and-braces.
			//
			// And it would COST something real: a cancel-only context advertises no
			// Deadline(), and backend.doJSON applies its own 60s default only when the
			// caller has none. Dropping the deadline silently halved the watcher
			// classify / model-judge budget from this 120s item allowance to 60s, so a
			// task finishing at 75s would start timing out and the watcher would
			// re-arm on an unmatched fallback. Keeping WithTimeout keeps the item
			// budget the single, honest deadline every downstream consumer sees.
			jctx, cancel := context.WithTimeout(p.ctx, time.Duration(itemDeadlineMS)*time.Millisecond)
			defer cancel()
			job(jctx)
			return true
		}()
		if settled && p.onSettled != nil {
			p.onSettled()
		}
	}
}

// requestNotify triggers attention delivery, coalescing concurrent requests: if a
// delivery pass is already running, the request just latches `pending` and returns
// — the active runner re-runs notify() before retiring, so an event published
// before ANY request is always covered by a Digest that starts after it, and a
// burst of N near-simultaneous settles costs at most two passes instead of N.
// Never blocks the caller behind another caller's full delivery (beyond notify's
// own exactly-once serialization).
func (s *Scheduler) requestNotify() {
	s.notifyReqMu.Lock()
	if s.notifyActive {
		s.notifyPending = true
		s.notifyReqMu.Unlock()
		return
	}
	s.notifyActive = true
	s.notifyReqMu.Unlock()
	for {
		s.notify()
		s.notifyReqMu.Lock()
		if !s.notifyPending {
			s.notifyActive = false
			s.notifyReqMu.Unlock()
			return
		}
		s.notifyPending = false
		s.notifyReqMu.Unlock()
	}
}

// prefetchBudget bounds the tick-shared status prefetch. It is applied via a
// CANCEL-based timer (time.AfterFunc(cancel)), NOT context.WithTimeout: the mcp
// client degrades the connection on a DeadlineExceeded, and a best-effort
// prefetch running long must abort as a plain Canceled (no degrade, no retry)
// — the mcp-bestEffort-reads rule. Generous enough for the normal read-retry
// budget; a server that can't answer a status read inside it is the struggling
// case the failed-batch short-circuit exists for.
const prefetchBudget = 30 * time.Second

// prefetchWatcherStatuses performs the tick's ONE batched terminal.getStatus
// across the union of every due terminal watcher's targets (inline output
// included — the same read shape each watcher would issue individually), and
// the wall-clock the snapshot was taken at (watchers reject a stale snapshot —
// see CheckContext.PrefetchedStatusesAt). nil when there is nothing to read or
// no way to read it (no terminal watchers, no CtxFor, MCP disconnected) — each
// check then falls back to its own read. A FAILED read is returned non-nil
// deliberately: the one prefetch already spent the full retry budget, and
// handing the failure to every watcher (rather than letting each re-hammer the
// server) is the pressure-relief this exists for. Panic-guarded and
// cancel-bounded: the prefetch sits OUTSIDE the per-item isolation, so a fault
// here must degrade to "no prefetch", never take down the pass.
func (s *Scheduler) prefetchWatcherStatuses(ctx context.Context, watchers []domain.WatcherRecord) (batch *StatusBatch, readAt int64) {
	defer func() {
		if r := recover(); r != nil {
			batch = nil
		}
	}()
	if s.deps.CtxFor == nil {
		return nil, 0
	}
	seen := make(map[string]struct{})
	var ids []string
	for _, w := range watchers {
		if w.Kind != "terminal" {
			continue
		}
		var targets []string
		// Corrupt targets are skipped here; the watcher's own check disables the row.
		if err := json.Unmarshal([]byte(w.TargetsJson), &targets); err != nil {
			continue
		}
		for _, t := range targets {
			if t == "" {
				continue
			}
			if _, dup := seen[t]; dup {
				continue
			}
			seen[t] = struct{}{}
			ids = append(ids, t)
		}
	}
	if len(ids) == 0 {
		return nil, 0
	}
	cctx := s.deps.CtxFor(ctx, domain.ActorWatcher, "status-prefetch")
	if cctx == nil || cctx.MCP == nil || !cctx.MCP.Connected() {
		return nil, 0
	}
	rctx := cctx.Ctx
	if rctx == nil {
		rctx = ctx
	}
	pctx, cancel := context.WithCancel(rctx)
	timer := time.AfterFunc(prefetchBudget, cancel)
	b := readStatusesWith(pctx, cctx.MCP, ids, true)
	timer.Stop()
	cancel()
	return &b, domain.NowMS()
}

// NotifyNow delivers any fresh attention+ events immediately — the async
// coordinator calls it right after publishing a completion so the wake fires
// now instead of on the next tick. Routed through requestNotify: safe (and
// coalesced) concurrently with the per-job settles and the tick's backstop, and
// a push arriving mid-delivery is covered by the runner's pending re-run instead
// of blocking the caller.
func (s *Scheduler) NotifyNow() { s.requestNotify() }

// notifyPageSize bounds one Digest page inside a notify pass. notify() DRAINS —
// it keeps digesting while pages come back full — so the cap bounds memory per
// read, not delivery: a burst larger than one page is fully delivered within the
// same pass instead of stranding its tail until an unrelated tick.
const notifyPageSize = 20

// notifyMaxPages hard-bounds the drain loop so a publisher continuously bumping
// an event (whose conditional ack then keeps failing) can never wedge a notify
// pass; whatever remains is picked up by the pending re-run / next tick.
const notifyMaxPages = 50

// notify delivers newly-unnotified attention+ events exactly once, draining page
// by page until the digest runs dry. Events are marked notified REGARDLESS of
// delivery success (else the same events re-fire forever) — but the mark is
// VERSION-CONDITIONAL (daemon.Queue.MarkNotified): an event a publisher
// materially updated between the Digest read and the mark keeps notifiedAt NULL,
// so the update is re-delivered on the next page/pass instead of being stamped
// away undelivered.
func (s *Scheduler) notify() {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()

	s.mu.Lock()
	cb := s.onAttention
	s.mu.Unlock()
	if cb == nil {
		return
	}
	atLeast := domain.SeverityAttention
	maxItems := notifyPageSize
	for page := 0; page < notifyMaxPages; page++ {
		fresh, err := s.deps.Queue.Digest(domain.QueueDigestOptions{
			SeverityAtLeast: &atLeast,
			NotifiedIsNull:  true,
			MaxItems:        &maxItems,
		})
		if err != nil || len(fresh) == 0 {
			return
		}
		func() {
			// Best-effort delivery: a panic in the callback must NOT skip markNotified.
			defer func() { _ = recover() }()
			cb(fresh)
		}()
		// A failed acknowledgement leaves this page NotifiedIsNull, so the very next
		// notify pass hands the same events over again — for a scheduled message, the
		// same instruction. The reactors dedupe what is still queued, but that only
		// covers the window before a turn starts, so stop paging rather than spinning
		// the same burst around the loop.
		if err := s.deps.Queue.MarkNotified(fresh); err != nil {
			return
		}
		if len(fresh) < maxItems {
			return // short page: the digest is drained
		}
	}
	// The pass exhausted its page bound with the digest still returning full
	// pages (>1000-event burst, or a publisher whose bumps keep failing the
	// conditional ack). Schedule another coalesced pass instead of stranding
	// the tail until an unrelated tick — the bound keeps each PASS finite, not
	// overall delivery.
	s.spawnNotifyRerun()
}

// spawnNotifyRerun schedules a coalesced follow-up notify pass on its own
// goroutine. It must be a goroutine: notify() is also called directly by the tick
// backstop and still holds notifyMu here, so a synchronous requestNotify would
// re-enter it.
//
// The runner is TRACKED (notifyWG) and refused once Stop() has latched, because
// Drain's contract is "call after Stop, before tearing down deps" — an untracked
// runner could outlive Drain() and call Queue.Digest / MarkNotified against an
// already-closed Store, silently dropping the delivery it was spawned to make.
// Both the stopped check and the Add happen under stateMu, which Stop() also
// holds, so no runner can be added after Stop() returns and Drain's Wait cannot
// miss one.
func (s *Scheduler) spawnNotifyRerun() {
	s.stateMu.Lock()
	if s.stopped {
		s.stateMu.Unlock()
		return // shutting down: the next owner's boot pass delivers the tail
	}
	s.notifyWG.Add(1)
	s.stateMu.Unlock()
	go func() {
		defer s.notifyWG.Done()
		s.requestNotify()
	}()
}

// timerPayload is the parsed timer payload shape.
type timerPayload struct {
	Type        string `json:"type"`
	Message     string `json:"message,omitempty"`
	CheckPrompt string `json:"checkPrompt,omitempty"`
	ToolCall    *struct {
		ToolName string          `json:"toolName"`
		Args     json.RawMessage `json:"args"`
	} `json:"toolCall,omitempty"`
}

// missedOccurrences reports how many whole repeat intervals elapsed between a
// timer's due deadline (rec.FireAt) and now — the occurrences that were collapsed
// away while the assistant was closed. The catch-up reschedule folds all of them
// into this single fire (see rescheduleePatch), so this count is the only record
// that intervening occurrences were skipped. Returns 0 for one-shot timers and
// for an on-time fire (elapsed < one interval).
func missedOccurrences(rec domain.TimerRecord, now int64) int64 {
	if rec.RepeatEveryMs == nil || *rec.RepeatEveryMs <= 0 {
		return 0
	}
	// Below the tick, a repeat collapses occurrences on EVERY normal fire (the
	// daemon only checks every SchedulerTickMS), so a multi-interval gap does not
	// imply downtime — don't claim occurrences were "skipped while closed".
	if *rec.RepeatEveryMs < SchedulerTickMS {
		return 0
	}
	elapsed := now - rec.FireAt
	if elapsed <= 0 {
		return 0
	}
	// elapsed and the interval are both non-negative here, so Go's truncating
	// integer division equals floor — the issue's prescribed formula.
	return elapsed / *rec.RepeatEveryMs
}

// catchUpClause renders the operator-facing suffix appended to a fired timer's
// summary when missed > 0, so a single catch-up fire still reports the backlog it
// stood in for. Empty for the common on-time path (nothing skipped).
func catchUpClause(missed int64) string {
	switch {
	case missed <= 0:
		return ""
	case missed == 1:
		return " (caught up — 1 occurrence was skipped while the assistant was closed; fired once now)"
	default:
		return fmt.Sprintf(" (caught up — %d occurrences were skipped while the assistant was closed; fired once now)", missed)
	}
}

// fireTimer fires one due timer, then reschedules it (always). A corrupt row is
// disabled with a visible error event so it can't throw every tick.
func (s *Scheduler) fireTimer(ctx context.Context, rec domain.TimerRecord, now int64) {
	var payload timerPayload
	var target *domain.EventTarget
	if err := json.Unmarshal([]byte(rec.PayloadJson), &payload); err != nil {
		s.disableCorruptTimer(rec, now, err)
		return
	}
	if rec.TargetJson != nil && *rec.TargetJson != "" {
		var t domain.EventTarget
		if err := json.Unmarshal([]byte(*rec.TargetJson), &t); err != nil {
			s.disableCorruptTimer(rec, now, err)
			return
		}
		target = &t
	}
	// Stamp the timer onto every event this fire publishes, whether or not the model
	// gave the timer a target of its own. It is what lets a surface join an outcome
	// back to the schedule row STRUCTURALLY — the dedupe key is not a substitute, since
	// it is "timer:<id>" for a fire but "denied:timer:<id>" for a blocked dispatch, and
	// parsing either is a promise about a string nothing guarantees.
	if target == nil {
		target = &domain.EventTarget{}
	}
	target.TimerID = rec.ID

	// Stable across every firing (NOT keyed by runCount) so a repeating timer
	// updates one live inbox item in place. Shared by success and error paths.
	dedupeKey := "timer:" + rec.ID
	// Which firing this is, read BEFORE the claim below advances RunCount. A message
	// needs a per-fire identity; everything else keeps the aggregating key above.
	occurrence := rec.RunCount + 1
	// Dispatch on the JSON's type, falling back to the typed DB column.
	payloadType := payload.Type
	if payloadType == "" {
		payloadType = rec.PayloadType
	}

	// A MESSAGE that is long overdue is dropped, not delivered late.
	//
	// One contract, applied on both sides of the claim. Recovery already refuses to
	// republish an occurrence older than this window, and without the same rule here an
	// identical outage would produce opposite outcomes purely by where the crash landed:
	// a machine off for three days would silently execute an instruction if it died
	// BEFORE the claim, and silently drop it if it died just after.
	//
	// Freshness rather than catch-up, because a message is not a reminder. "Run the
	// migration in an hour" delivered three days later is not a late delivery; it is the
	// wrong action, against a world that has moved on. The user is told it was missed
	// instead — which they can act on — rather than having it happen underneath them.
	//
	// enqueue and call_safe_tool keep catch-up unchanged: a note is still worth reading
	// late, and a fixed tool call was chosen with its own arguments frozen.
	if payloadType == "message" && now-rec.FireAt > staleMessageWindowMs {
		// The ORDINARY post-fire patch, unmodified. This skips one occurrence; it does
		// not cancel a schedule. A nightly message whose machine was off for a night
		// must run tomorrow — forcing the row terminal here would kill the whole
		// standing instruction because a single delivery was missed, which is a much
		// larger loss than the one being avoided. A one-shot is already terminal in
		// this patch, so it needs no special case.
		patch, terminal := rescheduleePatch(rec, now)
		if claimed, _ := s.deps.Store.ClaimDueTimer(rec.ID, rec.FireAt, patch); claimed {
			_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
				Source: domain.SourceTimer, Severity: domain.SeverityAttention,
				Title: rec.Title,
				Summary: fmt.Sprintf(
					"This scheduled message came due %s ago and was not delivered — the assistant was not running. "+
						"It has NOT been carried out, because acting on it this late could be wrong. Ask again if you still want it.",
					overdueFor(now-rec.FireAt)),
				Target: failureTarget(target),
				// The PER-FIRE key, not the aggregating one. This row is the record that
				// occurrence N was accounted for: recovery looks for exactly this key to
				// decide whether a fire went missing, so publishing the skip under the
				// generic "timer:<id>" left the occurrence looking unpublished — and the
				// next restart within the window rebuilt and delivered the very
				// instruction this branch had just decided was too stale to run.
				DedupeKey: fmt.Sprintf("timer:%s:fire:%d", rec.ID, occurrence),
			})
			// Only on the LAST fire, matching the normal path: a repeat that continues
			// still needs its authority for the occurrences ahead of it.
			if terminal {
				_, _ = s.deps.Store.RevokeGrantsByActor(rec.ID, now)
			}
		}
		return
	}

	// CLAIM the timer BEFORE firing: atomically advance it to its post-fire state, but only
	// while it is still the due row DueTimers read (same status+fireAt). If the claim fails,
	// the main turn cancelled or edited it under us — do NOT fire, and never write it back
	// (which would resurrect a cancelled timer). Finalizing first also stops an overrunning
	// tick from re-selecting it (no double-fire). Grants are revoked only on a TERMINAL claim.
	patch, terminal := rescheduleePatch(rec, now)
	claimed, _ := s.deps.Store.ClaimDueTimer(rec.ID, rec.FireAt, patch)
	if !claimed {
		return
	}
	if terminal {
		// DEFER the revoke past the dispatch below. A terminal claim is the LAST fire, and
		// revoking inline here would retire the grant before the call_safe_tool payload that
		// needs it: ConsumeGrant only matches rows with revokedAt IS NULL, so a one-shot timer
		// (terminal on its very first fire) could never spend the grant it was given. The call
		// came back CONFIRMATION_REQUIRED and dispatch published a blocked item offering to
		// authorize a timer that had already fired — a remediation that cannot work. Deferring
		// keeps the grant live for exactly the fire it was minted for and still retires it
		// before fireTimer returns, including on the panic path (defer runs while unwinding).
		defer func() { _, _ = s.deps.Store.RevokeGrantsByActor(rec.ID, now) }()
	}

	// A long closure collapses every missed repeat into this one fire; surface how
	// many occurrences it stands in for so the operational record isn't silently
	// short. Empty for one-shot/on-time timers, so the common path is unchanged.
	// Skip TERMINAL fires (maxRuns/repeatUntil reached): a capped timer has no
	// remaining backlog, so floor(elapsed/every) would over-report occurrences that
	// were never going to fire.
	clause := ""
	if !terminal {
		clause = catchUpClause(missedOccurrences(rec, now))
	}

	func() {
		defer func() {
			if r := recover(); r != nil {
				// The message branch may already have stamped the marker onto the
				// SHARED target before panicking. Publishing a failure through it would
				// hand the model "Timer check failed: …" framed as the user's own
				// instruction — an error impersonating a request. Failures are never
				// messages, so clear it rather than trusting how far the branch got.
				failTarget := failureTarget(target)
				_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
					Source: domain.SourceTimer, Severity: domain.SeverityError,
					Title: rec.Title, Summary: fmt.Sprintf("Timer check failed: %v", r),
					Target: failTarget, DedupeKey: dedupeKey,
				})
			}
		}()
		switch payloadType {
		case "message":
			// A timed MESSAGE. Published under its own source so the wake filters can
			// tell it from the reminder below — same table, same fire, opposite
			// contract: this one is meant to start a turn.
			//
			// Attention severity, not urgent: a message coming due on time is the
			// system working, not a problem. It has to clear the surfacing threshold
			// (info does not) or the wake would depend on an event nobody delivers.
			msg := strings.TrimSpace(payload.Message)
			if msg == "" {
				msg = rec.Title
			}
			// Marked HERE, on this branch alone: an enqueue reminder, a tool-call
			// outcome and an error all share `target` and none of them may wake.
			target.TimerMessage = true
			target.TimerOccurrence = occurrence
			// The due time the USER chose, carried so the delivery gate can be exact.
			target.TimerDueAt = rec.FireAt
			err := s.deps.Queue.Publish(domain.QueuePublishArgs{
				Source: domain.SourceTimer, Severity: domain.SeverityAttention,
				Title: rec.Title, Summary: msg, Target: target,
				// PER-FIRE key. The stable "timer:<id>" the other branches share folds
				// every occurrence into one row whose notifiedAt is already set, so a
				// repeating message would wake once and then be silently swallowed
				// forever after. Aggregation is right for a reminder and wrong for an
				// instruction: each delivery is its own errand.
				DedupeKey: fmt.Sprintf("timer:%s:fire:%d", rec.ID, occurrence),
			})
			// The claim above already advanced the timer, so this occurrence is spent
			// whether or not the event landed. Swallowing the error here is precisely
			// the shape of the bug this feature exists to end: the schedule says fired,
			// and nothing whatsoever happens. If the message cannot be delivered, say so
			// where the user will see it — a visible failure is recoverable, silence is
			// not. Published through failureTarget so the error cannot itself be
			// mistaken for the instruction.
			if err != nil {
				_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
					Source: domain.SourceTimer, Severity: domain.SeverityError,
					Title: rec.Title,
					Summary: fmt.Sprintf(
						"The scheduled message came due but could not be delivered: %v. It has not been carried out.", err),
					Target: failureTarget(target), DedupeKey: dedupeKey,
				})
			}
		case "enqueue":
			// A scheduled enqueue is a user reminder — publish at attention so it
			// reaches the inbox (info sits below the surfacing threshold).
			summary := payload.Message
			if summary == "" {
				summary = rec.Title
			}
			_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
				Source: domain.SourceTimer, Severity: domain.SeverityAttention,
				Title: rec.Title, Summary: summary + clause, Target: target, DedupeKey: dedupeKey,
			})
		case "run_check":
			// Deprecated, legacy rows only — fire as a plain grounded reminder.
			prompt := payload.CheckPrompt
			if prompt == "" {
				prompt = rec.Title
			}
			_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
				Source: domain.SourceTimer, Severity: domain.SeverityAttention,
				Title:   rec.Title,
				Summary: "Reminder (run_check is deprecated — use a watcher to observe real state): " + prompt + clause,
				Target:  target, DedupeKey: dedupeKey,
			})
		case "call_safe_tool":
			if payload.ToolCall == nil {
				break
			}
			argsJSON := string(payload.ToolCall.Args)
			if argsJSON == "" {
				argsJSON = "{}"
			}
			// Thread rec.ID as the actorId so a timer-scoped grant can be consumed.
			// A stored payload may not schedule another timer, even if it was written
			// before timer.schedule refused it. Validation guards what is CREATED; the
			// rows already on disk were created under the old rules, and a migration
			// cannot reach a project that is offline right now. This is the same rule
			// enforced where it can never be out of date.
			if isTimerScheduleTool(payload.ToolCall.ToolName) {
				_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
					Source: domain.SourceTimer, Severity: domain.SeverityError,
					Title: rec.Title,
					Summary: "This timer tried to schedule another timer, which is not allowed. " +
						"It has been retired without running. Schedule the work itself instead.",
					Target: failureTarget(target), DedupeKey: dedupeKey,
				})
				// RETIRED, not merely skipped. The claim above has already rescheduled a
				// repeating row, so leaving it scheduled would republish this same
				// refusal on every interval for ever — a row that can never run and
				// never stops complaining. It is forbidden, so it is over.
				_ = s.deps.Store.UpdateTimer(rec.ID, map[string]any{"status": "error", "lastFiredAt": now})
				_, _ = s.deps.Store.RevokeGrantsByActor(rec.ID, now)
				break
			}
			res, err := s.deps.Registry.Dispatch(ctx, domain.ActorTimer, rec.ID, payload.ToolCall.ToolName, argsJSON)
			if err != nil {
				_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
					Source: domain.SourceTimer, Severity: domain.SeverityError,
					Title: rec.Title, Summary: fmt.Sprintf("Timer check failed: %v", err),
					Target: target, DedupeKey: dedupeKey,
				})
				break
			}
			// A confirm-required tool denied to a non-interactive actor is an
			// expected structural outcome — don't double-raise a timer error.
			if res.Error == nil || res.Error.Code != "CONFIRMATION_REQUIRED" {
				sev := domain.SeverityInfo
				if !res.Ok {
					sev = domain.SeverityError
				}
				// Only the success path carries the catch-up clause; a failure is an
				// error-severity event and shouldn't be diluted with backlog context.
				summary := res.Summary
				if res.Ok {
					summary += clause
				}
				_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
					Source: domain.SourceTimer, Severity: sev,
					Title: rec.Title, Summary: summary, Target: target, DedupeKey: dedupeKey,
				})
			}
		}
	}()
	// The timer was already advanced by the claim above (ClaimDueTimer), so there is no
	// reschedule here — re-writing it would race a cancel that arrived during the fire.

	// AFTER the dispatch and its publish, so a host that reacts by re-reading sees the
	// outcome this fire produced rather than the state just before it.
	if fired := s.deps.OnTimerFired; fired != nil {
		fired(rec.ID)
	}
}

// disableCorruptTimer publishes a visible error and disables a corrupt timer row.
func (s *Scheduler) disableCorruptTimer(rec domain.TimerRecord, now int64, err error) {
	_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityError,
		Title:   rec.Title,
		Summary: fmt.Sprintf("Disabling corrupt timer %s: %v", rec.ID, err),
		// Stamped here too: a corrupt row is the timer a user is most likely to want
		// to find, and it is the one whose own target could not be parsed.
		Target: &domain.EventTarget{TimerID: rec.ID},
	})
	_ = s.deps.Store.UpdateTimer(rec.ID, map[string]any{"status": "fired", "lastFiredAt": now})
	_, _ = s.deps.Store.RevokeGrantsByActor(rec.ID, now)
}

// rescheduleePatch computes a fired timer's post-fire COLUMN PATCH without writing it, plus
// whether the timer is now TERMINAL (fired/done → its grants should be revoked). The caller
// applies the patch atomically via ClaimDueTimer so it never overwrites a concurrent cancel.
func rescheduleePatch(rec domain.TimerRecord, now int64) (patch map[string]any, terminal bool) {
	runCount := rec.RunCount + 1
	repeats := rec.RepeatEveryMs != nil && *rec.RepeatEveryMs > 0
	repeatDone := !repeats ||
		(rec.MaxRuns != nil && runCount >= *rec.MaxRuns) ||
		// Stop if the NEXT fire would land past the deadline (no extra fire at/after it).
		(rec.RepeatUntil != nil && repeats && now+*rec.RepeatEveryMs > *rec.RepeatUntil)

	if repeatDone {
		status := "fired"
		if repeats {
			status = "done"
		}
		return map[string]any{"status": status, "runCount": runCount, "lastFiredAt": now}, true
	}
	// Catch-up: schedule next fire relative to NOW, not the missed deadline, so a
	// long sleep produces a single catch-up fire rather than a storm.
	//
	// The addition is overflow-checked because a row stored before the interval was
	// bounded can still carry one near MaxInt64. Wrapping produces a NEGATIVE fireAt,
	// which every due check reads as permanently overdue — so the timer that asked to
	// run once in ten thousand years instead runs on every three-second tick, for ever.
	// Retired rather than clamped: the schedule is nonsense, and inventing a plausible
	// one for it would be guessing at an intent nobody expressed.
	// A row that costs a model call per fire must be BOUNDED and SLOW, enforced here as
	// well as at schedule time. Validation guards what is created; these rows were
	// written under the old rules, and no migration reaches a project that is offline
	// right now. An unbounded `everyMs:1` call_safe_tool row would otherwise dispatch on
	// every three-second tick for the life of the project, which is the runaway the
	// schedule-time bounds exist to prevent.
	// Scoped to the RUNAWAY, not to everything the schedule-time rules now refuse.
	//
	// The pathological row is one that costs a model call and repeats FASTER than the
	// scheduler's own tick: it fires on every pass, for the life of the project, and no
	// bound it carries is reached in any useful time. That is the case worth retiring a
	// stored row over.
	//
	// An unbounded but SLOW legacy repeat is left alone deliberately. It is long-standing
	// behaviour with tests that encode it, newly created ones are already refused at
	// schedule time, and silently retiring a minute-by-minute job somebody is relying on
	// would be a worse surprise than the one being prevented.
	// Retire a stored paid repeat that is UNBOUNDED — no maxRuns, no until. That is the
	// row that never stops on its own, whatever its interval: at a minute apart it is
	// 1,440 paid fires a day, for the life of the project, from a schedule nobody can
	// see the end of.
	//
	// A BOUNDED row is left alone even if it is faster than the current floor. It stops
	// by itself, the user chose the count, and retiring it would break a schedule that
	// is already running — the surprise would be larger than the spend.
	// Scoped to "message", the payload this feature introduced, and enforced here as
	// well as at schedule time so a row that reached the store by any other route still
	// stops. A message repeat must be bounded and no faster than the floor; one that is
	// neither would start a paid TURN on every fire with nobody watching.
	//
	// Deliberately NOT applied to call_safe_tool. An unbounded repeating tool call is
	// long-standing behaviour with tests that encode it (TestScheduler_RepeatingFireKeepsItsGrants,
	// the catch-up tests), and there are real schedules relying on it. Retiring those
	// retroactively is a product decision about an EXISTING feature, not something this
	// one gets to make on its way past — so it is reported rather than done.
	if rec.PayloadType == "message" &&
		((rec.MaxRuns == nil && rec.RepeatUntil == nil) || *rec.RepeatEveryMs < minPaidRepeatMs) {
		return map[string]any{"status": "done", "runCount": runCount, "lastFiredAt": now}, true
	}
	next := now + *rec.RepeatEveryMs
	if next < now {
		return map[string]any{"status": "done", "runCount": runCount, "lastFiredAt": now}, true
	}
	return map[string]any{
		"fireAt": next, "runCount": runCount,
		"lastFiredAt": now, "status": "scheduled",
	}, false
}

// failureTarget copies a fire's target with the scheduled-message marker cleared.
//
// Every failure path in fireTimer shares one `target` with the success paths, and the
// message branch mutates it. A failure that inherited the marker would satisfy
// IsTimerMessageWake and be delivered to the model as an instruction the user never
// wrote. Copying rather than mutating keeps the success path's target intact.
func failureTarget(t *domain.EventTarget) *domain.EventTarget {
	if t == nil {
		return nil
	}
	out := *t
	out.TimerMessage = false
	out.TimerOccurrence = 0
	return &out
}

// isTimerScheduleTool reports whether a stored payload names the timer scheduler, in
// either spelling. Mirrors the schedule-time refusal in internal/tools/timer; both
// exist because they catch different rows — one what is being written, the other what
// was written before the rule.
func isTimerScheduleTool(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	return strings.ReplaceAll(n, "__", ".") == "timer.schedule"
}

// The fire-time mirrors of the schedule-time limits in internal/tools/timer. Duplicated
// deliberately rather than shared: the two run in different processes at different
// times, and the whole point of this pair is that a row can reach the second without
// ever having passed the first.
const minPaidRepeatMs int64 = 60_000

// staleMessageWindowMs is how long after its due time a scheduled message may still be
// delivered. Deliberately the same window storage uses to recover a lost occurrence, so
// the two halves of "was it delivered?" cannot disagree: one hour covers a crash, a
// restart and a handover, and stops short of the point where acting on the instruction
// would be acting on a stale reading of the world.
const staleMessageWindowMs int64 = 60 * 60 * 1000

// overdueFor renders a lateness for a human, coarsely — the exact milliseconds are
// noise next to "this did not happen".
func overdueFor(ms int64) string {
	switch {
	case ms < 2*60*60*1000:
		return fmt.Sprintf("%d minutes", ms/60_000)
	case ms < 48*60*60*1000:
		return fmt.Sprintf("%d hours", ms/(60*60*1000))
	default:
		return fmt.Sprintf("%d days", ms/(24*60*60*1000))
	}
}
