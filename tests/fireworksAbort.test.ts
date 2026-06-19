import { describe, it, expect, vi } from "vitest";
import { z } from "zod";
import { APIUserAbortError } from "openai";
import { CancelledError, FireworksClient } from "../src/models/fireworks.js";
import type { AppConfig } from "../src/config.js";

const CFG = {
  offline: false,
  fireworksApiKey: "test-key",
  fireworksBaseUrl: "https://example.invalid/v1",
} as unknown as AppConfig;

/**
 * Build a FireworksClient whose underlying OpenAI client is a recording fake. The
 * fake captures the second RequestOptions argument so we can assert the abort
 * signal is forwarded, and `behaviour` lets a test inject an abort.
 */
function clientWithRecorder(
  behaviour: "ok" | "abort-before-stream" | "abort-mid-stream" = "ok",
) {
  const optionsSeen: Array<unknown> = [];
  const create = vi.fn(
    async (payload: Record<string, unknown>, options?: unknown) => {
      optionsSeen.push(options);
      if (behaviour === "abort-before-stream") {
        // The SDK rejects the create() call when the signal is already aborted.
        throw new APIUserAbortError();
      }
      return (async function* () {
        yield { choices: [{ delta: { content: "hel" } }] };
        if (behaviour === "abort-mid-stream") {
          // Interrupting a `for await` surfaces as an AbortError-named exception.
          const err = new Error("This operation was aborted");
          err.name = "AbortError";
          throw err;
        }
        yield { choices: [{ delta: { content: "lo" }, finish_reason: "stop" }] };
      })();
    },
  );
  const fw = new FireworksClient(CFG);
  (fw as unknown as { client: unknown }).client = {
    chat: { completions: { create } },
  };
  return { fw, optionsSeen };
}

/**
 * Build a FireworksClient whose non-streaming create() resolves to a plain
 * completion (chat/json), or throws an abort when asked. Captures the second
 * RequestOptions arg so we can assert the signal is forwarded.
 */
function nonStreamingClient(
  behaviour: "ok" | "abort" = "ok",
  content = "hello",
) {
  const optionsSeen: Array<unknown> = [];
  const create = vi.fn(
    async (_payload: Record<string, unknown>, options?: unknown) => {
      optionsSeen.push(options);
      if (behaviour === "abort") throw new APIUserAbortError();
      return { choices: [{ message: { content }, finish_reason: "stop" }] };
    },
  );
  const fw = new FireworksClient(CFG);
  (fw as unknown as { client: unknown }).client = {
    chat: { completions: { create } },
  };
  return { fw, optionsSeen };
}

describe("FireworksClient non-streaming abort (chat/json)", () => {
  it("chat() forwards the abort signal as the second RequestOptions argument", async () => {
    const { fw, optionsSeen } = nonStreamingClient("ok");
    const controller = new AbortController();
    await fw.chat({
      model: "m",
      messages: [{ role: "user", content: "hi" }],
      signal: controller.signal,
    });
    expect(optionsSeen[0]).toEqual({ signal: controller.signal });
  });

  it("chat() omits RequestOptions when no signal is provided", async () => {
    const { fw, optionsSeen } = nonStreamingClient("ok");
    await fw.chat({ model: "m", messages: [{ role: "user", content: "hi" }] });
    expect(optionsSeen[0]).toBeUndefined();
  });

  it("chat() normalises an abort into CancelledError", async () => {
    const { fw } = nonStreamingClient("abort");
    await expect(
      fw.chat({ model: "m", messages: [{ role: "user", content: "hi" }] }),
    ).rejects.toBeInstanceOf(CancelledError);
  });

  it("json() forwards the abort signal and normalises an abort into CancelledError", async () => {
    const ok = nonStreamingClient("ok", '{"value":1}');
    const controller = new AbortController();
    const schema = z.object({ value: z.number() });
    await ok.fw.json(
      { model: "m", messages: [{ role: "user", content: "hi" }], signal: controller.signal },
      schema,
    );
    expect(ok.optionsSeen[0]).toEqual({ signal: controller.signal });

    const aborted = nonStreamingClient("abort");
    await expect(
      aborted.fw.json({ model: "m", messages: [{ role: "user", content: "hi" }] }, schema),
    ).rejects.toBeInstanceOf(CancelledError);
  });
});

describe("FireworksClient streaming abort", () => {
  it("forwards the abort signal as the second RequestOptions argument", async () => {
    const { fw, optionsSeen } = clientWithRecorder("ok");
    const controller = new AbortController();
    await fw.chatStream({
      model: "m",
      messages: [{ role: "user", content: "hi" }],
      signal: controller.signal,
    });
    expect(optionsSeen[0]).toEqual({ signal: controller.signal });
  });

  it("omits RequestOptions when no signal is provided", async () => {
    const { fw, optionsSeen } = clientWithRecorder("ok");
    await fw.chatStream({ model: "m", messages: [{ role: "user", content: "hi" }] });
    expect(optionsSeen[0]).toBeUndefined();
  });

  it("normalises an APIUserAbortError before streaming into CancelledError", async () => {
    const { fw } = clientWithRecorder("abort-before-stream");
    await expect(
      fw.chatStream({ model: "m", messages: [{ role: "user", content: "hi" }] }),
    ).rejects.toBeInstanceOf(CancelledError);
  });

  it("normalises a mid-stream AbortError into CancelledError", async () => {
    const { fw } = clientWithRecorder("abort-mid-stream");
    await expect(
      fw.chatStream({ model: "m", messages: [{ role: "user", content: "hi" }] }),
    ).rejects.toBeInstanceOf(CancelledError);
  });

  it("leaves non-abort errors untouched", async () => {
    const { fw } = clientWithRecorder("ok");
    (fw as unknown as { client: { chat: { completions: { create: unknown } } } }).client.chat.completions.create =
      vi.fn(async () => {
        throw new Error("boom");
      });
    await expect(
      fw.chatStream({ model: "m", messages: [{ role: "user", content: "hi" }] }),
    ).rejects.toThrow("boom");
  });
});
