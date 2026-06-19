/**
 * In-process scheduler / daemon.
 *
 * Drives all autonomous work: it fires due timers and runs due terminal watchers
 * on a tick, persisting everything to SQLite so it survives restarts and performs
 * sleep catch-up. For the prototype the daemon runs inside the CLI process; the
 * architecture (state in SQLite, idempotent ticks) is ready to split into a
 * detached process later.
 */
import type { ToolContext } from "../tools/types.js";
import type { ToolRegistry } from "../tools/registry.js";
import type { Db } from "../storage/db.js";
import type { Queue } from "../queue.js";
import type { ModelRouter } from "../models/router.js";
import type { EventTarget, QueueEvent, TimerRecord } from "../schemas.js";
import { runTerminalWatcherCheck } from "./watcherEngine.js";
import { runPrWatcherCheck } from "./prWatcherEngine.js";
import { SCHEDULER_TICK_MS } from "../watcherCadence.js";

export interface SchedulerDeps {
  db: Db;
  queue: Queue;
  router: ModelRouter;
  registry: ToolRegistry;
  ctxFor: (actor: ToolContext["actor"], actorId?: string) => ToolContext;
  tickMs?: number;
  /** Called with newly-created attention+ events after each tick. */
  onAttention?: (events: QueueEvent[]) => void;
}

export class Scheduler {
  private timer?: NodeJS.Timeout;
  private running = false;
  private current?: Promise<void>;
  private readonly tickMs: number;
  /** Mutable so a UI remount can rebind a fresh callback (see App.startScheduler). */
  private onAttention?: (events: QueueEvent[]) => void;

  constructor(private deps: SchedulerDeps) {
    this.tickMs = deps.tickMs ?? SCHEDULER_TICK_MS;
    this.onAttention = deps.onAttention;
  }

  /** Replace the attention callback (e.g. when the UI controller remounts). */
  setOnAttention(onAttention?: (events: QueueEvent[]) => void): void {
    this.onAttention = onAttention;
  }

  start(): void {
    if (this.timer) return;
    this.timer = setInterval(() => {
      // Don't replace `this.current` while a tick is still in flight — otherwise a
      // skipped (early-returning) tick would overwrite the handle drain() awaits,
      // letting stop()/drain() return before the real tick releases MCP/DB.
      if (this.running) return;
      this.current = this.tick().catch(() => {});
    }, this.tickMs);
    // Don't keep the process alive solely for the scheduler.
    this.timer.unref?.();
  }

  stop(): void {
    if (this.timer) clearInterval(this.timer);
    this.timer = undefined;
  }

  /** Await any in-flight tick — call after stop() before tearing down deps. */
  async drain(): Promise<void> {
    await this.current;
  }

  /** One scheduler pass. Safe to call directly in tests. */
  async tick(now = Date.now()): Promise<void> {
    if (this.running) return;
    this.running = true;
    try {
      for (const t of this.deps.db.dueTimers(now)) {
        // Isolate per-timer failures the same way the watcher loop below does.
        // fireTimer's inner try/catch only covers payload execution; reschedule()
        // (and any publish path) runs OUTSIDE it, so a SQLite throw there would
        // otherwise abort the whole loop — starving later timers AND skipping the
        // notify() that delivers this tick's attention events.
        try {
          await this.fireTimer(t, now);
        } catch {
          /* one timer's failure must not starve the others or skip notify */
        }
      }
      for (const w of this.deps.db.dueWatchers(now)) {
        // Isolate per-watcher failures — including a throwing ctxFor(), which sits
        // OUTSIDE a promise .catch — so one bad watcher can't abort the whole tick
        // (which would also skip notify()).
        try {
          // Route by kind. "terminal" watchers run the small-model FSM; "pr_state"
          // watchers poll forge.getPR deterministically. An unknown kind fails
          // closed to `error` (the worktree kind was removed precisely because it
          // silently rescheduled forever, implying supervision that never happened
          // — a misrouted unknown kind would do the same, or worse, run the
          // terminal check against a record with no terminal targets).
          switch (w.kind) {
            case "terminal":
              await runTerminalWatcherCheck(w, this.deps.ctxFor("watcher", w.id));
              break;
            case "pr_state":
              await runPrWatcherCheck(w, this.deps.ctxFor("watcher", w.id));
              break;
            default:
              this.deps.db.updateWatcher(w.id, {
                status: "error",
                lastCheckedAt: now,
              });
          }
        } catch {
          /* one watcher's failure must not starve the others or skip notify */
        }
      }
      this.notify();
    } finally {
      this.running = false;
    }
  }

  private notify(): void {
    const onAttention = this.onAttention;
    if (!onAttention) return;
    // Push each attention+ event exactly once: select those never notified, then
    // stamp them. This survives the dedupe path (which pins createdAt) and still
    // catches a below-threshold event that later escalates to attention+.
    const fresh = this.deps.queue.digest({
      severityAtLeast: "attention",
      notifiedIsNull: true,
      maxItems: 20,
    });
    if (fresh.length > 0) {
      // Best-effort delivery: a throwing onAttention must NOT skip markNotified, or
      // the same events would re-fire every tick forever. Mark them regardless.
      try {
        onAttention(fresh);
      } catch {
        /* delivery failed; we still mark notified to avoid a re-notify loop */
      }
      this.deps.queue.markNotified(fresh.map((e) => e.id));
    }
  }

  private async fireTimer(rec: TimerRecord, now: number): Promise<void> {
    let payload: {
      type: TimerRecord["payloadType"];
      message?: string;
      checkPrompt?: string;
      toolCall?: { toolName: string; args: unknown };
    };
    let target: EventTarget | undefined;
    try {
      payload = JSON.parse(rec.payloadJson);
      target = rec.targetJson ? JSON.parse(rec.targetJson) : undefined;
    } catch (err) {
      // A corrupt row would otherwise throw every tick and starve later timers.
      this.deps.queue.publish({
        source: "timer",
        severity: "error",
        title: rec.title,
        summary: `Disabling corrupt timer ${rec.id}: ${err instanceof Error ? err.message : String(err)}`,
      });
      this.deps.db.updateTimer(rec.id, { status: "fired", lastFiredAt: now });
      // A disabled timer can never fire again — release any scoped grants it held.
      this.deps.db.revokeGrantsByActor(rec.id, now);
      return;
    }

    // Stable across every firing of this timer (NOT keyed by runCount) so a
    // repeating timer updates one live inbox item in place instead of leaving a
    // stale open row behind on each tick. Shared by the success and catch paths.
    const dedupeKey = `timer:${rec.id}`;
    // Dispatch on the typed DB column, falling back from the JSON blob's own
    // `type`. They always agree for rows the tool wrote, but a hand-written or
    // legacy row whose JSON omits `type` would otherwise match no branch and fire
    // as a silent no-op; the column is the authoritative payload kind.
    const payloadType = payload.type ?? rec.payloadType;
    try {
      if (payloadType === "enqueue") {
        // A scheduled enqueue is a user-requested reminder. Publish at
        // "attention" so it reaches the inbox/notifier — "info" sits below the
        // surfacing threshold, which made reminders silently never appear.
        this.deps.queue.publish({
          source: "timer",
          severity: "attention",
          title: rec.title,
          summary: payload.message ?? rec.title,
          target,
          dedupeKey,
        });
      } else if (payloadType === "run_check") {
        // run_check is deprecated and no longer creatable (removed from the
        // timer.schedule tool). It never observed real state — it asked the small
        // model to judge a prompt with no terminal output, git state, or queue
        // attached, so its verdicts were pure priors. Legacy DB rows still fire:
        // surface the prompt as a plain reminder and point at watchers, which DO
        // ground their checks in read-only observations.
        const prompt = payload.checkPrompt ?? rec.title;
        this.deps.queue.publish({
          source: "timer",
          severity: "attention",
          title: rec.title,
          summary: `Reminder (run_check is deprecated — use a watcher to observe real state): ${prompt}`,
          target,
          dedupeKey,
        });
      } else if (payloadType === "call_safe_tool" && payload.toolCall) {
        const res = await this.deps.registry.dispatch(
          payload.toolCall.toolName,
          payload.toolCall.args,
          this.deps.ctxFor("timer", rec.id),
        );
        // A confirm-required tool denied to a non-interactive actor is an
        // expected, structural outcome that the registry already surfaces as a
        // low-severity event — don't also raise a timer 'error' for it.
        if (res.error?.code !== "CONFIRMATION_REQUIRED") {
          this.deps.queue.publish({
            source: "timer",
            severity: res.ok ? "info" : "error",
            title: rec.title,
            summary: res.summary,
            target,
            dedupeKey,
          });
        }
      }
    } catch (err) {
      this.deps.queue.publish({
        source: "timer",
        severity: "error",
        title: rec.title,
        summary: `Timer check failed: ${err instanceof Error ? err.message : String(err)}`,
        target,
        dedupeKey,
      });
    }

    this.reschedule(rec, now);
  }

  /** Advance a fired timer: reschedule repeats (with sleep catch-up) or finish. */
  private reschedule(rec: TimerRecord, now: number): void {
    const runCount = rec.runCount + 1;
    const repeatDone =
      !rec.repeatEveryMs ||
      (rec.maxRuns !== undefined && rec.maxRuns !== null && runCount >= rec.maxRuns) ||
      // Stop if the NEXT fire would land past the deadline (no extra fire).
      (rec.repeatUntil !== undefined &&
        rec.repeatUntil !== null &&
        now + rec.repeatEveryMs > rec.repeatUntil);

    if (repeatDone) {
      this.deps.db.updateTimer(rec.id, {
        status: rec.repeatEveryMs ? "done" : "fired",
        runCount,
        lastFiredAt: now,
      });
      // The timer is finished and will never fire again — release its grants so a
      // recycled actor id can't inherit a stale authorization.
      this.deps.db.revokeGrantsByActor(rec.id, now);
      return;
    }
    // Catch-up: schedule next fire relative to NOW, not the missed deadline, so a
    // long sleep produces a single catch-up fire rather than a storm.
    this.deps.db.updateTimer(rec.id, {
      fireAt: now + rec.repeatEveryMs!,
      runCount,
      lastFiredAt: now,
      status: "scheduled",
    });
  }
}
