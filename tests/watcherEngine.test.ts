import { describe, it, expect } from "vitest";
import {
  evaluateCondition,
  decideOutcome,
  readStatuses,
  readOutput,
  type WatcherSignals,
} from "../src/daemon/watcherEngine.js";
import type { ModelJudgeAnswer } from "../src/schemas.js";
import type { ToolContext } from "../src/tools/types.js";

/** Build a judge-answer map keyed by question. */
function judges(
  entries: Record<string, Partial<ModelJudgeAnswer>>,
): Map<string, ModelJudgeAnswer> {
  const m = new Map<string, ModelJudgeAnswer>();
  for (const [q, a] of Object.entries(entries)) {
    m.set(q, { reason: "r", confidence: 0.9, matched: true, ...a });
  }
  return m;
}

function sig(overrides: Partial<WatcherSignals> = {}): WatcherSignals {
  return { tail: "", ...overrides };
}

/** A ToolContext whose terminal.getStatus returns the given raw entries. */
function ctxWithStatus(entries: Array<Record<string, unknown>>): ToolContext {
  return {
    mcp: {
      isConnected: () => true,
      callTool: async (name: string) =>
        name === "terminal.getStatus"
          ? { isError: false, text: "", structuredContent: { terminals: entries } }
          : { isError: true, text: "", structuredContent: {} },
    },
  } as unknown as ToolContext;
}

/**
 * A ToolContext that returns its terminal payloads ONLY in the `text` body with
 * NO structuredContent — Daintree's real shape (#108). Status is JSON-encoded,
 * output is raw scrollback text.
 */
function ctxTextOnly(
  statusEntries: Array<Record<string, unknown>>,
  outputText = "",
): ToolContext {
  return {
    mcp: {
      isConnected: () => true,
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return {
            isError: false,
            text: JSON.stringify({ terminals: statusEntries }),
            structuredContent: undefined,
          };
        }
        if (name === "terminal.getOutput") {
          return { isError: false, text: outputText, structuredContent: undefined };
        }
        return { isError: true, text: "", structuredContent: {} };
      },
    },
  } as unknown as ToolContext;
}

describe("evaluateCondition", () => {
  it("matches stateIs against agentState", () => {
    expect(evaluateCondition({ stateIs: "waiting" }, sig({ agentState: "waiting" }))).toBe(true);
    expect(evaluateCondition({ stateIs: "waiting" }, sig({ agentState: "working" }))).toBe(false);
    expect(evaluateCondition({ stateIs: "waiting" }, sig())).toBe(false);
  });

  it("matches contains against the tail", () => {
    expect(evaluateCondition({ contains: "y/n" }, sig({ tail: "Proceed? y/n" }))).toBe(true);
    expect(evaluateCondition({ contains: "y/n" }, sig({ tail: "all good" }))).toBe(false);
  });

  it("matches regex against the tail", () => {
    expect(evaluateCondition({ regex: "conflict" }, sig({ tail: "merge conflict in foo.ts" }))).toBe(true);
    expect(evaluateCondition({ regex: "conflict" }, sig({ tail: "clean merge" }))).toBe(false);
  });

  it("returns false for an invalid regex without throwing", () => {
    expect(evaluateCondition({ regex: "[" }, sig({ tail: "anything" }))).toBe(false);
  });

  it("matches noOutputForMs against msSinceOutput", () => {
    expect(evaluateCondition({ noOutputForMs: 1000 }, sig({ msSinceOutput: 1500 }))).toBe(true);
    expect(evaluateCondition({ noOutputForMs: 1000 }, sig({ msSinceOutput: 1000 }))).toBe(true);
    expect(evaluateCondition({ noOutputForMs: 1000 }, sig({ msSinceOutput: 500 }))).toBe(false);
    // missing msSinceOutput is treated as 0
    expect(evaluateCondition({ noOutputForMs: 1000 }, sig())).toBe(false);
  });

  it("matches all only when every sub-condition holds", () => {
    const cond = { all: [{ stateIs: "waiting" as const }, { contains: "y/n" }] };
    expect(evaluateCondition(cond, sig({ agentState: "waiting", tail: "do it? y/n" }))).toBe(true);
    expect(evaluateCondition(cond, sig({ agentState: "waiting", tail: "no prompt" }))).toBe(false);
    expect(evaluateCondition(cond, sig({ agentState: "working", tail: "do it? y/n" }))).toBe(false);
  });

  it("matches any when at least one sub-condition holds", () => {
    const cond = { any: [{ regex: "conflict" }, { contains: "y/n" }] };
    expect(evaluateCondition(cond, sig({ tail: "merge conflict" }))).toBe(true);
    expect(evaluateCondition(cond, sig({ tail: "press y/n" }))).toBe(true);
    expect(evaluateCondition(cond, sig({ tail: "nothing here" }))).toBe(false);
  });

  it("negates with not", () => {
    expect(evaluateCondition({ not: { contains: "y/n" } }, sig({ tail: "clean" }))).toBe(true);
    expect(evaluateCondition({ not: { contains: "y/n" } }, sig({ tail: "y/n" }))).toBe(false);
  });

  describe("modelJudge (#57)", () => {
    const q = "Did the migration finish?";

    it("evaluates each judge against its OWN precomputed answer, not the classification", () => {
      // A confident, MEANINGFUL classification used to make any modelJudge fire.
      // Now the judge's own answer is what matters: a NO must stay false even when
      // the general classification is meaningful and confident.
      const meaningful = sig({ classification: "tests_failed", confidence: 0.95 });
      expect(
        evaluateCondition({ modelJudge: q }, meaningful, judges({ [q]: { matched: false } })),
      ).toBe(false);
      expect(
        evaluateCondition({ modelJudge: q }, meaningful, judges({ [q]: { matched: true } })),
      ).toBe(true);
    });

    it("fires only on a confident match", () => {
      expect(
        evaluateCondition({ modelJudge: q }, sig(), judges({ [q]: { matched: true, confidence: 0.9 } })),
      ).toBe(true);
      expect(
        evaluateCondition({ modelJudge: q }, sig(), judges({ [q]: { matched: false, confidence: 0.9 } })),
      ).toBe(false);
      // Below the 0.6 confidence floor → no fire even when matched.
      expect(
        evaluateCondition({ modelJudge: q }, sig(), judges({ [q]: { matched: true, confidence: 0.59 } })),
      ).toBe(false);
      // Exactly at the floor → fires.
      expect(
        evaluateCondition({ modelJudge: q }, sig(), judges({ [q]: { matched: true, confidence: 0.6 } })),
      ).toBe(true);
    });

    it("is false when the question has no answer (no judge run / model failure / empty map)", () => {
      expect(evaluateCondition({ modelJudge: q }, sig(), new Map())).toBe(false);
      expect(evaluateCondition({ modelJudge: q }, sig())).toBe(false);
      expect(
        evaluateCondition({ modelJudge: q }, sig(), judges({ "other?": { matched: true } })),
      ).toBe(false);
    });

    it("evaluates multiple judges independently inside all/any", () => {
      const cond = { all: [{ modelJudge: "a?" }, { modelJudge: "b?" }] };
      expect(
        evaluateCondition(cond, sig(), judges({ "a?": { matched: true }, "b?": { matched: true } })),
      ).toBe(true);
      // One judge says no → the `all` fails (the old single-answer behavior could
      // not even see the second question).
      expect(
        evaluateCondition(cond, sig(), judges({ "a?": { matched: true }, "b?": { matched: false } })),
      ).toBe(false);

      const anyCond = { any: [{ modelJudge: "a?" }, { modelJudge: "b?" }] };
      expect(
        evaluateCondition(anyCond, sig(), judges({ "a?": { matched: false }, "b?": { matched: true } })),
      ).toBe(true);
      expect(
        evaluateCondition(anyCond, sig(), judges({ "a?": { matched: false }, "b?": { matched: false } })),
      ).toBe(false);
    });
  });
});

describe("decideOutcome", () => {
  const base = { summary: "s", evidence: [] as string[], signals: sig() };

  it("publishes on a meaningful change (waiting_for_input after still_working)", () => {
    const out = decideOutcome({
      ...base,
      classification: "waiting_for_input",
      confidence: 0.9,
      previous: "still_working",
    });
    expect(out.shouldPublish).toBe(true);
    expect(out.stop).toBe(false);
    expect(out.severity).toBe("attention");
  });

  it("suppresses a repeated still_working classification", () => {
    const out = decideOutcome({
      ...base,
      classification: "still_working",
      confidence: 0.7,
      previous: "still_working",
    });
    expect(out.shouldPublish).toBe(false);
    expect(out.stop).toBe(false);
  });

  it("stops on completed_success (terminal classification)", () => {
    const out = decideOutcome({
      ...base,
      classification: "completed_success",
      confidence: 0.9,
      previous: "still_working",
    });
    expect(out.stop).toBe(true);
    expect(out.stopReason).toBe("terminal");
    expect(out.shouldPublish).toBe(true);
  });

  it("stops with condition_met when stopWhen matches", () => {
    const out = decideOutcome({
      ...base,
      classification: "still_working",
      confidence: 0.7,
      previous: "still_working",
      signals: sig({ tail: "merge conflict detected" }),
      stopWhen: { regex: "conflict" },
    });
    expect(out.stop).toBe(true);
    expect(out.stopReason).toBe("condition_met");
    expect(out.shouldPublish).toBe(true);
  });

  it("stops and publishes on timeout regardless of classification", () => {
    const out = decideOutcome({
      ...base,
      classification: "still_working",
      confidence: 0.7,
      previous: "still_working",
      timedOut: true,
    });
    expect(out.stop).toBe(true);
    expect(out.stopReason).toBe("timeout");
    expect(out.shouldPublish).toBe(true);
    expect(out.severity).toBe("attention");
  });

  // Issue #85 — the outcome carries epistemic provenance derived from the
  // classification and whether the small model was consulted.
  it("tags a deterministic terminal_exited outcome observed", () => {
    const out = decideOutcome({ ...base, classification: "terminal_exited", confidence: 0.95 });
    expect(out.epistemicKind).toBe("observed");
  });

  it("tags a model-derived waiting_for_input inferred but a deterministic one observed", () => {
    expect(
      decideOutcome({ ...base, classification: "waiting_for_input", confidence: 0.9 })
        .epistemicKind,
    ).toBe("observed");
    expect(
      decideOutcome({
        ...base,
        classification: "waiting_for_input",
        confidence: 0.85,
        usedModel: true,
      }).epistemicKind,
    ).toBe("inferred");
  });

  it("tags a model classification inferred and an unknown outcome unverified", () => {
    expect(
      decideOutcome({ ...base, classification: "tests_failed", confidence: 0.85, usedModel: true })
        .epistemicKind,
    ).toBe("inferred");
    expect(
      decideOutcome({ ...base, classification: "unknown", confidence: 0.4 }).epistemicKind,
    ).toBe("unverified");
  });
});

describe("readStatuses — exit metadata parsing (#22)", () => {
  it("preserves numeric exitCode (including 0), spawnedAt, lastTransitionAt", async () => {
    const ctx = ctxWithStatus([
      {
        terminalId: "t1",
        agentState: "exited",
        exitCode: 0,
        spawnedAt: 1_700_000_000_000,
        lastTransitionAt: 1_700_000_001_000,
      },
      { terminalId: "t2", agentState: "exited", exitCode: 1 },
    ]);
    const batch = await readStatuses(ctx, ["t1", "t2"]);
    expect(batch.ok).toBe(true);
    expect(batch.byId.get("t1")).toMatchObject({
      exitCode: 0,
      spawnedAt: 1_700_000_000_000,
      lastTransitionAt: 1_700_000_001_000,
    });
    expect(batch.byId.get("t2")?.exitCode).toBe(1);
  });

  it("coerces null / string / NaN / Infinity / fractional exit metadata to undefined", async () => {
    const ctx = ctxWithStatus([
      { terminalId: "n", agentState: "exited", exitCode: null },
      { terminalId: "s", agentState: "exited", exitCode: "1" },
      { terminalId: "nan", agentState: "exited", exitCode: Number.NaN },
      { terminalId: "inf", agentState: "exited", exitCode: Number.POSITIVE_INFINITY },
      { terminalId: "frac", agentState: "exited", exitCode: 1.5 },
      { terminalId: "tsStr", agentState: "exited", spawnedAt: "2024-01-01" },
      { terminalId: "tsStr2", agentState: "exited", lastTransitionAt: "2026-06-17T10:00:00Z" },
    ]);
    const batch = await readStatuses(ctx, ["n", "s", "nan", "inf", "frac", "tsStr", "tsStr2"]);
    expect(batch.byId.get("n")?.exitCode).toBeUndefined();
    expect(batch.byId.get("s")?.exitCode).toBeUndefined();
    expect(batch.byId.get("nan")?.exitCode).toBeUndefined();
    expect(batch.byId.get("inf")?.exitCode).toBeUndefined();
    expect(batch.byId.get("frac")?.exitCode).toBeUndefined();
    expect(batch.byId.get("tsStr")?.spawnedAt).toBeUndefined();
    expect(batch.byId.get("tsStr2")?.lastTransitionAt).toBeUndefined();
  });

  it("leaves exit metadata undefined when the fields are absent (backwards compat)", async () => {
    const ctx = ctxWithStatus([{ terminalId: "t1", agentState: "working" }]);
    const batch = await readStatuses(ctx, ["t1"]);
    const e = batch.byId.get("t1")!;
    expect(e.exitCode).toBeUndefined();
    expect(e.spawnedAt).toBeUndefined();
    expect(e.lastTransitionAt).toBeUndefined();
  });
});

describe("readStatuses / readOutput — text-body fallback (#108)", () => {
  it("readStatuses parses terminals from the text body when structuredContent is absent", async () => {
    const ctx = ctxTextOnly([
      { terminalId: "t1", agentState: "waiting", exitCode: 0 },
      { terminalId: "t2", agentState: "working" },
    ]);
    const batch = await readStatuses(ctx, ["t1", "t2"]);
    expect(batch.ok).toBe(true);
    expect(batch.byId.get("t1")).toMatchObject({ agentState: "waiting", exitCode: 0 });
    expect(batch.byId.get("t2")?.agentState).toBe("working");
  });

  it("readOutput returns the raw text body when structuredContent is absent (not an empty string)", async () => {
    const ctx = ctxTextOnly([], "build finished\nall green");
    const res = await readOutput(ctx, "t1");
    expect(res.ok).toBe(true);
    expect(res.value).toBe("build finished\nall green");
  });

  it("readStatuses still returns ok with an empty byId when neither source has terminals", async () => {
    const ctx = {
      mcp: {
        isConnected: () => true,
        callTool: async () => ({ isError: false, text: "", structuredContent: undefined }),
      },
    } as unknown as ToolContext;
    const batch = await readStatuses(ctx, ["t1"]);
    // ok reflects call success, not byId population — the caller interprets empty.
    expect(batch.ok).toBe(true);
    expect(batch.byId.size).toBe(0);
  });
});
