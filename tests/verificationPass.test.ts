import { describe, it, expect } from "vitest";
import {
  runTerminalWatcherCheck,
  runVerificationPass,
  deriveVerification,
} from "../src/daemon/watcherEngine.js";
import { VERIFICATION_EVIDENCE_PREFIX, VerificationResult } from "../src/schemas.js";
import { Db } from "../src/storage/db.js";
import { Queue } from "../src/queue.js";
import type { ToolContext } from "../src/tools/types.js";
import type { ModelRouter } from "../src/models/router.js";

function fakeRouter(classification = "still_working"): ModelRouter {
  return {
    chat: async () => ({ content: "(no change)" }),
    json: async () => ({
      classification,
      confidence: 0.7,
      summary: classification,
      evidence: [],
      recommendedAction: "none",
    }),
  } as unknown as ModelRouter;
}

type PulseResult = {
  structuredContent?: Record<string, unknown>;
  text?: string;
  isError?: boolean;
};

/**
 * MCP fake where each terminal can be configured with an agentState, and the
 * git.getProjectPulse response is controllable per test (defaults to clean).
 */
function fakeMcp(
  perTerminal: Record<string, { agentState?: string; tail?: string }>,
  pulse:
    | PulseResult
    | (() => PulseResult) = { structuredContent: { isDirty: false, changedFiles: 0 } },
  connected = true,
) {
  const pulseOf = (): PulseResult => (typeof pulse === "function" ? pulse() : pulse);
  return {
    isConnected: () => connected,
    status: () => ({ connected, transport: "injected" as const }),
    listTools: async () => [],
    callTool: async (name: string, args?: Record<string, unknown>) => {
      if (name === "terminal.getStatus") {
        const ids = Array.isArray(args?.terminalIds)
          ? (args!.terminalIds as unknown[]).map(String)
          : [];
        const terminals = ids.map((tid) => {
          const cfg = perTerminal[tid] ?? {};
          return {
            terminalId: tid,
            ...(cfg.agentState ? { agentState: cfg.agentState } : {}),
          };
        });
        return { text: "", content: [], structuredContent: { terminals }, isError: false };
      }
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
      if (name === "git.getProjectPulse") {
        const p = pulseOf();
        return {
          text: p.text ?? "",
          content: [],
          structuredContent: p.structuredContent,
          isError: p.isError ?? false,
        };
      }
      return { text: "", content: [], isError: false };
    },
  };
}

function ctxWith(
  db: Db,
  queue: Queue,
  mcp: unknown,
  router: ModelRouter = fakeRouter(),
): ToolContext {
  return {
    config: {} as ToolContext["config"],
    mcp: mcp as ToolContext["mcp"],
    db,
    queue,
    router,
    projectPath: "/tmp/p",
    actor: "watcher",
    confirm: async () => true,
    log: () => {},
  } as ToolContext;
}

function makeWatcher(db: Db, targets: string[]) {
  return db.insertWatcher({
    kind: "terminal",
    title: "verify",
    goal: "g",
    targetsJson: JSON.stringify(targets),
    cadenceMs: 10_000,
    modelTier: "small",
    status: "active",
    nextCheckAt: 0,
  });
}

describe("deriveVerification (#3)", () => {
  it("reads an explicit dirty/clean flag", () => {
    expect(deriveVerification({ isDirty: false }, "").verdict).toBe("clean");
    expect(deriveVerification({ isDirty: true }, "").verdict).toBe("dirty");
    expect(deriveVerification({ clean: true }, "").verdict).toBe("clean");
    expect(deriveVerification({ clean: false }, "").verdict).toBe("dirty");
  });

  it("derives dirtiness from a changed-file count or arrays", () => {
    expect(deriveVerification({ changedFiles: 0 }, "").verdict).toBe("clean");
    const dirty = deriveVerification({ changedFiles: 3 }, "");
    expect(dirty.verdict).toBe("dirty");
    expect(dirty.changedFiles).toBe(3);
    const grouped = deriveVerification(
      { staged: ["a.ts"], unstaged: ["b.ts", "c.ts"] },
      "",
    );
    expect(grouped.verdict).toBe("dirty");
    expect(grouped.changedFiles).toBe(3);
  });

  it("falls back to git status text markers", () => {
    expect(
      deriveVerification({}, "nothing to commit, working tree clean").verdict,
    ).toBe("clean");
    expect(
      deriveVerification({}, "Changes not staged for commit:\n  modified: x.ts")
        .verdict,
    ).toBe("dirty");
  });

  it("returns unknown when nothing is conclusive (never a false clean)", () => {
    const r = deriveVerification({}, "");
    expect(r.verdict).toBe("unknown");
    expect(r.hasGitChanges).toBe(false);
  });

  it("dirty wins over a contradictory clean flag (count > 0)", () => {
    // A self-contradictory pulse must never be read as clean.
    const r = deriveVerification({ isDirty: false, changedFiles: 3 }, "");
    expect(r.verdict).toBe("dirty");
    expect(r.changedFiles).toBe(3);
  });
});

describe("runVerificationPass (#3)", () => {
  it("returns clean for a clean pulse", async () => {
    const db = new Db(":memory:");
    const ctx = ctxWith(db, new Queue(db), fakeMcp({}));
    const r = await runVerificationPass(ctx);
    expect(r.verdict).toBe("clean");
    db.close();
  });

  it("returns dirty for a dirty pulse", async () => {
    const db = new Db(":memory:");
    const ctx = ctxWith(
      db,
      new Queue(db),
      fakeMcp({}, { structuredContent: { isDirty: true, changedFiles: 2 } }),
    );
    const r = await runVerificationPass(ctx);
    expect(r.verdict).toBe("dirty");
    expect(r.changedFiles).toBe(2);
    db.close();
  });

  it("returns unknown when the pulse call errors", async () => {
    const db = new Db(":memory:");
    const ctx = ctxWith(db, new Queue(db), fakeMcp({}, { isError: true }));
    const r = await runVerificationPass(ctx);
    expect(r.verdict).toBe("unknown");
    db.close();
  });

  it("returns unknown when MCP is disconnected (never throws)", async () => {
    const db = new Db(":memory:");
    const ctx = ctxWith(db, new Queue(db), fakeMcp({}, undefined, false));
    const r = await runVerificationPass(ctx);
    expect(r.verdict).toBe("unknown");
    db.close();
  });

  it("parses git status text when structuredContent is absent", async () => {
    const db = new Db(":memory:");
    const ctx = ctxWith(
      db,
      new Queue(db),
      fakeMcp({}, { structuredContent: undefined, text: "working tree clean" }),
    );
    const r = await runVerificationPass(ctx);
    expect(r.verdict).toBe("clean");
    db.close();
  });
});

describe("post-completion gate in runTerminalWatcherCheck (#3)", () => {
  it("a completed agent with a clean worktree becomes completed_success and stops", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(db, queue, fakeMcp({ "term-a": { agentState: "completed" } }));
    const w = makeWatcher(db, ["term-a"]);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("completed_success");
    expect(outcome.severity).toBe("done");
    expect(db.getWatcher(w.id)!.status).toBe("condition_met");

    // The clean completion event carries a parseable VerificationResult.
    const evt = queue
      .digest({ severityAtLeast: "done" })
      .find((e) => e.target?.terminalId === "term-a");
    const blob = evt?.evidence?.find((e) => e.startsWith(VERIFICATION_EVIDENCE_PREFIX));
    expect(blob).toBeDefined();
    const parsed = VerificationResult.parse(
      JSON.parse(blob!.slice(VERIFICATION_EVIDENCE_PREFIX.length)),
    );
    expect(parsed.verdict).toBe("clean");
    db.close();
  });

  it("a model-claimed completion (FSM still working) is routed through the same gate", async () => {
    // FSM is "working" but the small model classifies the tail as completed_success.
    // With a dirty worktree it must demote to completed_unverified, NOT stop with a
    // clean done event — otherwise the model bypasses the verification gate.
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(
      db,
      queue,
      fakeMcp(
        { "term-a": { agentState: "working", tail: "All done! Task complete." } },
        { structuredContent: { isDirty: true, changedFiles: 2 } },
      ),
      fakeRouter("completed_success"),
    );
    const w = makeWatcher(db, ["term-a"]);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("completed_unverified");
    expect(db.getWatcher(w.id)!.status).toBe("active");

    const evt = queue
      .digest({ severityAtLeast: "attention" })
      .find((e) => e.target?.terminalId === "term-a");
    expect(
      evt?.evidence?.some((e) => e.startsWith(VERIFICATION_EVIDENCE_PREFIX)),
    ).toBe(true);
    db.close();
  });

  it("a completed agent with a dirty worktree becomes completed_unverified and keeps polling", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(
      db,
      queue,
      fakeMcp(
        { "term-a": { agentState: "completed" } },
        { structuredContent: { isDirty: true, changedFiles: 4 } },
      ),
    );
    const w = makeWatcher(db, ["term-a"]);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("completed_unverified");
    expect(outcome.severity).toBe("attention");
    // NOT terminal — watcher keeps running so a later clean state can upgrade it.
    expect(db.getWatcher(w.id)!.status).toBe("active");

    const events = queue.digest({ severityAtLeast: "attention" });
    const evt = events.find((e) => e.target?.terminalId === "term-a");
    expect(evt).toBeDefined();
    // Recommended action points the user at the terminal to review.
    expect(evt?.recommendedActions?.[0]?.toolName).toBe("terminal.focus");
    // The structured VerificationResult is attached as evidence.
    const verEvidence = evt?.evidence?.find((e) =>
      e.startsWith(VERIFICATION_EVIDENCE_PREFIX),
    );
    expect(verEvidence).toBeDefined();
    const parsed = JSON.parse(verEvidence!.slice(VERIFICATION_EVIDENCE_PREFIX.length));
    expect(parsed.verdict).toBe("dirty");
    expect(parsed.changedFiles).toBe(4);
    db.close();
  });

  it("refreshes evidence when a deduped event re-publishes (no frozen VerificationResult)", async () => {
    // A repeated event with the same dedupeKey must carry the latest evidence so a
    // changed VerificationResult reaches the conductor instead of being frozen at
    // the first publish.
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const key = "watcher:w1:term-a:completed_unverified";
    queue.publish({
      source: "terminal_watcher",
      severity: "attention",
      title: "t",
      summary: "first",
      target: { terminalId: "term-a" },
      evidence: [`${VERIFICATION_EVIDENCE_PREFIX}${JSON.stringify({ verdict: "unknown" })}`],
      dedupeKey: key,
    });
    queue.publish({
      source: "terminal_watcher",
      severity: "attention",
      title: "t",
      summary: "second",
      target: { terminalId: "term-a" },
      evidence: [
        `${VERIFICATION_EVIDENCE_PREFIX}${JSON.stringify({ verdict: "dirty", changedFiles: 5 })}`,
      ],
      dedupeKey: key,
    });

    const evt = queue
      .digest({ severityAtLeast: "attention" })
      .find((e) => e.dedupeKey === key);
    expect(evt?.count).toBe(2); // deduped, not a second row
    const blob = evt?.evidence?.find((e) => e.startsWith(VERIFICATION_EVIDENCE_PREFIX));
    const parsed = JSON.parse(blob!.slice(VERIFICATION_EVIDENCE_PREFIX.length));
    expect(parsed.verdict).toBe("dirty");
    expect(parsed.changedFiles).toBe(5);
    db.close();
  });

  it("an unverifiable git state (MCP error) is treated as unverified, not clean", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(
      db,
      queue,
      fakeMcp({ "term-a": { agentState: "completed" } }, { isError: true }),
    );
    const w = makeWatcher(db, ["term-a"]);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("completed_unverified");
    expect(db.getWatcher(w.id)!.status).toBe("active");
    db.close();
  });
});
