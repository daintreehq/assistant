import { describe, it, expect, vi } from "vitest";
import { FireworksClient } from "../src/models/fireworks.js";
import type { AppConfig } from "../src/config.js";

const CFG = {
  offline: false,
  fireworksApiKey: "test-key",
  fireworksBaseUrl: "https://example.invalid/v1",
} as unknown as AppConfig;

/**
 * Build a FireworksClient whose underlying OpenAI client is a recording fake. The
 * stream yields two content chunks then a final usage-only chunk with an empty
 * `choices` array — the shape the OpenAI-compatible endpoint sends when
 * `stream_options.include_usage` is set.
 */
function clientWithUsageStream() {
  const calls: Array<Record<string, unknown>> = [];
  const create = vi.fn(async (payload: Record<string, unknown>) => {
    calls.push(payload);
    return (async function* () {
      yield { choices: [{ delta: { content: "Hel" } }] };
      yield { choices: [{ delta: { content: "lo" }, finish_reason: "stop" }] };
      yield {
        choices: [],
        usage: {
          prompt_tokens: 100,
          completion_tokens: 50,
          total_tokens: 150,
          prompt_tokens_details: { cached_tokens: 40 },
        },
      };
    })();
  });
  const fw = new FireworksClient(CFG);
  (fw as unknown as { client: unknown }).client = {
    chat: { completions: { create } },
  };
  return { fw, calls };
}

describe("Fireworks streaming usage capture", () => {
  it("requests usage via stream_options.include_usage", async () => {
    const { fw, calls } = clientWithUsageStream();
    await fw.chatStream({ model: "m", messages: [{ role: "user", content: "hi" }] });
    expect(calls[0].stream).toBe(true);
    expect(calls[0].stream_options).toEqual({ include_usage: true });
  });

  it("captures token usage from the final usage-only chunk", async () => {
    const { fw } = clientWithUsageStream();
    const res = await fw.chatStream({
      model: "m",
      messages: [{ role: "user", content: "hi" }],
    });
    expect(res.content).toBe("Hello");
    expect(res.usage).toEqual({
      promptTokens: 100,
      completionTokens: 50,
      totalTokens: 150,
      cachedTokens: 40,
    });
  });

  it("returns undefined usage when no usage chunk arrives", async () => {
    const create = vi.fn(async () =>
      (async function* () {
        yield { choices: [{ delta: { content: "hi" }, finish_reason: "stop" }] };
      })(),
    );
    const fw = new FireworksClient(CFG);
    (fw as unknown as { client: unknown }).client = {
      chat: { completions: { create } },
    };
    const res = await fw.chatStream({
      model: "m",
      messages: [{ role: "user", content: "hi" }],
    });
    expect(res.usage).toBeUndefined();
  });
});
