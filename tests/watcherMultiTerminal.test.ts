import { describe, it, expect } from "vitest";
import {
  runTerminalWatcherCheck,
  nextOutputState,
  collectModelJudges,
  hasTextCondition,
  hashTail,
} from "../src/daemon/watcherEngine.js";
import { Db } from "../src/storage/db.js";
import { Queue } from "../src/queue.js";
import { WATCHER_SPAWN_GRACE_MS } from "../src/watcherCadence.js";
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
    {
      agentState?: string;
      waitingReason?: string;
      tail?: string;
      recentOutput?: string;
      exitCode?: number | null;
    }
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
            ...(cfg.waitingReason ? { waitingReason: cfg.waitingReason } : {}),
            ...(wantOutput && cfg.recentOutput !== undefined
              ? { recentOutput: cfg.recentOutput }
              : {}),
            ...(cfg.exitCode !== undefined ? { exitCode: cfg.exitCode } : {}),
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

describe("collectModelJudges (#57)", () => {
  it("collects a single judge from a leaf or composite condition", () => {
    expect(collectModelJudges({ modelJudge: "done?" })).toEqual(["done?"]);
    expect(
      collectModelJudges({ any: [{ contains: "x" }, { modelJudge: "ready?" }] }),
    ).toEqual(["ready?"]);
    expect(collectModelJudges({ not: { all: [{ modelJudge: "ok?" }] } })).toEqual(["ok?"]);
  });

  it("collects EVERY judge in a multi-judge group, in first-seen order", () => {
    expect(
      collectModelJudges({ all: [{ modelJudge: "a?" }, { modelJudge: "b?" }] }),
    ).toEqual(["a?", "b?"]);
    expect(
      collectModelJudges({
        any: [
          { all: [{ modelJudge: "a?" }, { contains: "x" }] },
          { not: { modelJudge: "b?" } },
        ],
      }),
    ).toEqual(["a?", "b?"]);
  });

  it("deduplicates repeated questions across and within conditions", () => {
    expect(
      collectModelJudges({ any: [{ modelJudge: "q?" }, { modelJudge: "q?" }] }),
    ).toEqual(["q?"]);
    // Across both conditions passed as separate arguments (alertWhen + stopWhen).
    expect(
      collectModelJudges({ modelJudge: "shared?" }, { modelJudge: "shared?" }),
    ).toEqual(["shared?"]);
    expect(
      collectModelJudges({ modelJudge: "a?" }, { modelJudge: "b?" }),
    ).toEqual(["a?", "b?"]);
  });

  it("returns an empty array when there is no judge", () => {
    expect(collectModelJudges({ contains: "x" })).toEqual([]);
    expect(collectModelJudges(undefined)).toEqual([]);
    expect(collectModelJudges(undefined, undefined)).toEqual([]);
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

  it("stops and alerts when a PREVIOUSLY-SEEN terminal is closed (absent from status)", async () => {
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
        // A readable, empty inventory: the terminal really is gone.
        if (name === "terminal.list") {
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
      // It was observed on a prior check, so its disappearance is a real exit.
      optionsJson: JSON.stringify({ perTerminal: { "term-x": { seen: true } } }),
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

  it("does NOT declare exited while a just-spawned terminal is still registering (spawn grace)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // Status call succeeds but the freshly-spawned terminal isn't listed yet.
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        // A readable, empty inventory: the terminal really is gone.
        if (name === "terminal.list") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "just spawned",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    // Fresh watcher (createdAt ≈ now), never-seen terminal → still registering.
    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).not.toBe("terminal_exited");
    expect(outcome.stop).toBe(false);
    expect(db.getWatcher(w.id)!.status).toBe("active");
    db.close();
  });

  it("declares exited if a never-seen terminal is still absent past the spawn grace", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        // A readable, empty inventory: the terminal really is gone.
        if (name === "terminal.list") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "never came up",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    // Age the watcher past the grace: a terminal that never registered = failed launch.
    const aged = {
      ...db.getWatcher(w.id)!,
      createdAt: Date.now() - WATCHER_SPAWN_GRACE_MS - 1_000,
    };
    const outcome = await runTerminalWatcherCheck(aged, ctx);
    expect(outcome.classification).toBe("terminal_exited");
    expect(outcome.stop).toBe(true);
    db.close();
  });

  it("treats a getStatus-absent terminal as ALIVE when terminal.list still reports it", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // getStatus omits the terminal, but terminal.list reports it alive + waiting.
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        if (name === "terminal.list") {
          return {
            text: "",
            content: [],
            structuredContent: { terminals: [{ id: "term-x", agentState: "waiting" }] },
            isError: false,
          };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "alive in list",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    // Aged past the grace, so the only reason it isn't exited is the list cross-check.
    const aged = {
      ...db.getWatcher(w.id)!,
      createdAt: Date.now() - WATCHER_SPAWN_GRACE_MS - 1_000,
    };
    const outcome = await runTerminalWatcherCheck(aged, ctx);
    expect(outcome.classification).toBe("waiting_for_input");
    expect(outcome.stop).toBe(false);
    expect(db.getWatcher(w.id)!.status).toBe("active");
    // A supervisor-grade "waiting" surfaces to the inbox so the main loop can react.
    const events = queue.digest({ severityAtLeast: "attention" });
    expect(events.some((e) => e.target?.terminalId === "term-x")).toBe(true);
    db.close();
  });

  // #38: a one-shot explore agent goes agentState=waiting the instant it finishes
  // its turn (idle at the prompt). For explore-mode watchers that end-of-turn wait
  // is completion, not a human-input block — it must route through the verification
  // gate, not fire a false attention wake. waitingReason="question" is the carve-out.
  it("explore-mode + waitingReason=prompt (getStatus) → completion, no false wake", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const mcp = fakeMcp({ "term-x": { agentState: "waiting", waitingReason: "prompt" } });
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "explore idle",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
      optionsJson: JSON.stringify({ spawnMode: "explore" }),
    });
    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    // Clean git pulse (fakeMcp default) → completed_success, severity "done".
    expect(outcome.classification).toBe("completed_success");
    // The bug was a spurious attention event — assert none surfaced.
    const events = queue.digest({ severityAtLeast: "attention" });
    expect(events.some((e) => e.target?.terminalId === "term-x")).toBe(false);
    db.close();
  });

  it("explore-mode + absent waitingReason (getStatus) → completion (not waiting_for_input)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // No waitingReason at all — still "not a question", so still completion.
    const mcp = fakeMcp({ "term-x": { agentState: "waiting" } });
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "explore idle no reason",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
      optionsJson: JSON.stringify({ spawnMode: "explore" }),
    });
    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("completed_success");
    db.close();
  });

  it("explore-mode + waitingReason=question (getStatus) → still waiting_for_input", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // An explore agent CAN genuinely ask a question — that must still wake the user.
    const mcp = fakeMcp({ "term-x": { agentState: "waiting", waitingReason: "question" } });
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "explore question",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
      optionsJson: JSON.stringify({ spawnMode: "explore" }),
    });
    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("waiting_for_input");
    const events = queue.digest({ severityAtLeast: "attention" });
    expect(events.some((e) => e.target?.terminalId === "term-x")).toBe(true);
    db.close();
  });

  it("explore-mode + waitingReason=prompt via terminal.list cross-check → completion", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // getStatus omits the terminal; terminal.list reports it waiting at the prompt.
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        if (name === "terminal.list") {
          return {
            text: "",
            content: [],
            structuredContent: {
              terminals: [{ id: "term-x", agentState: "waiting", waitingReason: "prompt" }],
            },
            isError: false,
          };
        }
        if (name === "git.getProjectPulse") {
          return {
            text: "",
            content: [],
            structuredContent: { isDirty: false, changedFiles: 0 },
            isError: false,
          };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "explore idle in list",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
      optionsJson: JSON.stringify({ spawnMode: "explore" }),
    });
    // Aged past the grace, so the only reason it isn't exited is the list cross-check.
    const aged = {
      ...db.getWatcher(w.id)!,
      createdAt: Date.now() - WATCHER_SPAWN_GRACE_MS - 1_000,
    };
    const outcome = await runTerminalWatcherCheck(aged, ctx);
    expect(outcome.classification).toBe("completed_success");
    const events = queue.digest({ severityAtLeast: "attention" });
    expect(events.some((e) => e.target?.terminalId === "term-x")).toBe(false);
    db.close();
  });

  it("explore-mode completion is terminal even when the worktree is dirty (no infinite poll)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // Pre-existing dirty worktree: an explore agent is read-only and can never clean
    // it, so routing through git-verification would loop on completed_unverified
    // forever. Explore completion must be terminal regardless of git state.
    const mcp = fakeMcp({ "term-x": { agentState: "waiting", waitingReason: "prompt" } });
    const origCall = mcp.callTool;
    mcp.callTool = async (name: string, a?: Record<string, unknown>) => {
      if (name === "git.getProjectPulse") {
        return {
          text: "",
          content: [],
          structuredContent: { isDirty: true, changedFiles: 3 },
          isError: false,
        };
      }
      return origCall(name, a);
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "explore dirty",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
      optionsJson: JSON.stringify({
        spawnMode: "explore",
        verificationScope: { worktreeId: "wt-x" },
      }),
    });
    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    // Terminal completion — NOT completed_unverified (which would keep polling).
    expect(outcome.classification).toBe("completed_success");
    db.close();
  });

  it("edit-mode + waitingReason=prompt stays waiting_for_input (explore routing does not leak)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const mcp = fakeMcp({ "term-x": { agentState: "waiting", waitingReason: "prompt" } });
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "edit idle",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
      optionsJson: JSON.stringify({ spawnMode: "edit" }),
    });
    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("waiting_for_input");
    const events = queue.digest({ severityAtLeast: "attention" });
    expect(events.some((e) => e.target?.terminalId === "term-x")).toBe(true);
    db.close();
  });

  it("reads scrollback via getOutput and model-classifies a listed-but-omitted WORKING terminal", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    let getOutputCalls = 0;
    // getStatus omits the terminal; terminal.list reports it alive and plain
    // "working" (the case the old code short-circuited to no_change without ever
    // reading output). terminal.getOutput still has the scrollback.
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        if (name === "terminal.list") {
          return {
            text: "",
            content: [],
            structuredContent: { terminals: [{ id: "term-x", agentState: "working" }] },
            isError: false,
          };
        }
        if (name === "terminal.getOutput") {
          getOutputCalls += 1;
          return {
            text: "",
            content: [],
            structuredContent: { content: "npm ERR! command failed\n" },
            isError: false,
          };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    // A distinctive model verdict proves classifyWithModel was actually reached —
    // the prior bug never consulted the model for these terminals (it returned
    // no_change with tail: "").
    ctx.router = {
      chat: async () => ({ content: "" }),
      json: async () => ({
        classification: "command_failed",
        confidence: 0.9,
        summary: "A command failed.",
        evidence: ["npm ERR!"],
        recommendedAction: "none",
      }),
    } as unknown as ModelRouter;
    const w = db.insertWatcher({
      kind: "terminal",
      title: "working in list",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });
    const aged = {
      ...db.getWatcher(w.id)!,
      createdAt: Date.now() - WATCHER_SPAWN_GRACE_MS - 1_000,
    };
    const outcome = await runTerminalWatcherCheck(aged, ctx);
    // Scrollback was read once and fed through the small model.
    expect(getOutputCalls).toBe(1);
    expect(outcome.classification).toBe("command_failed");
    db.close();
  });

  it("falls back to no_change when getOutput is empty for a listed-but-omitted WORKING terminal", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    let getOutputCalls = 0;
    // Same listed-but-omitted working terminal, but Daintree returns no scrollback
    // even via getOutput. The watcher must still ATTEMPT the read (proving the new
    // path was entered, not the old tail: "" short-circuit) yet degrade gracefully
    // to no_change without ever consulting the model on empty output.
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        if (name === "terminal.list") {
          return {
            text: "",
            content: [],
            structuredContent: { terminals: [{ id: "term-x", agentState: "working" }] },
            isError: false,
          };
        }
        if (name === "terminal.getOutput") {
          getOutputCalls += 1;
          return { text: "", content: [], structuredContent: { content: "" }, isError: false };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    // The model guard (tail.trim().length > 0) must prevent any model call when the
    // scrollback is empty — make that explicit by throwing if json() is reached.
    ctx.router = {
      chat: async () => ({ content: "" }),
      json: async () => {
        throw new Error("model must not be consulted on empty scrollback");
      },
    } as unknown as ModelRouter;
    const w = db.insertWatcher({
      kind: "terminal",
      title: "working in list, no scrollback",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });
    const aged = {
      ...db.getWatcher(w.id)!,
      createdAt: Date.now() - WATCHER_SPAWN_GRACE_MS - 1_000,
    };
    const outcome = await runTerminalWatcherCheck(aged, ctx);
    // The read was attempted (the new code path), but the model guard held.
    expect(getOutputCalls).toBe(1);
    expect(outcome.classification).toBe("no_change");
    db.close();
  });

  it("routes a model-claimed completion through the git gate on the listed-but-omitted path", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // Listed-but-omitted working terminal whose scrollback makes the small model
    // claim completed_success. The git gate must still run (and, finding a dirty
    // tree, demote to completed_unverified) — a model claim cannot bypass it here
    // any more than it can on the normal path.
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        if (name === "terminal.list") {
          return {
            text: "",
            content: [],
            structuredContent: { terminals: [{ id: "term-x", agentState: "working" }] },
            isError: false,
          };
        }
        if (name === "terminal.getOutput") {
          return { text: "", content: [], structuredContent: { content: "All done.\n" }, isError: false };
        }
        if (name === "git.getProjectPulse") {
          // Uncommitted work remains — completion is NOT yet trustworthy.
          return {
            text: "",
            content: [],
            structuredContent: { isDirty: true, changedFiles: 2 },
            isError: false,
          };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    ctx.router = {
      chat: async () => ({ content: "" }),
      json: async () => ({
        classification: "completed_success",
        confidence: 0.9,
        summary: "Agent reports done.",
        evidence: [],
        recommendedAction: "none",
      }),
    } as unknown as ModelRouter;
    const w = db.insertWatcher({
      kind: "terminal",
      title: "claims done in list",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });
    const aged = {
      ...db.getWatcher(w.id)!,
      createdAt: Date.now() - WATCHER_SPAWN_GRACE_MS - 1_000,
    };
    const outcome = await runTerminalWatcherCheck(aged, ctx);
    expect(outcome.classification).toBe("completed_unverified");
    db.close();
  });

  it("declares exited only when terminal.list ALSO reports the terminal exited", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        if (name === "terminal.list") {
          return {
            text: "",
            content: [],
            structuredContent: { terminals: [{ id: "term-x", agentState: "exited", exitCode: 1 }] },
            isError: false,
          };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "exited in list",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("terminal_exited");
    expect(outcome.stop).toBe(true);
    db.close();
  });

  it("does NOT declare exited when getStatus omits the terminal AND terminal.list is unreadable", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // getStatus omits term-x; terminal.list ERRORS — we cannot prove an exit.
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        if (name === "terminal.list") {
          return { text: "boom", content: [], isError: true };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "list unreadable",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
      // Already seen + aged past grace: only the unreadable-list guard keeps it alive.
      optionsJson: JSON.stringify({ perTerminal: { "term-x": { seen: true } } }),
    });
    const aged = {
      ...db.getWatcher(w.id)!,
      createdAt: Date.now() - WATCHER_SPAWN_GRACE_MS - 1_000,
    };

    const outcome = await runTerminalWatcherCheck(aged, ctx);
    expect(outcome.classification).not.toBe("terminal_exited");
    expect(outcome.stop).toBe(false);
    expect(db.getWatcher(w.id)!.status).toBe("active");
    db.close();
  });

  it("treats a non-errored but unparseable terminal.list as unreadable (stays alive)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // isError:false, but NO recognizable `terminals` array → ok:false → can't prove exit.
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        if (name === "terminal.list") {
          return { text: "not json", content: [], structuredContent: { stuff: 1 }, isError: false };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "unparseable list",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
      optionsJson: JSON.stringify({ perTerminal: { "term-x": { seen: true } } }),
    });
    const aged = {
      ...db.getWatcher(w.id)!,
      createdAt: Date.now() - WATCHER_SPAWN_GRACE_MS - 1_000,
    };

    const outcome = await runTerminalWatcherCheck(aged, ctx);
    expect(outcome.classification).not.toBe("terminal_exited");
    expect(db.getWatcher(w.id)!.status).toBe("active");
    db.close();
  });

  it("classifies a mixed batch: one present in getStatus, one only in terminal.list", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    let listCalls = 0;
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string, args?: Record<string, unknown>) => {
        if (name === "terminal.getStatus") {
          const ids = Array.isArray(args?.terminalIds)
            ? (args!.terminalIds as unknown[]).map(String)
            : [];
          // Only term-a is reported by getStatus; term-b is omitted.
          const terminals = ids
            .filter((id) => id === "term-a")
            .map((id) => ({ terminalId: id, agentState: "waiting" }));
          return { text: "", content: [], structuredContent: { terminals }, isError: false };
        }
        if (name === "terminal.list") {
          listCalls += 1;
          return {
            text: "",
            content: [],
            structuredContent: {
              terminals: [
                { id: "term-a", agentState: "waiting" },
                { id: "term-b", agentState: "waiting" },
              ],
            },
            isError: false,
          };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "mixed sources",
      goal: "g",
      targetsJson: JSON.stringify(["term-a", "term-b"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    // Neither terminal is exited; the watcher stays active.
    expect(outcome.classification).not.toBe("terminal_exited");
    expect(db.getWatcher(w.id)!.status).toBe("active");
    // terminal.list is consulted once for the whole batch, not per missing target.
    expect(listCalls).toBe(1);
    db.close();
  });

  it("promotes a supervisor's clean completion to attention so it surfaces", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // term-x completed cleanly; git pulse is clean → completed_success (severity "done").
    const mcp = fakeMcp({ "term-x": { agentState: "completed" } });
    const ctx = ctxWith(db, queue, mcp);
    const w = db.insertWatcher({
      kind: "terminal",
      title: "supervised done",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      isSupervisor: true,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    // "done" sits below the scheduler's surfacing threshold; the supervisor bump
    // promotes the published event to >= attention so onAttention/the wake-up sees it.
    const surfaced = queue.digest({ severityAtLeast: "attention" });
    expect(surfaced.some((e) => e.target?.terminalId === "term-x")).toBe(true);
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

  it("surfaces a nonzero exitCode as evidence on a terminal_exited event (#22)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(db, queue, fakeMcp({ "term-a": { agentState: "exited", exitCode: 1 } }));
    const w = db.insertWatcher({
      kind: "terminal",
      title: "exit-fail",
      goal: "g",
      targetsJson: JSON.stringify(["term-a"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("terminal_exited");
    expect(outcome.evidence).toContain("agentState=exited");
    expect(outcome.evidence).toContain("exitCode=1 (nonzero)");
    // The evidence reaches the published queue event, not just the outcome.
    const evt = queue
      .digest({ severityAtLeast: "info" })
      .find((e) => e.target?.terminalId === "term-a");
    expect(evt?.evidence).toContain("exitCode=1 (nonzero)");
    db.close();
  });

  it("does NOT add exitCode evidence for a clean (0) or absent exit (#22)", async () => {
    for (const cfg of [
      { agentState: "exited", exitCode: 0 },
      { agentState: "exited" },
      { agentState: "exited", exitCode: null as number | null },
    ]) {
      const db = new Db(":memory:");
      const queue = new Queue(db);
      const ctx = ctxWith(db, queue, fakeMcp({ "term-a": cfg }));
      const w = db.insertWatcher({
        kind: "terminal",
        title: "exit-clean",
        goal: "g",
        targetsJson: JSON.stringify(["term-a"]),
        cadenceMs: 10_000,
        modelTier: "small",
        status: "active",
        nextCheckAt: 0,
      });

      const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
      expect(outcome.classification).toBe("terminal_exited");
      expect(outcome.evidence).toEqual(["agentState=exited"]);
      expect(outcome.evidence.some((e) => e.includes("exitCode"))).toBe(false);
      db.close();
    }
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

  // #60 regression: a single watcher+terminal whose classification evolves across
  // check cycles must update ONE live inbox item in place — not leave a stale row
  // per classification. The dedupeKey is keyed on watcher+terminal only, so the
  // second publish bumps the first row's count and refreshes its title/summary.
  it("updates one live inbox item in place as a terminal's classification changes (#60)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    // Mutable per-terminal config so we can flip the terminal's state between cycles.
    const cfg: Record<string, { agentState?: string; waitingReason?: string }> = {
      "term-x": { agentState: "waiting", waitingReason: "question" },
    };
    const ctx = ctxWith(db, queue, fakeMcp(cfg));
    const w = db.insertWatcher({
      kind: "terminal",
      title: "evolving",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 10_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
    });

    // Cycle 1: the agent is waiting on a question → waiting_for_input.
    const first = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(first.classification).toBe("waiting_for_input");

    // Cycle 2: the agent has now completed → completed_success (clean git pulse).
    cfg["term-x"] = { agentState: "completed" };
    const second = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(second.classification).toBe("completed_success");

    // Exactly ONE open inbox item for the terminal — the classification change
    // updated it in place rather than spawning a second stale row.
    const events = queue.digest().filter((e) => e.target?.terminalId === "term-x");
    expect(events.length).toBe(1);
    expect(events[0].count).toBe(2);
    // Title tracks the latest state, not the first classification.
    expect(events[0].title).toBe("evolving: completed success");
    expect(events[0].title).not.toContain("waiting");
    // The stale "Focus terminal" action from waiting_for_input must not cling to
    // the now-completed item — completed_success carries no recommended actions.
    expect(events[0].recommendedActions ?? []).toEqual([]);
    db.close();
  });
});

// A read failure (Daintree errored / the call threw) must be distinguishable from
// a genuinely silent terminal: it must not advance noOutputForMs, must not invoke
// the small model on stale/empty output, and must be counted so the engine can
// back off. Separately, the model must be skipped entirely when the inputs that
// determine its answer (agentState, exitCode, output hash) are unchanged.
describe("watcher read-failure vs silence + classify dedup (#56)", () => {
  /** A working terminal present in getStatus but with no inline tail, so the
   *  watcher takes the deep terminal.getOutput path — whose result we control. */
  function mcpDeepRead(getOutput: () => { content?: string; isError?: boolean }) {
    return {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string, args?: Record<string, unknown>) => {
        if (name === "terminal.getStatus") {
          const ids = (args!.terminalIds as string[]).map(String);
          return {
            text: "",
            content: [],
            structuredContent: {
              terminals: ids.map((terminalId) => ({ terminalId, agentState: "working" })),
            },
            isError: false,
          };
        }
        if (name === "terminal.getOutput") {
          const r = getOutput();
          return {
            text: "",
            content: [],
            structuredContent: r.isError ? {} : { content: r.content ?? "" },
            isError: Boolean(r.isError),
          };
        }
        return { text: "", content: [], isError: false };
      },
    };
  }

  function insertTermWatcher(db: Db, extra?: Record<string, unknown>) {
    return db.insertWatcher({
      kind: "terminal",
      title: "t",
      goal: "g",
      targetsJson: JSON.stringify(["term-x"]),
      cadenceMs: 3_000,
      modelTier: "small",
      status: "active",
      nextCheckAt: 0,
      ...extra,
    });
  }

  const termState = (db: Db, id: string) =>
    JSON.parse(db.getWatcher(id)!.optionsJson!).perTerminal["term-x"];

  it("a failed deep read is a re-check, not silence: skips the model and counts the failure", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    let jsonCalls = 0;
    const ctx = ctxWith(db, queue, mcpDeepRead(() => ({ isError: true })));
    ctx.router = {
      chat: async () => ({ content: "" }),
      json: async () => {
        jsonCalls += 1;
        throw new Error("model must not be consulted on a failed read");
      },
    } as unknown as ModelRouter;
    const w = insertTermWatcher(db);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);

    expect(jsonCalls).toBe(0);
    expect(outcome.classification).toBe("no_change");
    expect(termState(db, w.id).readFailures).toBe(1);
    db.close();
  });

  it("read failures freeze outAt and accumulate, then reset on a successful read", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    let mode: "ok" | "fail" = "ok";
    const ctx = ctxWith(
      db,
      queue,
      mcpDeepRead(() => (mode === "fail" ? { isError: true } : { content: "building..." })),
    );
    const w = insertTermWatcher(db);

    // 1) Successful read records outAt and a zero failure count.
    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    const after1 = termState(db, w.id);
    expect(after1.readFailures).toBe(0);
    const frozenOutAt = after1.outAt;
    expect(typeof frozenOutAt).toBe("number");

    // 2+3) Consecutive failures accumulate and do NOT advance outAt.
    mode = "fail";
    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(termState(db, w.id).readFailures).toBe(1);
    expect(termState(db, w.id).outAt).toBe(frozenOutAt);
    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(termState(db, w.id).readFailures).toBe(2);
    expect(termState(db, w.id).outAt).toBe(frozenOutAt);

    // 4) A successful read clears the counter.
    mode = "ok";
    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(termState(db, w.id).readFailures).toBe(0);
    db.close();
  });

  it("repeated read failures do not trip a noOutputForMs stop condition", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(db, queue, mcpDeepRead(() => ({ isError: true })));
    ctx.router = {
      chat: async () => ({ content: "" }),
      json: async () => {
        throw new Error("model must not be consulted on a failed read");
      },
    } as unknown as ModelRouter;
    // A 1ms silence threshold would trip on the SECOND consecutive empty read if a
    // read failure were treated as silence (the pre-fix bug).
    const w = insertTermWatcher(db, { stopWhenJson: JSON.stringify({ noOutputForMs: 1 }) });

    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(db.getWatcher(w.id)!.status).toBe("active");
    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(db.getWatcher(w.id)!.status).toBe("active");
    expect(termState(db, w.id).readFailures).toBe(2);
    db.close();
  });

  it("skips the model when agentState, exitCode and output hash are unchanged", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    let jsonCalls = 0;
    const ctx = ctxWith(
      db,
      queue,
      fakeMcp({ "term-x": { agentState: "working", recentOutput: "compiling module..." } }),
    );
    ctx.router = {
      chat: async () => ({ content: "" }),
      json: async () => {
        jsonCalls += 1;
        return {
          classification: "still_working",
          confidence: 0.7,
          summary: "still",
          evidence: [],
          recommendedAction: "none",
        };
      },
    } as unknown as ModelRouter;
    const w = insertTermWatcher(db);

    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(jsonCalls).toBe(1);
    expect(typeof termState(db, w.id).lastClassifyKey).toBe("string");

    // Identical inputs on the next tick → the model is not consulted again.
    const outcome2 = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(jsonCalls).toBe(1);
    expect(outcome2.classification).toBe("no_change");
    db.close();
  });

  it("re-invokes the model when the output changes (hash component of the key)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    let jsonCalls = 0;
    let recent = "line one";
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string, args?: Record<string, unknown>) => {
        if (name === "terminal.getStatus") {
          const ids = (args!.terminalIds as string[]).map(String);
          return {
            text: "",
            content: [],
            structuredContent: {
              terminals: ids.map((terminalId) => ({
                terminalId,
                agentState: "working",
                recentOutput: recent,
              })),
            },
            isError: false,
          };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    ctx.router = {
      chat: async () => ({ content: "" }),
      json: async () => {
        jsonCalls += 1;
        return {
          classification: "still_working",
          confidence: 0.7,
          summary: "still",
          evidence: [],
          recommendedAction: "none",
        };
      },
    } as unknown as ModelRouter;
    const w = insertTermWatcher(db);

    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(jsonCalls).toBe(1);
    // New output → key changes → the model runs again.
    recent = "line two";
    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(jsonCalls).toBe(2);
    db.close();
  });

  it("re-invokes the model when agentState changes even if output is unchanged", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    let jsonCalls = 0;
    let agentState = "working";
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string, args?: Record<string, unknown>) => {
        if (name === "terminal.getStatus") {
          const ids = (args!.terminalIds as string[]).map(String);
          return {
            text: "",
            content: [],
            structuredContent: {
              terminals: ids.map((terminalId) => ({
                terminalId,
                agentState,
                recentOutput: "same output",
              })),
            },
            isError: false,
          };
        }
        return { text: "", content: [], structuredContent: { content: "" }, isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    ctx.router = {
      chat: async () => ({ content: "" }),
      json: async () => {
        jsonCalls += 1;
        return {
          classification: "still_working",
          confidence: 0.7,
          summary: "still",
          evidence: [],
          recommendedAction: "none",
        };
      },
    } as unknown as ModelRouter;
    const w = insertTermWatcher(db);

    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(jsonCalls).toBe(1);
    // Same output but a different agentState (still non-terminal) → re-classify.
    agentState = "busy";
    await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(jsonCalls).toBe(2);
    db.close();
  });

  it("a failed deep read on the listed-but-omitted path skips the model and counts the failure", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    let jsonCalls = 0;
    // getStatus omits the terminal; terminal.list reports it working; the deep
    // getOutput read then fails — the listed-but-omitted path must treat that as a
    // re-check, not classify on an empty tail.
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string) => {
        if (name === "terminal.getStatus") {
          return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
        }
        if (name === "terminal.list") {
          return {
            text: "",
            content: [],
            structuredContent: { terminals: [{ id: "term-x", agentState: "working" }] },
            isError: false,
          };
        }
        if (name === "terminal.getOutput") {
          return { text: "", content: [], structuredContent: {}, isError: true };
        }
        return { text: "", content: [], isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    ctx.router = {
      chat: async () => ({ content: "" }),
      json: async () => {
        jsonCalls += 1;
        throw new Error("model must not be consulted on a failed read");
      },
    } as unknown as ModelRouter;
    const w = insertTermWatcher(db);
    const aged = {
      ...db.getWatcher(w.id)!,
      createdAt: Date.now() - WATCHER_SPAWN_GRACE_MS - 1_000,
    };

    const outcome = await runTerminalWatcherCheck(aged, ctx);

    expect(jsonCalls).toBe(0);
    expect(outcome.classification).toBe("no_change");
    expect(termState(db, w.id).readFailures).toBe(1);
    db.close();
  });

  it("a thrown deep read (not just isError) is counted as a failure", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const mcp = {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string, args?: Record<string, unknown>) => {
        if (name === "terminal.getStatus") {
          const ids = (args!.terminalIds as string[]).map(String);
          return {
            text: "",
            content: [],
            structuredContent: {
              terminals: ids.map((terminalId) => ({ terminalId, agentState: "working" })),
            },
            isError: false,
          };
        }
        if (name === "terminal.getOutput") {
          throw new Error("transport blew up");
        }
        return { text: "", content: [], isError: false };
      },
    };
    const ctx = ctxWith(db, queue, mcp);
    ctx.router = {
      chat: async () => ({ content: "" }),
      json: async () => {
        throw new Error("model must not be consulted on a failed read");
      },
    } as unknown as ModelRouter;
    const w = insertTermWatcher(db);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);

    expect(outcome.classification).toBe("no_change");
    expect(termState(db, w.id).readFailures).toBe(1);
    db.close();
  });

  /** getStatus reports the terminal working with a tail the model calls done; the
   *  git pulse cleanliness is controlled per call so a completed_unverified can
   *  later resolve to completed_success without any new terminal output. */
  function mcpCompletion(getStatusShape: "normal" | "listed", dirty: () => boolean) {
    return {
      isConnected: () => true,
      status: () => ({ connected: true, transport: "injected" as const }),
      listTools: async () => [],
      callTool: async (name: string, args?: Record<string, unknown>) => {
        if (name === "terminal.getStatus") {
          if (getStatusShape === "listed") {
            return { text: "", content: [], structuredContent: { terminals: [] }, isError: false };
          }
          const ids = (args!.terminalIds as string[]).map(String);
          return {
            text: "",
            content: [],
            structuredContent: {
              terminals: ids.map((terminalId) => ({
                terminalId,
                agentState: "working",
                recentOutput: "all done!",
              })),
            },
            isError: false,
          };
        }
        if (name === "terminal.list") {
          return {
            text: "",
            content: [],
            structuredContent: { terminals: [{ id: "term-x", agentState: "working" }] },
            isError: false,
          };
        }
        if (name === "terminal.getOutput") {
          return { text: "", content: [], structuredContent: { content: "all done!" }, isError: false };
        }
        if (name === "git.getProjectPulse") {
          const d = dirty();
          return {
            text: "",
            content: [],
            structuredContent: { isDirty: d, changedFiles: d ? 2 : 0 },
            isError: false,
          };
        }
        return { text: "", content: [], isError: false };
      },
    };
  }

  const completionRouter = () =>
    ({
      chat: async () => ({ content: "" }),
      json: async () => ({
        classification: "completed_success",
        confidence: 0.9,
        summary: "done",
        evidence: ["all done"],
        recommendedAction: "none",
      }),
    }) as unknown as ModelRouter;

  it("re-runs the git gate when a completed_unverified worktree is later cleaned (normal path)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    let dirty = true;
    const ctx = ctxWith(db, queue, mcpCompletion("normal", () => dirty));
    ctx.router = completionRouter();
    const w = insertTermWatcher(db);

    // Tick 1: dirty worktree → gate demotes completed_success → completed_unverified,
    // watcher stays active (completion is not yet trusted).
    const o1 = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(o1.classification).toBe("completed_unverified");
    expect(db.getWatcher(w.id)!.status).toBe("active");

    // Tick 2: identical tail but the tree is now clean. The dedup must NOT suppress
    // this — the key was deliberately not latched on a completed_success verdict —
    // so the gate re-runs and the watcher reaches completed_success.
    dirty = false;
    const o2 = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(o2.classification).toBe("completed_success");
    db.close();
  });

  it("re-runs the git gate when a completed_unverified worktree is cleaned (listed-but-omitted path)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    let dirty = true;
    const ctx = ctxWith(db, queue, mcpCompletion("listed", () => dirty));
    ctx.router = completionRouter();
    const w = insertTermWatcher(db);

    const o1 = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(o1.classification).toBe("completed_unverified");
    expect(db.getWatcher(w.id)!.status).toBe("active");

    dirty = false;
    const o2 = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(o2.classification).toBe("completed_success");
    db.close();
  });
});
