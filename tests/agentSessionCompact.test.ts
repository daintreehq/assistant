import { describe, it, expect } from "vitest";
import { AgentSession, serializeToolResult, CLEAR_MARKER } from "../src/agent/loop.js";
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

describe("AgentSession.clear (#114)", () => {
  it("drops working history to the 3 control messages with no summary note", () => {
    const { session } = makeSession();
    session.injectNote("alpha");
    session.injectNote("beta");
    expect(session.getMessages().length).toBeGreaterThan(3);

    session.clear();

    const msgs = session.getMessages();
    // Exactly the 3 control messages — and, unlike compact(), NO 4th summary note.
    expect(msgs.length).toBe(3);
    expect(msgs[0].role).toBe("system");
    expect(msgs[1].content).toContain("# Runtime context");
    expect(msgs[2].content).toContain("# Loaded recipes");
    // Old turns and any compaction summary are gone from context.
    expect(msgs.some((m) => m.content?.includes("alpha"))).toBe(false);
    expect(msgs.some((m) => m.content?.includes("compacted summary"))).toBe(false);
  });

  it("appends a clear marker to the durable log without resetting seq", () => {
    const { session, db } = makeSession();
    session.injectNote("history-row");
    const before = db.listMessages("ses_compact");
    const maxSeqBefore = before.reduce((m, r) => Math.max(m, r.seq), 0);

    session.clear();

    const rows = db.listMessages("ses_compact");
    const marker = rows.find((r) => r.content === CLEAR_MARKER);
    expect(marker).toBeDefined();
    // seq keeps climbing — never reset (a reset would collide on the UNIQUE
    // (sessionId, seq) index and corrupt resume). The marker sits above prior rows.
    expect(marker!.seq).toBeGreaterThan(maxSeqBefore);
    // The history row is still in the durable log — clear is append-only, not a DELETE.
    expect(rows.some((r) => r.content === "[system event]\nhistory-row")).toBe(true);
  });

  it("is idempotent — a second clear keeps exactly the 3 control messages", () => {
    const { session } = makeSession();
    session.injectNote("once");
    session.clear();
    session.clear();
    expect(session.getMessages().length).toBe(3);
  });

  it("drops a prior compaction summary too — clear after compact leaves no 4th note", () => {
    const { session } = makeSession();
    session.injectNote("pre");
    session.compact("goals: X. open: none. next: Y.");
    // compact() leaves controls + 1 summary note.
    expect(session.getMessages().length).toBe(4);

    session.clear();

    const msgs = session.getMessages();
    expect(msgs.length).toBe(3);
    expect(msgs.some((m) => m.content?.includes("compacted summary"))).toBe(false);
    expect(msgs.some((m) => m.content?.includes("goals: X"))).toBe(false);
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

  it("caps an oversized summary so the stub itself stays within the inline limit", () => {
    // A huge summary with a tiny result still overflows; the stub must not echo it whole.
    const s = serializeToolResult({ ok: true, summary: "S".repeat(8000), result: "tiny" });
    expect(s.length).toBeLessThan(8000);
    const parsed = JSON.parse(s);
    expect(parsed.result.truncated).toBe(true);
    expect(parsed.summary.length).toBeLessThanOrEqual(500);
  });

  it("surfaces the error class in the stub for a failed oversized result", () => {
    const s = serializeToolResult({
      ok: false,
      summary: "failed",
      result: "x".repeat(9000),
      error: { code: "DB_ERROR", message: "boom", recoverable: false },
    });
    const parsed = JSON.parse(s);
    expect(parsed.ok).toBe(false);
    expect(parsed.result.errorCode).toBe("DB_ERROR");
    expect(parsed.result.recoverable).toBe(false);
  });

  it("a re-paged slice of a stored artifact does not itself re-overflow (escape amplification)", () => {
    // Escape-heavy content (backslashes) doubles under each JSON.stringify. Storing it
    // and then paging a MAX_READ_CHARS slice back through serializeToolResult must stay
    // valid and within the inline limit — not produce a second, nested artifact stub.
    const store = new Map<string, string>();
    const heavy = "\\".repeat(8000);
    const stub = serializeToolResult({ ok: true, summary: "heavy", result: heavy }, store);
    const id = JSON.parse(stub).result.artifactId as string;
    const full = store.get(id)!;
    // Simulate artifact.read returning its largest possible slice, then re-serializing it.
    const slice = full.slice(0, 3500);
    const reSerialized = serializeToolResult({
      ok: true,
      summary: "page",
      result: { artifactId: id, content: slice, offset: 0, eof: false },
    });
    expect(reSerialized.length).toBeLessThanOrEqual(8000);
    // It stayed inline (no nested truncation) — parses to the real content, not another stub.
    const parsed = JSON.parse(reSerialized);
    expect(parsed.result.truncated).toBeUndefined();
    expect(parsed.result.content).toBe(slice);
  });
});
