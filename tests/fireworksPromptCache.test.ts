import { describe, it, expect, vi } from "vitest";
import { FireworksClient } from "../src/models/fireworks.js";
import type { AppConfig } from "../src/config.js";

const CFG = {
  offline: false,
  fireworksApiKey: "test-key",
  fireworksBaseUrl: "https://example.invalid/v1",
} as unknown as AppConfig;

/** Build a FireworksClient whose underlying OpenAI client is a recording fake. */
function clientWithRecorder() {
  const calls: Array<Record<string, unknown>> = [];
  const create = vi.fn(async (payload: Record<string, unknown>) => {
    calls.push(payload);
    if (payload.stream) {
      return (async function* () {
        yield { choices: [{ delta: { content: "hi" }, finish_reason: "stop" }] };
      })();
    }
    return {
      choices: [{ message: { content: "hi", tool_calls: undefined }, finish_reason: "stop" }],
      usage: undefined,
    };
  });
  const fw = new FireworksClient(CFG);
  // Swap the private OpenAI client for our recorder.
  (fw as unknown as { client: unknown }).client = {
    chat: { completions: { create } },
  };
  return { fw, calls };
}

describe("Fireworks promptCacheKey forwarding", () => {
  it("chat() forwards promptCacheKey as prompt_cache_key", async () => {
    const { fw, calls } = clientWithRecorder();
    await fw.chat({
      model: "m",
      messages: [{ role: "user", content: "hi" }],
      promptCacheKey: "daintree-main-system-v1",
    });
    expect(calls[0].prompt_cache_key).toBe("daintree-main-system-v1");
  });

  it("chatStream() forwards promptCacheKey as prompt_cache_key", async () => {
    const { fw, calls } = clientWithRecorder();
    await fw.chatStream({
      model: "m",
      messages: [{ role: "user", content: "hi" }],
      promptCacheKey: "daintree-main-system-v1",
    });
    expect(calls[0].stream).toBe(true);
    expect(calls[0].prompt_cache_key).toBe("daintree-main-system-v1");
  });

  it("omits prompt_cache_key when no key is provided", async () => {
    const { fw, calls } = clientWithRecorder();
    await fw.chat({ model: "m", messages: [{ role: "user", content: "hi" }] });
    expect(calls[0]).not.toHaveProperty("prompt_cache_key");
  });
});
