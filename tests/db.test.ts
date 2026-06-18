import { createRequire } from "node:module";
import { mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { Db } from "../src/storage/db.js";

const { DatabaseSync } = createRequire(import.meta.url)("node:sqlite") as {
  DatabaseSync: typeof import("node:sqlite").DatabaseSync;
};

describe("Db migration v2 -> v3 (isSupervisor)", () => {
  let dir: string;
  let path: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "db-mig-"));
    path = join(dir, "state.db");
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it("adds isSupervisor=false to rows from a pre-isSupervisor schema", () => {
    // Build a v2 database by hand: watchers table WITHOUT the isSupervisor
    // column, user_version pinned to 2 (the two pre-existing event migrations).
    const raw = new DatabaseSync(path);
    raw.exec(`CREATE TABLE watchers (
      id TEXT PRIMARY KEY, kind TEXT NOT NULL, title TEXT NOT NULL,
      goal TEXT NOT NULL, targetsJson TEXT NOT NULL, cadenceMs INTEGER NOT NULL,
      modelTier TEXT NOT NULL, startAfterMs INTEGER, stopAfterMs INTEGER,
      stopWhenJson TEXT, alertWhenJson TEXT, optionsJson TEXT,
      status TEXT NOT NULL DEFAULT 'created', lastClassification TEXT,
      lastCheckedAt INTEGER, nextCheckAt INTEGER NOT NULL, createdAt INTEGER NOT NULL
    )`);
    raw.exec(
      `INSERT INTO watchers (id,kind,title,goal,targetsJson,cadenceMs,modelTier,status,nextCheckAt,createdAt)
       VALUES ('wch_old','terminal','old','g','[]',120000,'small','active',0,0)`,
    );
    raw.exec("PRAGMA user_version = 2");
    raw.close();

    // Opening through Db runs the forward-only migrations.
    const db = new Db(path);
    const old = db.getWatcher("wch_old");
    expect(old?.isSupervisor).toBe(false);
    // New inserts work against the migrated schema.
    const fresh = db.insertWatcher({
      kind: "terminal",
      title: "new",
      goal: "g",
      targetsJson: "[]",
      cadenceMs: 3000,
      modelTier: "small",
      nextCheckAt: 0,
      isSupervisor: true,
    });
    expect(db.getWatcher(fresh.id)?.isSupervisor).toBe(true);
    // Opening runs the full forward-only ladder, so user_version lands on the
    // current migration count (now 5 with grant provenance + workflow_runs),
    // not just 3.
    const version = db
      .raw()
      .prepare("PRAGMA user_version")
      .get() as { user_version: number };
    expect(version.user_version).toBe(5);
    db.close();
  });
});

describe("Db migration v3 -> v4 (grant provenance)", () => {
  let dir: string;
  let path: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "db-mig4-"));
    path = join(dir, "state.db");
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it("backfills source='local' on grants and adds grant columns to audit_log", () => {
    // Build a v3 database by hand: automation_grants WITHOUT the source column
    // and audit_log WITHOUT grantSource/grantId, user_version pinned to 3.
    const raw = new DatabaseSync(path);
    raw.exec(`CREATE TABLE automation_grants (
      id TEXT PRIMARY KEY, actorId TEXT NOT NULL, actorType TEXT NOT NULL,
      allowedRiskClassesJson TEXT, allowedToolNamesJson TEXT,
      expiresAt INTEGER NOT NULL, maxUses INTEGER NOT NULL,
      usesRemaining INTEGER NOT NULL, revokedAt INTEGER, createdAt INTEGER NOT NULL
    )`);
    raw.exec(
      `INSERT INTO automation_grants (id,actorId,actorType,allowedRiskClassesJson,allowedToolNamesJson,expiresAt,maxUses,usesRemaining,revokedAt,createdAt)
       VALUES ('grt_old','wch_old','watcher','["git"]',NULL,9999999999999,3,3,NULL,0)`,
    );
    raw.exec(`CREATE TABLE audit_log (
      id TEXT PRIMARY KEY, ts INTEGER NOT NULL, actor TEXT NOT NULL,
      toolName TEXT NOT NULL, argsJson TEXT NOT NULL, outcome TEXT NOT NULL,
      durationMs INTEGER NOT NULL, summary TEXT NOT NULL, resultJson TEXT
    )`);
    raw.exec("PRAGMA user_version = 3");
    raw.close();

    // Opening through Db runs the forward-only migrations.
    const db = new Db(path);
    const old = db.getGrant("grt_old");
    expect(old?.source).toBe("local");
    // The backfilled source also flows through the consume path, not just getGrant.
    const consumed = db.consumeGrant("wch_old", "watcher", "git.commit", "git");
    expect(consumed?.id).toBe("grt_old");
    expect(consumed?.source).toBe("local");
    // New inserts can carry an explicit source and audit grant provenance.
    const fresh = db.insertGrant({
      actorId: "wch_new",
      actorType: "watcher",
      allowedRiskClassesJson: JSON.stringify(["git"]),
      allowedToolNamesJson: null,
      expiresAt: 9999999999999,
      maxUses: 1,
    });
    expect(db.getGrant(fresh.id)?.source).toBe("local");
    const aud = db.insertAudit({
      actor: "watcher",
      toolName: "git.commit",
      argsJson: "{}",
      outcome: "grant_ok",
      durationMs: 1,
      summary: "ok",
      grantSource: "local",
      grantId: fresh.id,
    });
    const back = db.listAudit().find((r) => r.id === aud.id)!;
    expect(back.grantSource).toBe("local");
    expect(back.grantId).toBe(fresh.id);
    // Opening a pre-current (v3) DB runs the full forward-only ladder, so
    // user_version lands on the current migration count (now 5 with the
    // workflow_runs step appended after grant provenance).
    const version = db
      .raw()
      .prepare("PRAGMA user_version")
      .get() as { user_version: number };
    expect(version.user_version).toBe(5);
    db.close();
  });
});

describe("Db migration v4 -> v5 (workflow_runs)", () => {
  let dir: string;
  let path: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "db-mig-wf-"));
    path = join(dir, "state.db");
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it("creates the workflow_runs table for a pre-workflow (v4) database", () => {
    // Build a v4 database by hand: a watchers table WITH isSupervisor (the v3
    // schema) but NO workflow_runs table, user_version pinned to 4 (after the
    // grant-provenance migration but before workflow_runs).
    const raw = new DatabaseSync(path);
    raw.exec(`CREATE TABLE watchers (
      id TEXT PRIMARY KEY, kind TEXT NOT NULL, title TEXT NOT NULL,
      goal TEXT NOT NULL, targetsJson TEXT NOT NULL, cadenceMs INTEGER NOT NULL,
      isSupervisor INTEGER NOT NULL DEFAULT 0, modelTier TEXT NOT NULL,
      startAfterMs INTEGER, stopAfterMs INTEGER, stopWhenJson TEXT,
      alertWhenJson TEXT, optionsJson TEXT, status TEXT NOT NULL DEFAULT 'created',
      lastClassification TEXT, lastCheckedAt INTEGER, nextCheckAt INTEGER NOT NULL,
      createdAt INTEGER NOT NULL
    )`);
    raw.exec("PRAGMA user_version = 4");
    // No workflow_runs table exists yet.
    const before = raw
      .prepare(
        "SELECT name FROM sqlite_master WHERE type='table' AND name='workflow_runs'",
      )
      .all();
    expect(before).toHaveLength(0);
    raw.close();

    // Opening through Db runs the forward-only migrations.
    const db = new Db(path);
    // The migrated schema carries every workflow_runs column and the status index.
    const cols = (
      db.raw().prepare("PRAGMA table_info(workflow_runs)").all() as Array<{
        name: string;
      }>
    ).map((c) => c.name);
    expect(cols).toEqual(
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
    const indexes = (
      db.raw().prepare("PRAGMA index_list(workflow_runs)").all() as Array<{
        name: string;
      }>
    ).map((i) => i.name);
    expect(indexes).toContain("idx_workflow_runs_status");
    // The table accepts inserts and the run round-trips.
    const rec = db.insertWorkflowRun({ issueNumber: 25, status: "active" });
    expect(rec.id).toMatch(/^wfr_[0-9a-f]{8}$/);
    expect(db.getWorkflowRun(rec.id)?.issueNumber).toBe(25);
    const version = db
      .raw()
      .prepare("PRAGMA user_version")
      .get() as { user_version: number };
    expect(version.user_version).toBe(5);
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
        title: "ignored on bump",
        summary: "second summary",
        dedupeKey: "dup-1",
      });
      expect(second.id).toBe(first.id);
      expect(second.count).toBe(2);
      // The bump updates summary + severity.
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
});
