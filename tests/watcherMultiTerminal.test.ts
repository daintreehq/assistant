import { describe, it, expect } from "vitest";
import {
  runTerminalWatcherCheck,
  nextOutputState,
  findModelJudge,
  hasTextCondition,
  hashTail,
} from "../src/daemon/watcherEngine.js";
import { Db } from "../src/storage/db.js";
import { Queue } from "../src/queue.js";
import type { ToolContext } from "../src/tools/types.js";
import type { ModelRouter } from "../src/models/router.js";

function fakeRouter(): ModelRouter {
  return {
    chat: async () => ({ content: "(no change)" }),
    json: async () => ({
      classification: "still_working",
      confidence: 0.7,
      summary: "still working",
      evidence: [],
      recommendedAction: "none",
    }),
  } as unknown as ModelRouter;
}

function fakeMcp(
  perTerminal: Record<
    string,
    { agentState?: string; tail?: string; recentOutput?: string }
  >,
) {
  return {
    isConnected: () => true,
    status: () => ({ connected: true, transport: "injected" as const }),
    listTools: async () => [],
    callTool: async (name: string, args?: Record<string, unknown>) => {
      // terminal.getStatus is batched: { terminalIds } -> { terminals: [...] }.
      if (name === "terminal.getStatus") {
        const ids = Array.isArray(args?.terminalIds)
          ? (args!.terminalIds as unknown[]).map(String)
          : [];
        // recentOutput is only echoed back when the caller asked for it.
        const wantOutput = Boolean(args?.includeOutput);
        const terminals = ids.map((tid) => {
          const cfg = perTerminal[tid] ?? {};
          return {
            terminalId: tid,
            ...(cfg.agentState ? { agentState: cfg.agentState } : {}),
            ...(wantOutput && cfg.recentOutput !== undefined
              ? { recentOutput: cfg.recentOutput }
              : {}),
          };
        });
        return { text: "", content: [], structuredContent: { terminals }, isError: false };
      }
      // terminal.getOutput returns scrollback under structuredContent.content.
      if (name === "terminal.getOutput") {
        const tid = String(args?.terminalId ?? "");
        const cfg = perTerminal[tid] ?? {};
        return {
          text: "",
          content: [],
          structuredContent: { terminalId: tid, content: cfg.tail ?? "" },
          isError: false,
        };
      }
      // Post-completion verification reads git cleanliness; default to clean so a
      // "completed" agent resolves to completed_success unless a test overrides.
      if (name === "git.getProjectPulse") {
        return {
          text: "",
          content: [],
          structuredContent: { isDirty: false, changedFiles: 0 },
          isError: false,
        };
      }
      return { text: "", content: [], isError: false };
    },
  };
}

function ctxWith(db: Db, queue: Queue, mcp: unknown): ToolContext {
  return {
    config: {} as ToolContext["config"],
    mcp: mcp as ToolContext["mcp"],
    db,
    queue,
    router: fakeRouter(),
    projectPath: "/tmp/p",
    actor: "watcher",
    confirm: async () => true,
    log: () => {},
  } as ToolContext;
}

describe("nextOutputState (#4)", () => {
  it("resets outAt when the tail changes, otherwise grows msSinceOutput", () => {
    const first = nextOutputState(undefined, "abc", 1000);
    expect(first.msSinceOutput).toBe(0);
    expect(first.state.outAt).toBe(1000);

    // Same tail later → time since last output keeps growing.
    const same = nextOutputState(first.state, "abc", 5000);
    expect(same.msSinceOutput).toBe(4000);
    expect(same.state.outAt).toBe(1000);

    // New tail → clock resets.
    const changed = nextOutputState(same.state, "abcd", 6000);
    expect(changed.msSinceOutput).toBe(0);
    expect(changed.state.outAt).toBe(6000);
    expect(hashTail("abcd")).not.toBe(hashTail("abc"));
  });
});

describe("findModelJudge (#15)", () => {
  it("locates a modelJudge inside composite conditions", () => {
    expect(findModelJudge({ modelJudge: "done?" })).toBe("done?");
    expect(findModelJudge({ any: [{ contains: "x" }, { modelJudge: "ready?" }] })).toBe("ready?");
    expect(findModelJudge({ not: { all: [{ modelJudge: "ok?" }] } })).toBe("ok?");
    expect(findModelJudge({ contains: "x" })).toBeUndefined();
    expect(findModelJudge(undefined)).toBeUndefined();
  });
});

describe("hasTextCondition (#23)", () => {
  it("detects contains/regex anywhere in a composite condition", () => {
    expect(hasTextCondition({ contains: "FAILED" })).toBe(true);
    expect(hasTextCondition({ regex: "err\\d+" })).toBe(true);
    expect(hasTextCondition({ any: [{ stateIs: "exited" }, { contains: "x" }] })).toBe(true);
    expect(hasTextCondition({ not: { all: [{ regex: "x" }] } })).toBe(true);
    expect(hasTextCondition({ stateIs: "completed" })).toBe(false);
    expect(hasTextCondition({ all: [{ stateIs: "exited" }, { modelJudge: "done?" }] })).toBe(false);
    expect(hasTextCondition(undefined)).toBe(false);
  });
});

describe("runTerminalWatcherCheck multi-terminal (#3)", () => {
  it("checks EVERY target terminal and publishes a per-terminal event", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const mcp = fakeMcp({
      "term-a": { agentState: "waiting" },
      "term-b": { agentState: "exited" },
    });
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "multi",
      goal: "g",
      targetsJson: JSON.stringify(["term-a", "term-b"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);

    const events = queue.digest({ severityAtLeast: "attention" });
    const terminals = events.map((e) => e.target?.terminalId).sort();
    expect(terminals).toEqual(["term-a", "term-b"]);

    // Per-terminal state was persisted.
    const after = db.getWatcher(w.id)!;
    const options = JSON.parse(after.optionsJson!);
    expect(Object.keys(options.perTerminal).sort()).toEqual(["term-a", "term-b"]);
    db.close();
  });

  it("stops only when EVERY terminal is terminal; stays active if one is still working (#stop)", async () => {
    // Both completed → watcher stops.
    {
      const db = new Db(":memory:");
      const queue = new Queue(db);
      const ctx = ctxWith(
        db,
        queue,
        fakeMcp({ "term-a": { agentState: "completed" }, "term-b": { agentState: "completed" } }),
      );
      const w = db.insertWatcher({
        kind: "terminal",
        title: "both done",
        goal: "g",
        targetsJson: JSON.stringify(["term-a", "term-b"]),
        cadenceMs: 10_000,
        modelTier: "small",
        status: "active",
        nextCheckAt: 0,
      });
      await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
      expect(db.getWatcher(w.id)!.status).toBe("condition_met");
      db.close();
    }
    // One completed, one still waiting → watcher stays active (not prematurely stopped).
    {
      const db = new Db(":memory:");
      const queue = new Queue(db);
      const ctx = ctxWith(
        db,
        queue,
        fakeMcp({ "term-a": { agentState: "completed" }, "term-b": { agentState: "waiting" } }),
      );
      const w = db.insertWatcher({
        kind: "terminal",
        title: "mixed",
        goal: "g",
        targetsJson: JSON.stringify(["term-a", "term-b"]),
        cadenceMs: 10_000,
        modelTier: "small",
        status: "active",
        nextCheckAt: 0,
      });
      await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
      expect(db.getWatcher(w.id)!.status).toBe("active");
      db.close();
    }
  });

  it("does not throw when MCP is disconnected (no per-terminal output captured)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const offline = {
      isConnected: () => false,
      status: () => ({ connected: false, transport: "none" as const }),
      listTools: async () => [],
      callTool: async () => ({ text: "", content: [], isError: false }),
    };
    const ctx = ctxWith(db, queue, offline);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "offline",
      goal: "g",
      targetsJson: JSON.stringify(["term-a", "term-b"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("needs_large_model");
    const after = db.getWatcher(w.id)!;
    const options = JSON.parse(after.optionsJson!);
    expect(Object.keys(options.perTerminal).sort()).toEqual(["term-a", "term-b"]);
    db.close();
  });

  it("batches terminal.getStatus into ONE call for N targets and threads waitingReason", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const calls: Array<{ name: string; args?: Record<string, unknown> }> = [];
    const base = fakeMcp({
      "term-a": { agentState: "waiting" },
      "term-b": { agentState: "working" },
    });
    const mcp = {
      ...base,
      callTool: async (name: string, args?: Record<string, unknown>) => {
        calls.push({ name, args });
        // term-a is waiting for a "question" specifically.
        if (name === "terminal.getStatus") {
          const ids = (args!.terminalIds as string[]).map(String);
          return {
            text: "",
            content: [],
            structuredContent: {
              terminals: ids.map((terminalId) =>
                terminalId === "term-a"
                  ? { terminalId, agentState: "waiting", waitingReason: "question" }
                  : { terminalId, agentState: "working" },
              ),
            },
            isError: false,
          };
        }
        return base.callTool(name, args);
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "batch",
      goal: "g",
      targetsJson: JSON.stringify(["term-a", "term-b"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);

    // Exactly one status call covering both terminals (not one per terminal).
    const statusCalls = calls.filter((c) => c.name === "terminal.getStatus");
    expect(statusCalls).toHaveLength(1);
    expect((statusCalls[0].args!.terminalIds as string[]).sort()).toEqual([
      "term-a",
      "term-b",
    ]);
    // The status call piggybacks a bounded recent-output tail (<=50 lines).
    expect(statusCalls[0].args!.includeOutput).toEqual({
      lines: 50,
      stripAnsi: true,
    });

    // waitingReason "question" reaches the published event's evidence.
    const events = queue.digest({ severityAtLeast: "attention" });
    const waitEvt = events.find((e) => e.target?.terminalId === "term-a");
    expect(waitEvt?.evidence?.some((x) => x.includes("question"))).toBe(true);
    db.close();
  });

  it("uses the inline recentOutput tail and skips terminal.getOutput entirely", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const calls: Array<{ name: string; args?: Record<string, unknown> }> = [];
    const base = fakeMcp({
      "term-a": { agentState: "working", recentOutput: "building module A..." },
      "term-b": { agentState: "working", recentOutput: "compiling B..." },
    });
    const mcp = {
      ...base,
      callTool: async (name: string, args?: Record<string, unknown>) => {
        calls.push({ name, args });
        return base.callTool(name, args);
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "inline-tail",
      goal: "g",
      targetsJson: JSON.stringify(["term-a", "term-b"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);

    // recentOutput satisfied the watcher → zero per-terminal getOutput calls.
    expect(calls.filter((c) => c.name === "terminal.getOutput")).toHaveLength(0);
    expect(calls.filter((c) => c.name === "terminal.getStatus")).toHaveLength(1);
    db.close();
  });

  it("falls back to terminal.getOutput when recentOutput is absent", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const calls: Array<{ name: string; args?: Record<string, unknown> }> = [];
    // No recentOutput configured → Daintree omits it → fallback per terminal.
    const base = fakeMcp({
      "term-a": { agentState: "working", tail: "deep scrollback A" },
      "term-b": { agentState: "working", tail: "deep scrollback B" },
    });
    const mcp = {
      ...base,
      callTool: async (name: string, args?: Record<string, unknown>) => {
        calls.push({ name, args });
        return base.callTool(name, args);
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "fallback-tail",
      goal: "g",
      targetsJson: JSON.stringify(["term-a", "term-b"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);

    // One getOutput per terminal since the inline tail was not provided.
    const outputCalls = calls
      .filter((c) => c.name === "terminal.getOutput")
      .map((c) => String(c.args?.terminalId))
      .sort();
    expect(outputCalls).toEqual(["term-a", "term-b"]);
    db.close();
  });

  it("reads the deep getOutput tail (not just inline) when a contains condition is set", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const calls: Array<{ name: string; args?: Record<string, unknown> }> = [];
    // Inline tail is clean; the marker only lives in the deep scrollback. A
    // contains condition must still match it, so the watcher must read deep.
    const base = fakeMcp({
      "term-a": {
        agentState: "working",
        recentOutput: "...recent clean progress lines...",
        tail: "earlier output\nFAILED: build broke\nmore lines",
      },
    });
    const mcp = {
      ...base,
      callTool: async (name: string, args?: Record<string, unknown>) => {
        calls.push({ name, args });
        return base.callTool(name, args);
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "contains-deep",
      goal: "g",
      targetsJson: JSON.stringify(["term-a"]),
      alertWhenJson: JSON.stringify({ contains: "FAILED" }),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);

    // The contains condition forced a deep read despite recentOutput present.
    expect(calls.filter((c) => c.name === "terminal.getOutput")).toHaveLength(1);
    // And the marker found in deep output produced an attention-level alert.
    const events = queue.digest({ severityAtLeast: "attention" });
    expect(events.some((e) => e.target?.terminalId === "term-a")).toBe(true);
    db.close();
  });

  it("stops and alerts when a watched terminal is closed (absent from status)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // Status call succeeds but returns NO terminals — the terminal is gone.
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "gone",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("terminal_exited");
    expect(outcome.stop).toBe(true);
    // Watcher stopped polling; an event was surfaced for the closed terminal.
    expect(db.getWatcher(w.id)!.status).not.toBe("active");
    const events = queue.digest({ severityAtLeast: "info" });
    expect(events.some((e) => e.target?.terminalId === "term-x")).toBe(true);
    db.close();
  });

  it("does NOT treat a terminal as gone when the status call itself fails", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // getStatus errors → ok:false → absence must not be read as "closed".
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "boom", content: [], isError: true };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "transient",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).not.toBe("terminal_exited");
    expect(db.getWatcher(w.id)!.status).toBe("active");
    db.close();
  });

  it("disables a watcher with corrupt target JSON and raises an error event (#14)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(db, queue, fakeMcp({}));
    const w = db.insertWatcher({
      kind: "terminal",
      title: "broken",
      goal: "g",
      targetsJson: "{not valid json",
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);

    expect(db.getWatcher(w.id)!.status).toBe("error");
    const errs = queue.digest({ severityAtLeast: "error" });
    expect(errs.some((e) => e.title.includes("watcher disabled"))).toBe(true);
    db.close();
  });
});
