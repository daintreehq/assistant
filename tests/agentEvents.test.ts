import { AgentSession } from "../src/agent/loop.js";
import type { AgentEventSink } from "../src/agent/events.js";
import type { MainPromptContext } from "../src/models/prompts/index.js";
import { RecipeRegistry } from "../src/recipes/registry.js";

// Recipe selection runs at the top of send(); a stub keeps it a no-op so these
// tests stay focused on the event sink.
const recipeRegistry = new RecipeRegistry();
const selectNone = async () => ({
  recipeIds: [],
  confidence: 0,
  reason: "test",
  taskType: "none",
  keepExisting: false,
});

function recordingSink() {
  const events: string[] = [];
  const sink: AgentEventSink = {
    assistantStart: () => events.push("start"),
    assistantToken: (t) => events.push(`tok:${t}`),
    assistantEnd: (c) => events.push(`end:${c}`),
    toolCall: (n) => events.push(`call:${n}`),
    toolResult: (n, r) => events.push(`result:${n}:${r.ok}`),
    error: (m) => events.push(`error:${m}`),
    info: (m) => events.push(`info:${m}`),
  };
  return { events, sink };
}

const PROMPT_CTX: MainPromptContext = {
  tier: "operator",
  projectPath: "/tmp/x",
  mcpConnected: false,
  mcpStatusLine: "degraded",
  largeModel: "large",
  smallModel: "small",
};

const ctx = {
  db: { insertMessage: () => {}, insertRecipeSelection: () => {} },
} as any;

function chatResult(over: Partial<{ content: string; toolCalls: any[] }>) {
  return {
    content: over.content ?? "",
    reasoning: "",
    toolCalls: over.toolCalls ?? [],
    finishReason: "stop",
  };
}

describe("AgentSession emits structured events instead of rendering", () => {
  it("streams tokens then ends a plain answer", async () => {
    const { events, sink } = recordingSink();
    const router = {
      stream: async (
        _tier: string,
        _opts: unknown,
        onToken?: (t: string) => void,
      ) => {
        onToken?.("Hel");
        onToken?.("lo");
        return chatResult({ content: "Hello" });
      },
      json: selectNone,
    } as any;
    const registry = {
      toOpenAITools: () => [],
      resolveWireName: () => undefined,
      dispatch: async () => ({}),
    } as any;

    const session = new AgentSession({
      router,
      registry,
      recipeRegistry,
      ctx,
      promptContext: PROMPT_CTX,
      sessionId: "t1",
      events: sink,
    });

    const out = await session.send("hi");
    expect(out).toBe("Hello");
    expect(events).toEqual(["start", "tok:Hel", "tok:lo", "end:Hello"]);
  });

  it("emits toolCall/toolResult then the final answer", async () => {
    const { events, sink } = recordingSink();
    const responses = [
      chatResult({
        // The model returns the OpenAI-legal wire name; the loop must translate
        // it back to the internal dotted name before dispatch and events.
        toolCalls: [
          { id: "c1", type: "function", function: { name: "fs__search", arguments: "{}" } },
        ],
      }),
      chatResult({ content: "done" }),
    ];
    let n = 0;
    const router = { stream: async () => responses[n++], json: selectNone } as any;
    const registry = {
      toOpenAITools: () => [],
      resolveWireName: (w: string) => w.replaceAll("__", "."),
      dispatch: async () => ({ ok: true, summary: "found 2 files" }),
    } as any;

    const session = new AgentSession({
      router,
      registry,
      recipeRegistry,
      ctx,
      promptContext: PROMPT_CTX,
      sessionId: "t2",
      events: sink,
    });

    const out = await session.send("search");
    expect(out).toBe("done");
    expect(events).toContain("call:fs.search");
    expect(events).toContain("result:fs.search:true");
    expect(events[events.length - 1]).toBe("end:done");
  });

  it("reports model errors through the sink", async () => {
    const { events, sink } = recordingSink();
    const router = {
      stream: async () => {
        throw new Error("boom");
      },
      json: selectNone,
    } as any;
    const registry = {
      toOpenAITools: () => [],
      resolveWireName: () => undefined,
      dispatch: async () => ({}),
    } as any;

    const session = new AgentSession({
      router,
      registry,
      recipeRegistry,
      ctx,
      promptContext: PROMPT_CTX,
      sessionId: "t3",
      events: sink,
    });

    const out = await session.send("hi");
    expect(out).toContain("Model error: boom");
    expect(events.some((e) => e.startsWith("error:"))).toBe(true);
  });
});
