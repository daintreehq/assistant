import { describe, it, expect } from "vitest";
import {
  evaluateCondition,
  decideOutcome,
  type WatcherSignals,
} from "../src/daemon/watcherEngine.js";

function sig(overrides: Partial<WatcherSignals> = {}): WatcherSignals {
  return { tail: "", ...overrides };
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
});
