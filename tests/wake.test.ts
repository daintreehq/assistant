import {
  isActionableWake,
  buildWakePrompt,
  isWakeFailureReply,
} from "../src/agent/wake.js";
import type { QueueEvent } from "../src/schemas.js";

/**
 * Minimal QueueEvent factory — only the fields the wake helpers read matter; the
 * rest are filled with valid defaults so the object satisfies the interface.
 */
function makeEvent(over: Partial<QueueEvent> = {}): QueueEvent {
  return {
    id: over.id ?? "evt-1",
    source: over.source ?? "terminal_watcher",
    severity: over.severity ?? "attention",
    title: over.title ?? "supervised waiting: Terminal waiting for input",
    summary: over.summary ?? "agent paused for input",
    createdAt: over.createdAt ?? 1000,
    count: over.count ?? 1,
    ...over,
  };
}

function termEvent(terminalId: string, over: Partial<QueueEvent> = {}): QueueEvent {
  return makeEvent({ ...over, target: { terminalId, ...over.target } });
}

describe("isActionableWake", () => {
  it("is true for a terminal_watcher event with a terminalId", () => {
    expect(isActionableWake(termEvent("t1"))).toBe(true);
  });

  it("is false when target/terminalId is missing", () => {
    expect(isActionableWake(makeEvent({ target: undefined }))).toBe(false);
    expect(isActionableWake(makeEvent({ target: { terminalId: "" } }))).toBe(false);
  });

  it("is false for non-watcher sources even with a terminalId", () => {
    expect(isActionableWake(termEvent("t1", { source: "user" }))).toBe(false);
    expect(isActionableWake(termEvent("t1", { source: "system" }))).toBe(false);
  });
});

describe("buildWakePrompt", () => {
  it("requests a full summary for a first-time terminal (no opts)", () => {
    const prompt = buildWakePrompt([termEvent("t1")]);
    expect(prompt).toContain("terminal.summarize");
    expect(prompt).not.toContain("already reported");
  });

  it("treats an empty alreadySummarized set the same as no opts", () => {
    const prompt = buildWakePrompt([termEvent("t1")], {
      alreadySummarized: new Set(),
    });
    expect(prompt).toContain("terminal.summarize");
    expect(prompt).not.toContain("already reported");
  });

  it("downgrades a follow-up event for an already-summarized terminal to a one-line ack", () => {
    const prompt = buildWakePrompt(
      [termEvent("t1", { title: "supervised done: Terminal exited" })],
      { alreadySummarized: new Set(["t1"]) },
    );
    expect(prompt).toContain("already reported");
    expect(prompt).toContain("do NOT call terminal.summarize");
    // The per-event line names the terminal so the model knows which one.
    expect(prompt).toContain("[terminal t1]");
  });

  it("does not give contradictory guidance when every event is a follow-up", () => {
    const prompt = buildWakePrompt([termEvent("t1"), termEvent("t1")], {
      alreadySummarized: new Set(["t1"]),
    });
    // The positive "summarize and report" instruction must be absent — there is
    // nothing new to summarize, so it would contradict the per-event ack markers.
    expect(prompt).not.toContain("give the user a concise update");
    expect(prompt).toContain("Acknowledge each in one short line");
  });

  it("emits the full-summary guidance and a per-event line free of the ack marker for a first-time terminal", () => {
    const prompt = buildWakePrompt([termEvent("t1")]);
    expect(prompt).toContain("give the user a concise update");
    const eventLine = prompt
      .split("\n")
      .find((l) => l.startsWith("- ") && l.includes("[terminal t1]"));
    expect(eventLine).toBeDefined();
    expect(eventLine).not.toContain("already reported");
  });

  it("models the issue #39 lifecycle: a terminal summarized in one burst is a follow-up in the next", () => {
    // Mirrors how the controller threads its summarizedTerminals set across bursts.
    const summarized = new Set<string>();
    const first = buildWakePrompt(
      [termEvent("t1", { title: "supervised waiting: Terminal waiting" })],
      { alreadySummarized: summarized },
    );
    expect(first).toContain("give the user a concise update");
    expect(first).not.toContain("already reported");
    // Caller records the terminal after a successful turn.
    summarized.add("t1");
    const second = buildWakePrompt(
      [termEvent("t1", { title: "supervised done: Terminal exited" })],
      { alreadySummarized: summarized },
    );
    expect(second).toContain("already reported");
    expect(second).toContain("do NOT call terminal.summarize");
  });

  it("summarizes a new terminal even when another was already reported (per-terminal granularity)", () => {
    const prompt = buildWakePrompt([termEvent("t1"), termEvent("t2")], {
      alreadySummarized: new Set(["t1"]),
    });
    // t1 is a follow-up, t2 is brand new and still earns a full summary.
    expect(prompt).toContain("already reported");
    expect(prompt).toContain("[terminal t2]");
    const t2Line = prompt
      .split("\n")
      .find((l) => l.includes("[terminal t2]"));
    expect(t2Line).toBeDefined();
    expect(t2Line).not.toContain("already reported");
  });

  it("only summarizes the first occurrence when the same terminal appears twice in one batch", () => {
    const prompt = buildWakePrompt(
      [
        termEvent("t1", { title: "supervised waiting: Terminal waiting" }),
        termEvent("t1", { title: "supervised done: Terminal exited" }),
      ],
      { alreadySummarized: new Set() },
    );
    // Only per-event lines (they start with "- "); the guidance header also
    // mentions "already reported" and must not be counted.
    const followUps = prompt
      .split("\n")
      .filter((l) => l.startsWith("- ") && l.includes("already reported"));
    // Exactly one of the two per-event lines is downgraded (the second).
    expect(followUps).toHaveLength(1);
    expect(followUps[0]).toContain("Terminal exited");
  });

  it("renders events without a terminalId neutrally and never crashes", () => {
    const prompt = buildWakePrompt([makeEvent({ target: undefined })], {
      alreadySummarized: new Set(["t1"]),
    });
    expect(prompt).toContain("New events:");
    expect(prompt).not.toContain("already reported");
  });
});

describe("isWakeFailureReply", () => {
  it("recognizes every send() failure sentinel", () => {
    expect(isWakeFailureReply("Model unavailable: 503")).toBe(true);
    expect(isWakeFailureReply("Model error: boom")).toBe(true);
    expect(isWakeFailureReply("Tool projection failed: dup name")).toBe(true);
    expect(
      isWakeFailureReply("Reached the tool-iteration limit without a final answer."),
    ).toBe(true);
    expect(isWakeFailureReply("Turn cancelled")).toBe(true);
    expect(
      isWakeFailureReply(
        "Stopped: called watcher.terminal.create 3 times this turn with identical arguments, each failing the same way (INVALID_ARGS: ...).",
      ),
    ).toBe(true);
  });

  it("treats a real model reply as success", () => {
    expect(isWakeFailureReply("Terminal t1 finished cleanly; tests passed.")).toBe(
      false,
    );
    expect(isWakeFailureReply("")).toBe(false);
  });
});
