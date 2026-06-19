import { mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { Db } from "../src/storage/db.js";

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

    // recipe_run_state table + its unique natural-key index exist on a fresh DB.
    expect(colNames("recipe_run_state")).toEqual(
      expect.arrayContaining([
        "id",
        "sessionId",
        "recipeId",
        "currentStep",
        "stepsJson",
        "status",
        "startedAt",
        "updatedAt",
        "completedAt",
      ]),
    );
    expect(indexNames("recipe_run_state")).toContain(
      "idx_recipe_run_state_key",
    );

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

  describe("recipe run state", () => {
    it("insertRecipeRunState applies defaults (rrs_ id, active status, timestamps)", () => {
      const rec = db.insertRecipeRunState({
        sessionId: "ses_a",
        recipeId: "daintree.orchestration.basic",
      });
      expect(rec.id).toMatch(/^rrs_[0-9a-f]{8}$/);
      expect(rec.status).toBe("active");
      expect(rec.currentStep).toBe(0);
      expect(rec.stepsJson).toBe("[]");
      expect(rec.startedAt).toBeGreaterThan(0);
      expect(rec.updatedAt).toBe(rec.startedAt);
      expect(rec.completedAt).toBeUndefined();
    });

    it("getRecipeRunState looks up by the (session, recipe) natural key", () => {
      db.insertRecipeRunState({ sessionId: "ses_a", recipeId: "r.one" });
      db.insertRecipeRunState({ sessionId: "ses_a", recipeId: "r.two" });
      db.insertRecipeRunState({ sessionId: "ses_b", recipeId: "r.one" });

      const found = db.getRecipeRunState("ses_a", "r.two")!;
      expect(found.recipeId).toBe("r.two");
      expect(found.sessionId).toBe("ses_a");
      expect(db.getRecipeRunState("ses_missing", "r.one")).toBeUndefined();
    });

    it("round-trips the stepsJson checkpoint array", () => {
      const steps = [
        { index: 1, status: "done", notes: "cloned repo", ts: 111 },
        { index: 2, status: "skipped", ts: 222 },
      ];
      const rec = db.insertRecipeRunState({
        sessionId: "ses_a",
        recipeId: "r.steps",
        currentStep: 3,
        stepsJson: JSON.stringify(steps),
      });
      const fetched = db.getRecipeRunState("ses_a", "r.steps")!;
      expect(fetched.currentStep).toBe(3);
      expect(JSON.parse(fetched.stepsJson)).toEqual(steps);
      expect(rec.id).toBe(fetched.id);
    });

    it("the (session, recipe) index is unique — duplicate insert throws", () => {
      db.insertRecipeRunState({ sessionId: "ses_a", recipeId: "r.dup" });
      expect(() =>
        db.insertRecipeRunState({ sessionId: "ses_a", recipeId: "r.dup" }),
      ).toThrow();
    });

    it("updateRecipeRunState patches allowed columns and advances updatedAt", () => {
      const rec = db.insertRecipeRunState({
        sessionId: "ses_a",
        recipeId: "r.upd",
        startedAt: 1000,
      });
      db.updateRecipeRunState(rec.id, {
        currentStep: 2,
        stepsJson: JSON.stringify([{ index: 1, status: "done", ts: 500 }]),
        status: "active",
      });
      const fetched = db.getRecipeRunState("ses_a", "r.upd")!;
      expect(fetched.currentStep).toBe(2);
      expect(JSON.parse(fetched.stepsJson)).toHaveLength(1);
      // updatedAt advances past the seeded startedAt; startedAt is unchanged.
      expect(fetched.updatedAt).toBeGreaterThan(1000);
      expect(fetched.startedAt).toBe(1000);
    });

    it("updateRecipeRunState ignores unknown / immutable columns", () => {
      const rec = db.insertRecipeRunState({
        sessionId: "ses_a",
        recipeId: "r.imm",
        startedAt: 1000,
      });
      db.updateRecipeRunState(rec.id, {
        id: "rrs_hacked",
        sessionId: "ses_other",
        recipeId: "r.other",
        startedAt: 5,
        bogus: "nope",
      } as Record<string, unknown>);
      const fetched = db.getRecipeRunState("ses_a", "r.imm")!;
      expect(fetched.id).toBe(rec.id);
      expect(fetched.sessionId).toBe("ses_a");
      expect(fetched.recipeId).toBe("r.imm");
      expect(fetched.startedAt).toBe(1000);
    });

    it("listRecipeRunStates filters by session and orders by updatedAt desc", () => {
      db.insertRecipeRunState({
        sessionId: "ses_a",
        recipeId: "r.1",
        startedAt: 100,
        updatedAt: 100,
      });
      db.insertRecipeRunState({
        sessionId: "ses_a",
        recipeId: "r.2",
        startedAt: 300,
        updatedAt: 300,
      });
      db.insertRecipeRunState({
        sessionId: "ses_b",
        recipeId: "r.1",
        startedAt: 200,
        updatedAt: 200,
      });
      expect(db.listRecipeRunStates("ses_a").map((r) => r.recipeId)).toEqual([
        "r.2",
        "r.1",
      ]);
      expect(db.listRecipeRunStates()).toHaveLength(3);
    });

    it("maps SQL NULL completedAt to undefined and persists across reopen", () => {
      const dir = mkdtempSync(join(tmpdir(), "db-rrs-"));
      const path = join(dir, "state.db");
      try {
        const first = new Db(path);
        const rec = first.insertRecipeRunState({
          sessionId: "ses_a",
          recipeId: "r.persist",
        });
        expect(first.getRecipeRunState("ses_a", "r.persist")!.completedAt).toBeUndefined();
        first.updateRecipeRunState(rec.id, { status: "completed", completedAt: 8888 });
        first.close();

        const second = new Db(path);
        const fetched = second.getRecipeRunState("ses_a", "r.persist")!;
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
