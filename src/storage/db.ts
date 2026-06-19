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
  AutomationGrantRecord,
  ConversationMessageRecord,
  MemoryRecord,
  MemorySource,
  QueueEvent,
  RecipeRunStateRecord,
  RecipeSelectionLogRecord,
  RunEventRecord,
  RunSummaryRecord,
  TimerRecord,
  WatcherRecord,
  WorkflowRunRecord,
  WorkflowRunStatus,
} from "../schemas.js";
import { SCHEDULER_TICK_MS } from "../watcherCadence.js";

/**
 * Optional, AND-combined filters for {@link Db.queryAudit}. Time bounds are
 * inclusive Unix-ms integers (matching the `ts` column); omit any field to leave
 * that dimension unconstrained. `limit` defaults to 200 when absent.
 */
export interface AuditFilters {
  actor?: string;
  toolName?: string;
  outcome?: string;
  tsFrom?: number;
  tsTo?: number;
  limit?: number;
}

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
  isSupervisor INTEGER NOT NULL DEFAULT 0,
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
  updatedAt INTEGER,
  notifiedAt INTEGER,
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
  resultJson TEXT,
  grantSource TEXT,
  grantId TEXT,
  runId TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log (ts);

-- Append-only event log per run (one AgentSession.send() turn). Rows are written
-- in seq order by a single-writer sink so a finished run can be replayed. The
-- UNIQUE (runId, seq) index is the DB backstop against a duplicated seq if the
-- per-run counter is ever shared across concurrent writers.
CREATE TABLE IF NOT EXISTS run_events (
  id TEXT PRIMARY KEY,
  runId TEXT NOT NULL,
  seq INTEGER NOT NULL,
  ts INTEGER NOT NULL,
  type TEXT NOT NULL,
  payload TEXT
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_run_events_run ON run_events (runId, seq);

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

CREATE TABLE IF NOT EXISTS automation_grants (
  id TEXT PRIMARY KEY,
  actorId TEXT NOT NULL,
  actorType TEXT NOT NULL,
  allowedRiskClassesJson TEXT,
  allowedToolNamesJson TEXT,
  expiresAt INTEGER NOT NULL,
  maxUses INTEGER NOT NULL,
  usesRemaining INTEGER NOT NULL,
  revokedAt INTEGER,
  createdAt INTEGER NOT NULL,
  source TEXT NOT NULL DEFAULT 'local'
);
CREATE INDEX IF NOT EXISTS idx_grants_actor ON automation_grants (actorId, revokedAt, expiresAt);

CREATE TABLE IF NOT EXISTS workflow_runs (
  id TEXT PRIMARY KEY,
  issueNumber INTEGER,
  issueUrl TEXT,
  issueTitle TEXT,
  branch TEXT,
  worktreeId TEXT,
  prNumber INTEGER,
  prUrl TEXT,
  terminalIdsJson TEXT,
  watcherIdsJson TEXT,
  queueEventIdsJson TEXT,
  status TEXT NOT NULL DEFAULT 'pending',
  nextActionJson TEXT,
  notesJson TEXT,
  createdAt INTEGER NOT NULL,
  updatedAt INTEGER NOT NULL,
  completedAt INTEGER
);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_status ON workflow_runs (status, updatedAt);

CREATE TABLE IF NOT EXISTS recipe_run_state (
  id TEXT PRIMARY KEY,
  sessionId TEXT NOT NULL,
  recipeId TEXT NOT NULL,
  currentStep INTEGER NOT NULL DEFAULT 0,
  stepsJson TEXT NOT NULL DEFAULT '[]',
  status TEXT NOT NULL DEFAULT 'active',
  startedAt INTEGER NOT NULL,
  updatedAt INTEGER NOT NULL,
  completedAt INTEGER
);
-- One run per (session, recipe): the selector caps a session at three mutually
-- exclusive recipes, so the natural key is unique and lets the tool upsert by it.
CREATE UNIQUE INDEX IF NOT EXISTS idx_recipe_run_state_key ON recipe_run_state (sessionId, recipeId);

-- Cross-session project memory. Each row is one durable fact/decision/procedure
-- scoped to this (already per-project) database. forget = soft delete (deletedAt);
-- recall/list filter deletedAt IS NULL so a dropped fact isn't re-derived blind.
CREATE TABLE IF NOT EXISTS memories (
  id TEXT PRIMARY KEY,
  content TEXT NOT NULL,
  category TEXT,
  source TEXT NOT NULL DEFAULT 'assistant',
  pinnedAt INTEGER,
  deletedAt INTEGER,
  createdAt INTEGER NOT NULL,
  updatedAt INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memories_category ON memories (category, deletedAt);
CREATE INDEX IF NOT EXISTS idx_memories_pinned ON memories (pinnedAt) WHERE pinnedAt IS NOT NULL AND deletedAt IS NULL;

-- FTS5 external-content index over memories.content for cross-session recall
-- (BM25-ranked). External content (content='memories') keeps the base table the
-- single source of truth — no duplicated text — and the triggers below keep the
-- index in lockstep with INSERT/UPDATE/DELETE. Soft-deleted rows stay indexed but
-- are filtered out by the recall JOIN's m.deletedAt IS NULL.
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
  content,
  content='memories',
  content_rowid='rowid'
);
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
  INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_ad AFTER DELETE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS memories_au AFTER UPDATE ON memories BEGIN
  INSERT INTO memories_fts(memories_fts, rowid, content) VALUES ('delete', old.rowid, old.content);
  INSERT INTO memories_fts(rowid, content) VALUES (new.rowid, new.content);
END;
`;

export interface QueueDigestOptions {
  severityAtLeast?: QueueEvent["severity"];
  maxItems?: number;
  includeResolved?: boolean;
  /** Only events that have never been pushed to the attention notifier. */
  notifiedIsNull?: boolean;
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
  "title", "goal", "targetsJson", "cadenceMs", "isSupervisor", "modelTier", "startAfterMs",
  "stopAfterMs", "stopWhenJson", "alertWhenJson", "optionsJson", "status",
  "lastClassification", "lastCheckedAt", "nextCheckAt",
]);
// `id`/`createdAt` are immutable; `updatedAt` is in the list but always forced by
// the store (never taken from a caller patch — see updateWorkflowRun).
const WORKFLOW_UPDATE_COLS: ReadonlySet<string> = new Set([
  "issueNumber", "issueUrl", "issueTitle", "branch", "worktreeId", "prNumber", "prUrl",
  "terminalIdsJson", "watcherIdsJson", "queueEventIdsJson", "status",
  "nextActionJson", "notesJson", "updatedAt", "completedAt",
]);
// `id`/`sessionId`/`recipeId`/`startedAt` are immutable; `updatedAt` is always
// forced by the store (see updateRecipeRunState).
const RECIPE_RUN_UPDATE_COLS: ReadonlySet<string> = new Set([
  "currentStep", "stepsJson", "status", "updatedAt", "completedAt",
]);

type SqlIn = string | number | bigint | null | Uint8Array;
function toSqlValue(v: unknown): SqlIn {
  if (v === undefined || v === null) return null;
  // SQLite has no boolean type; store as 0/1 so it round-trips correctly. Without
  // this, String(false) → "false" and Boolean("false") reads back as true.
  if (typeof v === "boolean") return v ? 1 : 0;
  if (typeof v === "string" || typeof v === "number" || typeof v === "bigint") return v;
  if (v instanceof Uint8Array) return v;
  return String(v);
}

/** Parse a stored JSON string-array column, tolerating null/garbage. */
function parseJsonArray(s: string | null): string[] {
  if (!s) return [];
  try {
    const v = JSON.parse(s);
    return Array.isArray(v) ? v.map(String) : [];
  } catch {
    return [];
  }
}

/**
 * Union semantics: a grant authorizes a call when the tool name is in its
 * allowed-tool-names list OR the tool's risk class is in its allowed-risk-classes
 * list. An empty/absent list simply contributes no matches on that axis.
 */
function grantAuthorizes(
  g: AutomationGrantRecord,
  toolName: string,
  riskClass: string,
): boolean {
  if (parseJsonArray(g.allowedToolNamesJson).includes(toolName)) return true;
  if (parseJsonArray(g.allowedRiskClassesJson).includes(riskClass)) return true;
  return false;
}

export class Db {
  private db: InstanceType<typeof DatabaseSync>;

  constructor(dbPath: string) {
    this.db = new DatabaseSync(dbPath);
    // 5 s — generous for a single-writer local CLI; without it SQLite fails a
    // contended lock immediately (0 ms wait) instead of retrying. Set first so
    // the retry budget also covers the WAL pragma below, which takes a write
    // lock when transitioning a fresh file. Per connection (not persisted).
    this.db.exec("PRAGMA busy_timeout = 5000;");
    this.db.exec("PRAGMA journal_mode = WAL;");
    this.db.exec("PRAGMA foreign_keys = ON;");
    this.db.exec(SCHEMA);
    this.migrate();
    this.cancelStaleWatchers();
  }

  /**
   * Watchers are session-scoped: they supervise terminals that only exist for
   * the life of the session that spawned them. Unlike timers (which legitimately
   * persist and resume), an active watcher inherited by a *new* session points
   * at terminals that are gone — it would immediately fire false alerts
   * (e.g. `terminal_exited`). So on every DB open we treat construction as a
   * fresh session boundary and discard any watcher left non-terminal by a prior
   * session, whether it shut down cleanly or was SIGKILL'd (no shutdown hook
   * required — startup is the single, reliable invalidation point).
   *
   * Order matters: revoke the stale watchers' automation grants *before*
   * flipping their status, so no grant is ever live for a cancelled watcher.
   * Grant revocation is filtered to `actorType = 'watcher'` so a timer grant is
   * never collaterally revoked. Only the non-terminal statuses are swept —
   * `condition_met`/`timeout`/`cancelled`/`error` are already terminal and may
   * back the UI's history view, so they are left untouched.
   *
   * Finally, resolve the inbox events those watchers published. The events table
   * is *not* session-scoped (no sessionId) and watcher publishes carry no TTL
   * (`ttlMs` is never set), so a prior session's `terminal_exited` alert would
   * otherwise sit open forever and resurface in the inbox on every launch —
   * reading as a stale watch that escaped this sweep, when it is really an
   * orphaned event. Cancelling the watcher rows above is not enough: the UI
   * renders the event, not the watcher. Since every watcher is now cancelled,
   * every open watcher-sourced event is by definition orphaned, so the whole set
   * is resolved at the same session boundary. Scoped to the watcher sources only
   * (`terminal_watcher`/`worktree_watcher`) so timer/system/user events — which
   * legitimately persist — are never collaterally resolved.
   *
   * Assumes a single assistant process owns the DB at a time (the foreground-only
   * daemon invariant). DatabaseSync is synchronous, so the statements run
   * without interleaving and need no transaction wrapper.
   */
  private cancelStaleWatchers(now = Date.now()): void {
    this.db
      .prepare(
        `UPDATE automation_grants SET revokedAt = ?
         WHERE actorType = 'watcher'
           AND revokedAt IS NULL
           AND actorId IN (
             SELECT id FROM watchers WHERE status IN ('active','created','paused')
           )`,
      )
      .run(now);
    this.db
      .prepare(
        "UPDATE watchers SET status = 'cancelled' WHERE status IN ('active','created','paused')",
      )
      .run();
    this.db
      .prepare(
        `UPDATE events SET resolvedAt = ?
         WHERE resolvedAt IS NULL
           AND source IN ('terminal_watcher','worktree_watcher')`,
      )
      .run(now);
  }

  /**
   * Schema migrations keyed on `PRAGMA user_version`. The base SCHEMA above is
   * the single source of truth: it uses CREATE TABLE IF NOT EXISTS to build the
   * complete current schema for a fresh database. This collection is therefore
   * reduced to a single baseline migration that simply lands user_version at 1
   * for a newly-built DB; the SCHEMA exec has already created every table,
   * column, and index. Incremental migrations will be reintroduced at release —
   * during development the DB is hard-reset (delete the local file).
   */
  private migrate(): void {
    const migrations: Array<() => void> = [
      // v1: baseline — the complete initial schema. SCHEMA (run in the
      // constructor) creates every table/column/index, so this step is a
      // marker that brings a freshly-built database up to user_version 1.
      () => {},
    ];
    const row = this.db.prepare("PRAGMA user_version").get() as
      | { user_version?: number }
      | undefined;
    const current = Number(row?.user_version ?? 0);
    for (let v = current; v < migrations.length; v++) migrations[v]();
    // PRAGMA values can't be bound as parameters; the count is an internal int.
    this.db.exec(`PRAGMA user_version = ${migrations.length}`);
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
    const isSupervisor = Boolean(rec.isSupervisor ?? false);
    const full: WatcherRecord = {
      id: rec.id ?? `wch_${randomUUID().slice(0, 8)}`,
      kind: rec.kind,
      title: rec.title,
      goal: rec.goal,
      targetsJson: rec.targetsJson,
      // A supervisor cannot be checked faster than the scheduler tick, so floor
      // its cadence to the tick — storing a sub-tick value would misrepresent
      // the actual check interval.
      cadenceMs: isSupervisor
        ? Math.max(rec.cadenceMs, SCHEDULER_TICK_MS)
        : rec.cadenceMs,
      isSupervisor,
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
        `INSERT INTO watchers (id,kind,title,goal,targetsJson,cadenceMs,isSupervisor,modelTier,startAfterMs,stopAfterMs,stopWhenJson,alertWhenJson,optionsJson,status,lastClassification,lastCheckedAt,nextCheckAt,createdAt)
         VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
      )
      .run(
        full.id,
        full.kind,
        full.title,
        full.goal,
        full.targetsJson,
        full.cadenceMs,
        full.isSupervisor ? 1 : 0,
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
    const row = this.db.prepare("SELECT * FROM watchers WHERE id = ?").get(id) as
      | Record<string, unknown>
      | undefined;
    return row ? this.rowToWatcher(row) : undefined;
  }

  listWatchers(status?: WatcherRecord["status"]): WatcherRecord[] {
    const rows = (status
      ? this.db.prepare("SELECT * FROM watchers WHERE status = ? ORDER BY createdAt").all(status)
      : this.db.prepare("SELECT * FROM watchers ORDER BY createdAt").all()) as Record<string, unknown>[];
    return rows.map((r) => this.rowToWatcher(r));
  }

  dueWatchers(now: number): WatcherRecord[] {
    const rows = this.db
      .prepare("SELECT * FROM watchers WHERE status = 'active' AND nextCheckAt <= ? ORDER BY nextCheckAt")
      .all(now) as Record<string, unknown>[];
    return rows.map((r) => this.rowToWatcher(r));
  }

  /** Coerce a raw watchers row into a WatcherRecord. SQLite stores booleans as
   * 0/1 integers, so isSupervisor must be mapped back to a real boolean. */
  private rowToWatcher(r: Record<string, unknown>): WatcherRecord {
    return { ...r, isSupervisor: Boolean(r.isSupervisor) } as unknown as WatcherRecord;
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
        // Bump recency via updatedAt and refresh TTL, but DO NOT touch createdAt.
        // The scheduler's "is this new?" check keys on createdAt, so refreshing it
        // here made a recurring deduped event look new every tick and re-notify.
        // Title/summary/severity/recommendedActions are refreshed to the latest
        // publish: now that dedupeKeys are stable across state transitions (a
        // watcher's classification / a timer's run-count no longer live in the
        // key), a frozen title would show e.g. "still working" forever while the
        // summary moved on — and stale recommendedActions (e.g. a "Focus terminal"
        // action left over from waiting_for_input) would cling to a now-completed
        // item. recommendedActions are therefore overwritten outright, clearing to
        // null when the new event carries none. Evidence is the one exception: it
        // falls back to the existing value when omitted, so a deduped watcher event
        // (e.g. a repeated completed_unverified poll) never loses its latest
        // VerificationResult and feed the conductor stale git state.
        this.db
          .prepare(
            "UPDATE events SET count = count + 1, title = ?, summary = ?, severity = ?, evidenceJson = ?, recommendedActionsJson = ?, updatedAt = ?, expiresAt = ? WHERE id = ?",
          )
          .run(
            ev.title,
            ev.summary,
            ev.severity,
            ev.evidence ? JSON.stringify(ev.evidence) : (existing.evidenceJson as string | null) ?? null,
            ev.recommendedActions ? JSON.stringify(ev.recommendedActions) : null,
            now,
            ev.expiresAt ?? null,
            id,
          );
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
        `INSERT INTO events (id,source,severity,title,summary,targetJson,evidenceJson,recommendedActionsJson,dedupeKey,createdAt,updatedAt,expiresAt,resolvedAt,count)
         VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
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
    if (opts.notifiedIsNull) where.push("notifiedAt IS NULL");
    if (opts.severityAtLeast) {
      where.push(`${SEV_CASE} >= ?`);
      params.push(SEVERITY_ORDER[opts.severityAtLeast]);
    }
    // Order by recency-of-update so a recurring (deduped) event stays near the
    // top even though its createdAt is pinned; fall back to createdAt for rows
    // migrated before updatedAt existed.
    let sql = `SELECT * FROM events WHERE ${where.join(" AND ")} ORDER BY ${SEV_CASE} DESC, COALESCE(updatedAt, createdAt) DESC`;
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

  /** Stamp notifiedAt on the given events so they are not re-notified. */
  markNotified(ids: string[], ts = Date.now()): void {
    if (ids.length === 0) return;
    const stmt = this.db.prepare("UPDATE events SET notifiedAt = ? WHERE id = ?");
    for (const id of ids) stmt.run(ts, id);
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
      updatedAt: (r.updatedAt as number) ?? (r.createdAt as number),
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
      grantSource: rec.grantSource,
      grantId: rec.grantId,
      runId: rec.runId,
    };
    this.db
      .prepare(
        `INSERT INTO audit_log (id,ts,actor,toolName,argsJson,outcome,durationMs,summary,resultJson,grantSource,grantId,runId)
         VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
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
        full.grantSource ?? null,
        full.grantId ?? null,
        full.runId ?? null,
      );
    return full;
  }

  listAudit(limit = 50): AuditRecord[] {
    return this.db
      .prepare("SELECT * FROM audit_log ORDER BY ts DESC LIMIT ?")
      .all(limit) as unknown as AuditRecord[];
  }

  /**
   * Filtered audit query backing the export feature. Every filter is optional and
   * AND-combined; an omitted filter is bound as SQL NULL so the matching
   * `(? IS NULL OR col = ?)` clause short-circuits to true. Rows are returned
   * newest-first (matching `listAudit`) and always bounded by `limit`.
   *
   * node:sqlite throws if a bound parameter is `undefined`, so every optional is
   * coerced with `?? null` before binding. The `?` placeholders that test a
   * filter for NULL and compare it are bound to the same value, hence each filter
   * appears twice in the argument list.
   */
  queryAudit(filters: AuditFilters = {}): AuditRecord[] {
    const limit = filters.limit ?? 200;
    const actor = filters.actor ?? null;
    const toolName = filters.toolName ?? null;
    const outcome = filters.outcome ?? null;
    const tsFrom = filters.tsFrom ?? null;
    const tsTo = filters.tsTo ?? null;
    return this.db
      .prepare(
        `SELECT * FROM audit_log
         WHERE (? IS NULL OR actor = ?)
           AND (? IS NULL OR toolName = ?)
           AND (? IS NULL OR outcome = ?)
           AND (? IS NULL OR ts >= ?)
           AND (? IS NULL OR ts <= ?)
         ORDER BY ts DESC
         LIMIT ?`,
      )
      .all(
        actor,
        actor,
        toolName,
        toolName,
        outcome,
        outcome,
        tsFrom,
        tsFrom,
        tsTo,
        tsTo,
        limit,
      ) as unknown as AuditRecord[];
  }

  /* --------------------------- run events -------------------------------- */

  insertRunEvent(
    rec: Omit<RunEventRecord, "id" | "ts"> & Partial<RunEventRecord>,
  ): RunEventRecord {
    const full: RunEventRecord = {
      id: rec.id ?? `rne_${randomUUID().slice(0, 8)}`,
      ts: rec.ts ?? Date.now(),
      runId: rec.runId,
      seq: rec.seq,
      type: rec.type,
      payload: rec.payload,
    };
    this.db
      .prepare(
        `INSERT INTO run_events (id,runId,seq,ts,type,payload)
         VALUES (?,?,?,?,?,?)`,
      )
      .run(full.id, full.runId, full.seq, full.ts, full.type, full.payload ?? null);
    return full;
  }

  /** All events for a run, oldest first — the replay order. */
  listRunEvents(runId: string): RunEventRecord[] {
    return this.db
      .prepare("SELECT * FROM run_events WHERE runId = ? ORDER BY seq ASC")
      .all(runId) as unknown as RunEventRecord[];
  }

  /**
   * The most recent runs, newest first — an index over `run_events` so a user can
   * discover run ids for `/explain` without already knowing one. Aggregated on the
   * fly (there is no run table); `firstTs`/`lastTs` bracket each run's lifetime.
   * Ordered by `lastTs` (most-recently-active first) so a long run that ended
   * recently isn't buried beneath one that merely started later.
   */
  listRuns(limit = 20): RunSummaryRecord[] {
    return this.db
      .prepare(
        `SELECT runId,
                MIN(ts) AS firstTs,
                MAX(ts) AS lastTs,
                COUNT(*) AS eventCount
           FROM run_events
          GROUP BY runId
          ORDER BY lastTs DESC
          LIMIT ?`,
      )
      .all(limit) as unknown as RunSummaryRecord[];
  }

  /**
   * The audit rows for one run, oldest first. `audit_log.runId` is stamped by the
   * registry on every dispatched tool call, so this is the precise tool-dispatch
   * detail for a run — cross-referenced from `tool:result` events when `/explain`
   * reconstructs a timeline. Calls that never reached dispatch (refused, unparsable)
   * leave no audit row, so a run's event log may reference more calls than this returns.
   */
  listAuditByRunId(runId: string): AuditRecord[] {
    return this.db
      .prepare("SELECT * FROM audit_log WHERE runId = ? ORDER BY ts ASC")
      .all(runId) as unknown as AuditRecord[];
  }

  /* ----------------------- automation grants ----------------------------- */

  insertGrant(
    rec: Omit<
      AutomationGrantRecord,
      "id" | "createdAt" | "usesRemaining" | "revokedAt" | "source"
    > &
      Partial<AutomationGrantRecord>,
  ): AutomationGrantRecord {
    const full: AutomationGrantRecord = {
      id: rec.id ?? `grt_${randomUUID().slice(0, 8)}`,
      actorId: rec.actorId,
      actorType: rec.actorType,
      allowedRiskClassesJson: rec.allowedRiskClassesJson ?? null,
      allowedToolNamesJson: rec.allowedToolNamesJson ?? null,
      expiresAt: rec.expiresAt,
      maxUses: rec.maxUses,
      usesRemaining: rec.usesRemaining ?? rec.maxUses,
      revokedAt: rec.revokedAt ?? null,
      createdAt: rec.createdAt ?? Date.now(),
      source: rec.source ?? "local",
    };
    this.db
      .prepare(
        `INSERT INTO automation_grants (id,actorId,actorType,allowedRiskClassesJson,allowedToolNamesJson,expiresAt,maxUses,usesRemaining,revokedAt,createdAt,source)
         VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
      )
      .run(
        full.id,
        full.actorId,
        full.actorType,
        full.allowedRiskClassesJson,
        full.allowedToolNamesJson,
        full.expiresAt,
        full.maxUses,
        full.usesRemaining,
        full.revokedAt,
        full.createdAt,
        full.source,
      );
    return full;
  }

  getGrant(id: string): AutomationGrantRecord | undefined {
    return this.db.prepare("SELECT * FROM automation_grants WHERE id = ?").get(id) as
      | unknown as AutomationGrantRecord | undefined;
  }

  /** Live grants (non-revoked, non-expired, with uses left), optionally scoped to one actor. */
  listGrants(actorId?: string, now = Date.now()): AutomationGrantRecord[] {
    const rows = actorId
      ? this.db
          .prepare(
            "SELECT * FROM automation_grants WHERE actorId = ? AND revokedAt IS NULL AND expiresAt > ? AND usesRemaining > 0 ORDER BY createdAt",
          )
          .all(actorId, now)
      : this.db
          .prepare(
            "SELECT * FROM automation_grants WHERE revokedAt IS NULL AND expiresAt > ? AND usesRemaining > 0 ORDER BY createdAt",
          )
          .all(now);
    return rows as unknown as AutomationGrantRecord[];
  }

  /**
   * Find a live grant for `actorId`/`actorType` that authorizes `toolName` (or
   * its `riskClass`) and atomically consume one use. Returns the updated grant on
   * success, or undefined when no in-scope live grant exists. The `actorType`
   * must also match so a grant minted for a timer can never be consumed by a
   * watcher that happens to share an id (and vice versa).
   *
   * The `UPDATE ... WHERE usesRemaining > 0 AND revokedAt IS NULL AND
   * expiresAt > ?` is the consume guard; in this single-threaded synchronous
   * store the follow-up read cannot interleave with another consume, so the
   * check-and-decrement is effectively atomic (same shape as resolveEvent).
   */
  consumeGrant(
    actorId: string,
    actorType: string,
    toolName: string,
    riskClass: string,
    now = Date.now(),
  ): AutomationGrantRecord | undefined {
    const stmt = this.db.prepare(
      "UPDATE automation_grants SET usesRemaining = usesRemaining - 1 WHERE id = ? AND usesRemaining > 0 AND revokedAt IS NULL AND expiresAt > ?",
    );
    for (const g of this.listGrants(actorId, now)) {
      if (g.actorType !== actorType) continue;
      if (!grantAuthorizes(g, toolName, riskClass)) continue;
      const res = stmt.run(g.id, now);
      if (Number(res.changes) > 0) return this.getGrant(g.id);
    }
    return undefined;
  }

  /** Explicitly revoke one grant by id. Returns true if it was still live. */
  revokeGrant(id: string, now = Date.now()): boolean {
    const res = this.db
      .prepare("UPDATE automation_grants SET revokedAt = ? WHERE id = ? AND revokedAt IS NULL")
      .run(now, id);
    return Number(res.changes) > 0;
  }

  /** Revoke every live grant for an actor — called on watcher/timer stop or cancel. */
  revokeGrantsByActor(actorId: string, now = Date.now()): number {
    const res = this.db
      .prepare(
        "UPDATE automation_grants SET revokedAt = ? WHERE actorId = ? AND revokedAt IS NULL",
      )
      .run(now, actorId);
    return Number(res.changes);
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

  /* -------------------------- workflow runs ------------------------------ */

  insertWorkflowRun(
    rec: Omit<WorkflowRunRecord, "id" | "status" | "createdAt" | "updatedAt"> &
      Partial<WorkflowRunRecord>,
  ): WorkflowRunRecord {
    const now = rec.createdAt ?? Date.now();
    const full: WorkflowRunRecord = {
      id: rec.id ?? `wfr_${randomUUID().slice(0, 8)}`,
      issueNumber: rec.issueNumber,
      issueUrl: rec.issueUrl,
      issueTitle: rec.issueTitle,
      branch: rec.branch,
      worktreeId: rec.worktreeId,
      prNumber: rec.prNumber,
      prUrl: rec.prUrl,
      terminalIdsJson: rec.terminalIdsJson,
      watcherIdsJson: rec.watcherIdsJson,
      queueEventIdsJson: rec.queueEventIdsJson,
      status: rec.status ?? "pending",
      nextActionJson: rec.nextActionJson,
      notesJson: rec.notesJson,
      createdAt: now,
      updatedAt: rec.updatedAt ?? now,
      completedAt: rec.completedAt,
    };
    this.db
      .prepare(
        `INSERT INTO workflow_runs (id,issueNumber,issueUrl,issueTitle,branch,worktreeId,prNumber,prUrl,terminalIdsJson,watcherIdsJson,queueEventIdsJson,status,nextActionJson,notesJson,createdAt,updatedAt,completedAt)
         VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
      )
      .run(
        full.id,
        full.issueNumber ?? null,
        full.issueUrl ?? null,
        full.issueTitle ?? null,
        full.branch ?? null,
        full.worktreeId ?? null,
        full.prNumber ?? null,
        full.prUrl ?? null,
        full.terminalIdsJson ?? null,
        full.watcherIdsJson ?? null,
        full.queueEventIdsJson ?? null,
        full.status,
        full.nextActionJson ?? null,
        full.notesJson ?? null,
        full.createdAt,
        full.updatedAt,
        full.completedAt ?? null,
      );
    return full;
  }

  getWorkflowRun(id: string): WorkflowRunRecord | undefined {
    const row = this.db
      .prepare("SELECT * FROM workflow_runs WHERE id = ?")
      .get(id) as Record<string, unknown> | undefined;
    return row ? this.rowToWorkflowRun(row) : undefined;
  }

  listWorkflowRuns(status?: WorkflowRunStatus): WorkflowRunRecord[] {
    const rows = (status
      ? this.db
          .prepare("SELECT * FROM workflow_runs WHERE status = ? ORDER BY updatedAt DESC")
          .all(status)
      : this.db
          .prepare("SELECT * FROM workflow_runs ORDER BY updatedAt DESC")
          .all()) as Record<string, unknown>[];
    return rows.map((r) => this.rowToWorkflowRun(r));
  }

  updateWorkflowRun(id: string, patch: Partial<WorkflowRunRecord>): void {
    // No-op patches must not advance recency, so only touch the row when the
    // caller actually changes an allowed column (updatedAt itself doesn't count).
    const changesSomething = Object.keys(patch).some(
      (k) => k !== "updatedAt" && WORKFLOW_UPDATE_COLS.has(k),
    );
    if (!changesSomething) return;
    // updatedAt is always advanced by the store and never taken from a caller's
    // patch, so an update can't write a stale recency value. completedAt IS
    // caller-settable (the tool layer stamps it on terminal transitions).
    const next = { ...patch, updatedAt: Date.now() };
    this.applyUpdate(
      "workflow_runs",
      WORKFLOW_UPDATE_COLS,
      id,
      next as Record<string, unknown>,
    );
  }

  /** Coerce a raw workflow_runs row into a record, mapping SQL NULL → undefined
   * for optional columns (the `*Json` fields stay raw JSON strings — the tool
   * layer deserializes them). */
  private rowToWorkflowRun(r: Record<string, unknown>): WorkflowRunRecord {
    return {
      id: r.id as string,
      issueNumber: (r.issueNumber as number) ?? undefined,
      issueUrl: (r.issueUrl as string) ?? undefined,
      issueTitle: (r.issueTitle as string) ?? undefined,
      branch: (r.branch as string) ?? undefined,
      worktreeId: (r.worktreeId as string) ?? undefined,
      prNumber: (r.prNumber as number) ?? undefined,
      prUrl: (r.prUrl as string) ?? undefined,
      terminalIdsJson: (r.terminalIdsJson as string) ?? undefined,
      watcherIdsJson: (r.watcherIdsJson as string) ?? undefined,
      queueEventIdsJson: (r.queueEventIdsJson as string) ?? undefined,
      status: r.status as WorkflowRunStatus,
      nextActionJson: (r.nextActionJson as string) ?? undefined,
      notesJson: (r.notesJson as string) ?? undefined,
      createdAt: r.createdAt as number,
      updatedAt: r.updatedAt as number,
      completedAt: (r.completedAt as number) ?? undefined,
    };
  }

  /* ------------------------ recipe run state ----------------------------- */

  insertRecipeRunState(
    rec: Omit<RecipeRunStateRecord, "id" | "status" | "startedAt" | "updatedAt"> &
      Partial<RecipeRunStateRecord>,
  ): RecipeRunStateRecord {
    const now = rec.startedAt ?? Date.now();
    const full: RecipeRunStateRecord = {
      id: rec.id ?? `rrs_${randomUUID().slice(0, 8)}`,
      sessionId: rec.sessionId,
      recipeId: rec.recipeId,
      currentStep: rec.currentStep ?? 0,
      stepsJson: rec.stepsJson ?? "[]",
      status: rec.status ?? "active",
      startedAt: now,
      updatedAt: rec.updatedAt ?? now,
      completedAt: rec.completedAt,
    };
    this.db
      .prepare(
        `INSERT INTO recipe_run_state (id,sessionId,recipeId,currentStep,stepsJson,status,startedAt,updatedAt,completedAt)
         VALUES (?,?,?,?,?,?,?,?,?)`,
      )
      .run(
        full.id,
        full.sessionId,
        full.recipeId,
        full.currentStep,
        full.stepsJson,
        full.status,
        full.startedAt,
        full.updatedAt,
        full.completedAt ?? null,
      );
    return full;
  }

  /* ----------------------------- memories -------------------------------- */

  insertMemory(
    rec: Omit<
      MemoryRecord,
      "id" | "source" | "pinnedAt" | "deletedAt" | "createdAt" | "updatedAt"
    > &
      Partial<
        Pick<
          MemoryRecord,
          "id" | "source" | "pinnedAt" | "deletedAt" | "createdAt" | "updatedAt"
        >
      >,
  ): MemoryRecord {
    const now = rec.createdAt ?? Date.now();
    const full: MemoryRecord = {
      id: rec.id ?? `mem_${randomUUID().slice(0, 8)}`,
      content: rec.content,
      category: rec.category,
      source: rec.source ?? "assistant",
      pinnedAt: rec.pinnedAt,
      deletedAt: rec.deletedAt,
      createdAt: now,
      updatedAt: rec.updatedAt ?? now,
    };
    this.db
      .prepare(
        `INSERT INTO memories (id,content,category,source,pinnedAt,deletedAt,createdAt,updatedAt)
         VALUES (?,?,?,?,?,?,?,?)`,
      )
      .run(
        full.id,
        full.content,
        full.category ?? null,
        full.source,
        full.pinnedAt ?? null,
        full.deletedAt ?? null,
        full.createdAt,
        full.updatedAt,
      );
    return full;
  }

  /** Look up the single run for a (session, recipe) pair — the natural key. */
  getRecipeRunState(
    sessionId: string,
    recipeId: string,
  ): RecipeRunStateRecord | undefined {
    const row = this.db
      .prepare(
        "SELECT * FROM recipe_run_state WHERE sessionId = ? AND recipeId = ?",
      )
      .get(sessionId, recipeId) as Record<string, unknown> | undefined;
    return row ? this.rowToRecipeRunState(row) : undefined;
  }

  /** All runs for a session (most-recently-touched first), or every run. */
  listRecipeRunStates(sessionId?: string): RecipeRunStateRecord[] {
    const rows = (sessionId
      ? this.db
          .prepare(
            "SELECT * FROM recipe_run_state WHERE sessionId = ? ORDER BY updatedAt DESC",
          )
          .all(sessionId)
      : this.db
          .prepare("SELECT * FROM recipe_run_state ORDER BY updatedAt DESC")
          .all()) as Record<string, unknown>[];
    return rows.map((r) => this.rowToRecipeRunState(r));
  }

  updateRecipeRunState(id: string, patch: Partial<RecipeRunStateRecord>): void {
    // updatedAt is always advanced by the store and never taken from a caller's
    // patch, so an update can't write a stale recency value. completedAt IS
    // caller-settable (the tool stamps it on the terminal transition).
    const next = { ...patch, updatedAt: Date.now() };
    this.applyUpdate(
      "recipe_run_state",
      RECIPE_RUN_UPDATE_COLS,
      id,
      next as Record<string, unknown>,
    );
  }

  /** Coerce a raw recipe_run_state row into a record (SQL NULL → undefined for
   * the only optional column, completedAt; stepsJson stays raw JSON). */
  private rowToRecipeRunState(r: Record<string, unknown>): RecipeRunStateRecord {
    return {
      id: r.id as string,
      sessionId: r.sessionId as string,
      recipeId: r.recipeId as string,
      currentStep: r.currentStep as number,
      stepsJson: r.stepsJson as string,
      status: r.status as RecipeRunStateRecord["status"],
      startedAt: r.startedAt as number,
      updatedAt: r.updatedAt as number,
      completedAt: (r.completedAt as number) ?? undefined,
    };
  }

  /* ----------------------------- memories -------------------------------- */

  /** Fetch one memory by id. Soft-deleted rows are hidden unless includeDeleted. */
  getMemory(id: string, opts: { includeDeleted?: boolean } = {}): MemoryRecord | undefined {
    const row = this.db.prepare("SELECT * FROM memories WHERE id = ?").get(id) as
      | Record<string, unknown>
      | undefined;
    if (!row) return undefined;
    const mem = this.rowToMemory(row);
    // Explicit null check (not falsy): a soft-delete at epoch ms 0 is still deleted.
    if (mem.deletedAt != null && !opts.includeDeleted) return undefined;
    return mem;
  }

  /** Browse memories (pinned first, then most-recently-touched). */
  listMemories(
    opts: {
      category?: string;
      pinnedOnly?: boolean;
      includeDeleted?: boolean;
      limit?: number;
    } = {},
  ): MemoryRecord[] {
    const where: string[] = [];
    const params: SqlIn[] = [];
    if (!opts.includeDeleted) where.push("deletedAt IS NULL");
    if (opts.category) {
      where.push("category = ?");
      params.push(opts.category);
    }
    if (opts.pinnedOnly) where.push("pinnedAt IS NOT NULL");
    const limit = Math.max(1, Math.min(opts.limit ?? 50, 200));
    const whereSql = where.length ? `WHERE ${where.join(" AND ")}` : "";
    // Pinned rows float to the top; within each group, most-recent first.
    const sql = `SELECT * FROM memories ${whereSql} ORDER BY (pinnedAt IS NOT NULL) DESC, COALESCE(pinnedAt, updatedAt) DESC LIMIT ?`;
    params.push(limit);
    const rows = this.db.prepare(sql).all(...params) as Record<string, unknown>[];
    return rows.map((r) => this.rowToMemory(r));
  }

  /**
   * Full-text recall, BM25-ranked (best match first), excluding soft-deleted rows.
   *
   * The user's query is wrapped as a single FTS5 quoted phrase (doubling any
   * internal `"`). This is a hard safety boundary, not a nicety: a bare FTS5
   * MATCH string is a query *expression*, so unescaped input containing `"`,
   * `(`, `*`, or a bare keyword like `OR`/`NEAR` raises a SQLite syntax error
   * (crashing the call), not an empty result. Escaping lives here — inside the
   * store — so no tool/caller can bypass it. An empty/whitespace query short-
   * circuits to `[]` because `MATCH ""` is itself a syntax error.
   */
  recallMemories(
    query: string,
    opts: { category?: string; limit?: number } = {},
  ): MemoryRecord[] {
    const trimmed = query.trim();
    if (!trimmed) return [];
    // Tokenize on whitespace and quote EACH term (doubling internal quotes), then
    // space-join. FTS5's implicit AND across terms gives keyword search — every
    // word must appear, in any order. Quoting the whole string instead would make
    // it one rigid phrase, so "vitest tsc" would only match those words adjacent
    // (and usually return nothing). Per-token quoting still neutralizes every FTS5
    // operator/punctuation char, so arbitrary input can't raise a syntax error.
    const match = trimmed
      .split(/\s+/)
      .map((tok) => `"${tok.replaceAll('"', '""')}"`)
      .join(" ");
    const where = ["m.deletedAt IS NULL", "memories_fts MATCH ?"];
    const params: SqlIn[] = [match];
    if (opts.category) {
      where.push("m.category = ?");
      params.push(opts.category);
    }
    const limit = Math.max(1, Math.min(opts.limit ?? 10, 50));
    const sql = `SELECT m.* FROM memories_fts JOIN memories m ON m.rowid = memories_fts.rowid WHERE ${where.join(
      " AND ",
    )} ORDER BY bm25(memories_fts) LIMIT ?`;
    params.push(limit);
    const rows = this.db.prepare(sql).all(...params) as Record<string, unknown>[];
    return rows.map((r) => this.rowToMemory(r));
  }

  /** Soft-delete ("forget") a memory. Returns true if a live row was stamped. */
  forgetMemory(id: string, now = Date.now()): boolean {
    const res = this.db
      .prepare(
        "UPDATE memories SET deletedAt = ?, updatedAt = ? WHERE id = ? AND deletedAt IS NULL",
      )
      .run(now, now, id);
    return Number(res.changes) > 0;
  }

  /** Pin a memory (idempotent). Returns the updated row, or undefined if absent/deleted.
   * The `pinnedAt IS NULL` guard makes re-pinning a true no-op — otherwise each
   * repeat call would rewrite pinnedAt to `now` and jump the row ahead of other
   * pinned rows in listMemories' pinnedAt-desc ordering. */
  pinMemory(id: string, now = Date.now()): MemoryRecord | undefined {
    this.db
      .prepare(
        "UPDATE memories SET pinnedAt = ?, updatedAt = ? WHERE id = ? AND deletedAt IS NULL AND pinnedAt IS NULL",
      )
      .run(now, now, id);
    return this.getMemory(id);
  }

  /** Unpin a memory (idempotent). Returns the updated row, or undefined if absent/deleted.
   * The `pinnedAt IS NOT NULL` guard avoids bumping updatedAt on an already-unpinned row. */
  unpinMemory(id: string, now = Date.now()): MemoryRecord | undefined {
    this.db
      .prepare(
        "UPDATE memories SET pinnedAt = NULL, updatedAt = ? WHERE id = ? AND deletedAt IS NULL AND pinnedAt IS NOT NULL",
      )
      .run(now, id);
    return this.getMemory(id);
  }

  /** Coerce a raw memories row, mapping SQL NULL → undefined for optional columns. */
  private rowToMemory(r: Record<string, unknown>): MemoryRecord {
    return {
      id: r.id as string,
      content: r.content as string,
      category: (r.category as string) ?? undefined,
      source: r.source as MemorySource,
      pinnedAt: (r.pinnedAt as number) ?? undefined,
      deletedAt: (r.deletedAt as number) ?? undefined,
      createdAt: r.createdAt as number,
      updatedAt: r.updatedAt as number,
    };
  }
}
