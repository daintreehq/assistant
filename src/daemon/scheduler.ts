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

export interface SchedulerDeps {
  db: Db;
  queue: Queue;
  router: ModelRouter;
  registry: ToolRegistry;
  ctxFor: (actor: ToolContext["actor"]) => ToolContext;
  tickMs?: number;
  /** Called with newly-created attention+ events after each tick. */
  onAttention?: (events: QueueEvent[]) => void;
}

export class Scheduler {
  private timer?: NodeJS.Timeout;
  private running = false;
  private current?: Promise<void>;
  private readonly tickMs: number;

  constructor(private deps: SchedulerDeps) {
    this.tickMs = deps.tickMs ?? 5000;
  }

  start(): void {
    if (this.timer) return;
    this.timer = setInterval(() => {
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
        if (w.kind === "terminal") {
          await runTerminalWatcherCheck(w, this.deps.ctxFor("watcher")).catch(
            () => {},
          );
        } else {
          // Worktree watchers: reschedule (full git-state checks land in a later phase).
          this.deps.db.updateWatcher(w.id, { nextCheckAt: now + w.cadenceMs });
        }
      }
      this.notify();
    } finally {
      this.running = false;
    }
  }

  private notify(): void {
    if (!this.deps.onAttention) return;
    // Push each attention+ event exactly once: select those never notified, then
    // stamp them. This survives the dedupe path (which pins createdAt) and still
    // catches a below-threshold event that later escalates to attention+.
    const fresh = this.deps.queue.digest({
      severityAtLeast: "attention",
      notifiedIsNull: true,
      maxItems: 20,
    });
    if (fresh.length > 0) {
      this.deps.onAttention(fresh);
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
      return;
    }

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
          dedupeKey: `timer:${rec.id}:${rec.runCount}`,
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
          dedupeKey: `timer:${rec.id}:${rec.runCount}`,
        });
      } else if (payload.type === "call_safe_tool" && payload.toolCall) {
        const res = await this.deps.registry.dispatch(
          payload.toolCall.toolName,
          payload.toolCall.args,
          this.deps.ctxFor("timer"),
        );
        this.deps.queue.publish({
          source: "timer",
          severity: res.ok ? "info" : "error",
          title: rec.title,
          summary: res.summary,
          target,
          dedupeKey: `timer:${rec.id}:${rec.runCount}`,
        });
      }
    } catch (err) {
      this.deps.queue.publish({
        source: "timer",
        severity: "error",
        title: rec.title,
        summary: `Timer check failed: ${err instanceof Error ? err.message : String(err)}`,
        target,
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
