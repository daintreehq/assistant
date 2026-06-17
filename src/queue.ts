/**
 * The attention queue. All sub-threads (timers, watchers, workflows) report to
 * the main thread through here instead of interrupting it directly. Events are
 * deduplicated by dedupeKey and surfaced via the /inbox digest.
 */
import type { Db, QueueDigestOptions } from "./storage/db.js";
import { QueuePublishArgs, type QueueEvent } from "./schemas.js";

export class Queue {
  constructor(private db: Db) {}

  publish(args: QueuePublishArgs): QueueEvent {
    const parsed = QueuePublishArgs.parse(args);
    const expiresAt = parsed.ttlMs ? Date.now() + parsed.ttlMs : undefined;
    return this.db.upsertEvent({
      source: parsed.source,
      severity: parsed.severity,
      title: parsed.title,
      summary: parsed.summary,
      target: parsed.target,
      evidence: parsed.evidence,
      recommendedActions: parsed.recommendedActions,
      dedupeKey: parsed.dedupeKey,
      expiresAt,
    });
  }

  digest(opts: QueueDigestOptions = {}): QueueEvent[] {
    return this.db.listEvents(opts);
  }

  resolve(id: string): boolean {
    return this.db.resolveEvent(id);
  }

  /** Mark events as already pushed to the attention notifier. */
  markNotified(ids: string[]): void {
    this.db.markNotified(ids);
  }

  /** Compact, human-readable digest for /inbox and the main model context. */
  format(events: QueueEvent[]): string {
    if (events.length === 0) return "Inbox is empty.";
    const icon: Record<QueueEvent["severity"], string> = {
      debug: "·",
      info: "ℹ",
      done: "✓",
      attention: "!",
      blocked: "⛔",
      urgent: "‼",
      error: "✗",
    };
    return events
      .map((e) => {
        const t = e.target?.terminalId
          ? ` [term ${e.target.terminalId}]`
          : e.target?.worktreeId
            ? ` [wt ${e.target.worktreeId}]`
            : "";
        const dup = e.count > 1 ? ` (×${e.count})` : "";
        const ev = e.evidence?.length ? `\n     evidence: ${e.evidence.join(" | ")}` : "";
        return `  ${icon[e.severity]} ${e.id} ${e.title}${t}${dup}\n     ${e.summary}${ev}`;
      })
      .join("\n");
  }
}
