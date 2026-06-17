import { describe, it, expect } from "vitest";
import { ToolRegistry } from "../src/tools/registry.js";
import { mcpTools } from "../src/tools/mcpTools.js";
import { Db } from "../src/storage/db.js";
import type { ToolContext } from "../src/tools/types.js";

function ctx(tier: "supervisor" | "operator" | "system"): ToolContext {
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
    confirm: async () => true,
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

  it("workflow mutations are denied below operator tier", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const c = ctx("supervisor");
    const res = await reg.dispatch(
      "workflow.startWorkOnIssue",
      { arguments: { issueId: "42" } },
      c,
    );
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("TIER_DENIED");
  });
});
