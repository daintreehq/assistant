import {
  timelineReducer,
  type ControllerAction,
} from "../src/ui/hooks/useDaintreeController.js";
import type { TimelineItem } from "../src/ui/types.js";

function run(actions: ControllerAction[]): TimelineItem[] {
  return actions.reduce<TimelineItem[]>((s, a) => timelineReducer(s, a), []);
}

describe("timelineReducer", () => {
  it("accumulates streamed tokens into one assistant row", () => {
    const out = run([
      { type: "assistant:start" },
      { type: "assistant:token", token: "He" },
      { type: "assistant:token", token: "llo" },
      { type: "assistant:end", content: "Hello" },
    ]);
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({
      kind: "assistant",
      text: "Hello",
      streaming: false,
    });
  });

  it("drops an empty streaming row when a tool call starts (no blank ▌ row)", () => {
    const out = run([
      { type: "assistant:start" },
      { type: "tool:call", name: "fs.read", args: {} },
    ]);
    expect(out).toHaveLength(1);
    expect(out[0].kind).toBe("tool");
  });

  it("finalizes (keeps) pre-tool assistant text, then adds the tool row", () => {
    const out = run([
      { type: "assistant:start" },
      { type: "assistant:token", token: "Let me look…" },
      { type: "tool:call", name: "fs.search", args: {} },
    ]);
    expect(out).toHaveLength(2);
    expect(out[0]).toMatchObject({
      kind: "assistant",
      text: "Let me look…",
      streaming: false,
    });
    expect(out[1].kind).toBe("tool");
  });

  it("matches tool:result to the pending tool row by name", () => {
    const out = run([
      { type: "tool:call", name: "fs.read", args: {} },
      { type: "tool:result", name: "fs.read", result: { ok: true, summary: "read 1 file" } },
    ]);
    const tool = out[0];
    expect(tool.kind === "tool" && tool.ok).toBe(true);
    expect(tool.kind === "tool" && tool.summary).toBe("read 1 file");
  });

  it("clears a stale caret when an error log arrives", () => {
    const out = run([
      { type: "assistant:start" },
      { type: "log", level: "error", message: "Model error: boom" },
    ]);
    expect(out).toHaveLength(1);
    expect(out[0]).toMatchObject({ kind: "system", level: "error" });
  });

  it("caps the timeline length", () => {
    const actions: ControllerAction[] = Array.from({ length: 250 }, (_, i) => ({
      type: "user:add" as const,
      text: `m${i}`,
    }));
    expect(run(actions).length).toBeLessThanOrEqual(200);
  });
});
