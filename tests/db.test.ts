import { mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { Db, DEFAULT_RETENTION } from "../src/storage/db.js";
import type { RetentionPolicy } from "../src/storage/db.js";

describe("Db fresh schema (single baseline migration)", () => {
  let dir: string;
  let path: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "db-fresh-"));
    path = join(dir, "state.db");
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it("builds the complete schema and lands user_version at 1", () => {
    // A fresh DB file gets the entire current schema from the SCHEMA constant;
    // the single baseline migration simply stamps user_version = 1.
    const db = new Db(path);
    const raw = db.raw();

    const colNames = (table: string) =>
      (raw.prepare(`PRAGMA table_info(${table})`).all() as Array<{
        name: string;
      }>).map((c) => c.name);
    const indexNames = (table: string) =>
      (raw.prepare(`PRAGMA index_list(${table})`).all() as Array<{
        name: string;
      }>).map((i) => i.name);

    // Columns folded in from the former incremental migrations.
    expect(colNames("events")).toEqual(
      expect.arrayContaining(["updatedAt", "notifiedAt"]),
    );
    expect(colNames("watchers")).toContain("isSupervisor");
    expect(colNames("automation_grants")).toContain("source");
    expect(colNames("audit_log")).toEqual(
      expect.arrayContaining(["grantSource", "grantId", "runId"]),
    );

    // run_events table + its unique (runId, seq) index exist on a fresh DB.
    expect(colNames("run_events")).toEqual(
      expect.arrayContaining(["id", "runId", "seq", "ts", "type", "payload"]),
    );
    expect(indexNames("run_events")).toContain("idx_run_events_run");
    // Retention-sweep support indexes (issue #145): a plain ts index on
    // run_events and a plain createdAt index on conversation so the age-cutoff
    // scans are indexed rather than full-table.
    expect(indexNames("run_events")).toContain("idx_run_events_ts");
    expect(indexNames("conversation")).toContain("idx_conv_createdat");

    // workflow_runs table + its index exist on a fresh DB.
    expect(colNames("workflow_runs")).toEqual(
      expect.arrayContaining([
        "id",
        "issueNumber",
        "terminalIdsJson",
        "watcherIdsJson",
        "queueEventIdsJson",
        "status",
        "nextActionJson",
        "notesJson",
        "createdAt",
        "updatedAt",
        "completedAt",
      ]),
    );
    expect(indexNames("workflow_runs")).toContain("idx_workflow_runs_status");

    // skill_run_state table + its unique natural-key index exist on a fresh DB.
    expect(colNames("skill_run_state")).toEqual(
      expect.arrayContaining([
        "id",
        "sessionId",
        "skillId",
        "currentStep",
        "stepsJson",
        "status",
        "startedAt",
        "updatedAt",
        "completedAt",
      ]),
    );
    expect(indexNames("skill_run_state")).toContain(
      "idx_skill_run_state_key",
    );

    // agent_launches table + its idempotency-key index exist on a fresh DB.
    expect(colNames("agent_launches")).toEqual(
      expect.arrayContaining([
        "id",
        "idempotencyKey",
        "agentId",
        "worktreeId",
        "mode",
        "title",
        "name",
        "terminalId",
        "watcherId",
        "stage",
        "errorCode",
        "errorMessage",
        "createdAt",
        "updatedAt",
      ]),
    );
    expect(indexNames("agent_launches")).toContain("idx_agent_launches_key");

    // memories table + its FTS5 index and triggers exist on a fresh DB.
    expect(colNames("memories")).toEqual(
      expect.arrayContaining([
        "id",
        "content",
        "category",
        "source",
        "pinnedAt",
        "deletedAt",
        "createdAt",
        "updatedAt",
      ]),
    );
    const tableNames = (
      raw
        .prepare("SELECT name FROM sqlite_master WHERE type IN ('table','trigger')")
        .all() as Array<{ name: string }>
    ).map((t) => t.name);
    expect(tableNames).toEqual(
      expect.arrayContaining([
        "memories_fts",
        "memories_ai",
        "memories_au",
        "memories_ad",
      ]),
    );

    const version = raw.prepare("PRAGMA user_version").get() as {
      user_version: number;
    };
    expect(version.user_version).toBe(1);

    // A fresh grant defaults its source to 'local' (the column default backfill
    // behaviour now lives entirely in SCHEMA).
    const grant = db.insertGrant({
      actorId: "wch_fresh",
      actorType: "watcher",
      allowedRiskClassesJson: JSON.stringify(["git"]),
      allowedToolNamesJson: null,
      expiresAt: 9999999999999,
      maxUses: 1,
    });
    expect(db.getGrant(grant.id)?.source).toBe("local");

    db.close();
  });
});

describe("Db startup watcher invalidation", () => {
  let dir: string;
  let path: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "db-watcher-invalidate-"));
    path = join(dir, "state.db");
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  const insertWatcherWithStatus = (
    db: Db,
    id: string,
    status:
      | "active"
      | "created"
      | "paused"
      | "condition_met"
      | "timeout"
      | "cancelled"
      | "error",
  ) =>
    db.insertWatcher({
      id,
      kind: "terminal",
      title: `watcher ${id}`,
      goal: "supervise",
      targetsJson: JSON.stringify(["term_1"]),
      cadenceMs: 5000,
      modelTier: "small",
      nextCheckAt: 0,
      status,
    });

  it("cancels non-terminal watchers from a prior session and revokes their grants on reopen", () => {
    // Watchers are session-scoped: a new session must never inherit a prior
    // session's watchers (their terminals are gone). The terminal statuses
    // (condition_met/timeout/cancelled/error) back the UI history and must
    // survive untouched.
    const nonTerminal = ["active", "created", "paused"] as const;
    const terminal = ["condition_met", "timeout", "cancelled", "error"] as const;

    {
      const db = new Db(path);

      for (const s of nonTerminal) insertWatcherWithStatus(db, `wch_${s}`, s);
      for (const s of terminal) insertWatcherWithStatus(db, `wch_${s}`, s);

      // A live grant for EACH stale (non-terminal) watcher — all must be
      // revoked, so the subquery is exercised across active/created/paused, not
      // just the one status.
      for (const s of nonTerminal) {
        db.insertGrant({
          id: `grt_${s}`,
          actorId: `wch_${s}`,
          actorType: "watcher",
          allowedRiskClassesJson: JSON.stringify(["terminal"]),
          allowedToolNamesJson: null,
          expiresAt: 9999999999999,
          maxUses: 5,
        });
      }
      // A live grant for a terminal-state (condition_met) watcher — that watcher
      // is NOT swept, so its grant must survive (the subquery is bounded to
      // non-terminal statuses).
      db.insertGrant({
        id: "grt_terminal_state",
        actorId: "wch_condition_met",
        actorType: "watcher",
        allowedRiskClassesJson: JSON.stringify(["terminal"]),
        allowedToolNamesJson: null,
        expiresAt: 9999999999999,
        maxUses: 5,
      });
      // A live timer grant that shares the same actorId — must NOT be revoked,
      // proving the sweep is scoped by actorType.
      db.insertGrant({
        id: "grt_timer",
        actorId: "wch_active",
        actorType: "timer",
        allowedRiskClassesJson: JSON.stringify(["terminal"]),
        allowedToolNamesJson: null,
        expiresAt: 9999999999999,
        maxUses: 5,
      });
      // A persistent timer — must survive the reopen untouched.
      db.insertTimer({
        id: "tmr_keep",
        title: "keep me",
        fireAt: 9999999999999,
        payloadType: "enqueue",
        payloadJson: "{}",
      });

      db.close();
    }

    // Reopen on the same file: construction runs cancelStaleWatchers().
    const db = new Db(path);

    // Non-terminal watchers are now cancelled...
    for (const s of nonTerminal) {
      expect(db.getWatcher(`wch_${s}`)?.status).toBe("cancelled");
    }
    // ...terminal-state watchers are untouched.
    for (const s of terminal) {
      expect(db.getWatcher(`wch_${s}`)?.status).toBe(s);
    }

    // Nothing is due — the scheduler sees no inherited watchers.
    expect(db.dueWatchers(Date.now())).toHaveLength(0);

    // Every stale watcher's grant is revoked (active/created/paused)...
    for (const s of nonTerminal) {
      expect(db.getGrant(`grt_${s}`)?.revokedAt).toBeTruthy();
    }
    // ...the terminal-state watcher's grant survives (not swept)...
    expect(db.getGrant("grt_terminal_state")?.revokedAt).toBeFalsy();
    // ...and the timer grant (same actorId as wch_active) is untouched.
    expect(db.getGrant("grt_timer")?.revokedAt).toBeFalsy();

    // The persistent timer is untouched.
    expect(db.getTimer("tmr_keep")?.status).toBe("scheduled");

    db.close();
  });

  it("cancels a prior session's pr_state watcher and revokes its grant on reopen", () => {
    // PR watchers are session-scoped like terminal ones — the kind-agnostic row
    // sweep must cancel them and revoke their grants, never carry them over.
    {
      const db = new Db(path);
      db.insertWatcher({
        id: "wch_pr",
        kind: "pr_state",
        title: "PR #5",
        goal: "watch pr",
        targetsJson: JSON.stringify(["PR #5"]),
        cadenceMs: 60_000,
        modelTier: "small",
        optionsJson: JSON.stringify({ prNumber: 5, lastState: "open" }),
        nextCheckAt: 0,
        status: "active",
      });
      db.insertGrant({
        id: "grt_pr",
        actorId: "wch_pr",
        actorType: "watcher",
        allowedRiskClassesJson: JSON.stringify(["read"]),
        allowedToolNamesJson: null,
        expiresAt: 9999999999999,
        maxUses: 5,
      });
      db.close();
    }

    const db = new Db(path);
    expect(db.getWatcher("wch_pr")?.status).toBe("cancelled");
    expect(db.getGrant("grt_pr")?.revokedAt).toBeTruthy();
    expect(db.dueWatchers(Date.now())).toHaveLength(0);
    db.close();
  });

  it("resolves open watcher-sourced inbox events on reopen, sparing other sources", () => {
    // The events table is not session-scoped and watcher publishes carry no TTL,
    // so a prior session's watcher alert would otherwise resurface in the inbox
    // on every launch (reading as a stale watch that escaped the sweep). Both
    // watcher sources are swept; timer/system/user events legitimately persist.
    let resolvedWatcherAt: number | undefined;
    {
      const db = new Db(path);
      db.upsertEvent({
        source: "terminal_watcher",
        severity: "attention",
        title: "Claude: terminal exited",
        summary: "Terminal is no longer reported by Daintree (closed or removed).",
        dedupeKey: "watcher:wch_old:term_1",
      });
      db.upsertEvent({
        source: "worktree_watcher",
        severity: "attention",
        title: "worktree gone",
        summary: "Worktree is no longer present.",
      });
      // PR watchers are session-scoped too — a leftover "PR updated" alert from a
      // prior session must be swept, or it resurfaces in every new inbox.
      db.upsertEvent({
        source: "pr_watcher",
        severity: "attention",
        title: "PR #7 merged",
        summary: "PR #7 is merged.",
        dedupeKey: "pr_watcher:wch_old:state_change",
      });
      // Non-watcher sources must survive the session boundary.
      db.upsertEvent({
        source: "timer",
        severity: "info",
        title: "timer fired",
        summary: "A scheduled timer fired.",
      });
      db.upsertEvent({
        source: "system",
        severity: "info",
        title: "system note",
        summary: "A system event.",
      });
      // An already-resolved watcher event must stay resolved, not be re-stamped:
      // the sweep is guarded on `resolvedAt IS NULL`, so its original timestamp
      // must survive the reopen unchanged.
      const resolved = db.upsertEvent({
        source: "terminal_watcher",
        severity: "done",
        title: "earlier alert",
        summary: "Already handled.",
      });
      db.resolveEvent(resolved.id);
      resolvedWatcherAt = db.getEvent(resolved.id)!.resolvedAt;
      db.close();
    }

    const db = new Db(path);

    // Open watcher events are gone from the default (unresolved) digest...
    const open = db.listEvents();
    const openSources = open.map((e) => e.source);
    expect(openSources).not.toContain("terminal_watcher");
    expect(openSources).not.toContain("worktree_watcher");
    expect(openSources).not.toContain("pr_watcher");
    // ...while timer and system events are untouched.
    expect(openSources).toContain("timer");
    expect(openSources).toContain("system");

    // The watcher events are resolved, not deleted: they remain retrievable for
    // the UI history view, now stamped with a resolvedAt.
    const all = db.listEvents({ includeResolved: true });
    const sweptWatcherEvents = all.filter(
      (e) =>
        (e.source === "terminal_watcher" ||
          e.source === "worktree_watcher" ||
          e.source === "pr_watcher") &&
        e.title !== "earlier alert",
    );
    expect(sweptWatcherEvents.length).toBe(3);
    for (const e of sweptWatcherEvents) expect(e.resolvedAt).toBeTruthy();

    // The already-resolved watcher event keeps its ORIGINAL resolvedAt — the
    // `resolvedAt IS NULL` guard means the sweep never re-stamps it.
    const earlier = all.find((e) => e.title === "earlier alert");
    expect(earlier?.resolvedAt).toBe(resolvedWatcherAt);

    db.close();
  });
});

describe("Db", () => {
  let db: Db;

  beforeEach(() => {
    db = new Db(":memory:");
  });

  afterEach(() => {
    db.close();
  });

  describe("connection pragmas", () => {
    // Readback key is `timeout`, not `busy_timeout` (node:sqlite quirk).
    const readTimeout = (d: Db) =>
      (d.raw().prepare("PRAGMA busy_timeout").get() as { timeout: number })
        .timeout;

    it("sets busy_timeout to 5000ms on the connection", () => {
      expect(readTimeout(db)).toBe(5000);
    });

    it("sets busy_timeout on every connection to a file DB (survives reopen)", () => {
      // A persisted file means the WAL pragma is a no-op on reopen, but
      // busy_timeout is per-connection and must be re-applied each time.
      const dir = mkdtempSync(join(tmpdir(), "db-busy-"));
      const path = join(dir, "state.db");
      const first = new Db(path);
      expect(readTimeout(first)).toBe(5000);
      first.close();
      const second = new Db(path);
      expect(readTimeout(second)).toBe(5000);
      second.close();
      rmSync(dir, { recursive: true, force: true });
    });
  });

  describe("timers", () => {
    it("dueTimers returns only scheduled timers with fireAt <= now", () => {
      const now = 1_000_000;
      const due = db.insertTimer({
        title: "due",
        fireAt: now - 1,
        payloadType: "enqueue",
        payloadJson: "{}",
      });
      const exactly = db.insertTimer({
        title: "exactly now",
        fireAt: now,
        payloadType: "enqueue",
        payloadJson: "{}",
      });
      // Future timer — should not be returned.
      db.insertTimer({
        title: "future",
        fireAt: now + 1,
        payloadType: "enqueue",
        payloadJson: "{}",
      });
      // Past but already fired — wrong status, should not be returned.
      db.insertTimer({
        title: "already fired",
        fireAt: now - 100,
        payloadType: "enqueue",
        payloadJson: "{}",
        status: "fired",
      });

      const result = db.dueTimers(now);
      const ids = result.map((t) => t.id).sort();
      expect(ids).toEqual([due.id, exactly.id].sort());
      expect(result.every((t) => t.status === "scheduled")).toBe(true);
      expect(result.every((t) => t.fireAt <= now)).toBe(true);
    });
  });

  describe("watchers", () => {
    it("dueWatchers returns only active watchers with nextCheckAt <= now", () => {
      const now = 2_000_000;
      const base = {
        kind: "terminal" as const,
        title: "w",
        goal: "g",
        targetsJson: "[]",
        cadenceMs: 1000,
        modelTier: "small" as const,
      };
      const due = db.insertWatcher({ ...base, nextCheckAt: now - 1 });
      const exactly = db.insertWatcher({ ...base, nextCheckAt: now });
      // Future check time — not due.
      db.insertWatcher({ ...base, nextCheckAt: now + 1 });
      // Past check time but not active — excluded.
      db.insertWatcher({ ...base, nextCheckAt: now - 100, status: "paused" });

      const result = db.dueWatchers(now);
      const ids = result.map((w) => w.id).sort();
      expect(ids).toEqual([due.id, exactly.id].sort());
      expect(result.every((w) => w.status === "active")).toBe(true);
      expect(result.every((w) => w.nextCheckAt <= now)).toBe(true);
    });

    const base = {
      kind: "terminal" as const,
      title: "w",
      goal: "g",
      targetsJson: "[]",
      modelTier: "small" as const,
      nextCheckAt: 0,
    };

    it("floors a supervisor cadence to the scheduler tick", () => {
      const w = db.insertWatcher({
        ...base,
        cadenceMs: 1000,
        isSupervisor: true,
      });
      // 1000ms is below the 3000ms scheduler tick — clamped up.
      expect(w.cadenceMs).toBe(3000);
      expect(w.isSupervisor).toBe(true);
    });

    it("leaves a supervisor cadence at or above the tick untouched", () => {
      const w = db.insertWatcher({
        ...base,
        cadenceMs: 10_000,
        isSupervisor: true,
      });
      expect(w.cadenceMs).toBe(10_000);
    });

    it("does not floor a non-supervisor (monitor) cadence", () => {
      const w = db.insertWatcher({
        ...base,
        cadenceMs: 1000,
        isSupervisor: false,
      });
      expect(w.cadenceMs).toBe(1000);
      expect(w.isSupervisor).toBe(false);
    });

    it("defaults isSupervisor to false when omitted", () => {
      const w = db.insertWatcher({ ...base, cadenceMs: 1000 });
      expect(w.isSupervisor).toBe(false);
    });

    it("round-trips isSupervisor as a boolean through getWatcher", () => {
      const w = db.insertWatcher({
        ...base,
        cadenceMs: 5000,
        isSupervisor: true,
      });
      const fetched = db.getWatcher(w.id);
      // SQLite stores 0/1; the read path must coerce back to a real boolean.
      expect(fetched?.isSupervisor).toBe(true);
      expect(db.listWatchers()[0].isSupervisor).toBe(true);
    });

    it("stores a boolean update as 0/1, not the string 'false'", () => {
      const w = db.insertWatcher({
        ...base,
        cadenceMs: 5000,
        isSupervisor: true,
      });
      db.updateWatcher(w.id, { isSupervisor: false });
      // Without boolean handling in toSqlValue, String(false) → "false" and
      // Boolean("false") reads back as true.
      expect(db.getWatcher(w.id)?.isSupervisor).toBe(false);
    });
  });

  describe("events", () => {
    it("upsertEvent with the same dedupeKey bumps count to 2", () => {
      const first = db.upsertEvent({
        source: "watcher",
        severity: "attention",
        title: "first",
        summary: "first summary",
        dedupeKey: "dup-1",
      });
      expect(first.count).toBe(1);

      const second = db.upsertEvent({
        source: "watcher",
        severity: "urgent",
        title: "refreshed on bump",
        summary: "second summary",
        dedupeKey: "dup-1",
      });
      expect(second.id).toBe(first.id);
      expect(second.count).toBe(2);
      // The bump refreshes title + summary + severity in place so a stable
      // dedupeKey's live item tracks the latest state instead of freezing.
      expect(second.title).toBe("refreshed on bump");
      expect(second.summary).toBe("second summary");
      expect(second.severity).toBe("urgent");

      // Only one row exists.
      expect(db.listEvents().length).toBe(1);
    });

    it("resolveEvent hides the event from listEvents() by default", () => {
      const ev = db.upsertEvent({
        source: "watcher",
        severity: "attention",
        title: "to resolve",
        summary: "s",
      });
      expect(db.listEvents().map((e) => e.id)).toContain(ev.id);

      const changed = db.resolveEvent(ev.id);
      expect(changed).toBe(true);

      expect(db.listEvents().map((e) => e.id)).not.toContain(ev.id);
      // But visible when includeResolved is set.
      expect(
        db.listEvents({ includeResolved: true }).map((e) => e.id),
      ).toContain(ev.id);
    });

    it("listEvents sorts higher severity first", () => {
      const now = 5_000_000;
      const info = db.upsertEvent({
        source: "watcher",
        severity: "info",
        title: "info",
        summary: "s",
        createdAt: now + 100,
      });
      const error = db.upsertEvent({
        source: "watcher",
        severity: "error",
        title: "error",
        summary: "s",
        createdAt: now,
      });
      const attention = db.upsertEvent({
        source: "watcher",
        severity: "attention",
        title: "attention",
        summary: "s",
        createdAt: now + 50,
      });

      const ordered = db.listEvents().map((e) => e.id);
      // error (6) > attention (3) > info (1), regardless of createdAt.
      expect(ordered).toEqual([error.id, attention.id, info.id]);
    });
  });

  describe("automation grants", () => {
    const T0 = 10_000_000;

    function grant(over: Partial<Parameters<Db["insertGrant"]>[0]> = {}) {
      return db.insertGrant({
        actorId: "wch_abc",
        actorType: "watcher",
        allowedRiskClassesJson: JSON.stringify(["git"]),
        allowedToolNamesJson: null,
        expiresAt: T0 + 60_000,
        maxUses: 3,
        createdAt: T0,
        ...over,
      });
    }

    it("insertGrant defaults usesRemaining to maxUses and revokedAt to null", () => {
      const g = grant({ maxUses: 5 });
      const fetched = db.getGrant(g.id)!;
      expect(fetched.usesRemaining).toBe(5);
      expect(fetched.revokedAt).toBeNull();
      expect(fetched.actorId).toBe("wch_abc");
    });

    it("insertGrant defaults source to 'local' and lists it", () => {
      const g = grant();
      expect(db.getGrant(g.id)?.source).toBe("local");
      expect(db.listGrants("wch_abc", T0)[0]?.source).toBe("local");
    });

    it("listGrants returns only live grants, optionally scoped to an actor", () => {
      grant({ actorId: "wch_a" });
      grant({ actorId: "wch_b" });
      grant({ actorId: "wch_expired", expiresAt: T0 - 1 });
      grant({ actorId: "wch_used", usesRemaining: 0 });

      // Scoped lookups exclude expired/exhausted grants.
      expect(db.listGrants("wch_a", T0).map((g) => g.actorId)).toEqual(["wch_a"]);
      expect(db.listGrants("wch_expired", T0)).toHaveLength(0);
      expect(db.listGrants("wch_used", T0)).toHaveLength(0);
      // Unscoped lists every live grant.
      expect(db.listGrants(undefined, T0).map((g) => g.actorId).sort()).toEqual([
        "wch_a",
        "wch_b",
      ]);
    });

    it("consumeGrant decrements a use and returns the updated grant on a risk-class match", () => {
      const g = grant({ maxUses: 2 });
      const consumed = db.consumeGrant("wch_abc", "watcher", "git.commit", "git", T0);
      expect(consumed?.id).toBe(g.id);
      expect(consumed?.usesRemaining).toBe(1);
    });

    it("consumeGrant matches by tool name (union semantics)", () => {
      grant({
        allowedRiskClassesJson: null,
        allowedToolNamesJson: JSON.stringify(["terminal.send"]),
      });
      // The risk class is not allowed, but the exact tool name is.
      const consumed = db.consumeGrant("wch_abc", "watcher", "terminal.send", "terminal", T0);
      expect(consumed).toBeDefined();
      // A different tool of the same (un-allowed) risk class is rejected.
      expect(db.consumeGrant("wch_abc", "watcher", "terminal.other", "terminal", T0)).toBeUndefined();
    });

    it("consumeGrant returns undefined when nothing matches the scope", () => {
      grant(); // allows git only
      expect(db.consumeGrant("wch_abc", "watcher", "project.spawn", "project", T0)).toBeUndefined();
      // A wrong actor never matches.
      expect(db.consumeGrant("wch_other", "watcher", "git.commit", "git", T0)).toBeUndefined();
    });

    it("consumeGrant exhausts after maxUses and then denies", () => {
      grant({ maxUses: 2 });
      expect(db.consumeGrant("wch_abc", "watcher", "git.commit", "git", T0)?.usesRemaining).toBe(1);
      expect(db.consumeGrant("wch_abc", "watcher", "git.commit", "git", T0)?.usesRemaining).toBe(0);
      // Third call: exhausted (usesRemaining = 0 fails the WHERE guard).
      expect(db.consumeGrant("wch_abc", "watcher", "git.commit", "git", T0)).toBeUndefined();
    });

    it("consumeGrant denies an expired grant", () => {
      grant({ expiresAt: T0 + 1000 });
      // now is past expiry.
      expect(db.consumeGrant("wch_abc", "watcher", "git.commit", "git", T0 + 2000)).toBeUndefined();
    });

    it("revokeGrant prevents further consumption and is idempotent", () => {
      const g = grant();
      expect(db.revokeGrant(g.id, T0)).toBe(true);
      expect(db.consumeGrant("wch_abc", "watcher", "git.commit", "git", T0)).toBeUndefined();
      // Already revoked → no longer live.
      expect(db.revokeGrant(g.id, T0)).toBe(false);
    });

    it("revokeGrantsByActor revokes every live grant for an actor and returns the count", () => {
      grant({ actorId: "wch_x" });
      grant({ actorId: "wch_x" });
      grant({ actorId: "wch_y" });
      expect(db.revokeGrantsByActor("wch_x", T0)).toBe(2);
      expect(db.listGrants("wch_x", T0)).toHaveLength(0);
      expect(db.listGrants("wch_y", T0)).toHaveLength(1);
    });
  });

  describe("workflow runs", () => {
    it("insertWorkflowRun applies defaults (wfr_ id, pending status, timestamps)", () => {
      const rec = db.insertWorkflowRun({ issueNumber: 25 });
      expect(rec.id).toMatch(/^wfr_[0-9a-f]{8}$/);
      expect(rec.status).toBe("pending");
      expect(rec.createdAt).toBeGreaterThan(0);
      expect(rec.updatedAt).toBe(rec.createdAt);
      expect(rec.completedAt).toBeUndefined();
    });

    it("round-trips every optional JSON column through getWorkflowRun", () => {
      const rec = db.insertWorkflowRun({
        issueNumber: 25,
        issueUrl: "https://example.test/issues/25",
        issueTitle: "Add a durable workflow ledger",
        branch: "feature/issue-25",
        worktreeId: "wt_abc",
        prNumber: 99,
        prUrl: "https://example.test/pull/99",
        terminalIdsJson: JSON.stringify(["term_1", "term_2"]),
        watcherIdsJson: JSON.stringify(["wch_1"]),
        queueEventIdsJson: JSON.stringify(["evt_1"]),
        status: "active",
        nextActionJson: JSON.stringify({
          label: "Open the PR",
          toolName: "workflow.update",
        }),
        notesJson: JSON.stringify(["seeded from issue body"]),
      });
      const fetched = db.getWorkflowRun(rec.id)!;
      expect(fetched.issueNumber).toBe(25);
      expect(fetched.prNumber).toBe(99);
      expect(JSON.parse(fetched.terminalIdsJson!)).toEqual(["term_1", "term_2"]);
      expect(JSON.parse(fetched.watcherIdsJson!)).toEqual(["wch_1"]);
      expect(JSON.parse(fetched.queueEventIdsJson!)).toEqual(["evt_1"]);
      expect(JSON.parse(fetched.nextActionJson!).label).toBe("Open the PR");
      expect(JSON.parse(fetched.notesJson!)).toEqual(["seeded from issue body"]);
    });

    it("getWorkflowRun returns undefined for an unknown id", () => {
      expect(db.getWorkflowRun("wfr_missing")).toBeUndefined();
    });

    it("maps SQL NULL columns to undefined, not null", () => {
      const rec = db.insertWorkflowRun({});
      const fetched = db.getWorkflowRun(rec.id)!;
      expect(fetched.issueNumber).toBeUndefined();
      expect(fetched.terminalIdsJson).toBeUndefined();
      expect(fetched.nextActionJson).toBeUndefined();
      expect(fetched.completedAt).toBeUndefined();
    });

    it("listWorkflowRuns filters by status and returns all when unfiltered", () => {
      db.insertWorkflowRun({ issueNumber: 1, status: "active" });
      db.insertWorkflowRun({ issueNumber: 2, status: "blocked" });
      db.insertWorkflowRun({ issueNumber: 3, status: "active" });

      expect(db.listWorkflowRuns("active")).toHaveLength(2);
      expect(db.listWorkflowRuns("blocked")).toHaveLength(1);
      expect(db.listWorkflowRuns("done")).toHaveLength(0);
      expect(db.listWorkflowRuns()).toHaveLength(3);
      expect(
        db.listWorkflowRuns("active").every((r) => r.status === "active"),
      ).toBe(true);
    });

    it("updateWorkflowRun patches allowed columns and advances updatedAt", () => {
      const rec = db.insertWorkflowRun({ issueNumber: 7, createdAt: 1000 });
      db.updateWorkflowRun(rec.id, {
        status: "active",
        prNumber: 42,
        terminalIdsJson: JSON.stringify(["term_9"]),
      });
      const fetched = db.getWorkflowRun(rec.id)!;
      expect(fetched.status).toBe("active");
      expect(fetched.prNumber).toBe(42);
      expect(JSON.parse(fetched.terminalIdsJson!)).toEqual(["term_9"]);
      // updatedAt advances past the seeded createdAt; createdAt is unchanged.
      expect(fetched.updatedAt).toBeGreaterThan(1000);
      expect(fetched.createdAt).toBe(1000);
    });

    it("updateWorkflowRun does not advance updatedAt for a no-op patch", () => {
      const rec = db.insertWorkflowRun({ issueNumber: 7, createdAt: 1000 });
      const before = db.getWorkflowRun(rec.id)!.updatedAt;
      // An empty patch (and a patch of only-unknown keys) is a no-op.
      db.updateWorkflowRun(rec.id, {});
      db.updateWorkflowRun(rec.id, { bogus: "x" } as Record<string, unknown>);
      expect(db.getWorkflowRun(rec.id)!.updatedAt).toBe(before);
    });

    it("listWorkflowRuns orders by updatedAt descending", () => {
      const a = db.insertWorkflowRun({ issueNumber: 1, createdAt: 100, updatedAt: 100 });
      const b = db.insertWorkflowRun({ issueNumber: 2, createdAt: 300, updatedAt: 300 });
      const c = db.insertWorkflowRun({ issueNumber: 3, createdAt: 200, updatedAt: 200 });
      expect(db.listWorkflowRuns().map((r) => r.id)).toEqual([b.id, c.id, a.id]);
    });

    it("updateWorkflowRun ignores unknown / immutable columns", () => {
      const rec = db.insertWorkflowRun({ issueNumber: 7, createdAt: 1000 });
      db.updateWorkflowRun(rec.id, {
        id: "wfr_hacked",
        createdAt: 5,
        bogus: "nope",
      } as Record<string, unknown>);
      const fetched = db.getWorkflowRun(rec.id)!;
      expect(fetched.id).toBe(rec.id);
      expect(fetched.createdAt).toBe(1000);
    });

    it("persists completedAt and survives a close + reopen", () => {
      const dir = mkdtempSync(join(tmpdir(), "db-wf-"));
      const path = join(dir, "state.db");
      try {
        const first = new Db(path);
        const rec = first.insertWorkflowRun({ issueNumber: 11, status: "active" });
        first.updateWorkflowRun(rec.id, { status: "done", completedAt: 7777 });
        first.close();

        const second = new Db(path);
        const fetched = second.getWorkflowRun(rec.id)!;
        expect(fetched.status).toBe("done");
        expect(fetched.completedAt).toBe(7777);
        second.close();
      } finally {
        rmSync(dir, { recursive: true, force: true });
      }
    });
  });

  describe("agent launches", () => {
    it("insertAgentLaunch applies defaults (agt_ id, launch_requested stage, timestamps)", () => {
      const rec = db.insertAgentLaunch({
        idempotencyKey: "k1",
        agentId: "claude",
        mode: "edit",
        title: "Fix OAuth",
        name: "Claude: Fix OAuth",
      });
      expect(rec.id).toMatch(/^agt_[0-9a-f]{8}$/);
      expect(rec.stage).toBe("launch_requested");
      expect(rec.createdAt).toBeGreaterThan(0);
      expect(rec.updatedAt).toBe(rec.createdAt);
      expect(rec.terminalId).toBeUndefined();
    });

    it("maps SQL NULL columns to undefined, not null", () => {
      const rec = db.insertAgentLaunch({
        idempotencyKey: "k2",
        agentId: "claude",
        mode: "edit",
        title: "t",
        name: "Claude: t",
      });
      const fetched = db.getAgentLaunch(rec.id)!;
      expect(fetched.worktreeId).toBeUndefined();
      expect(fetched.terminalId).toBeUndefined();
      expect(fetched.watcherId).toBeUndefined();
      expect(fetched.errorCode).toBeUndefined();
      expect(fetched.errorMessage).toBeUndefined();
    });

    it("updateAgentLaunch advances the stage and forces updatedAt; createdAt is immutable", () => {
      const rec = db.insertAgentLaunch({
        idempotencyKey: "k3",
        agentId: "claude",
        mode: "edit",
        title: "t",
        name: "Claude: t",
        createdAt: 1000,
      });
      db.updateAgentLaunch(rec.id, { stage: "terminal_bound", terminalId: "term_9" });
      const fetched = db.getAgentLaunch(rec.id)!;
      expect(fetched.stage).toBe("terminal_bound");
      expect(fetched.terminalId).toBe("term_9");
      expect(fetched.updatedAt).toBeGreaterThan(1000);
      expect(fetched.createdAt).toBe(1000);
    });

    it("updateAgentLaunch ignores unknown / immutable columns", () => {
      const rec = db.insertAgentLaunch({
        idempotencyKey: "k4",
        agentId: "claude",
        mode: "edit",
        title: "t",
        name: "Claude: t",
        createdAt: 1000,
      });
      db.updateAgentLaunch(rec.id, {
        id: "agt_hacked",
        idempotencyKey: "rekeyed",
        createdAt: 5,
      } as Record<string, unknown>);
      const fetched = db.getAgentLaunch(rec.id)!;
      expect(fetched.id).toBe(rec.id);
      expect(fetched.idempotencyKey).toBe("k4");
      expect(fetched.createdAt).toBe(1000);
    });

    it("findActiveAgentLaunch returns the in-flight record but excludes terminal stages", () => {
      const rec = db.insertAgentLaunch({
        idempotencyKey: "dup",
        agentId: "claude",
        mode: "edit",
        title: "t",
        name: "Claude: t",
      });
      // In-flight (ambiguous) → found.
      db.updateAgentLaunch(rec.id, { stage: "ambiguous" });
      expect(db.findActiveAgentLaunch("dup")?.id).toBe(rec.id);
      // Confirmed → terminal, no longer blocks a fresh launch of the same task.
      db.updateAgentLaunch(rec.id, { stage: "confirmed" });
      expect(db.findActiveAgentLaunch("dup")).toBeUndefined();
      // Failed is likewise terminal.
      const rec2 = db.insertAgentLaunch({
        idempotencyKey: "dup2",
        agentId: "claude",
        mode: "edit",
        title: "t",
        name: "Claude: t",
      });
      db.updateAgentLaunch(rec2.id, { stage: "failed" });
      expect(db.findActiveAgentLaunch("dup2")).toBeUndefined();
    });

    it("findActiveAgentLaunch returns the most recently touched in-flight record", () => {
      const a = db.insertAgentLaunch({
        idempotencyKey: "same",
        agentId: "claude",
        mode: "edit",
        title: "t",
        name: "Claude: t",
        createdAt: 100,
        updatedAt: 100,
      });
      const b = db.insertAgentLaunch({
        idempotencyKey: "same",
        agentId: "claude",
        mode: "edit",
        title: "t",
        name: "Claude: t",
        createdAt: 200,
        updatedAt: 200,
      });
      expect(db.findActiveAgentLaunch("same")?.id).toBe(b.id);
      void a;
    });

    it("cancelStaleAgentLaunches retires non-terminal records from a prior session on reopen", () => {
      const dir = mkdtempSync(join(tmpdir(), "db-agt-"));
      const path = join(dir, "state.db");
      try {
        const first = new Db(path);
        const inflight = first.insertAgentLaunch({
          idempotencyKey: "stale",
          agentId: "claude",
          mode: "edit",
          title: "t",
          name: "Claude: t",
        });
        first.updateAgentLaunch(inflight.id, { stage: "ambiguous" });
        const done = first.insertAgentLaunch({
          idempotencyKey: "done",
          agentId: "claude",
          mode: "edit",
          title: "t",
          name: "Claude: t",
        });
        first.updateAgentLaunch(done.id, { stage: "confirmed" });
        first.close();

        // New session: the in-flight row is marked failed (session-scoped), so its
        // key no longer blocks a fresh launch; the confirmed row is untouched.
        const second = new Db(path);
        expect(second.findActiveAgentLaunch("stale")).toBeUndefined();
        const fetched = second.getAgentLaunch(inflight.id)!;
        expect(fetched.stage).toBe("failed");
        expect(fetched.errorCode).toBe("SESSION_ENDED");
        expect(second.getAgentLaunch(done.id)!.stage).toBe("confirmed");
        second.close();
      } finally {
        rmSync(dir, { recursive: true, force: true });
      }
    });
  });

  describe("skill run state", () => {
    it("insertSkillRunState applies defaults (rrs_ id, active status, timestamps)", () => {
      const rec = db.insertSkillRunState({
        sessionId: "ses_a",
        skillId: "daintree.orchestration.basic",
      });
      expect(rec.id).toMatch(/^rrs_[0-9a-f]{8}$/);
      expect(rec.status).toBe("active");
      expect(rec.currentStep).toBe(0);
      expect(rec.stepsJson).toBe("[]");
      expect(rec.startedAt).toBeGreaterThan(0);
      expect(rec.updatedAt).toBe(rec.startedAt);
      expect(rec.completedAt).toBeUndefined();
    });

    it("getSkillRunState looks up by the (session, skill) natural key", () => {
      db.insertSkillRunState({ sessionId: "ses_a", skillId: "r.one" });
      db.insertSkillRunState({ sessionId: "ses_a", skillId: "r.two" });
      db.insertSkillRunState({ sessionId: "ses_b", skillId: "r.one" });

      const found = db.getSkillRunState("ses_a", "r.two")!;
      expect(found.skillId).toBe("r.two");
      expect(found.sessionId).toBe("ses_a");
      expect(db.getSkillRunState("ses_missing", "r.one")).toBeUndefined();
    });

    it("round-trips the stepsJson checkpoint array", () => {
      const steps = [
        { index: 1, status: "done", notes: "cloned repo", ts: 111 },
        { index: 2, status: "skipped", ts: 222 },
      ];
      const rec = db.insertSkillRunState({
        sessionId: "ses_a",
        skillId: "r.steps",
        currentStep: 3,
        stepsJson: JSON.stringify(steps),
      });
      const fetched = db.getSkillRunState("ses_a", "r.steps")!;
      expect(fetched.currentStep).toBe(3);
      expect(JSON.parse(fetched.stepsJson)).toEqual(steps);
      expect(rec.id).toBe(fetched.id);
    });

    it("the (session, skill) index is unique — duplicate insert throws", () => {
      db.insertSkillRunState({ sessionId: "ses_a", skillId: "r.dup" });
      expect(() =>
        db.insertSkillRunState({ sessionId: "ses_a", skillId: "r.dup" }),
      ).toThrow();
    });

    it("updateSkillRunState patches allowed columns and advances updatedAt", () => {
      const rec = db.insertSkillRunState({
        sessionId: "ses_a",
        skillId: "r.upd",
        startedAt: 1000,
      });
      db.updateSkillRunState(rec.id, {
        currentStep: 2,
        stepsJson: JSON.stringify([{ index: 1, status: "done", ts: 500 }]),
        status: "active",
      });
      const fetched = db.getSkillRunState("ses_a", "r.upd")!;
      expect(fetched.currentStep).toBe(2);
      expect(JSON.parse(fetched.stepsJson)).toHaveLength(1);
      // updatedAt advances past the seeded startedAt; startedAt is unchanged.
      expect(fetched.updatedAt).toBeGreaterThan(1000);
      expect(fetched.startedAt).toBe(1000);
    });

    it("updateSkillRunState ignores unknown / immutable columns", () => {
      const rec = db.insertSkillRunState({
        sessionId: "ses_a",
        skillId: "r.imm",
        startedAt: 1000,
      });
      db.updateSkillRunState(rec.id, {
        id: "rrs_hacked",
        sessionId: "ses_other",
        skillId: "r.other",
        startedAt: 5,
        bogus: "nope",
      } as Record<string, unknown>);
      const fetched = db.getSkillRunState("ses_a", "r.imm")!;
      expect(fetched.id).toBe(rec.id);
      expect(fetched.sessionId).toBe("ses_a");
      expect(fetched.skillId).toBe("r.imm");
      expect(fetched.startedAt).toBe(1000);
    });

    it("listSkillRunStates filters by session and orders by updatedAt desc", () => {
      db.insertSkillRunState({
        sessionId: "ses_a",
        skillId: "r.1",
        startedAt: 100,
        updatedAt: 100,
      });
      db.insertSkillRunState({
        sessionId: "ses_a",
        skillId: "r.2",
        startedAt: 300,
        updatedAt: 300,
      });
      db.insertSkillRunState({
        sessionId: "ses_b",
        skillId: "r.1",
        startedAt: 200,
        updatedAt: 200,
      });
      expect(db.listSkillRunStates("ses_a").map((r) => r.skillId)).toEqual([
        "r.2",
        "r.1",
      ]);
      expect(db.listSkillRunStates()).toHaveLength(3);
    });

    it("maps SQL NULL completedAt to undefined and persists across reopen", () => {
      const dir = mkdtempSync(join(tmpdir(), "db-rrs-"));
      const path = join(dir, "state.db");
      try {
        const first = new Db(path);
        const rec = first.insertSkillRunState({
          sessionId: "ses_a",
          skillId: "r.persist",
        });
        expect(first.getSkillRunState("ses_a", "r.persist")!.completedAt).toBeUndefined();
        first.updateSkillRunState(rec.id, { status: "completed", completedAt: 8888 });
        first.close();

        const second = new Db(path);
        const fetched = second.getSkillRunState("ses_a", "r.persist")!;
        expect(fetched.status).toBe("completed");
        expect(fetched.completedAt).toBe(8888);
        second.close();
      } finally {
        rmSync(dir, { recursive: true, force: true });
      }
    });
  });

  describe("audit runId", () => {
    it("stamps and round-trips runId on an audit row", () => {
      const row = db.insertAudit({
        actor: "main",
        toolName: "fs.read",
        argsJson: "{}",
        outcome: "ok",
        durationMs: 5,
        summary: "ok",
        runId: "run_abc123",
      });
      expect(row.runId).toBe("run_abc123");
      expect(db.listAudit().find((r) => r.id === row.id)?.runId).toBe("run_abc123");
    });

    it("maps an absent runId to undefined (e.g. the scheduler path)", () => {
      const row = db.insertAudit({
        actor: "timer",
        toolName: "fs.read",
        argsJson: "{}",
        outcome: "ok",
        durationMs: 5,
        summary: "ok",
      });
      expect(db.listAudit().find((r) => r.id === row.id)?.runId ?? undefined).toBeUndefined();
    });
  });

  describe("run events", () => {
    it("insertRunEvent applies defaults (rne_ id, ts) and round-trips", () => {
      const rec = db.insertRunEvent({
        runId: "run_1",
        seq: 0,
        type: "assistant:start",
      });
      expect(rec.id).toMatch(/^rne_[0-9a-f]{8}$/);
      expect(rec.ts).toBeGreaterThan(0);
      const fetched = db.listRunEvents("run_1");
      expect(fetched).toHaveLength(1);
      expect(fetched[0].type).toBe("assistant:start");
      expect(fetched[0].payload ?? undefined).toBeUndefined();
    });

    it("preserves a JSON payload verbatim", () => {
      db.insertRunEvent({
        runId: "run_1",
        seq: 0,
        type: "tool:call",
        payload: JSON.stringify({ name: "fs.read", id: "c1" }),
      });
      const fetched = db.listRunEvents("run_1");
      expect(JSON.parse(fetched[0].payload!)).toEqual({ name: "fs.read", id: "c1" });
    });

    it("listRunEvents returns rows oldest-first by seq, scoped to one run", () => {
      db.insertRunEvent({ runId: "run_a", seq: 2, type: "assistant:end" });
      db.insertRunEvent({ runId: "run_a", seq: 0, type: "assistant:start" });
      db.insertRunEvent({ runId: "run_a", seq: 1, type: "tool:call" });
      db.insertRunEvent({ runId: "run_b", seq: 0, type: "assistant:start" });

      const a = db.listRunEvents("run_a");
      expect(a.map((e) => e.seq)).toEqual([0, 1, 2]);
      expect(a.map((e) => e.type)).toEqual([
        "assistant:start",
        "tool:call",
        "assistant:end",
      ]);
      // A separate run's events never leak into another run's log.
      expect(db.listRunEvents("run_b")).toHaveLength(1);
      expect(db.listRunEvents("run_missing")).toEqual([]);
    });

    it("enforces the UNIQUE (runId, seq) backstop against a duplicated seq", () => {
      db.insertRunEvent({ runId: "run_dup", seq: 0, type: "assistant:start" });
      expect(() =>
        db.insertRunEvent({ runId: "run_dup", seq: 0, type: "assistant:end" }),
      ).toThrow();
    });

    it("listRuns aggregates per run, newest-first, with first/last/count", () => {
      expect(db.listRuns()).toEqual([]);
      db.insertRunEvent({ runId: "run_old", seq: 0, type: "assistant:start", ts: 1000 });
      db.insertRunEvent({ runId: "run_old", seq: 1, type: "assistant:end", ts: 1500 });
      db.insertRunEvent({ runId: "run_new", seq: 0, type: "assistant:start", ts: 2000 });

      const runs = db.listRuns();
      expect(runs.map((r) => r.runId)).toEqual(["run_new", "run_old"]); // lastTs DESC
      const old = runs.find((r) => r.runId === "run_old")!;
      expect(old.firstTs).toBe(1000);
      expect(old.lastTs).toBe(1500);
      expect(old.eventCount).toBe(2);
    });

    it("listRuns honors the limit", () => {
      for (let i = 0; i < 5; i++) {
        db.insertRunEvent({ runId: `run_${i}`, seq: 0, type: "assistant:start", ts: 1000 + i });
      }
      expect(db.listRuns(2)).toHaveLength(2);
    });

    it("listAuditByRunId returns a run's audit rows oldest-first, scoped to the run", () => {
      db.insertAudit({ ts: 200, actor: "main", toolName: "fs.read", argsJson: "{}", outcome: "ok", durationMs: 1, summary: "b", runId: "run_x" });
      db.insertAudit({ ts: 100, actor: "main", toolName: "fs.list", argsJson: "{}", outcome: "ok", durationMs: 1, summary: "a", runId: "run_x" });
      db.insertAudit({ ts: 150, actor: "main", toolName: "git.commit", argsJson: "{}", outcome: "ok", durationMs: 1, summary: "c", runId: "run_y" });

      const rows = db.listAuditByRunId("run_x");
      expect(rows.map((r) => r.toolName)).toEqual(["fs.list", "fs.read"]); // ts ASC
      expect(db.listAuditByRunId("run_y").map((r) => r.toolName)).toEqual(["git.commit"]);
      expect(db.listAuditByRunId("run_none")).toEqual([]);
    });
  });

  describe("memories", () => {
    it("insertMemory + getMemory round-trips with defaults", () => {
      const rec = db.insertMemory({ content: "deploy from main only" });
      expect(rec.id).toMatch(/^mem_[0-9a-f]{8}$/);
      expect(rec.source).toBe("assistant");
      const got = db.getMemory(rec.id)!;
      expect(got.content).toBe("deploy from main only");
      expect(got.pinnedAt).toBeUndefined();
      expect(got.deletedAt).toBeUndefined();
    });

    it("recallMemories returns BM25-ranked matches and excludes soft-deleted", () => {
      db.insertMemory({ content: "the CI pipeline runs vitest and tsc" });
      const drop = db.insertMemory({ content: "vitest config pins the debug log off" });
      let hits = db.recallMemories("vitest");
      expect(hits.length).toBe(2);
      db.forgetMemory(drop.id);
      hits = db.recallMemories("vitest");
      expect(hits.map((m) => m.id)).not.toContain(drop.id);
      expect(hits.length).toBe(1);
    });

    it("recallMemories matches multi-word queries as AND-of-terms (not a rigid phrase)", () => {
      const rec = db.insertMemory({ content: "the CI pipeline runs vitest and tsc" });
      // Words present but non-adjacent — must still match (regression: whole-query
      // phrase quoting returned []).
      expect(db.recallMemories("vitest tsc").map((m) => m.id)).toContain(rec.id);
      // A term absent from the row excludes it (AND semantics).
      expect(db.recallMemories("vitest playwright")).toEqual([]);
    });

    it("recallMemories filters by category", () => {
      db.insertMemory({ content: "use NodeNext imports", category: "convention" });
      db.insertMemory({ content: "NodeNext is also fine here", category: "note" });
      const hits = db.recallMemories("NodeNext", { category: "convention" });
      expect(hits.length).toBe(1);
      expect(hits[0].category).toBe("convention");
    });

    it("recallMemories does not throw on FTS operator / quote injection", () => {
      db.insertMemory({ content: "watch out for quotes and operators" });
      for (const q of ['"', 'a "b" c', "watch OR operators", "near NEAR(x)", "watch*", "(unbalanced"]) {
        expect(() => db.recallMemories(q)).not.toThrow();
      }
    });

    it("recallMemories returns [] for a blank/whitespace query", () => {
      db.insertMemory({ content: "something" });
      expect(db.recallMemories("")).toEqual([]);
      expect(db.recallMemories("   ")).toEqual([]);
    });

    it("listMemories honors pinnedOnly, category, soft-delete, and limit", () => {
      const a = db.insertMemory({ content: "alpha", category: "x" });
      db.insertMemory({ content: "beta", category: "y" });
      const gone = db.insertMemory({ content: "gamma", category: "x" });
      db.forgetMemory(gone.id);
      db.pinMemory(a.id);

      expect(db.listMemories({ pinnedOnly: true }).map((m) => m.id)).toEqual([a.id]);
      expect(db.listMemories({ category: "x" }).map((m) => m.id)).toEqual([a.id]);
      expect(db.listMemories().length).toBe(2); // gone is excluded
      expect(db.listMemories({ includeDeleted: true }).length).toBe(3);
      expect(db.listMemories({ limit: 1 }).length).toBe(1);
    });

    it("listMemories floats pinned rows to the top", () => {
      db.insertMemory({ content: "first" });
      const second = db.insertMemory({ content: "second" });
      db.pinMemory(second.id);
      expect(db.listMemories()[0].id).toBe(second.id);
    });

    it("re-pinning is a true no-op and keeps pinned ordering stable", () => {
      const a = db.insertMemory({ content: "alpha" });
      const b = db.insertMemory({ content: "beta" });
      db.pinMemory(a.id, 100);
      db.pinMemory(b.id, 200);
      // Most-recently-pinned first.
      expect(db.listMemories().map((m) => m.id)).toEqual([b.id, a.id]);
      // Re-pinning A must NOT rewrite its pinnedAt and jump it ahead of B.
      db.pinMemory(a.id, 300);
      expect(db.listMemories().map((m) => m.id)).toEqual([b.id, a.id]);
    });

    it("recall survives a pin/unpin update cycle (AFTER UPDATE trigger keeps FTS in sync)", () => {
      const rec = db.insertMemory({ content: "uniqueterm about deployment" });
      expect(db.recallMemories("uniqueterm").map((m) => m.id)).toContain(rec.id);
      db.pinMemory(rec.id);
      db.unpinMemory(rec.id);
      expect(db.recallMemories("uniqueterm").map((m) => m.id)).toContain(rec.id);
    });

    it("forgetMemory soft-deletes and is not repeatable", () => {
      const rec = db.insertMemory({ content: "temporary" });
      expect(db.forgetMemory(rec.id)).toBe(true);
      expect(db.forgetMemory(rec.id)).toBe(false); // already gone
      expect(db.getMemory(rec.id)).toBeUndefined();
      expect(db.getMemory(rec.id, { includeDeleted: true })?.deletedAt).toBeGreaterThan(0);
    });

    it("forgetMemory on an unknown id returns false", () => {
      expect(db.forgetMemory("mem_deadbeef")).toBe(false);
    });

    it("pinMemory / unpinMemory are idempotent and reversible", () => {
      const rec = db.insertMemory({ content: "pinnable" });
      expect(db.pinMemory(rec.id)?.pinnedAt).toBeGreaterThan(0);
      expect(db.pinMemory(rec.id)?.pinnedAt).toBeGreaterThan(0); // idempotent
      expect(db.unpinMemory(rec.id)?.pinnedAt).toBeUndefined();
      expect(db.unpinMemory(rec.id)?.pinnedAt).toBeUndefined(); // idempotent
    });

    it("pin/unpin/forget on a forgotten memory yield no live row", () => {
      const rec = db.insertMemory({ content: "doomed" });
      db.forgetMemory(rec.id);
      expect(db.pinMemory(rec.id)).toBeUndefined();
      expect(db.unpinMemory(rec.id)).toBeUndefined();
    });

    it("recall survives close + reopen (FTS index persists)", () => {
      const dir = mkdtempSync(join(tmpdir(), "db-mem-"));
      const path = join(dir, "state.db");
      try {
        const first = new Db(path);
        const rec = first.insertMemory({ content: "persisted fact about widgets" });
        first.close();

        const second = new Db(path);
        const hits = second.recallMemories("widgets");
        expect(hits.map((m) => m.id)).toContain(rec.id);
        second.close();
      } finally {
        rmSync(dir, { recursive: true, force: true });
      }
    });
  });
});

describe("Db.queryAudit (filtered audit export query)", () => {
  let dir: string;
  let db: Db;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "db-audit-"));
    db = new Db(join(dir, "state.db"));
    // Three rows spanning actors, tools, outcomes and timestamps.
    db.insertAudit({
      ts: 1000,
      actor: "main",
      toolName: "fs.read",
      argsJson: "{}",
      outcome: "ok",
      durationMs: 1,
      summary: "read a",
    });
    db.insertAudit({
      ts: 2000,
      actor: "watcher",
      toolName: "git.commit",
      argsJson: "{}",
      outcome: "grant_ok",
      durationMs: 2,
      summary: "committed",
    });
    db.insertAudit({
      ts: 3000,
      actor: "main",
      toolName: "git.commit",
      argsJson: "{}",
      outcome: "error",
      durationMs: 3,
      summary: "commit failed",
    });
  });

  afterEach(() => {
    db.close();
    rmSync(dir, { recursive: true, force: true });
  });

  it("returns all rows newest-first when no filters are given", () => {
    const rows = db.queryAudit();
    expect(rows.map((r) => r.ts)).toEqual([3000, 2000, 1000]);
  });

  it("filters by actor", () => {
    const rows = db.queryAudit({ actor: "main" });
    expect(rows.map((r) => r.ts)).toEqual([3000, 1000]);
  });

  it("filters by toolName", () => {
    const rows = db.queryAudit({ toolName: "git.commit" });
    expect(rows.map((r) => r.ts)).toEqual([3000, 2000]);
  });

  it("filters by outcome", () => {
    const rows = db.queryAudit({ outcome: "ok" });
    expect(rows.map((r) => r.summary)).toEqual(["read a"]);
  });

  it("AND-combines multiple filters", () => {
    const rows = db.queryAudit({ actor: "main", toolName: "git.commit" });
    expect(rows.map((r) => r.ts)).toEqual([3000]);
  });

  it("filters by an inclusive time range", () => {
    expect(db.queryAudit({ tsFrom: 2000 }).map((r) => r.ts)).toEqual([3000, 2000]);
    expect(db.queryAudit({ tsTo: 2000 }).map((r) => r.ts)).toEqual([2000, 1000]);
    expect(db.queryAudit({ tsFrom: 2000, tsTo: 2000 }).map((r) => r.ts)).toEqual([2000]);
  });

  it("bounds the result set by limit", () => {
    expect(db.queryAudit({ limit: 1 }).map((r) => r.ts)).toEqual([3000]);
  });

  it("returns an empty array when nothing matches", () => {
    expect(db.queryAudit({ actor: "system" })).toEqual([]);
  });

  it("orders strictly by ts DESC, not insertion order", () => {
    // A separate DB so insertion order (4000 then 500) differs from ts order.
    const d2 = mkdtempSync(join(tmpdir(), "db-audit-ord-"));
    const db2 = new Db(join(d2, "state.db"));
    try {
      db2.insertAudit({ ts: 4000, actor: "main", toolName: "a", argsJson: "{}", outcome: "ok", durationMs: 1, summary: "x" });
      db2.insertAudit({ ts: 500, actor: "main", toolName: "b", argsJson: "{}", outcome: "ok", durationMs: 1, summary: "y" });
      db2.insertAudit({ ts: 2500, actor: "main", toolName: "c", argsJson: "{}", outcome: "ok", durationMs: 1, summary: "z" });
      expect(db2.queryAudit().map((r) => r.ts)).toEqual([4000, 2500, 500]);
    } finally {
      db2.close();
      rmSync(d2, { recursive: true, force: true });
    }
  });
});

describe("Db.gcRetentionSweep (bounded retention — issue #145)", () => {
  let dir: string;
  let db: Db;
  // A far-future clock so rows stamped at `OLD` are unambiguously past every
  // default retention window, while rows stamped near NOW are unambiguously
  // inside it. (Real Date.now() ≈ 1.7e12; NOW here is ~285 000 AD.)
  const NOW = 9_000_000_000_000;
  const OLD = 1000; // ancient — older than any window

  const retention = (over: Partial<RetentionPolicy> = {}): RetentionPolicy => ({
    ...DEFAULT_RETENTION,
    ...over,
  });

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "db-gc-"));
    db = new Db(join(dir, "state.db"));
  });

  afterEach(() => {
    db.close();
    rmSync(dir, { recursive: true, force: true });
  });

  const audit = (ts: number, runId?: string) =>
    db.insertAudit({
      ts,
      actor: "main",
      toolName: "fs.read",
      argsJson: "{}",
      outcome: "ok",
      durationMs: 1,
      summary: "x",
      runId,
    });

  it("prunes audit_log rows past the age window but keeps recent ones", () => {
    audit(OLD);
    const recent = audit(NOW - 1000);
    // keepRows: 1 so the count floor only shields the single newest row, leaving
    // the age cutoff free to act on the ancient one.
    db.gcRetentionSweep(NOW, retention({ auditLogKeepRows: 1 }));
    expect(db.listAudit().map((r) => r.id)).toEqual([recent.id]);
  });

  it("keeps the last N audit rows even when all are past the age window (count floor)", () => {
    // Every row is ancient, but the count floor retains the most recent keepRows.
    for (let i = 0; i < 5; i++) audit(OLD + i);
    db.gcRetentionSweep(NOW, retention({ auditLogKeepRows: 2 }));
    const kept = db.listAudit();
    expect(kept.length).toBe(2);
    // The two newest by ts are the survivors.
    expect(kept.map((r) => r.ts)).toEqual([OLD + 4, OLD + 3]);
  });

  it("a fresh DB sweep deletes nothing (all rows within window)", () => {
    audit(NOW - 1000);
    audit(NOW - 2000);
    db.gcRetentionSweep(NOW, retention());
    expect(db.listAudit().length).toBe(2);
  });

  it("prunes run_events by whole run and co-prunes the run's audit rows", () => {
    // Old run: both events predate the window → whole run removed.
    db.insertRunEvent({ runId: "old", seq: 0, ts: OLD, type: "start" });
    db.insertRunEvent({ runId: "old", seq: 1, ts: OLD + 1, type: "end" });
    // Fresh run: recent → fully retained, every seq intact.
    db.insertRunEvent({ runId: "fresh", seq: 0, ts: NOW - 100, type: "start" });
    db.insertRunEvent({ runId: "fresh", seq: 1, ts: NOW - 90, type: "end" });
    // Audit rows keyed to each run. The old run's audit row is stamped RECENT on
    // purpose: it survives audit_log's own age sweep, proving the run co-prune
    // (keyed on runId, not ts) is what removes it.
    audit(NOW - 50, "old");
    audit(NOW - 40, "fresh");

    // keepRuns: 0 so the count floor shields nothing — the age cutoff alone
    // decides. The fresh run is excluded from the expired set by the MAX(ts)
    // HAVING filter regardless, so it is never a prune candidate.
    db.gcRetentionSweep(NOW, retention({ runEventsKeepRuns: 0 }));

    expect(db.listRunEvents("old")).toEqual([]);
    expect(db.listRunEvents("fresh").map((e) => e.seq)).toEqual([0, 1]);
    expect(db.listAuditByRunId("old")).toEqual([]); // co-pruned despite recent ts
    expect(db.listAuditByRunId("fresh").length).toBe(1);
  });

  it("keeps the last N runs even when all are past the age window (count floor)", () => {
    for (let i = 0; i < 4; i++) {
      db.insertRunEvent({ runId: `r${i}`, seq: 0, ts: OLD + i, type: "start" });
    }
    db.gcRetentionSweep(NOW, retention({ runEventsKeepRuns: 2 }));
    // The two most-recently-active runs survive; the oldest two are gone.
    expect(db.listRunEvents("r0")).toEqual([]);
    expect(db.listRunEvents("r1")).toEqual([]);
    expect(db.listRunEvents("r2").length).toBe(1);
    expect(db.listRunEvents("r3").length).toBe(1);
  });

  it("prunes conversation and skill_selection_log by age, keeping recent rows", () => {
    db.insertMessage({ sessionId: "s", seq: 0, role: "user", content: "old", createdAt: OLD });
    db.insertMessage({ sessionId: "s", seq: 1, role: "user", content: "new", createdAt: NOW - 100 });
    db.insertSkillSelection({
      ts: OLD,
      sessionId: "s",
      userInput: "old",
      selectedSkillIdsJson: "[]",
      confidence: 0.5,
    });
    db.insertSkillSelection({
      ts: NOW - 100,
      sessionId: "s",
      userInput: "new",
      selectedSkillIdsJson: "[]",
      confidence: 0.5,
    });

    // keepRows: 1 each so the age cutoff is free to drop the ancient row.
    db.gcRetentionSweep(NOW, retention({ conversationKeepRows: 1, skillSelLogKeepRows: 1 }));

    expect(db.listMessages("s").map((m) => m.content)).toEqual(["new"]);
    expect(db.listSkillSelections().map((r) => r.userInput)).toEqual(["new"]);
  });

  it("hard-deletes resolved/expired events past the window but keeps open and recent ones", () => {
    const open = db.upsertEvent({ source: "system", severity: "info", title: "open", summary: "s", createdAt: OLD });
    const resolvedOld = db.upsertEvent({
      source: "system", severity: "info", title: "ro", summary: "s",
      createdAt: OLD, resolvedAt: OLD,
    });
    const expiredOld = db.upsertEvent({
      source: "system", severity: "info", title: "eo", summary: "s",
      createdAt: OLD, expiresAt: OLD,
    });
    const resolvedRecent = db.upsertEvent({
      source: "system", severity: "info", title: "rr", summary: "s",
      createdAt: NOW - 100, resolvedAt: NOW - 100,
    });

    db.gcRetentionSweep(NOW, retention());

    expect(db.getEvent(open.id)).toBeTruthy(); // never resolved → kept
    expect(db.getEvent(resolvedOld.id)).toBeUndefined();
    expect(db.getEvent(expiredOld.id)).toBeUndefined();
    expect(db.getEvent(resolvedRecent.id)).toBeTruthy(); // within window → kept
  });

  it("hard-deletes soft-deleted memories past the window and evicts them from the FTS index", () => {
    const gone = db.insertMemory({ content: "uniquewidget alpha" });
    const live = db.insertMemory({ content: "uniquewidget beta" });
    db.forgetMemory(gone.id, OLD); // soft-deleted long ago

    // Direct FTS probe: before the sweep the soft-deleted row is still indexed.
    const ftsMatches = (term: string) =>
      (db.raw().prepare("SELECT count(*) AS n FROM memories_fts WHERE memories_fts MATCH ?").get(term) as { n: number }).n;
    expect(ftsMatches("uniquewidget")).toBe(2);

    db.gcRetentionSweep(NOW, retention());

    // Base row gone, and the AFTER DELETE trigger evicted it from the FTS index.
    expect(db.getMemory(gone.id, { includeDeleted: true })).toBeUndefined();
    expect(ftsMatches("uniquewidget")).toBe(1);
    expect(db.getMemory(live.id)).toBeTruthy();
  });

  it("keeps a recently soft-deleted memory (inside the undo window)", () => {
    const rec = db.insertMemory({ content: "recent forget" });
    db.forgetMemory(rec.id, NOW - 100);
    db.gcRetentionSweep(NOW, retention());
    expect(db.getMemory(rec.id, { includeDeleted: true })?.deletedAt).toBe(NOW - 100);
  });

  it("retains active runs and the keepRuns newest expired runs (HAVING + count floor)", () => {
    // 3 expired runs (events predate the cutoff) + 2 active runs (recent events).
    for (let i = 0; i < 3; i++) {
      db.insertRunEvent({ runId: `exp${i}`, seq: 0, ts: OLD + i, type: "start" });
    }
    db.insertRunEvent({ runId: "act0", seq: 0, ts: NOW - 200, type: "start" });
    db.insertRunEvent({ runId: "act1", seq: 0, ts: NOW - 100, type: "start" });

    db.gcRetentionSweep(NOW, retention({ runEventsKeepRuns: 2 }));

    // Active runs are excluded by the MAX(ts) HAVING filter — never candidates.
    expect(db.listRunEvents("act0").length).toBe(1);
    expect(db.listRunEvents("act1").length).toBe(1);
    // Among the 3 expired runs, the 2 newest survive the count floor; the oldest goes.
    expect(db.listRunEvents("exp0")).toEqual([]);
    expect(db.listRunEvents("exp1").length).toBe(1);
    expect(db.listRunEvents("exp2").length).toBe(1);
  });

  it("keepRows: 0 removes every row past the cutoff (no floor)", () => {
    audit(OLD);
    audit(OLD + 1);
    db.gcRetentionSweep(NOW, retention({ auditLogKeepRows: 0 }));
    expect(db.listAudit()).toEqual([]);
  });

  it("keepRuns greater than the expired count deletes nothing (quiet-project tail)", () => {
    db.insertRunEvent({ runId: "a", seq: 0, ts: OLD, type: "start" });
    db.insertRunEvent({ runId: "b", seq: 0, ts: OLD + 1, type: "start" });
    db.gcRetentionSweep(NOW, retention({ runEventsKeepRuns: 5 }));
    expect(db.listRunEvents("a").length).toBe(1);
    expect(db.listRunEvents("b").length).toBe(1);
  });

  it("keeps an event resolved long ago but expiring only recently (scalar MAX of stamps)", () => {
    const split = db.upsertEvent({
      source: "system", severity: "info", title: "split", summary: "s",
      createdAt: OLD, resolvedAt: OLD, expiresAt: NOW - 1,
    });
    db.gcRetentionSweep(NOW, retention());
    // MAX(OLD, NOW-1) = NOW-1, inside the 7-day terminal window → retained.
    expect(db.getEvent(split.id)).toBeTruthy();
  });

  it("never co-prunes a null-runId audit row when pruning expired runs", () => {
    db.insertRunEvent({ runId: "old", seq: 0, ts: OLD, type: "start" });
    audit(NOW - 10, "old"); // recent ts, belongs to the expired run
    const orphan = audit(NOW - 5); // no runId — must survive (NULL never matches IN)

    db.gcRetentionSweep(NOW, retention({ runEventsKeepRuns: 0 }));

    expect(db.listAuditByRunId("old")).toEqual([]);
    expect(db.listAudit().map((r) => r.id)).toContain(orphan.id);
  });

  it("runs the sweep at construction via DbOptions (now + retention overrides)", () => {
    const path = join(dir, "ctor.db");
    const seed = new Db(path);
    seed.insertAudit({ ts: OLD, actor: "main", toolName: "t", argsJson: "{}", outcome: "ok", durationMs: 1, summary: "old" });
    seed.insertAudit({ ts: OLD + 1, actor: "main", toolName: "t", argsJson: "{}", outcome: "ok", durationMs: 1, summary: "old2" });
    seed.close();

    // Reopen with a far-future clock: the constructor sweep prunes the ancient
    // rows down to the count floor.
    const reopened = new Db(path, { now: () => NOW, retention: { auditLogKeepRows: 1 } });
    expect(reopened.listAudit().length).toBe(1);
    reopened.close();
  });
});
