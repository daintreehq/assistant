/**
 * In-process scheduler / daemon.
 *
 * Drives all autonomous work: it fires due timers and runs due terminal watchers
 * on a tick, persisting everything to SQLite so it survives restarts and performs
 * sleep catch-up. For the prototype the daemon runs inside the CLI process; the
 * architecture (state in SQLite, idempotent ticks) is ready to split into a
 * detached process later.
 */
import {
  TIMER_CHECK_SYSTEM_PROMPT,
} from "../models/prompts/index.js";
import type { ToolContext } from "../tools/types.js";
import type { ToolRegistry } from "../tools/registry.js";
import type { Db } from "../storage/db.js";
import type { Queue } from "../queue.js";
import type { ModelRouter } from "../models/router.js";
import type { EventTarget, QueueEvent, TimerRecord } from "../schemas.js";
import { runTerminalWatcherCheck } from "./watcherEngine.js";
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
        await this.fireTimer(t, now);
      }
      for (const w of this.deps.db.dueWatchers(now)) {
        // Isolate per-watcher failures — including a throwing ctxFor(), which sits
        // OUTSIDE a promise .catch — so one bad watcher can't abort the whole tick
        // (which would also skip notify()).
        try {
          if (w.kind === "terminal") {
            await runTerminalWatcherCheck(w, this.deps.ctxFor("watcher", w.id));
          } else {
            // Worktree watchers: reschedule (full git-state checks land later).
            this.deps.db.updateWatcher(w.id, { nextCheckAt: now + w.cadenceMs });
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
    try {
      if (payload.type === "enqueue") {
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
      } else if (payload.type === "run_check") {
        const summary = await this.runCheck(payload.checkPrompt ?? rec.title);
        const noChange = summary.toLowerCase().startsWith("(no change)");
        this.deps.queue.publish({
          source: "timer",
          severity: noChange ? "debug" : "attention",
          title: rec.title,
          summary,
          target,
          dedupeKey,
        });
      } else if (payload.type === "call_safe_tool" && payload.toolCall) {
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

  private async runCheck(checkPrompt: string): Promise<string> {
    try {
      const res = await this.deps.router.chat("small", {
        messages: [
          { role: "system", content: TIMER_CHECK_SYSTEM_PROMPT },
          { role: "user", content: checkPrompt },
        ],
        temperature: 0,
        maxTokens: 200,
      });
      return res.content || "(no change) check returned no content.";
    } catch (err) {
      return `Check could not run: ${err instanceof Error ? err.message : String(err)}`;
    }
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
