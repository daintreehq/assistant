import { describe, it, expect, vi } from "vitest";
import { APIError, APIUserAbortError } from "openai";
import { CancelledError, FireworksClient } from "../src/models/fireworks.js";
import type { AppConfig } from "../src/config.js";

const CFG = {
  offline: false,
  fireworksApiKey: "test-key",
  fireworksBaseUrl: "https://example.invalid/v1",
} as unknown as AppConfig;

/** A FireworksClient whose underlying create() is supplied by the test, so we can
 *  drive transient failures and count attempts. */
function clientWith(create: ReturnType<typeof vi.fn>) {
  const fw = new FireworksClient(CFG);
  (fw as unknown as { client: unknown }).client = {
    chat: { completions: { create } },
  };
  return fw;
}

/** A non-streaming completion the SDK would return. */
function completion(content = "ok") {
  return { choices: [{ message: { content }, finish_reason: "stop" }] };
}

/** An async stream of content deltas, optionally throwing partway through. */
function streamOf(parts: string[], throwAfter?: { at: number; err: unknown }) {
  return (async function* () {
    for (let i = 0; i < parts.length; i++) {
      if (throwAfter && i === throwAfter.at) throw throwAfter.err;
      yield { choices: [{ delta: { content: parts[i] } }] };
    }
    yield { choices: [{ delta: {}, finish_reason: "stop" }] };
  })();
}

describe("FireworksClient model-call retry (chat/json)", () => {
  it("chat() retries a transient 5xx and then succeeds", async () => {
    let calls = 0;
    const create = vi.fn(async () => {
      calls++;
      if (calls === 1) throw new APIError(503, undefined, "service unavailable", undefined);
      return completion("recovered");
    });
    const res = await clientWith(create).chat({
      model: "m",
      messages: [{ role: "user", content: "hi" }],
    });
    expect(res.content).toBe("recovered");
    expect(create).toHaveBeenCalledTimes(2);
  });

  it("chat() honours a 429 with a Retry-After header and recovers", async () => {
    let calls = 0;
    const create = vi.fn(async () => {
      calls++;
      if (calls === 1)
        throw new APIError(429, undefined, "rate limited", { "retry-after-ms": "1" });
      return completion("after-throttle");
    });
    const res = await clientWith(create).chat({
      model: "m",
      messages: [{ role: "user", content: "hi" }],
    });
    expect(res.content).toBe("after-throttle");
    expect(create).toHaveBeenCalledTimes(2);
  });

  it("chat() does NOT retry a user abort and normalises it to CancelledError", async () => {
    const create = vi.fn(async () => {
      throw new APIUserAbortError();
    });
    await expect(
      clientWith(create).chat({ model: "m", messages: [{ role: "user", content: "hi" }] }),
    ).rejects.toBeInstanceOf(CancelledError);
    expect(create).toHaveBeenCalledTimes(1);
  });

  it("chat() does NOT retry a non-retriable 4xx", async () => {
    const create = vi.fn(async () => {
      throw new APIError(400, undefined, "bad request", undefined);
    });
    await expect(
      clientWith(create).chat({ model: "m", messages: [{ role: "user", content: "hi" }] }),
    ).rejects.toThrow("bad request");
    expect(create).toHaveBeenCalledTimes(1);
  });

  it("chat() gives up after exhausting the retry budget", async () => {
    const create = vi.fn(async () => {
      throw new APIError(500, undefined, "still broken", undefined);
    });
    await expect(
      clientWith(create).chat({ model: "m", messages: [{ role: "user", content: "hi" }] }),
    ).rejects.toThrow("still broken");
    // 1 initial attempt + MODEL_RETRY_POLICY.maxRetries (3) = 4 total.
    expect(create).toHaveBeenCalledTimes(4);
  });
});

describe("FireworksClient streaming retry", () => {
  it("retries a pre-token transient failure and restarts the stream cleanly", async () => {
    let calls = 0;
    const create = vi.fn(async () => {
      calls++;
      if (calls === 1) throw new APIError(503, undefined, "unavailable", undefined);
      return streamOf(["hel", "lo"]);
    });
    const tokens: string[] = [];
    const res = await clientWith(create).chatStream(
      { model: "m", messages: [{ role: "user", content: "hi" }] },
      (t) => tokens.push(t),
    );
    expect(res.content).toBe("hello");
    expect(tokens.join("")).toBe("hello");
    expect(create).toHaveBeenCalledTimes(2);
  });

  it("normalises a cancel that lands mid-backoff into CancelledError", async () => {
    const ctrl = new AbortController();
    let calls = 0;
    const create = vi.fn(async () => {
      calls++;
      // Fire the cancel as soon as the first attempt fails, so the abort lands
      // while the retry backoff is sleeping.
      ctrl.abort();
      throw new APIError(503, undefined, "unavailable", undefined);
    });
    await expect(
      clientWith(create).chatStream({
        model: "m",
        messages: [{ role: "user", content: "hi" }],
        signal: ctrl.signal,
      }),
    ).rejects.toBeInstanceOf(CancelledError);
    // The cancel ends the loop — no second attempt.
    expect(calls).toBe(1);
  });

  it("gives up streaming after exhausting the retry budget", async () => {
    const create = vi.fn(async () => {
      throw new APIError(503, undefined, "always down", undefined);
    });
    await expect(
      clientWith(create).chatStream({ model: "m", messages: [{ role: "user", content: "hi" }] }),
    ).rejects.toThrow("always down");
    expect(create).toHaveBeenCalledTimes(4); // 1 + 3 retries
  });

  it("does NOT retry once a token has already been emitted", async () => {
    let calls = 0;
    const create = vi.fn(async () => {
      calls++;
      // Emits "hel" (i=0) then fails before "lo" (i=1) with an otherwise-retriable 503.
      return streamOf(["hel", "lo"], {
        at: 1,
        err: new APIError(503, undefined, "mid-stream blip", undefined),
      });
    });
    const tokens: string[] = [];
    await expect(
      clientWith(create).chatStream(
        { model: "m", messages: [{ role: "user", content: "hi" }] },
        (t) => tokens.push(t),
      ),
    ).rejects.toThrow("mid-stream blip");
    // A retry here would duplicate "hel" into the transcript — so we must NOT retry.
    expect(create).toHaveBeenCalledTimes(1);
    expect(tokens.join("")).toBe("hel");
  });
});
