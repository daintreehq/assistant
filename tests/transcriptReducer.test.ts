import {
  transcriptReducer,
  type ControllerAction,
} from "../src/ui/hooks/useDaintreeController.js";
import type { TranscriptCell, TurnCell } from "../src/ui/types.js";

function run(actions: ControllerAction[]): TranscriptCell[] {
  return actions.reduce<TranscriptCell[]>(
    (s, a) => transcriptReducer(s, a),
    [],
  );
}

const turns = (cells: TranscriptCell[]) =>
  cells.filter((c): c is TurnCell => c.kind === "turn");

describe("transcriptReducer (run-oriented)", () => {
  it("opens a turn on a user message and accumulates streamed tokens", () => {
    const out = run([
      { type: "user:add", text: "watch the build" },
      { type: "assistant:start" },
      { type: "assistant:token", token: "On " },
      { type: "assistant:token", token: "it." },
      { type: "assistant:end", content: "On it." },
    ]);
    expect(turns(out)).toHaveLength(1);
    const t = turns(out)[0];
    expect(t.userText).toBe("watch the build");
    expect(t.assistantText).toBe("On it.");
    expect(t.streaming).toBe(false);
    expect(t.state).toBe("complete");
  });

  it("groups tool calls as activities under the turn and resolves by id", () => {
    const out = run([
      { type: "user:add", text: "fix tests" },
      { type: "tool:call", id: "c1", name: "fs.search", args: { query: "x" }, startedAt: 0 },
      { type: "tool:call", id: "c2", name: "agentTask.spawnForEdits", args: { title: "fix" }, startedAt: 1 },
      // Resolve out of order — must match by id, not by recency or name.
      { type: "tool:result", id: "c2", name: "agentTask.spawnForEdits", result: { ok: true, summary: "spawned term_8" }, endedAt: 5 },
      { type: "tool:result", id: "c1", name: "fs.search", result: { ok: true, summary: "8 matches" }, endedAt: 6 },
    ]);
    const t = turns(out)[0];
    expect(t.activities).toHaveLength(2);
    const search = t.activities.find((a) => a.id === "c1")!;
    const spawn = t.activities.find((a) => a.id === "c2")!;
    // Internal names become human verbs — no raw fn() syntax.
    expect(search.label).toBe("Searched");
    expect(spawn.label).toBe("Delegated");
    expect(search.state).toBe("done");
    expect(search.summary).toBe("8 matches");
    expect(search.endedAt).toBe(6);
  });

  it("marks a tool result as failed without failing the whole turn", () => {
    const out = run([
      { type: "user:add", text: "push" },
      { type: "tool:call", id: "c1", name: "fs.read", args: {}, startedAt: 0 },
      { type: "tool:result", id: "c1", name: "fs.read", result: { ok: false, summary: "denied" }, endedAt: 1 },
      { type: "assistant:end", content: "Couldn't read it, here's why." },
    ]);
    const t = turns(out)[0];
    expect(t.activities[0].state).toBe("failed");
    expect(t.state).toBe("complete");
  });

  it("fails the active turn when an error log arrives and stops the caret", () => {
    const out = run([
      { type: "user:add", text: "go" },
      { type: "assistant:start" },
      { type: "log", level: "error", message: "Model error: boom" },
    ]);
    const t = turns(out)[0];
    expect(t.state).toBe("failed");
    expect(t.streaming).toBe(false);
    expect(t.notes.some((n) => n.level === "error")).toBe(true);
  });

  it("routes attention events to standalone note cells", () => {
    const out = run([
      { type: "attention", events: [{ title: "Tests failed", summary: "term_8" }] },
    ]);
    expect(out).toHaveLength(1);
    expect(out[0].kind).toBe("note");
  });

  it("caps the transcript length", () => {
    const actions: ControllerAction[] = Array.from({ length: 250 }, (_, i) => ({
      type: "user:add" as const,
      text: `m${i}`,
    }));
    expect(run(actions).length).toBeLessThanOrEqual(200);
  });
});
