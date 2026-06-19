import { describe, it, expect } from "vitest";
import { AgentSession, serializeToolResult } from "../src/agent/loop.js";
import { RecipeRegistry } from "../src/recipes/registry.js";
import { Db } from "../src/storage/db.js";
import { ToolRegistry } from "../src/tools/registry.js";
import type { ModelRouter } from "../src/models/router.js";
import type { ToolContext } from "../src/tools/types.js";
import type { MainPromptContext } from "../src/models/prompts/runtimeContext.js";

function makeSession(routerOverrides: Partial<Record<string, unknown>> = {}) {
  const db = new Db(":memory:");
  const router = {
    json: async () => ({
      recipeIds: [],
      confidence: 0,
      reason: "",
      taskType: "qa",
      keepExisting: false,
    }),
    stream: async () => ({ content: "ok", reasoning: "", toolCalls: [], finishReason: "stop" }),
    chat: async () => ({ content: "SUMMARY", reasoning: "", toolCalls: [], finishReason: "stop" }),
    ...routerOverrides,
  } as unknown as ModelRouter;
  const ctx = { db, actor: "main" } as unknown as ToolContext;
  const promptContext: MainPromptContext = {
    tier: "operator",
    projectPath: "/proj",
    mcpConnected: true,
    mcpStatusLine: "connected",
    largeModel: "L",
    smallModel: "S",
    schedulerActive: true,
  };
  const session = new AgentSession({
    router,
    registry: new ToolRegistry(),
    recipeRegistry: new RecipeRegistry(),
    ctx,
    promptContext,
    sessionId: "ses_compact",
  });
  return { session, db };
}

describe("AgentSession.compact (#7)", () => {
  it("actually drops working history, keeping the 3 control messages + one summary", async () => {
    const { session } = makeSession();
    // Simulate a few turns of accumulated history.
    session.injectNote("first");
    session.injectNote("second");
    session.injectNote("third");
    expect(session.getMessages().length).toBeGreaterThan(4);

    session.compact("goals: X. open: none. next: Y.");

    const msgs = session.getMessages();
    // 3 control messages + exactly one compacted summary note.
    expect(msgs.length).toBe(4);
    expect(msgs[0].role).toBe("system");
    expect(msgs[1].content).toContain("# Runtime context");
    expect(msgs[2].content).toContain("# Loaded recipes");
    expect(msgs[3].role).toBe("user");
    expect(msgs[3].content).toContain("compacted summary");
    expect(msgs[3].content).toContain("goals: X");
    // The old turns are gone from context.
    expect(msgs.some((m) => m.content?.includes("first"))).toBe(false);
  });
});

describe("AgentSession auto-compaction (#7)", () => {
  it("auto-compacts before a turn when history exceeds the token threshold", async () => {
    let chatCalled = 0;
    const { session } = makeSession({
      chat: async () => {
        chatCalled++;
        return { content: "AUTO_SUMMARY", reasoning: "", toolCalls: [], finishReason: "stop" };
      },
    });
    // Two notes so there is real history beyond controls; one huge note pushes the
    // estimate past AUTO_COMPACT_TOKEN_THRESHOLD (60k tokens ≈ 240k chars).
    session.injectNote("keep-small");
    session.injectNote("GIANT_MARKER" + "x".repeat(260_000));
    expect(session.getMessages().length).toBeGreaterThan(4);

    await session.send("hi");

    expect(chatCalled).toBe(1);
    const msgs = session.getMessages();
    // The oversized history was summarized away…
    expect(msgs.some((m) => m.content?.includes("GIANT_MARKER"))).toBe(false);
    // …and replaced with a compacted summary note containing the summary text.
    expect(
      msgs.some(
        (m) =>
          m.content?.includes("compacted summary") &&
          m.content.includes("AUTO_SUMMARY"),
      ),
    ).toBe(true);
  });

  it("does not auto-compact an ordinary (small) conversation", async () => {
    let chatCalled = 0;
    const { session } = makeSession({
      chat: async () => {
        chatCalled++;
        return { content: "X", reasoning: "", toolCalls: [], finishReason: "stop" };
      },
    });
    session.injectNote("small note one");
    session.injectNote("small note two");

    await session.send("hi");

    expect(chatCalled).toBe(0);
    expect(session.getMessages().some((m) => m.content?.includes("small note one"))).toBe(
      true,
    );
  });
});

describe("serializeToolResult truncation (#78)", () => {
  it("returns a valid JSON stub and stores the full envelope when the payload is too large", () => {
    const store = new Map<string, string>();
    const big = "z".repeat(20_000);
    const s = serializeToolResult({ ok: true, summary: "big", result: big }, store);
    // Regression for #78: the old code sliced mid-JSON and appended a plain-text
    // marker, so the model received unparseable JSON. The stub must now parse.
    expect(s).not.toContain("[output truncated:");
    const parsed = JSON.parse(s);
    expect(parsed.ok).toBe(true);
    expect(parsed.summary).toBe("big");
    expect(parsed.result.truncated).toBe(true);
    expect(typeof parsed.result.artifactId).toBe("string");
    expect(typeof parsed.result.preview).toBe("string");
    expect(parsed.result.totalChars).toBeGreaterThan(20_000);
    // The stub itself stays small enough to be worth inlining.
    expect(s.length).toBeLessThan(8000);
    // The full serialized envelope is retrievable from the store under that id.
    const stored = store.get(parsed.result.artifactId);
    expect(stored).toBeDefined();
    expect(JSON.parse(stored!)).toMatchObject({ ok: true, summary: "big", result: big });
  });

  it("still returns valid JSON (without an artifactId) when no store is provided", () => {
    const big = "z".repeat(20_000);
    const s = serializeToolResult({ ok: true, summary: "big", result: big });
    expect(s).not.toContain("[output truncated:");
    const parsed = JSON.parse(s);
    expect(parsed.result.truncated).toBe(true);
    expect(parsed.result.artifactId).toBeUndefined();
  });

  it("leaves small payloads untouched (no truncation stub)", () => {
    const s = serializeToolResult({ ok: true, summary: "tiny", result: "hello" });
    const parsed = JSON.parse(s);
    expect(parsed).toMatchObject({ ok: true, summary: "tiny", result: "hello" });
    expect(parsed.result?.truncated).toBeUndefined();
  });
});
