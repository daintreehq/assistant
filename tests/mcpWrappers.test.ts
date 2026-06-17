import { describe, it, expect } from "vitest";
import { ToolRegistry } from "../src/tools/registry.js";
import { mcpTools } from "../src/tools/mcpTools.js";
import { Db } from "../src/storage/db.js";
import type { ToolContext } from "../src/tools/types.js";

function ctx(tier: "operator" | "system"): ToolContext {
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

  it("terminal.focus is allowed at operator tier without confirmation plumbing", async () => {
    const reg = new ToolRegistry();
    reg.registerAll(mcpTools);
    const res = await reg.dispatch("terminal.focus", { terminalId: "term_1" }, ctx("operator"));
    expect(res.ok).toBe(true);
  });
});
