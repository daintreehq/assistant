import { presentTool } from "../../src/ui/presentation/tools.js";
import { buildAgentRows, actionKey } from "../../src/ui/presentation/operations.js";
import type { WatcherRecord } from "../../src/schemas.js";
import type { TerminalPreview } from "../../src/ui/hooks/useTerminalPreview.js";

describe("presentTool", () => {
  it("maps known first-party tools to human verbs + targets", () => {
    expect(presentTool("fs.search", { query: "watcher" })).toEqual({
      label: "Searched",
      detail: "watcher",
    });
    expect(presentTool("agentTask.spawnForEdits", { title: "repair tests" })).toMatchObject({
      label: "Delegated",
      detail: "repair tests",
    });
    expect(presentTool("watcher.terminal.create", { goal: "wait for tests" })).toMatchObject({
      label: "Watching",
    });
  });

  it("falls back to the internal name (never raw fn syntax) for unknown tools", () => {
    const p = presentTool("some.exotic.tool", { a: 1 });
    expect(p.label).toBe("some.exotic.tool");
    expect(p.label).not.toContain("(");
  });
});

function watcher(over: Partial<WatcherRecord>): WatcherRecord {
  return {
    id: "wch_1",
    kind: "terminal",
    title: "repair tests",
    goal: "wait for tests",
    targetsJson: JSON.stringify(["term_8"]),
    cadenceMs: 1000,
    modelTier: "small",
    status: "active",
    lastClassification: "still_working",
    nextCheckAt: 0,
    createdAt: 0,
    ...over,
  };
}

describe("buildAgentRows", () => {
  it("merges a watcher with its terminal preview into one agent row", () => {
    const previews: TerminalPreview[] = [
      { terminalId: "term_8", tail: "line one\n  42 passed", updatedAt: 0, agentState: "working" },
    ];
    const rows = buildAgentRows([watcher({})], previews);
    expect(rows).toHaveLength(1);
    expect(rows[0].id).toBe("term_8");
    expect(rows[0].preview).toBe("42 passed");
    expect(rows[0].agentState).toBe("working");
  });

  it("orders rows by human urgency (needs-input before working)", () => {
    const rows = buildAgentRows([
      watcher({ id: "wch_working", lastClassification: "still_working" }),
      watcher({ id: "wch_input", lastClassification: "waiting_for_input" }),
    ]);
    expect(rows[0].watcherId).toBe("wch_input");
    expect(rows[0].needsAttention).toBe(true);
  });

  it("prefers the persisted lastEpistemicKind over the classification fallback (#85)", () => {
    // still_working would derive "inferred", but a stored kind is authoritative.
    const rows = buildAgentRows([
      watcher({ lastClassification: "still_working", lastEpistemicKind: "observed" }),
    ]);
    expect(rows[0].epistemicKind).toBe("observed");
  });

  it("falls back to a classification-derived kind for rows without lastEpistemicKind (#85)", () => {
    const [exited] = buildAgentRows([
      watcher({ id: "a", lastClassification: "terminal_exited" }),
    ]);
    const [working] = buildAgentRows([
      watcher({ id: "b", lastClassification: "still_working" }),
    ]);
    const [unknown] = buildAgentRows([watcher({ id: "c", lastClassification: "unknown" })]);
    expect(exited.epistemicKind).toBe("observed");
    expect(working.epistemicKind).toBe("inferred");
    expect(unknown.epistemicKind).toBe("unverified");
  });

  it("passes a raw (unvalidated) lastEpistemicKind straight through (#85)", () => {
    // rowToWatcher doesn't re-validate the stored string; a corrupt value degrades
    // safely downstream (epistemicMark returns null → no tag), so the row keeps it.
    const rows = buildAgentRows([
      watcher({ lastEpistemicKind: "bogus" as any }),
    ]);
    expect(rows[0].epistemicKind).toBe("bogus");
  });
});

describe("actionKey", () => {
  it("derives a key from the first letter of the label", () => {
    expect(actionKey("focus terminal", 0)).toBe("F");
    expect(actionKey("rerun", 1)).toBe("R");
  });
});
