import { describe, it, expect } from "vitest";
import {
  runTerminalWatcherCheck,
  runVerificationPass,
  deriveVerification,
} from "../src/daemon/watcherEngine.js";
import {
  VERIFICATION_EVIDENCE_PREFIX,
  VerificationResult,
  ModelJudgeAnswer,
} from "../src/schemas.js";
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

function makeWatcher(db: Db, targets: string[], optionsJson?: string) {
  return db.insertWatcher({
    kind: "terminal",
    title: "verify",
    goal: "g",
    targetsJson: JSON.stringify(targets),
    cadenceMs: 10_000,
    modelTier: "small",
    status: "active",
    nextCheckAt: 0,
    ...(optionsJson ? { optionsJson } : {}),
  });
}

/** Router whose json() answers a modelJudge with a fixed ModelJudgeAnswer shape —
 *  used to drive the acceptance-contract gate deterministically. */
function fakeJudgeRouter(
  matched: boolean,
  confidence = 0.9,
  reason = "Acceptance judged.",
): ModelRouter {
  return {
    chat: async () => ({ content: "(no change)" }),
    json: async () => ({ reason, confidence, matched }),
  } as unknown as ModelRouter;
}

/** Router that serves BOTH the tail classifier (a WatcherVerdict) and the acceptance
 *  judge (a ModelJudgeAnswer) from one fake, branching on the requested schema — so a
 *  model-CLAIMED completion can be routed through the contract gate in one test. */
function fakeClassifyAndJudgeRouter(
  judgeMatched: boolean,
  judgeConfidence = 0.9,
): ModelRouter {
  return {
    chat: async () => ({ content: "(no change)" }),
    json: async (_tier: unknown, _opts: unknown, schema: unknown) => {
      if (schema === ModelJudgeAnswer) {
        return { reason: "judged", confidence: judgeConfidence, matched: judgeMatched };
      }
      return {
        classification: "completed_success",
        confidence: 0.7,
        summary: "looks done",
        evidence: [],
        recommendedAction: "none",
      };
    },
  } as unknown as ModelRouter;
}

describe("deriveVerification (#3)", () => {
  it("reads an explicit dirty/clean flag (clean -> verified, dirty -> unknown)", () => {
    // A clean tree is "verified" git artifact state; a dirty tree is "unknown"
    // (uncommitted work is normal after edits, not a failure) — issue #83.
    expect(deriveVerification({ isDirty: false }, "").verdict).toBe("verified");
    expect(deriveVerification({ isDirty: true }, "").verdict).toBe("unknown");
    expect(deriveVerification({ clean: true }, "").verdict).toBe("verified");
    expect(deriveVerification({ clean: false }, "").verdict).toBe("unknown");
  });

  it("derives dirtiness from a changed-file count or arrays", () => {
    expect(deriveVerification({ changedFiles: 0 }, "").verdict).toBe("verified");
    const dirty = deriveVerification({ changedFiles: 3 }, "");
    expect(dirty.verdict).toBe("unknown");
    expect(dirty.hasGitChanges).toBe(true);
    expect(dirty.changedFiles).toBe(3);
    const grouped = deriveVerification(
      { staged: ["a.ts"], unstaged: ["b.ts", "c.ts"] },
      "",
    );
    expect(grouped.verdict).toBe("unknown");
    expect(grouped.changedFiles).toBe(3);
    // The evidence bundle lists the changed paths, deduped in first-seen order.
    expect(grouped.changedFileList).toEqual(["a.ts", "b.ts", "c.ts"]);
  });

  it("falls back to git status text markers", () => {
    expect(
      deriveVerification({}, "nothing to commit, working tree clean").verdict,
    ).toBe("verified");
    expect(
      deriveVerification({}, "Changes not staged for commit:\n  modified: x.ts")
        .verdict,
    ).toBe("unknown");
  });

  it("returns unknown when nothing is conclusive (never a false verified)", () => {
    const r = deriveVerification({}, "");
    expect(r.verdict).toBe("unknown");
    expect(r.hasGitChanges).toBe(false);
  });

  it("dirty wins over a contradictory clean flag (count > 0)", () => {
    // A self-contradictory pulse must never be read as clean/verified.
    const r = deriveVerification({ isDirty: false, changedFiles: 3 }, "");
    expect(r.verdict).toBe("unknown");
    expect(r.hasGitChanges).toBe(true);
    expect(r.changedFiles).toBe(3);
  });
});

describe("runVerificationPass (#3)", () => {
  it("returns verified for a clean pulse", async () => {
    const db = new Db(":memory:");
    const ctx = ctxWith(db, new Queue(db), fakeMcp({}));
    const r = await runVerificationPass(ctx);
    expect(r.verdict).toBe("verified");
    db.close();
  });

  it("returns unknown (with git changes) for a dirty pulse", async () => {
    const db = new Db(":memory:");
    const ctx = ctxWith(
      db,
      new Queue(db),
      fakeMcp({}, { structuredContent: { isDirty: true, changedFiles: 2 } }),
    );
    const r = await runVerificationPass(ctx);
    expect(r.verdict).toBe("unknown");
    expect(r.hasGitChanges).toBe(true);
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
    expect(r.verdict).toBe("verified");
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
    expect(parsed.verdict).toBe("verified");
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
    expect(parsed.verdict).toBe("unknown");
    expect(parsed.hasGitChanges).toBe(true);
    expect(parsed.changedFiles).toBe(4);
    db.close();
  });

  it("refreshes evidence when a deduped event re-publishes (no frozen VerificationResult)", async () => {
    // A repeated event with the same dedupeKey must carry the latest evidence so a
    // changed VerificationResult reaches the conductor instead of being frozen at
    // the first publish.
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const key = "watcher:w1:term-a";
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

describe("VerificationResult backward-compat (#83)", () => {
  it("deserializes a legacy clean/dirty blob to unknown (never a false verified)", () => {
    // Events persisted before #83 carry the old enum values. They must parse — and
    // must NEVER promote to verified — so old evidence can't be read as success.
    const legacyClean = VerificationResult.parse({
      verdict: "clean",
      hasGitChanges: false,
      gitSummary: "working tree clean",
    });
    expect(legacyClean.verdict).toBe("unknown");
    const legacyDirty = VerificationResult.parse({
      verdict: "dirty",
      hasGitChanges: true,
      changedFiles: 2,
      gitSummary: "2 uncommitted file change(s)",
    });
    expect(legacyDirty.verdict).toBe("unknown");
    // New optional bundle fields default rather than throwing on an old blob.
    expect(legacyClean.changedFileList).toEqual([]);
    expect(legacyClean.unresolvedWarnings).toEqual([]);
  });
});

describe("acceptance-contract gate in runTerminalWatcherCheck (#83)", () => {
  const criteriaOptions = JSON.stringify({
    acceptanceCriteria: "All tests pass and the bug is fixed.",
  });

  it("clean tree + confident contract match -> completed_success (verified)", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(
      db,
      queue,
      fakeMcp({ "term-a": { agentState: "completed", tail: "All tests pass. Done." } }),
      fakeJudgeRouter(true, 0.92, "Tests pass; bug fixed."),
    );
    const w = makeWatcher(db, ["term-a"], criteriaOptions);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("completed_success");
    expect(db.getWatcher(w.id)!.status).toBe("condition_met");

    const evt = queue
      .digest({ severityAtLeast: "done" })
      .find((e) => e.target?.terminalId === "term-a");
    const blob = evt?.evidence?.find((e) => e.startsWith(VERIFICATION_EVIDENCE_PREFIX));
    const parsed = VerificationResult.parse(
      JSON.parse(blob!.slice(VERIFICATION_EVIDENCE_PREFIX.length)),
    );
    expect(parsed.verdict).toBe("verified");
    expect(parsed.acceptanceCriteria).toBe("All tests pass and the bug is fixed.");
    expect(parsed.criteriaMetSummary).toBe("Tests pass; bug fixed.");
    db.close();
  });

  it("confident contract non-match -> completed_unverified (failed), keeps polling", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(
      db,
      queue,
      fakeMcp({ "term-a": { agentState: "completed", tail: "Ran the suite." } }),
      fakeJudgeRouter(false, 0.9, "Two tests still failing."),
    );
    const w = makeWatcher(db, ["term-a"], criteriaOptions);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("completed_unverified");
    expect(db.getWatcher(w.id)!.status).toBe("active");

    const evt = queue
      .digest({ severityAtLeast: "attention" })
      .find((e) => e.target?.terminalId === "term-a");
    const blob = evt?.evidence?.find((e) => e.startsWith(VERIFICATION_EVIDENCE_PREFIX));
    const parsed = VerificationResult.parse(
      JSON.parse(blob!.slice(VERIFICATION_EVIDENCE_PREFIX.length)),
    );
    expect(parsed.verdict).toBe("failed");
    db.close();
  });

  it("low-confidence judge -> completed_unverified (unknown), never upgraded", async () => {
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(
      db,
      queue,
      fakeMcp({ "term-a": { agentState: "completed", tail: "Finished, I think." } }),
      fakeJudgeRouter(true, 0.3, "Not sure the bug is actually fixed."),
    );
    const w = makeWatcher(db, ["term-a"], criteriaOptions);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("completed_unverified");
    expect(db.getWatcher(w.id)!.status).toBe("active");

    const evt = queue
      .digest({ severityAtLeast: "attention" })
      .find((e) => e.target?.terminalId === "term-a");
    const blob = evt?.evidence?.find((e) => e.startsWith(VERIFICATION_EVIDENCE_PREFIX));
    const parsed = VerificationResult.parse(
      JSON.parse(blob!.slice(VERIFICATION_EVIDENCE_PREFIX.length)),
    );
    expect(parsed.verdict).toBe("unknown");
    db.close();
  });

  it("contract met but dirty tree -> completed_unverified (unknown), not success", async () => {
    // The work satisfies the contract but uncommitted changes remain — there is
    // still something to review, so it must not promote to a clean success.
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(
      db,
      queue,
      fakeMcp(
        { "term-a": { agentState: "completed", tail: "Done with the changes." } },
        { structuredContent: { isDirty: true, changedFiles: 3 } },
      ),
      fakeJudgeRouter(true, 0.95, "Criteria satisfied."),
    );
    const w = makeWatcher(db, ["term-a"], criteriaOptions);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("completed_unverified");
    expect(db.getWatcher(w.id)!.status).toBe("active");

    const evt = queue
      .digest({ severityAtLeast: "attention" })
      .find((e) => e.target?.terminalId === "term-a");
    const blob = evt?.evidence?.find((e) => e.startsWith(VERIFICATION_EVIDENCE_PREFIX));
    const parsed = VerificationResult.parse(
      JSON.parse(blob!.slice(VERIFICATION_EVIDENCE_PREFIX.length)),
    );
    expect(parsed.verdict).toBe("unknown");
    expect(parsed.hasGitChanges).toBe(true);
    db.close();
  });

  it("empty tail (no evidence to judge) -> unknown, never a false failed", async () => {
    // A completed agent with no readable scrollback (transport hiccup / list
    // fallback) must NOT be judged against the contract — judging zero evidence
    // could harden into a confident "failed". No evidence → "unknown".
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(
      db,
      queue,
      // No tail configured → getOutput returns "" → empty signals.tail.
      fakeMcp({ "term-a": { agentState: "completed" } }),
      fakeJudgeRouter(false, 0.95, "Would say not met if asked."),
    );
    const w = makeWatcher(db, ["term-a"], criteriaOptions);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("completed_unverified");

    const evt = queue
      .digest({ severityAtLeast: "attention" })
      .find((e) => e.target?.terminalId === "term-a");
    const blob = evt?.evidence?.find((e) => e.startsWith(VERIFICATION_EVIDENCE_PREFIX));
    const parsed = VerificationResult.parse(
      JSON.parse(blob!.slice(VERIFICATION_EVIDENCE_PREFIX.length)),
    );
    expect(parsed.verdict).toBe("unknown");
    // The judge never ran, so no criteriaMetSummary was recorded.
    expect(parsed.criteriaMetSummary).toBeUndefined();
    db.close();
  });

  it("confidence floor boundary: 0.6 is confident, 0.599 is not", async () => {
    for (const [confidence, expected] of [
      [0.6, "verified"],
      [0.599, "unknown"],
    ] as const) {
      const db = new Db(":memory:");
      const queue = new Queue(db);
      const ctx = ctxWith(
        db,
        queue,
        fakeMcp({ "term-a": { agentState: "completed", tail: "All done." } }),
        fakeJudgeRouter(true, confidence, "Met."),
      );
      const w = makeWatcher(db, ["term-a"], criteriaOptions);

      const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
      void outcome;
      // "done" threshold captures both the verified (done) and the unknown
      // (attention) case, since this non-supervisor watcher does not promote.
      const evt = queue
        .digest({ severityAtLeast: "done" })
        .find((e) => e.target?.terminalId === "term-a");
      const blob = evt?.evidence?.find((e) =>
        e.startsWith(VERIFICATION_EVIDENCE_PREFIX),
      );
      const parsed = VerificationResult.parse(
        JSON.parse(blob!.slice(VERIFICATION_EVIDENCE_PREFIX.length)),
      );
      expect(parsed.verdict, `confidence ${confidence}`).toBe(expected);
      db.close();
    }
  });

  it("confident non-match on a dirty tree still -> failed (not unknown)", async () => {
    // The non-match branch fires before the dirty-tree fallthrough: a confidently
    // unmet contract is a failure regardless of git state.
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(
      db,
      queue,
      fakeMcp(
        { "term-a": { agentState: "completed", tail: "Gave up." } },
        { structuredContent: { isDirty: true, changedFiles: 2 } },
      ),
      fakeJudgeRouter(false, 0.9, "Contract not satisfied."),
    );
    const w = makeWatcher(db, ["term-a"], criteriaOptions);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("completed_unverified");
    const evt = queue
      .digest({ severityAtLeast: "attention" })
      .find((e) => e.target?.terminalId === "term-a");
    const blob = evt?.evidence?.find((e) => e.startsWith(VERIFICATION_EVIDENCE_PREFIX));
    const parsed = VerificationResult.parse(
      JSON.parse(blob!.slice(VERIFICATION_EVIDENCE_PREFIX.length)),
    );
    expect(parsed.verdict).toBe("failed");
    db.close();
  });

  it("model-claimed completion (FSM still working) is gated by the contract", async () => {
    // The small model concludes completion from tail text while the FSM is still
    // "working"; the contract gate must still run and, on a confident match + clean
    // tree, promote to verified — proving the call site passes acceptanceCriteria.
    const db = new Db(":memory:");
    const queue = new Queue(db);
    const ctx = ctxWith(
      db,
      queue,
      fakeMcp({ "term-a": { agentState: "working", tail: "All done! Task complete." } }),
      fakeClassifyAndJudgeRouter(true, 0.9),
    );
    const w = makeWatcher(db, ["term-a"], criteriaOptions);

    const outcome = await runTerminalWatcherCheck(db.getWatcher(w.id)!, ctx);
    expect(outcome.classification).toBe("completed_success");
    const evt = queue
      .digest({ severityAtLeast: "done" })
      .find((e) => e.target?.terminalId === "term-a");
    const blob = evt?.evidence?.find((e) => e.startsWith(VERIFICATION_EVIDENCE_PREFIX));
    const parsed = VerificationResult.parse(
      JSON.parse(blob!.slice(VERIFICATION_EVIDENCE_PREFIX.length)),
    );
    expect(parsed.verdict).toBe("verified");
    db.close();
  });
});
