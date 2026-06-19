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

  it("marks the active turn cancelled, keeps partial text, and stops the caret (#45)", () => {
    const out = run([
      { type: "user:add", text: "do the thing" },
      { type: "assistant:start" },
      { type: "assistant:token", token: "Wor" },
      { type: "assistant:token", token: "king" },
      { type: "assistant:cancelled", content: "" },
    ]);
    const t = turns(out)[0];
    expect(t.state).toBe("cancelled");
    expect(t.streaming).toBe(false);
    // The partial stream is preserved, not discarded.
    expect(t.assistantText).toBe("Working");
    expect(t.notes.some((n) => n.text === "Turn cancelled")).toBe(true);
  });

  it("a cancelled turn is not active, so a later assistant:end can't resurrect it (#45)", () => {
    const out = run([
      { type: "user:add", text: "go" },
      { type: "assistant:start" },
      { type: "assistant:cancelled", content: "" },
      // A stray end arriving after cancellation must not reopen the turn; with no
      // content it is dropped entirely (no new cell), leaving the cancel intact.
      { type: "assistant:end", content: "" },
    ]);
    expect(turns(out)).toHaveLength(1);
    expect(turns(out)[0].state).toBe("cancelled");
  });

  it("a late assistant:end with content after cancel does not manufacture a phantom turn (#45)", () => {
    const out = run([
      { type: "user:add", text: "go" },
      { type: "assistant:start" },
      { type: "assistant:cancelled", content: "" },
      // Even WITH content, a terminal event with no active turn must be a no-op —
      // only user:add (and the loop's own start/token/tool events) create turns.
      { type: "assistant:end", content: "late answer" },
    ]);
    expect(turns(out)).toHaveLength(1);
    expect(turns(out)[0].state).toBe("cancelled");
    expect(turns(out)[0].assistantText).not.toContain("late answer");
  });

  it("user:pullback removes a just-added pre-stream turn (#61)", () => {
    const out = run([
      { type: "user:add", text: "draft the release notes" },
      { type: "user:pullback" },
    ]);
    // The turn is gone entirely — pull-back leaves no trace in the transcript.
    expect(turns(out)).toHaveLength(0);
    expect(out).toHaveLength(0);
  });

  it("user:pullback is a no-op once the turn is streaming (#61)", () => {
    const out = run([
      { type: "user:add", text: "go" },
      { type: "assistant:start" },
      { type: "user:pullback" },
    ]);
    // The window closed when assistant:start flipped streaming true — the turn stays.
    expect(turns(out)).toHaveLength(1);
    expect(turns(out)[0].userText).toBe("go");
    expect(turns(out)[0].state).toBe("active");
  });

  it("user:pullback is a no-op once assistant text has landed (#61)", () => {
    const out = run([
      { type: "user:add", text: "go" },
      { type: "assistant:start" },
      { type: "assistant:token", token: "On it" },
      { type: "user:pullback" },
    ]);
    expect(turns(out)).toHaveLength(1);
    expect(turns(out)[0].assistantText).toBe("On it");
  });

  it("user:pullback is a no-op once the turn has run a tool, despite the stopped caret (#61)", () => {
    const out = run([
      { type: "user:add", text: "spawn an agent" },
      { type: "assistant:start" },
      // A tool:call runs stopCaret (streaming → false) and never sets assistantText,
      // so the turn must NOT read as pre-stream — the tool already executed and
      // erasing the turn would hide it.
      { type: "tool:call", id: "c1", name: "agentTask.spawnForEdits", args: { title: "x" }, startedAt: 0 },
      { type: "user:pullback" },
    ]);
    expect(turns(out)).toHaveLength(1);
    expect(turns(out)[0].activities).toHaveLength(1);
  });

  it("user:pullback removes the pre-stream turn but keeps a later attention note (#61)", () => {
    const out = run([
      { type: "user:add", text: "watch it" },
      // A background watcher event lands after the user message but before any reply.
      { type: "attention", events: [{ title: "Tests failed", summary: "term_8" }] },
      { type: "user:pullback" },
    ]);
    // The turn is pulled back; the unrelated note survives (removed by index, not tail).
    expect(turns(out)).toHaveLength(0);
    expect(out).toHaveLength(1);
    expect(out[0].kind).toBe("note");
  });

  it("user:pullback only removes the trailing turn, never an earlier one (#61)", () => {
    const out = run([
      { type: "user:add", text: "first" },
      { type: "assistant:start" },
      { type: "assistant:end", content: "done" },
      { type: "user:add", text: "second" },
      { type: "user:pullback" },
    ]);
    // The completed first turn is untouched; only the fresh second turn is pulled.
    expect(turns(out)).toHaveLength(1);
    expect(turns(out)[0].userText).toBe("first");
    expect(turns(out)[0].state).toBe("complete");
  });

  it("user:pullback with no turns is a no-op (#61)", () => {
    const out = run([{ type: "user:pullback" }]);
    expect(out).toHaveLength(0);
  });

  it("routes attention events to standalone note cells", () => {
    const out = run([
      { type: "attention", events: [{ title: "Tests failed", summary: "term_8" }] },
    ]);
    expect(out).toHaveLength(1);
    expect(out[0].kind).toBe("note");
  });

  it("wipes the transcript on transcript:clear, then accepts a fresh confirmation card (#114)", () => {
    const out = run([
      { type: "user:add", text: "old turn" },
      { type: "assistant:end", content: "old reply" },
      { type: "attention", events: [{ title: "note", summary: "n" }] },
      { type: "transcript:clear" },
      { type: "command:add", title: "Clear", text: "Conversation cleared — starting fresh." },
    ]);
    // Everything before the clear is gone; only the post-clear confirmation remains.
    expect(out).toHaveLength(1);
    expect(out[0].kind).toBe("command");
    expect((out[0] as Extract<TranscriptCell, { kind: "command" }>).text).toContain(
      "starting fresh",
    );
  });

  it("transcript:clear on an empty transcript stays empty (#114)", () => {
    expect(run([{ type: "transcript:clear" }])).toHaveLength(0);
  });

  it("retains every cell (no front-pruning) so <Static> stays append-only", () => {
    // Finished turns are committed to the terminal's scrollback via <Static>,
    // which renders each item exactly once by COUNT — dropping cells off the
    // front would desync that count and silently lose later output. So the
    // transcript must only ever grow; the terminal, not us, bounds history.
    const actions: ControllerAction[] = Array.from({ length: 250 }, (_, i) => ({
      type: "user:add" as const,
      text: `m${i}`,
    }));
    const out = run(actions);
    expect(out).toHaveLength(250);
    expect((out[0] as TurnCell).userText).toBe("m0");
  });
});
