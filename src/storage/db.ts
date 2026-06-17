/**
 * SQLite-backed durable state for the CLI daemon.
 *
 * Uses Node's built-in `node:sqlite` (Node 22+) — no native build step. The store
 * is append-heavy and auditable; every autonomous action carries an idempotency /
 * dedupe key. Pass ":memory:" as the path in tests.
 */
import { createRequire } from "node:module";
import { randomUUID } from "node:crypto";

// Loaded via createRequire so the bundler (esbuild) leaves the `node:sqlite`
// specifier intact — it otherwise strips the `node:` prefix off builtins it
// doesn't yet recognise, producing an unresolvable `import "sqlite"`.
const { DatabaseSync } = createRequire(import.meta.url)("node:sqlite") as {
  DatabaseSync: typeof import("node:sqlite").DatabaseSync;
};
import type {
  AuditRecord,
  ConversationMessageRecord,
  QueueEvent,
  RecipeSelectionLogRecord,
  TimerRecord,
  WatcherRecord,
} from "../schemas.js";

const SCHEMA = `
CREATE TABLE IF NOT EXISTS timers (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL,
  fireAt INTEGER NOT NULL,
  repeatEveryMs INTEGER,
  repeatUntil INTEGER,
  maxRuns INTEGER,
  runCount INTEGER NOT NULL DEFAULT 0,
  payloadType TEXT NOT NULL,
  payloadJson TEXT NOT NULL,
  targetJson TEXT,
  status TEXT NOT NULL DEFAULT 'scheduled',
  createdAt INTEGER NOT NULL,
  lastFiredAt INTEGER
);

CREATE TABLE IF NOT EXISTS watchers (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  title TEXT NOT NULL,
  goal TEXT NOT NULL,
  targetsJson TEXT NOT NULL,
  cadenceMs INTEGER NOT NULL,
  modelTier TEXT NOT NULL,
  startAfterMs INTEGER,
  stopAfterMs INTEGER,
  stopWhenJson TEXT,
  alertWhenJson TEXT,
  optionsJson TEXT,
  status TEXT NOT NULL DEFAULT 'created',
  lastClassification TEXT,
  lastCheckedAt INTEGER,
  nextCheckAt INTEGER NOT NULL,
  createdAt INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS events (
  id TEXT PRIMARY KEY,
  source TEXT NOT NULL,
  severity TEXT NOT NULL,
  title TEXT NOT NULL,
  summary TEXT NOT NULL,
  targetJson TEXT,
  evidenceJson TEXT,
  recommendedActionsJson TEXT,
  dedupeKey TEXT,
  createdAt INTEGER NOT NULL,
  expiresAt INTEGER,
  resolvedAt INTEGER,
  count INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_events_open ON events (resolvedAt, severity, createdAt);
CREATE INDEX IF NOT EXISTS idx_events_dedupe ON events (dedupeKey, resolvedAt);

CREATE TABLE IF NOT EXISTS audit_log (
  id TEXT PRIMARY KEY,
  ts INTEGER NOT NULL,
  actor TEXT NOT NULL,
  toolName TEXT NOT NULL,
  argsJson TEXT NOT NULL,
  outcome TEXT NOT NULL,
  durationMs INTEGER NOT NULL,
  summary TEXT NOT NULL,
  resultJson TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log (ts);

CREATE TABLE IF NOT EXISTS conversation (
  id TEXT PRIMARY KEY,
  sessionId TEXT NOT NULL,
  seq INTEGER NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL,
  toolCallsJson TEXT,
  toolCallId TEXT,
  createdAt INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conv_session ON conversation (sessionId, seq);

CREATE TABLE IF NOT EXISTS recipe_selection_log (
  id TEXT PRIMARY KEY,
  ts INTEGER NOT NULL,
  sessionId TEXT NOT NULL,
  userInput TEXT NOT NULL,
  selectedRecipeIdsJson TEXT NOT NULL,
  confidence REAL NOT NULL,
  taskType TEXT,
  reason TEXT
);
CREATE INDEX IF NOT EXISTS idx_recipe_sel_ts ON recipe_selection_log (ts);
`;

export interface QueueDigestOptions {
  severityAtLeast?: QueueEvent["severity"];
  maxItems?: number;
  includeResolved?: boolean;
}

const SEVERITY_ORDER: Record<QueueEvent["severity"], number> = {
  debug: 0,
  info: 1,
  done: 2,
  attention: 3,
  blocked: 4,
  urgent: 5,
  error: 6,
};

/** SQL expression mirroring SEVERITY_ORDER, for filtering/ordering before LIMIT. */
const SEV_CASE =
  "CASE severity WHEN 'debug' THEN 0 WHEN 'info' THEN 1 WHEN 'done' THEN 2 WHEN 'attention' THEN 3 WHEN 'blocked' THEN 4 WHEN 'urgent' THEN 5 WHEN 'error' THEN 6 ELSE 1 END";

/** Column allowlists for the dynamic UPDATE builders — keys are interpolated into
 * SQL, so only known columns may pass (prevents identifier injection). */
const TIMER_UPDATE_COLS: ReadonlySet<string> = new Set([
  "title", "fireAt", "repeatEveryMs", "repeatUntil", "maxRuns", "runCount",
  "payloadType", "payloadJson", "targetJson", "status", "lastFiredAt",
]);
const WATCHER_UPDATE_COLS: ReadonlySet<string> = new Set([
  "title", "goal", "targetsJson", "cadenceMs", "modelTier", "startAfterMs",
  "stopAfterMs", "stopWhenJson", "alertWhenJson", "optionsJson", "status",
  "lastClassification", "lastCheckedAt", "nextCheckAt",
]);

type SqlIn = string | number | bigint | null | Uint8Array;
function toSqlValue(v: unknown): SqlIn {
  if (v === undefined || v === null) return null;
  if (typeof v === "string" || typeof v === "number" || typeof v === "bigint") return v;
  if (v instanceof Uint8Array) return v;
  return String(v);
}

export class Db {
  private db: InstanceType<typeof DatabaseSync>;

  constructor(dbPath: string) {
    this.db = new DatabaseSync(dbPath);
    this.db.exec("PRAGMA journal_mode = WAL;");
    this.db.exec("PRAGMA foreign_keys = ON;");
    this.db.exec(SCHEMA);
  }

  close(): void {
    this.db.close();
  }

  /** Escape hatch for advanced queries / migrations. */
  raw(): InstanceType<typeof DatabaseSync> {
    return this.db;
  }

  /** Dynamic UPDATE with a column allowlist + value coercion. */
  private applyUpdate(
    table: string,
    allowed: ReadonlySet<string>,
    id: string,
    patch: Record<string, unknown>,
  ): void {
    const keys = Object.keys(patch).filter((k) => allowed.has(k));
    if (keys.length === 0) return;
    const sets = keys.map((k) => `${k} = ?`).join(", ");
    const vals = keys.map((k) => toSqlValue(patch[k]));
    this.db.prepare(`UPDATE ${table} SET ${sets} WHERE id = ?`).run(...vals, id);
  }

  /* ----------------------------- timers ---------------------------------- */

  insertTimer(rec: Omit<TimerRecord, "id" | "createdAt" | "runCount" | "status"> & Partial<TimerRecord>): TimerRecord {
    const full: TimerRecord = {
      id: rec.id ?? `tmr_${randomUUID().slice(0, 8)}`,
      title: rec.title,
      fireAt: rec.fireAt,
      repeatEveryMs: rec.repeatEveryMs,
      repeatUntil: rec.repeatUntil,
      maxRuns: rec.maxRuns,
      runCount: rec.runCount ?? 0,
      payloadType: rec.payloadType,
      payloadJson: rec.payloadJson,
      targetJson: rec.targetJson,
      status: rec.status ?? "scheduled",
      createdAt: rec.createdAt ?? Date.now(),
      lastFiredAt: rec.lastFiredAt,
    };
    this.db
      .prepare(
        `INSERT INTO timers (id,title,fireAt,repeatEveryMs,repeatUntil,maxRuns,runCount,payloadType,payloadJson,targetJson,status,createdAt,lastFiredAt)
         VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
      )
      .run(
        full.id,
        full.title,
        full.fireAt,
        full.repeatEveryMs ?? null,
        full.repeatUntil ?? null,
        full.maxRuns ?? null,
        full.runCount,
        full.payloadType,
        full.payloadJson,
        full.targetJson ?? null,
        full.status,
        full.createdAt,
        full.lastFiredAt ?? null,
      );
    return full;
  }

  getTimer(id: string): TimerRecord | undefined {
    return this.db.prepare("SELECT * FROM timers WHERE id = ?").get(id) as
      | unknown as TimerRecord | undefined;
  }

  listTimers(status?: TimerRecord["status"]): TimerRecord[] {
    const rows = status
      ? this.db.prepare("SELECT * FROM timers WHERE status = ? ORDER BY fireAt").all(status)
      : this.db.prepare("SELECT * FROM timers ORDER BY fireAt").all();
    return rows as unknown as TimerRecord[];
  }

  dueTimers(now: number): TimerRecord[] {
    return this.db
      .prepare("SELECT * FROM timers WHERE status = 'scheduled' AND fireAt <= ? ORDER BY fireAt")
      .all(now) as unknown as TimerRecord[];
  }

  updateTimer(id: string, patch: Partial<TimerRecord>): void {
    this.applyUpdate("timers", TIMER_UPDATE_COLS, id, patch as Record<string, unknown>);
  }

  /* ---------------------------- watchers --------------------------------- */

  insertWatcher(rec: Omit<WatcherRecord, "id" | "createdAt" | "status"> & Partial<WatcherRecord>): WatcherRecord {
    const full: WatcherRecord = {
      id: rec.id ?? `wch_${randomUUID().slice(0, 8)}`,
      kind: rec.kind,
      title: rec.title,
      goal: rec.goal,
      targetsJson: rec.targetsJson,
      cadenceMs: rec.cadenceMs,
      modelTier: rec.modelTier,
      startAfterMs: rec.startAfterMs,
      stopAfterMs: rec.stopAfterMs,
      stopWhenJson: rec.stopWhenJson,
      alertWhenJson: rec.alertWhenJson,
      optionsJson: rec.optionsJson,
      status: rec.status ?? "active",
      lastClassification: rec.lastClassification,
      lastCheckedAt: rec.lastCheckedAt,
      nextCheckAt: rec.nextCheckAt,
      createdAt: rec.createdAt ?? Date.now(),
    };
    this.db
      .prepare(
        `INSERT INTO watchers (id,kind,title,goal,targetsJson,cadenceMs,modelTier,startAfterMs,stopAfterMs,stopWhenJson,alertWhenJson,optionsJson,status,lastClassification,lastCheckedAt,nextCheckAt,createdAt)
         VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
      )
      .run(
        full.id,
        full.kind,
        full.title,
        full.goal,
        full.targetsJson,
        full.cadenceMs,
        full.modelTier,
        full.startAfterMs ?? null,
        full.stopAfterMs ?? null,
        full.stopWhenJson ?? null,
        full.alertWhenJson ?? null,
        full.optionsJson ?? null,
        full.status,
        full.lastClassification ?? null,
        full.lastCheckedAt ?? null,
        full.nextCheckAt,
        full.createdAt,
      );
    return full;
  }

  getWatcher(id: string): WatcherRecord | undefined {
    return this.db.prepare("SELECT * FROM watchers WHERE id = ?").get(id) as
      | unknown as WatcherRecord | undefined;
  }

  listWatchers(status?: WatcherRecord["status"]): WatcherRecord[] {
    const rows = status
      ? this.db.prepare("SELECT * FROM watchers WHERE status = ? ORDER BY createdAt").all(status)
      : this.db.prepare("SELECT * FROM watchers ORDER BY createdAt").all();
    return rows as unknown as WatcherRecord[];
  }

  dueWatchers(now: number): WatcherRecord[] {
    return this.db
      .prepare("SELECT * FROM watchers WHERE status = 'active' AND nextCheckAt <= ? ORDER BY nextCheckAt")
      .all(now) as unknown as WatcherRecord[];
  }

  updateWatcher(id: string, patch: Partial<WatcherRecord>): void {
    this.applyUpdate("watchers", WATCHER_UPDATE_COLS, id, patch as Record<string, unknown>);
  }

  /* ----------------------------- events ---------------------------------- */

  /** Insert an event, or bump the count of an existing open event with the same dedupeKey. */
  upsertEvent(ev: Omit<QueueEvent, "id" | "createdAt" | "count"> & Partial<QueueEvent>): QueueEvent {
    const now = ev.createdAt ?? Date.now();
    if (ev.dedupeKey) {
      const existing = this.db
        .prepare(
          "SELECT * FROM events WHERE dedupeKey = ? AND resolvedAt IS NULL AND (expiresAt IS NULL OR expiresAt > ?) ORDER BY createdAt DESC LIMIT 1",
        )
        .get(ev.dedupeKey, now) as Record<string, unknown> | undefined;
      if (existing) {
        const id = existing.id as string;
        // Refresh TTL on bump so an active, recurring event stays visible.
        this.db
          .prepare(
            "UPDATE events SET count = count + 1, summary = ?, severity = ?, createdAt = ?, expiresAt = ? WHERE id = ?",
          )
          .run(ev.summary, ev.severity, now, ev.expiresAt ?? null, id);
        return this.getEvent(id)!;
      }
    }
    const full: QueueEvent = {
      id: ev.id ?? `evt_${randomUUID().slice(0, 8)}`,
      source: ev.source,
      severity: ev.severity,
      title: ev.title,
      summary: ev.summary,
      target: ev.target,
      evidence: ev.evidence,
      recommendedActions: ev.recommendedActions,
      dedupeKey: ev.dedupeKey,
      createdAt: now,
      expiresAt: ev.expiresAt,
      resolvedAt: ev.resolvedAt,
      count: ev.count ?? 1,
    };
    this.db
      .prepare(
        `INSERT INTO events (id,source,severity,title,summary,targetJson,evidenceJson,recommendedActionsJson,dedupeKey,createdAt,expiresAt,resolvedAt,count)
         VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
      )
      .run(
        full.id,
        full.source,
        full.severity,
        full.title,
        full.summary,
        full.target ? JSON.stringify(full.target) : null,
        full.evidence ? JSON.stringify(full.evidence) : null,
        full.recommendedActions ? JSON.stringify(full.recommendedActions) : null,
        full.dedupeKey ?? null,
        full.createdAt,
        full.expiresAt ?? null,
        full.resolvedAt ?? null,
        full.count,
      );
    return full;
  }

  getEvent(id: string): QueueEvent | undefined {
    const row = this.db.prepare("SELECT * FROM events WHERE id = ?").get(id) as
      | Record<string, unknown>
      | undefined;
    return row ? this.rowToEvent(row) : undefined;
  }

  listEvents(opts: QueueDigestOptions = {}): QueueEvent[] {
    const now = Date.now();
    const where: string[] = ["(expiresAt IS NULL OR expiresAt > ?)"];
    const params: unknown[] = [now];
    if (!opts.includeResolved) where.push("resolvedAt IS NULL");
    if (opts.severityAtLeast) {
      where.push(`${SEV_CASE} >= ?`);
      params.push(SEVERITY_ORDER[opts.severityAtLeast]);
    }
    let sql = `SELECT * FROM events WHERE ${where.join(" AND ")} ORDER BY ${SEV_CASE} DESC, createdAt DESC`;
    if (opts.maxItems) {
      sql += " LIMIT ?";
      params.push(opts.maxItems);
    }
    const rows = this.db.prepare(sql).all(...(params as never[])) as Record<
      string,
      unknown
    >[];
    return rows.map((r) => this.rowToEvent(r));
  }

  resolveEvent(id: string): boolean {
    const res = this.db
      .prepare("UPDATE events SET resolvedAt = ? WHERE id = ? AND resolvedAt IS NULL")
      .run(Date.now(), id);
    return Number(res.changes) > 0;
  }

  private rowToEvent(r: Record<string, unknown>): QueueEvent {
    return {
      id: r.id as string,
      source: r.source as QueueEvent["source"],
      severity: r.severity as QueueEvent["severity"],
      title: r.title as string,
      summary: r.summary as string,
      target: r.targetJson ? JSON.parse(r.targetJson as string) : undefined,
      evidence: r.evidenceJson ? JSON.parse(r.evidenceJson as string) : undefined,
      recommendedActions: r.recommendedActionsJson
        ? JSON.parse(r.recommendedActionsJson as string)
        : undefined,
      dedupeKey: (r.dedupeKey as string) ?? undefined,
      createdAt: r.createdAt as number,
      expiresAt: (r.expiresAt as number) ?? undefined,
      resolvedAt: (r.resolvedAt as number) ?? undefined,
      count: r.count as number,
    };
  }

  /* ----------------------------- audit ----------------------------------- */

  insertAudit(rec: Omit<AuditRecord, "id" | "ts"> & Partial<AuditRecord>): AuditRecord {
    const full: AuditRecord = {
      id: rec.id ?? `aud_${randomUUID().slice(0, 8)}`,
      ts: rec.ts ?? Date.now(),
      actor: rec.actor,
      toolName: rec.toolName,
      argsJson: rec.argsJson,
      outcome: rec.outcome,
      durationMs: rec.durationMs,
      summary: rec.summary,
      resultJson: rec.resultJson,
    };
    this.db
      .prepare(
        `INSERT INTO audit_log (id,ts,actor,toolName,argsJson,outcome,durationMs,summary,resultJson)
         VALUES (?,?,?,?,?,?,?,?,?)`,
      )
      .run(
        full.id,
        full.ts,
        full.actor,
        full.toolName,
        full.argsJson,
        full.outcome,
        full.durationMs,
        full.summary,
        full.resultJson ?? null,
      );
    return full;
  }

  listAudit(limit = 50): AuditRecord[] {
    return this.db
      .prepare("SELECT * FROM audit_log ORDER BY ts DESC LIMIT ?")
      .all(limit) as unknown as AuditRecord[];
  }

  /* -------------------------- conversation ------------------------------- */

  insertMessage(rec: Omit<ConversationMessageRecord, "id" | "createdAt"> & Partial<ConversationMessageRecord>): ConversationMessageRecord {
    const full: ConversationMessageRecord = {
      id: rec.id ?? `msg_${randomUUID().slice(0, 8)}`,
      sessionId: rec.sessionId,
      seq: rec.seq,
      role: rec.role,
      content: rec.content,
      toolCallsJson: rec.toolCallsJson,
      toolCallId: rec.toolCallId,
      createdAt: rec.createdAt ?? Date.now(),
    };
    this.db
      .prepare(
        `INSERT INTO conversation (id,sessionId,seq,role,content,toolCallsJson,toolCallId,createdAt)
         VALUES (?,?,?,?,?,?,?,?)`,
      )
      .run(
        full.id,
        full.sessionId,
        full.seq,
        full.role,
        full.content,
        full.toolCallsJson ?? null,
        full.toolCallId ?? null,
        full.createdAt,
      );
    return full;
  }

  listMessages(sessionId: string): ConversationMessageRecord[] {
    return this.db
      .prepare("SELECT * FROM conversation WHERE sessionId = ? ORDER BY seq")
      .all(sessionId) as unknown as ConversationMessageRecord[];
  }

  /* ----------------------- recipe selection log -------------------------- */

  insertRecipeSelection(
    rec: Omit<RecipeSelectionLogRecord, "id" | "ts"> & Partial<RecipeSelectionLogRecord>,
  ): RecipeSelectionLogRecord {
    const full: RecipeSelectionLogRecord = {
      id: rec.id ?? `rsl_${randomUUID().slice(0, 8)}`,
      ts: rec.ts ?? Date.now(),
      sessionId: rec.sessionId,
      userInput: rec.userInput,
      selectedRecipeIdsJson: rec.selectedRecipeIdsJson,
      confidence: rec.confidence,
      taskType: rec.taskType,
      reason: rec.reason,
    };
    this.db
      .prepare(
        `INSERT INTO recipe_selection_log (id,ts,sessionId,userInput,selectedRecipeIdsJson,confidence,taskType,reason)
         VALUES (?,?,?,?,?,?,?,?)`,
      )
      .run(
        full.id,
        full.ts,
        full.sessionId,
        full.userInput,
        full.selectedRecipeIdsJson,
        full.confidence,
        full.taskType ?? null,
        full.reason ?? null,
      );
    return full;
  }

  listRecipeSelections(limit = 50): RecipeSelectionLogRecord[] {
    return this.db
      .prepare("SELECT * FROM recipe_selection_log ORDER BY ts DESC LIMIT ?")
      .all(limit) as unknown as RecipeSelectionLogRecord[];
  }
}
