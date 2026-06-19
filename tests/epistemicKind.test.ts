import { describe, it, expect } from "vitest";
import { classificationEpistemicKind } from "../src/schemas.js";
import { Db } from "../src/storage/db.js";
import { Queue } from "../src/queue.js";

// Issue #85 — provenance of a surfaced fact (observed / inferred / unverified):
// the pure mapping, plus the persistence round-trip through the queue and DB so a
// published event and a watcher record carry the kind back to the UI.

describe("classificationEpistemicKind", () => {
  it("maps a deterministic terminal exit to observed, a model-claimed one to inferred", () => {
    expect(classificationEpistemicKind("terminal_exited")).toBe("observed");
    expect(classificationEpistemicKind("terminal_exited", false)).toBe("observed");
    // The small model can also emit terminal_exited from tail text — that is a
    // claim, not a measured exit, so it must read as an inference.
    expect(classificationEpistemicKind("terminal_exited", true)).toBe("inferred");
  });

  it("disambiguates waiting_for_input by whether the model was consulted", () => {
    // agentState=waiting read straight from Daintree — an observed fact.
    expect(classificationEpistemicKind("waiting_for_input", false)).toBe("observed");
    // the small model concluded a wait from tail text — an inference.
    expect(classificationEpistemicKind("waiting_for_input", true)).toBe("inferred");
    // the UI fallback (no flag) treats it as observed: the agentState path dominates.
    expect(classificationEpistemicKind("waiting_for_input")).toBe("observed");
  });

  it("maps model-only and completion classes to inferred", () => {
    for (const c of [
      "permission_prompt",
      "still_working",
      "tests_failed",
      "tests_passed",
      "command_failed",
      "merge_conflict",
      "completed_success",
      "completed_unverified",
      "completed_unknown",
    ]) {
      expect(classificationEpistemicKind(c)).toBe("inferred");
    }
  });

  it("maps no-signal and unknown classes to unverified", () => {
    expect(classificationEpistemicKind("no_change")).toBe("unverified");
    expect(classificationEpistemicKind("unknown")).toBe("unverified");
    expect(classificationEpistemicKind("needs_large_model")).toBe("unverified");
    expect(classificationEpistemicKind(undefined)).toBe("unverified");
    expect(classificationEpistemicKind("garbage")).toBe("unverified");
  });
});

describe("event epistemicKind persistence", () => {
  it("round-trips epistemicKind through the queue and DB", () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ev = queue.publish({
      source: "terminal_watcher",
      severity: "done",
      title: "term_8 exited",
      summary: "Terminal exited.",
      epistemicKind: "observed",
    });
    expect(ev.epistemicKind).toBe("observed");
    expect(db.getEvent(ev.id)?.epistemicKind).toBe("observed");
    expect(queue.digest()[0].epistemicKind).toBe("observed");
  });

  it("leaves epistemicKind undefined when not supplied", () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ev = queue.publish({
      source: "system",
      severity: "info",
      title: "plain",
      summary: "no provenance",
    });
    expect(ev.epistemicKind).toBeUndefined();
    expect(db.getEvent(ev.id)?.epistemicKind).toBeUndefined();
  });

  it("preserves an existing epistemicKind across a dedupe bump that omits it", () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    queue.publish({
      source: "terminal_watcher",
      severity: "attention",
      title: "term_8",
      summary: "first",
      dedupeKey: "watcher:w:term_8",
      epistemicKind: "inferred",
    });
    // A later poll publishes the same dedupeKey without re-stating the kind.
    const bumped = queue.publish({
      source: "terminal_watcher",
      severity: "attention",
      title: "term_8",
      summary: "second",
      dedupeKey: "watcher:w:term_8",
    });
    expect(bumped.count).toBe(2);
    expect(bumped.epistemicKind).toBe("inferred");
  });

  it("refreshes epistemicKind on a dedupe bump in either direction", () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const pub = (summary: string, kind: "observed" | "inferred") =>
      queue.publish({
        source: "terminal_watcher",
        severity: "attention",
        title: "term_8",
        summary,
        dedupeKey: "watcher:w:term_8",
        epistemicKind: kind,
      });
    pub("first", "inferred");
    expect(pub("now exited", "observed").epistemicKind).toBe("observed");
    // ...and back the other way (an observed row re-classified by the model).
    expect(pub("model re-read", "inferred").epistemicKind).toBe("inferred");
  });

  it("leaves epistemicKind null when a first publish omits it, then adopts a later bump's kind", () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const first = queue.publish({
      source: "terminal_watcher",
      severity: "info",
      title: "term_8",
      summary: "no kind yet",
      dedupeKey: "watcher:w:term_8",
    });
    expect(first.epistemicKind).toBeUndefined();
    const bumped = queue.publish({
      source: "terminal_watcher",
      severity: "attention",
      title: "term_8",
      summary: "now classified",
      dedupeKey: "watcher:w:term_8",
      epistemicKind: "inferred",
    });
    expect(bumped.epistemicKind).toBe("inferred");
  });
});

describe("watcher lastEpistemicKind persistence", () => {
  it("round-trips lastEpistemicKind through insert and update", () => {
    const db = new Db(":memory:");
    const w = db.insertWatcher({
      kind: "terminal",
      title: "build",
      goal: "wait for exit",
      targetsJson: JSON.stringify(["term_8"]),
      cadenceMs: 1000,
      modelTier: "small",
      nextCheckAt: 0,
      lastEpistemicKind: "inferred",
    });
    expect(w.lastEpistemicKind).toBe("inferred");
    expect(db.getWatcher(w.id)?.lastEpistemicKind).toBe("inferred");

    db.updateWatcher(w.id, { lastEpistemicKind: "observed" });
    expect(db.getWatcher(w.id)?.lastEpistemicKind).toBe("observed");
  });
});
