import { Db } from "../src/storage/db.js";

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
});
