package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
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
	// ResourceUpdates is the MCP client's resource-update wake channel (each value
	// is a changed resource URI). When a subscribed terminal's agent state pushes a
	// transition, the loop nudges active terminal watchers due and ticks immediately
	// instead of waiting the next interval. nil disables the fast path (poll-only).
	ResourceUpdates <-chan string
}

// Scheduler is the in-process daemon. One pass per tick: fire due timers, run due
// watchers, then notify. No-overlap guarded; Drain waits for the in-flight tick.
type Scheduler struct {
	deps   SchedulerDeps
	tickMs int64

	mu          sync.Mutex
	onAttention func(events []domain.QueueEvent)

	// notifyMu serializes notify() between the tick path and NotifyNow (the async
	// coordinator's post-publish push). Two concurrent notify passes could both
	// digest the same fresh event before either marks it notified, delivering one
	// completion twice — the lock makes delivery exactly-once.
	notifyMu sync.Mutex

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
// then notify — each item isolated so one failure can't starve the others or skip
// notify(). Due timers and due watchers run as BOUNDED, INDEPENDENT jobs (each on
// its own goroutine with a per-item deadline) so a slow timer/watcher/MCP-read/
// model-judge can't block later items or delay notification delivery. Job
// EXECUTION is capped at tickJobConcurrency: an unbounded fan-out let ten due
// watchers land ten simultaneous MCP read bursts on Daintree in the same instant
// — the pool keeps the tick's aggregate pressure flat no matter how many items
// come due together. notify() runs only after every job settles, so attention is
// always delivered at the end of the tick. The whole pass stays no-overlap (the
// guard is in Tick).
func (s *Scheduler) runPass(ctx context.Context, now int64) {
	timers, _ := s.deps.Store.DueTimers(now)
	watchers, _ := s.deps.Store.DueWatchers(now)

	sem := make(chan struct{}, tickJobConcurrency)
	var wg sync.WaitGroup
	// runJob runs one isolated, deadline-bounded item under the concurrency cap.
	// The deadline starts when the job STARTS (inside the semaphore hold), so an
	// item queued behind a slow batch never burns its budget while waiting.
	runJob := func(job func(jctx context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Isolate per-item failures — including a panicking CtxFor, which sits
			// outside the check itself — so one bad item can't abort the pass.
			defer func() { _ = recover() }()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return // shutdown while queued: skip, the row stays due for the next owner
			}
			defer func() { <-sem }()
			jctx, cancel := context.WithTimeout(ctx, time.Duration(itemDeadlineMS)*time.Millisecond)
			defer cancel()
			job(jctx)
		}()
	}

	for _, t := range timers {
		rec := t
		runJob(func(jctx context.Context) {
			// fireTimer's inner handling covers payload execution; reschedule/publish
			// run outside it, so the runJob recover keeps a panic there from taking
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
		runJob(func(jctx context.Context) {
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

	// ALWAYS reach notification delivery: wait for every job to settle, then notify
	// once. A single slow/hung job is bounded by its per-item deadline, so this can
	// at worst wait a few deadline rounds under the pool — it never blocks
	// indefinitely on one item.
	wg.Wait()
	s.notify()
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
// now instead of on the next tick. Safe concurrently with the tick's own
// notify (notifyMu serializes delivery).
func (s *Scheduler) NotifyNow() { s.notify() }

// notify delivers newly-unnotified attention+ events exactly once, marking them
// notified REGARDLESS of delivery success (else the same events re-fire forever).
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
	maxItems := 20
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
	ids := make([]string, len(fresh))
	for i, e := range fresh {
		ids[i] = e.ID
	}
	_ = s.deps.Queue.MarkNotified(ids)
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

	// Stable across every firing (NOT keyed by runCount) so a repeating timer
	// updates one live inbox item in place. Shared by success and error paths.
	dedupeKey := "timer:" + rec.ID
	// Dispatch on the JSON's type, falling back to the typed DB column.
	payloadType := payload.Type
	if payloadType == "" {
		payloadType = rec.PayloadType
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
		_, _ = s.deps.Store.RevokeGrantsByActor(rec.ID, now)
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
				_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
					Source: domain.SourceTimer, Severity: domain.SeverityError,
					Title: rec.Title, Summary: fmt.Sprintf("Timer check failed: %v", r),
					Target: target, DedupeKey: dedupeKey,
				})
			}
		}()
		switch payloadType {
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
}

// disableCorruptTimer publishes a visible error and disables a corrupt timer row.
func (s *Scheduler) disableCorruptTimer(rec domain.TimerRecord, now int64, err error) {
	_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityError,
		Title:   rec.Title,
		Summary: fmt.Sprintf("Disabling corrupt timer %s: %v", rec.ID, err),
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
	return map[string]any{
		"fireAt": now + *rec.RepeatEveryMs, "runCount": runCount,
		"lastFiredAt": now, "status": "scheduled",
	}, false
}
