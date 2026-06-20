import { describe, it, expect } from "vitest";
import { ToolRegistry } from "../src/tools/registry.js";
import { mcpTools } from "../src/tools/mcpTools.js";
import { Db } from "../src/storage/db.js";
import type { ToolContext } from "../src/tools/types.js";

function ctx(
  tier: "supervisor" | "operator" | "system",
  confirm: () => Promise<boolean> = async () => true,
): ToolContext {
  const calls: Array<{ name: string; args: unknown }> = [];
  const mcp = {
    isConnected: () => true,
    callTool: async (name: string, args: Record<string, unknown>) => {
      calls.push({ name, args });
      return { text: "ok", content: [], structuredContent: { ran: name }, isError: false };
    },
  } as unknown as ToolContext["mcp"];
  const c = {
    config: { tier } as ToolContext["config"],
    mcp,
    db: new Db(":memory:"),
    queue: {} as ToolContext["queue"],
    router: {} as ToolContext["router"],
    projectPath: "/tmp/p",
    actor: "main",
    confirm,
    log: () => {},
  } as ToolContext;
  return Object.assign(c, { _calls: calls }) as ToolContext & { _calls: typeof calls };
}

describe("typed Daintree wrappers vs daintree.call (#2)", () => {
  it("operator can run recipe.list (read), but daintree.call stays system-gated", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator");

    const list = await reg.dispatch("recipe.list", {}, c);
    expect(list.ok).toBe(true);

    const raw = await reg.dispatch("daintree.call", { name: "anything" }, c);
    expect(raw.ok).toBe(false);
    if (!raw.ok) expect(raw.error.code).toBe("TIER_DENIED");
  });

  it("recipe.run forwards recipeId to the MCP recipe.run tool (with confirmation)", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    const res = await reg.dispatch("recipe.run", { recipeId: "pr-review" }, c);
    expect(res.ok).toBe(true);
    const call = c._calls.find((x) => x.name === "recipe.run");
    expect(call?.args.recipeId).toBe("pr-review");
  });

  it("daintree.call refuses tools that have a typed wrapper and names the wrapper", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("system") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };

    // The two recurring footguns: agent.launch and terminal.getOutput via the
    // raw escape hatch (with empty args). Both must redirect, not forward.
    for (const [name, hint] of [
      ["agent.launch", "agentTask.spawnForEdits"],
      ["terminal.getOutput", "terminal.summarize"],
      ["panel.focus", "terminal.focus"],
    ] as const) {
      const res = await reg.dispatch("daintree.call", { name, arguments: {} }, c);
      expect(res.ok).toBe(false);
      if (!res.ok) {
        expect(res.error.code).toBe("USE_TYPED_WRAPPER");
        expect(res.error.message).toContain(hint);
      }
    }
    // The redirect happens before MCP is touched — no raw call was forwarded.
    expect(c._calls.length).toBe(0);
  });

  it("daintree.call still forwards tools that have no wrapper", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("system") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    const res = await reg.dispatch(
      "daintree.call",
      { name: "git.getProjectPulse", arguments: {} },
      c,
    );
    expect(res.ok).toBe(true);
    expect(c._calls.some((x) => x.name === "git.getProjectPulse")).toBe(true);
  });

  it("terminal.focus maps to panel.focus({ panelId }) at operator tier", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    const res = await reg.dispatch("terminal.focus", { terminalId: "term_1" }, c);
    expect(res.ok).toBe(true);
    // There is no terminal.focus MCP tool — it must call panel.focus by panelId.
    const call = c._calls.find((x) => x.name === "panel.focus");
    expect(call).toBeDefined();
    expect(call?.args.panelId).toBe("term_1");
    expect(c._calls.some((x) => x.name === "terminal.focus")).toBe(false);
  });
});

describe("Daintree refusal text surfaces in the failure summary (#24)", () => {
  /** A ctx whose MCP returns an isError result carrying Daintree's reason text. */
  function errorCtx(reason: string): ToolContext {
    const mcp = {
      isConnected: () => true,
      callTool: async () => ({
        text: reason,
        content: [{ type: "text", text: reason }],
        structuredContent: undefined,
        isError: true,
      }),
    } as unknown as ToolContext["mcp"];
    return {
      config: { tier: "operator" } as ToolContext["config"],
      mcp,
      db: new Db(":memory:"),
      queue: {} as ToolContext["queue"],
      router: {} as ToolContext["router"],
      projectPath: "/tmp/p",
      actor: "main",
      confirm: async () => true,
      log: () => {},
    } as ToolContext;
  }

  it("prefixes the refusal with 'Daintree refused <tool>:' and keeps the reason", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const res = await reg.dispatch(
      "recipe.run",
      { recipeId: "pr-review" },
      errorCtx("session grant revoked"),
    );
    expect(res.ok).toBe(false);
    if (!res.ok) {
      expect(res.error.code).toBe("MCP_TOOL_ERROR");
      expect(res.summary).toContain("Daintree refused recipe.run");
      expect(res.summary).toContain("session grant revoked");
    }
  });

  it("falls back to a generic message when Daintree returns no reason text", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const res = await reg.dispatch(
      "recipe.run",
      { recipeId: "pr-review" },
      errorCtx(""),
    );
    expect(res.ok).toBe(false);
    if (!res.ok) {
      expect(res.summary).toContain("recipe.run");
      expect(res.summary).toContain("returned an error");
    }
  });
});

describe("typed forge + workflow wrappers (#26)", () => {
  it("forge reads forward arguments to the right MCP tools at operator tier", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };

    const issues = await reg.dispatch("forge.listIssues", { arguments: { state: "open" } }, c);
    expect(issues.ok).toBe(true);
    expect(c._calls.find((x) => x.name === "forge.listIssues")?.args).toEqual({ state: "open" });

    const issue = await reg.dispatch("forge.getIssue", { arguments: { issueId: "42" } }, c);
    expect(issue.ok).toBe(true);
    expect(c._calls.find((x) => x.name === "forge.getIssue")?.args.issueId).toBe("42");

    const prs = await reg.dispatch("forge.listPRs", {}, c);
    expect(prs.ok).toBe(true);
    expect(c._calls.find((x) => x.name === "forge.listPRs")?.args).toEqual({});
  });

  it("forge reads succeed at supervisor tier (read risk, no confirmation)", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("supervisor");
    const res = await reg.dispatch("forge.listIssues", {}, c);
    expect(res.ok).toBe(true);
  });

  it("workflow mutations forward arguments + requestKey and are external-gated", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };

    const start = await reg.dispatch(
      "workflow.startWorkOnIssue",
      { arguments: { issueId: "42" }, requestKey: "rk-1" },
      c,
    );
    expect(start.ok).toBe(true);
    const startCall = c._calls.find((x) => x.name === "workflow.startWorkOnIssue");
    expect(startCall?.args.issueId).toBe("42");
    expect(startCall?.args.requestKey).toBe("rk-1");

    const prep = await reg.dispatch(
      "workflow.prepBranchForReview",
      { arguments: { worktreeId: "wt-1" }, requestKey: "rk-2" },
      c,
    );
    expect(prep.ok).toBe(true);
    const prepCall = c._calls.find((x) => x.name === "workflow.prepBranchForReview");
    expect(prepCall?.args.worktreeId).toBe("wt-1");
    expect(prepCall?.args.requestKey).toBe("rk-2");
  });

  it("both workflow mutations are denied below operator tier", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("supervisor");
    for (const name of ["workflow.startWorkOnIssue", "workflow.prepBranchForReview"]) {
      const res = await reg.dispatch(name, { arguments: { issueId: "42" } }, c);
      expect(res.ok).toBe(false);
      if (!res.ok) expect(res.error.code).toBe("TIER_DENIED");
    }
  });

  it("declining confirmation on a workflow mutation blocks the MCP call", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator", async () => false) as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    const res = await reg.dispatch(
      "workflow.startWorkOnIssue",
      { arguments: { issueId: "42" } },
      c,
    );
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("USER_DECLINED");
    expect(c._calls.some((x) => x.name === "workflow.startWorkOnIssue")).toBe(false);
  });
});

describe("tool.search / daintree.listTools callable annotation (#80)", () => {
  const MCP_TOOLS = [
    { name: "recipe.run", description: "Run a recipe by id." },
    { name: "recipe.list", description: "List available recipes." },
    { name: "timer.schedule", description: "Schedule a timer." },
  ];

  /** A read-tier ctx whose MCP returns a fixed tool list; carries activeToolNames. */
  function discoveryCtx(activeToolNames?: string[]): ToolContext {
    const mcp = {
      isConnected: () => true,
      listTools: async () => MCP_TOOLS,
    } as unknown as ToolContext["mcp"];
    const c = {
      config: { tier: "supervisor" } as ToolContext["config"],
      mcp,
      db: new Db(":memory:"),
      queue: {} as ToolContext["queue"],
      router: {} as ToolContext["router"],
      projectPath: "/tmp/p",
      actor: "main",
      confirm: async () => true,
      log: () => {},
    } as ToolContext;
    if (activeToolNames !== undefined) c.activeToolNames = activeToolNames;
    return c;
  }

  it("tool.search marks only projected tools callable when a recipe narrows the turn", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = discoveryCtx(["tool.search", "recipe.run"]);
    const res = await reg.dispatch("tool.search", { query: "recipe" }, c);
    expect(res.ok).toBe(true);
    if (!res.ok) return;
    const result = res.result as {
      matches: Array<{ name: string; callable: boolean }>;
      note: string;
    };
    const byName = Object.fromEntries(result.matches.map((m) => [m.name, m.callable]));
    // Substring "recipe" matches recipe.run + recipe.list; only recipe.run is offered.
    expect(byName["recipe.run"]).toBe(true);
    expect(byName["recipe.list"]).toBe(false);
    // The note explains the callable flag so the model treats false as "not offered".
    expect(result.note).toContain("callable: false");
  });

  it("tool.search marks everything callable when the turn is unconstrained (undefined)", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = discoveryCtx(undefined);
    const res = await reg.dispatch("tool.search", { query: "recipe" }, c);
    expect(res.ok).toBe(true);
    if (!res.ok) return;
    const matches = (res.result as { matches: Array<{ callable: boolean }> }).matches;
    expect(matches.length).toBeGreaterThan(0);
    expect(matches.every((m) => m.callable)).toBe(true);
  });

  it("daintree.listTools annotates each tool against the active projection", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = discoveryCtx(["recipe.run"]);
    const res = await reg.dispatch("daintree.listTools", {}, c);
    expect(res.ok).toBe(true);
    if (!res.ok) return;
    const tools = (res.result as { tools: Array<{ name: string; callable: boolean }> }).tools;
    const byName = Object.fromEntries(tools.map((t) => [t.name, t.callable]));
    expect(byName["recipe.run"]).toBe(true);
    expect(byName["recipe.list"]).toBe(false);
    expect(byName["timer.schedule"]).toBe(false);
  });

  it("daintree.listTools marks all tools uncallable for an empty projection (boundary)", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = discoveryCtx([]);
    const res = await reg.dispatch("daintree.listTools", {}, c);
    expect(res.ok).toBe(true);
    if (!res.ok) return;
    const tools = (res.result as { tools: Array<{ callable: boolean }> }).tools;
    expect(tools.length).toBe(MCP_TOOLS.length);
    expect(tools.every((t) => !t.callable)).toBe(true);
  });

  it("marks an unwrapped MCP tool callable:false even when daintree.call is offered (pins escape-hatch semantics)", async () => {
    // `callable` reflects DIRECT invocability — membership in the turn's tool spec.
    // git.getProjectPulse is not offered directly even though daintree.call (offered
    // here) could reach it; the note carries that nuance. This pins the deliberately
    // simple predicate so a future change to "reachable via escape hatch" is visible.
    const mcp = {
      isConnected: () => true,
      listTools: async () => [
        { name: "git.getProjectPulse", description: "Project pulse." },
      ],
    } as unknown as ToolContext["mcp"];
    const c = {
      config: { tier: "system" } as ToolContext["config"],
      mcp,
      db: new Db(":memory:"),
      queue: {} as ToolContext["queue"],
      router: {} as ToolContext["router"],
      projectPath: "/tmp/p",
      actor: "main",
      confirm: async () => true,
      log: () => {},
      activeToolNames: ["tool.search", "daintree.call"],
    } as ToolContext;
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const res = await reg.dispatch("tool.search", { query: "pulse" }, c);
    expect(res.ok).toBe(true);
    if (!res.ok) return;
    const matches = (res.result as { matches: Array<{ name: string; callable: boolean }> })
      .matches;
    expect(matches.find((m) => m.name === "git.getProjectPulse")?.callable).toBe(false);
  });
});

describe("forge write + getPR wrappers (#29)", () => {
  const FORGE_WRITE_NAMES = [
    "forge.createIssue",
    "forge.closeIssue",
    "forge.reopenIssue",
    "forge.editIssue",
    "forge.addIssueComment",
    "forge.addIssueLabel",
    "forge.removeIssueLabel",
    "forge.assignIssue",
    "forge.unassignIssue",
    "forge.createPR",
    "forge.closePR",
    "forge.reopenPR",
    "forge.mergePR",
    "forge.convertPRToDraft",
    "forge.markPRReadyForReview",
    "forge.commentOnPR",
    "forge.editPR",
    "forge.approvePR",
    "forge.requestChanges",
    "forge.dismissReview",
    "forge.requestReviewers",
  ];

  function tool(name: string) {
    const def = mcpTools.find((t) => t.name === name);
    if (!def) throw new Error(`missing tool ${name}`);
    return def;
  }

  it("registers forge.getPR as a read-risk wrapper", () => {
    const getPR = tool("forge.getPR");
    expect(getPR.risk).toBe("read");
  });

  it("registers every forge write as external risk", () => {
    for (const name of FORGE_WRITE_NAMES) {
      const def = tool(name);
      expect(def.risk, name).toBe("external");
    }
  });

  it("gives every forge write a user-facing consequence, not the raw risk class", () => {
    for (const name of FORGE_WRITE_NAMES) {
      const def = tool(name);
      expect(def.consequence, name).toBeTruthy();
      expect((def.consequence ?? "").length, name).toBeGreaterThan(10);
      // The consequence must be prose, never just the risk class word.
      expect(def.consequence, name).not.toBe("external");
    }
  });

  it("forge.getPR forwards flat args (no nested bag, incl. cwd) at supervisor tier", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("supervisor") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    const res = await reg.dispatch("forge.getPR", { cwd: "/repo", prNumber: 7 }, c);
    expect(res.ok).toBe(true);
    expect(c._calls.find((x) => x.name === "forge.getPR")?.args).toEqual({
      cwd: "/repo",
      prNumber: 7,
    });
  });

  it("each forge write forwards exactly the expected field names to its MCP action", async () => {
    // Guards against a field-name typo (e.g. reviewID vs reviewId, prNumber vs
    // issueNumber) silently shipping. One representative valid payload per tool.
    const cases: Array<[string, Record<string, unknown>]> = [
      ["forge.createIssue", { title: "t" }],
      ["forge.closeIssue", { issueNumber: 1, stateReason: "completed" }],
      ["forge.reopenIssue", { issueNumber: 1 }],
      ["forge.editIssue", { issueNumber: 1, title: "t" }],
      ["forge.addIssueComment", { issueNumber: 1, body: "b" }],
      ["forge.addIssueLabel", { issueNumber: 1, label: "bug" }],
      ["forge.removeIssueLabel", { issueNumber: 1, label: "bug" }],
      ["forge.assignIssue", { issueNumber: 1, username: "me" }],
      ["forge.unassignIssue", { issueNumber: 1, username: "me" }],
      ["forge.createPR", { head: "feat", base: "main", title: "t" }],
      ["forge.closePR", { prNumber: 2 }],
      ["forge.reopenPR", { prNumber: 2 }],
      ["forge.mergePR", { prNumber: 2, mergeMethod: "squash" }],
      ["forge.convertPRToDraft", { prNumber: 2 }],
      ["forge.markPRReadyForReview", { prNumber: 2 }],
      ["forge.commentOnPR", { prNumber: 2, body: "b" }],
      ["forge.editPR", { prNumber: 2, body: "b" }],
      ["forge.approvePR", { prNumber: 2 }],
      ["forge.requestChanges", { prNumber: 2, body: "fix" }],
      ["forge.dismissReview", { prNumber: 2, reviewId: 5, message: "stale" }],
      ["forge.requestReviewers", { prNumber: 2, users: ["me"] }],
    ];
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    for (const [name, args] of cases) {
      const res = await reg.dispatch(name, args, c);
      expect(res.ok, name).toBe(true);
      // Args reach the same-named MCP action verbatim (no nested bag wrapper).
      expect(c._calls.find((x) => x.name === name)?.args, name).toEqual(args);
    }
  });

  it("forge writes forward flat args to the same-named MCP action (with confirmation)", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };

    const issue = await reg.dispatch(
      "forge.createIssue",
      { title: "Bug", body: "broken", labels: ["bug"] },
      c,
    );
    expect(issue.ok).toBe(true);
    expect(c._calls.find((x) => x.name === "forge.createIssue")?.args).toEqual({
      title: "Bug",
      body: "broken",
      labels: ["bug"],
    });

    const pr = await reg.dispatch(
      "forge.createPR",
      { head: "feat", base: "main", title: "Add feature" },
      c,
    );
    expect(pr.ok).toBe(true);
    expect(c._calls.find((x) => x.name === "forge.createPR")?.args).toEqual({
      head: "feat",
      base: "main",
      title: "Add feature",
    });

    const approve = await reg.dispatch("forge.approvePR", { prNumber: 12 }, c);
    expect(approve.ok).toBe(true);
    expect(c._calls.find((x) => x.name === "forge.approvePR")?.args).toEqual({ prNumber: 12 });
  });

  it("forwards requestKey via passthrough's dedicated param, not as a payload field", async () => {
    // The handler destructures requestKey out of the forwarded payload and hands
    // it to passthrough(), which re-attaches it as the idempotency key. End result
    // at callTool: the user payload plus requestKey — but it never travels as a
    // forge field the action schema would have to know about.
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    const res = await reg.dispatch(
      "forge.commentOnPR",
      { prNumber: 3, body: "lgtm", requestKey: "rk-test" },
      c,
    );
    expect(res.ok).toBe(true);
    const call = c._calls.find((x) => x.name === "forge.commentOnPR");
    expect(call?.args).toEqual({ prNumber: 3, body: "lgtm", requestKey: "rk-test" });
  });

  it("rejects whitespace-only required text locally (matches Daintree's trim().min(1))", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    for (const [name, args] of [
      ["forge.requestChanges", { prNumber: 1, body: "   " }],
      ["forge.dismissReview", { prNumber: 1, reviewId: 2, message: "  " }],
      ["forge.requestReviewers", { prNumber: 1, users: [""] }],
    ] as const) {
      const res = await reg.dispatch(name, args, c);
      expect(res.ok, name).toBe(false);
      if (!res.ok) expect(res.error.code, name).toBe("INVALID_ARGS");
    }
    expect(c._calls.length).toBe(0);
  });

  it("rejects edit/requestReviewers calls that satisfy no field-presence constraint", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator");
    for (const [name, args] of [
      ["forge.editIssue", { issueNumber: 1 }],
      ["forge.editPR", { prNumber: 1 }],
      ["forge.requestReviewers", { prNumber: 1 }],
    ] as const) {
      const res = await reg.dispatch(name, args, c);
      expect(res.ok, name).toBe(false);
      if (!res.ok) expect(res.error.code, name).toBe("INVALID_ARGS");
    }
  });

  it("coerces nothing — string issue numbers are rejected", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator");
    const res = await reg.dispatch("forge.reopenIssue", { issueNumber: "42" }, c);
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("INVALID_ARGS");
  });

  it("denies forge writes below operator tier", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("supervisor");
    const res = await reg.dispatch("forge.createIssue", { title: "x" }, c);
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("TIER_DENIED");
  });

  it("declining confirmation on a forge write blocks the MCP call", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator", async () => false) as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    const res = await reg.dispatch("forge.closePR", { prNumber: 9 }, c);
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("USER_DECLINED");
    expect(c._calls.some((x) => x.name === "forge.closePR")).toBe(false);
  });
});

describe("typed copyTree / terminal-input / agent-focus / git-snapshot wrappers (#120)", () => {
  function tool(name: string) {
    const def = mcpTools.find((t) => t.name === name);
    if (!def) throw new Error(`missing tool ${name}`);
    return def;
  }

  const UI_FOCUS_NAMES = [
    "agent.focusNextWaiting",
    "agent.focusNextWorking",
    "agent.focusNextAgent",
    "agent.focusPreviousAgent",
    "workflow.focusNextAttention",
  ];

  it("registers each new wrapper with its verified risk class", () => {
    expect(tool("copyTree.generate").risk).toBe("read");
    expect(tool("terminal.sendCommand").risk).toBe("terminal");
    expect(tool("copyTree.injectToTerminal").risk).toBe("terminal");
    for (const name of UI_FOCUS_NAMES) expect(tool(name).risk, name).toBe("ui");
    expect(tool("copyTree.generateAndCopyFile").risk).toBe("system");
    expect(tool("git.snapshotRevert").risk).toBe("git");
    expect(tool("git.snapshotDelete").risk).toBe("git");
  });

  it("gives every mutating new wrapper a user-facing consequence line", () => {
    for (const name of [
      "terminal.sendCommand",
      "copyTree.injectToTerminal",
      "copyTree.generateAndCopyFile",
      "git.snapshotRevert",
      "git.snapshotDelete",
    ]) {
      const def = tool(name);
      expect(def.consequence, name).toBeTruthy();
      expect((def.consequence ?? "").length, name).toBeGreaterThan(10);
      expect(def.consequence, name).not.toBe(def.risk);
    }
  });

  it("copyTree.generate is a read wrapper that forwards args at supervisor tier", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("supervisor") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    const res = await reg.dispatch(
      "copyTree.generate",
      { worktreeId: "wt-1", options: { maxFiles: 10 } },
      c,
    );
    expect(res.ok).toBe(true);
    expect(c._calls.find((x) => x.name === "copyTree.generate")?.args).toEqual({
      worktreeId: "wt-1",
      options: { maxFiles: 10 },
    });
  });

  it("UI focus wrappers forward an empty payload and run at supervisor tier (no confirmation)", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("supervisor", async () => false) as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    for (const name of UI_FOCUS_NAMES) {
      const res = await reg.dispatch(name, {}, c);
      expect(res.ok, name).toBe(true);
      expect(c._calls.find((x) => x.name === name)?.args, name).toEqual({});
    }
  });

  it("terminal.sendCommand forwards terminalId+command at operator tier (with confirmation)", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    const res = await reg.dispatch(
      "terminal.sendCommand",
      { terminalId: "term_1", command: "npm test" },
      c,
    );
    expect(res.ok).toBe(true);
    expect(c._calls.find((x) => x.name === "terminal.sendCommand")?.args).toEqual({
      terminalId: "term_1",
      command: "npm test",
    });
  });

  it("copyTree.injectToTerminal forwards its args at operator tier (with confirmation)", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("operator") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    const res = await reg.dispatch(
      "copyTree.injectToTerminal",
      { terminalId: "term_2", worktreeId: "wt-1" },
      c,
    );
    expect(res.ok).toBe(true);
    expect(c._calls.find((x) => x.name === "copyTree.injectToTerminal")?.args).toEqual({
      terminalId: "term_2",
      worktreeId: "wt-1",
    });
  });

  it("git snapshot wrappers forward worktreeId at system tier (with confirmation)", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("system") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    for (const name of ["git.snapshotRevert", "git.snapshotDelete"]) {
      const res = await reg.dispatch(name, { worktreeId: "wt-9" }, c);
      expect(res.ok, name).toBe(true);
      expect(c._calls.find((x) => x.name === name)?.args, name).toEqual({ worktreeId: "wt-9" });
    }
  });

  it("copyTree.generateAndCopyFile forwards args at system tier (with confirmation)", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("system") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    const res = await reg.dispatch("copyTree.generateAndCopyFile", { worktreeId: "wt-3" }, c);
    expect(res.ok).toBe(true);
    expect(c._calls.find((x) => x.name === "copyTree.generateAndCopyFile")?.args).toEqual({
      worktreeId: "wt-3",
    });
  });

  it("tier-gates terminal/system/git wrappers below their required tier", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    // terminal risk: denied at supervisor.
    let c = ctx("supervisor");
    let res = await reg.dispatch("terminal.sendCommand", { terminalId: "t", command: "x" }, c);
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("TIER_DENIED");
    // system + git risk: denied at operator.
    c = ctx("operator");
    for (const [name, args] of [
      ["copyTree.generateAndCopyFile", {}],
      ["git.snapshotRevert", { worktreeId: "w" }],
      ["git.snapshotDelete", { worktreeId: "w" }],
    ] as const) {
      res = await reg.dispatch(name, args, c);
      expect(res.ok, name).toBe(false);
      if (!res.ok) expect(res.error.code, name).toBe("TIER_DENIED");
    }
  });

  it("declining confirmation on a mutating new wrapper blocks the MCP call", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const term = ctx("operator", async () => false) as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    let res = await reg.dispatch("terminal.sendCommand", { terminalId: "t", command: "x" }, term);
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("USER_DECLINED");

    const sys = ctx("system", async () => false) as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    res = await reg.dispatch("git.snapshotRevert", { worktreeId: "w" }, sys);
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("USER_DECLINED");

    expect(term._calls.length).toBe(0);
    expect(sys._calls.length).toBe(0);
  });

  it("rejects missing, empty, and whitespace-only required fields locally", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("system") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    for (const [name, args] of [
      ["terminal.sendCommand", { command: "npm test" }], // missing terminalId
      ["terminal.sendCommand", { terminalId: "t", command: "" }], // empty command
      ["terminal.sendCommand", { terminalId: "   ", command: "npm test" }], // whitespace terminalId
      ["copyTree.injectToTerminal", { worktreeId: "wt" }], // missing terminalId
      ["git.snapshotRevert", {}], // missing worktreeId
      ["git.snapshotDelete", { worktreeId: "" }], // empty worktreeId
      ["git.snapshotDelete", { worktreeId: "   " }], // whitespace worktreeId
    ] as const) {
      const res = await reg.dispatch(name, args, c);
      expect(res.ok, name).toBe(false);
      if (!res.ok) expect(res.error.code, name).toBe("INVALID_ARGS");
    }
    // No invalid call ever reached MCP.
    expect(c._calls.length).toBe(0);
  });

  it("daintree.call refuses the new wrappers' tools and redirects to them", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("system") as ToolContext & {
      _calls: Array<{ name: string; args: Record<string, unknown> }>;
    };
    for (const name of [
      "terminal.sendCommand",
      "copyTree.injectToTerminal",
      "copyTree.generateAndCopyFile",
      "git.snapshotRevert",
      "git.snapshotDelete",
    ]) {
      const res = await reg.dispatch("daintree.call", { name, arguments: {} }, c);
      expect(res.ok, name).toBe(false);
      if (!res.ok) expect(res.error.code, name).toBe("USE_TYPED_WRAPPER");
    }
    // All redirected before MCP was touched.
    expect(c._calls.length).toBe(0);
  });
});
