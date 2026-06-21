package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// SchedulerDeps wires the scheduler to its dependencies (scheduler.ts §2.1).
type SchedulerDeps struct {
	Store    Store
	Queue    Queue
	Registry Registry
	// CtxFor builds a CheckContext for a non-interactive actor ("watcher"/"timer").
	// Mirrors the TS ctxFor(actor, actorId?) — it supplies the MCP/model/projectPath
	// for that actor. ctx is the scheduler's tick context (cancellation).
	CtxFor func(ctx context.Context, actor domain.ToolActor, actorID string) *CheckContext
	// TickMS overrides the tick interval (defaults to SchedulerTickMS).
	TickMS int64
	// OnAttention is called with newly-created attention+ events after each tick.
	OnAttention func(events []domain.QueueEvent)
}

// Scheduler is the in-process daemon. One pass per tick: fire due timers, run due
// watchers, then notify. No-overlap guarded; Drain waits for the in-flight tick.
type Scheduler struct {
	deps   SchedulerDeps
	tickMs int64

	mu          sync.Mutex
	onAttention func(events []domain.QueueEvent)

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

// Start begins ticking. Idempotent. The loop ends on Stop (context cancel). Unlike
// the TS unref'd interval there is no "don't keep the process alive" concern — the
// goroutine's lifecycle is owned by Stop/context cancellation.
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

	s.runPass(ctx, now)
}

// runPass is the unguarded body of one tick: fire due timers, run due watchers,
// then notify — each item isolated so one failure can't starve the others or skip
// notify(). Due timers and due watchers run as BOUNDED, INDEPENDENT jobs (each on
// its own goroutine with a per-item deadline) so a slow timer/watcher/MCP-read/
// model-judge can't block later items or delay notification delivery. notify()
// runs only after every job settles, so attention is always delivered at the end
// of the tick. The whole pass stays no-overlap (the guard is in Tick).
func (s *Scheduler) runPass(ctx context.Context, now int64) {
	timers, _ := s.deps.Store.DueTimers(now)
	watchers, _ := s.deps.Store.DueWatchers(now)

	var wg sync.WaitGroup

	for _, t := range timers {
		wg.Add(1)
		go func(rec domain.TimerRecord) {
			defer wg.Done()
			// Isolate per-timer failures. fireTimer's inner handling covers payload
			// execution; reschedule/publish run outside it, so a panic there would
			// otherwise abort the job — but a recover here keeps it from taking down
			// the pass. A per-item deadline bounds a slow call_safe_tool dispatch.
			defer func() { _ = recover() }()
			jctx, cancel := context.WithTimeout(ctx, time.Duration(itemDeadlineMS)*time.Millisecond)
			defer cancel()
			s.fireTimer(jctx, rec, now)
		}(t)
	}

	for _, w := range watchers {
		wg.Add(1)
		go func(rec domain.WatcherRecord) {
			defer wg.Done()
			// Isolate per-watcher failures — including a panicking CtxFor, which sits
			// outside the check itself — so one bad watcher can't abort the pass. A
			// per-item deadline bounds a slow MCP read / model judge.
			defer func() { _ = recover() }()
			jctx, cancel := context.WithTimeout(ctx, time.Duration(itemDeadlineMS)*time.Millisecond)
			defer cancel()
			switch rec.Kind {
			case "terminal":
				cctx := s.deps.CtxFor(jctx, domain.ActorWatcher, rec.ID)
				RunTerminalWatcherCheck(cctx, rec)
			case "pr_state":
				cctx := s.deps.CtxFor(jctx, domain.ActorWatcher, rec.ID)
				RunPrWatcherCheck(cctx, rec)
			default:
				// Fail closed: a misrouted unknown kind would silently reschedule
				// forever (false supervision).
				_ = s.deps.Store.UpdateWatcher(rec.ID, map[string]any{"status": "error", "lastCheckedAt": now})
			}
		}(w)
	}

	// ALWAYS reach notification delivery: wait for every job to settle, then notify
	// once. A single slow/hung job is bounded by its per-item deadline, so this can
	// at worst wait itemDeadlineMS — it never blocks indefinitely on one item.
	wg.Wait()
	s.notify()
}

// notify delivers newly-unnotified attention+ events exactly once, marking them
// notified REGARDLESS of delivery success (else the same events re-fire forever).
func (s *Scheduler) notify() {
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
				Title: rec.Title, Summary: summary, Target: target, DedupeKey: dedupeKey,
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
				Summary: "Reminder (run_check is deprecated — use a watcher to observe real state): " + prompt,
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
				_ = s.deps.Queue.Publish(domain.QueuePublishArgs{
					Source: domain.SourceTimer, Severity: sev,
					Title: rec.Title, Summary: res.Summary, Target: target, DedupeKey: dedupeKey,
				})
			}
		}
	}()

	s.reschedule(rec, now)
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

// reschedule advances a fired timer: repeat (with sleep catch-up) or finish.
func (s *Scheduler) reschedule(rec domain.TimerRecord, now int64) {
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
		_ = s.deps.Store.UpdateTimer(rec.ID, map[string]any{
			"status": status, "runCount": runCount, "lastFiredAt": now,
		})
		_, _ = s.deps.Store.RevokeGrantsByActor(rec.ID, now)
		return
	}
	// Catch-up: schedule next fire relative to NOW, not the missed deadline, so a
	// long sleep produces a single catch-up fire rather than a storm.
	_ = s.deps.Store.UpdateTimer(rec.ID, map[string]any{
		"fireAt": now + *rec.RepeatEveryMs, "runCount": runCount,
		"lastFiredAt": now, "status": "scheduled",
	})
}
