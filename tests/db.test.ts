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
    const version = db
      .raw()
      .prepare("PRAGMA user_version")
      .get() as { user_version: number };
    expect(version.user_version).toBe(3);
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
});
