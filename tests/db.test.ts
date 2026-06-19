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
      expect.arrayContaining(["grantSource", "grantId"]),
    );

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
