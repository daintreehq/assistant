import { describe, it, expect } from "vitest";
import { APIError, APIUserAbortError } from "openai";
import { McpError, ErrorCode } from "@modelcontextprotocol/sdk/types.js";
import {
  abortableSleep,
  detectRateLimitSignature,
  fullJitterDelay,
  isRateLimitModelError,
  isRetriableMcpError,
  isRetriableModelError,
  modelRetryDelayMs,
  parseRetryAfterMs,
} from "../src/reliability.js";

describe("fullJitterDelay", () => {
  it("never exceeds the exponential ceiling and stays non-negative", () => {
    for (let attempt = 0; attempt < 6; attempt++) {
      const ceiling = Math.min(10_000, 500 * 2 ** attempt);
      for (let i = 0; i < 50; i++) {
        const d = fullJitterDelay(attempt, 500, 10_000);
        expect(d).toBeGreaterThanOrEqual(0);
        expect(d).toBeLessThanOrEqual(ceiling);
      }
    }
  });
});

describe("isRetriableModelError", () => {
  it("retries 429, 500-599, and connection errors (undefined status)", () => {
    expect(isRetriableModelError(new APIError(429, undefined, "rl", undefined))).toBe(true);
    expect(isRetriableModelError(new APIError(500, undefined, "boom", undefined))).toBe(true);
    expect(isRetriableModelError(new APIError(503, undefined, "down", undefined))).toBe(true);
    expect(isRetriableModelError(new APIError(undefined, undefined, "conn", undefined))).toBe(true);
  });

  it("does NOT retry a user abort or a 4xx", () => {
    expect(isRetriableModelError(new APIUserAbortError())).toBe(false);
    expect(isRetriableModelError(new APIError(400, undefined, "bad", undefined))).toBe(false);
    expect(isRetriableModelError(new APIError(401, undefined, "auth", undefined))).toBe(false);
    const abort = new Error("The operation was aborted");
    abort.name = "AbortError";
    expect(isRetriableModelError(abort)).toBe(false);
    expect(isRetriableModelError(new Error("plain"))).toBe(false);
  });

  it("flags a 429 specifically as a rate-limit error", () => {
    expect(isRateLimitModelError(new APIError(429, undefined, "rl", undefined))).toBe(true);
    expect(isRateLimitModelError(new APIError(500, undefined, "boom", undefined))).toBe(false);
  });
});

describe("parseRetryAfterMs", () => {
  it("prefers retry-after-ms, then delta-seconds", () => {
    expect(parseRetryAfterMs({ "retry-after-ms": "1500" })).toBe(1500);
    expect(parseRetryAfterMs({ "retry-after": "2" })).toBe(2000);
  });

  it("returns undefined when no parseable header is present", () => {
    expect(parseRetryAfterMs(undefined)).toBeUndefined();
    expect(parseRetryAfterMs({})).toBeUndefined();
    expect(parseRetryAfterMs({ "retry-after": "not-a-date" })).toBeUndefined();
  });

  it("modelRetryDelayMs honours a capped Retry-After on a 429", () => {
    const err = new APIError(429, undefined, "rl", { "retry-after-ms": "2000" });
    expect(modelRetryDelayMs(0, err)).toBe(2000);
    // A pathological header is capped, not honoured verbatim.
    const huge = new APIError(429, undefined, "rl", { "retry-after-ms": "999999999" });
    expect(modelRetryDelayMs(0, huge)).toBeLessThanOrEqual(30_000);
  });
});

describe("isRetriableMcpError", () => {
  it("retries timeout / connection-closed McpErrors and transport errors", () => {
    expect(isRetriableMcpError(new McpError(ErrorCode.RequestTimeout, "timed out"))).toBe(true);
    expect(isRetriableMcpError(new McpError(ErrorCode.ConnectionClosed, "closed"))).toBe(true);
    expect(isRetriableMcpError(new Error("fetch failed"))).toBe(true);
    expect(isRetriableMcpError(new Error("read ECONNRESET"))).toBe(true);
  });

  it("does NOT retry a JSON-RPC application error", () => {
    expect(isRetriableMcpError(new McpError(ErrorCode.InvalidParams, "bad args"))).toBe(false);
    expect(isRetriableMcpError(new Error("tool returned an error"))).toBe(false);
  });
});

describe("detectRateLimitSignature", () => {
  it("matches common rate-limit / throttle fingerprints", () => {
    for (const s of [
      "API Error: Rate limit reached",
      "HTTP 429 Too Many Requests",
      "529 Overloaded",
      "Error: quota exceeded for this org",
      "please retry-after 30s",
      "This request would exceed your organization's rate limit",
      "server is temporarily limiting requests",
      "Server overloaded, try again",
    ]) {
      expect(detectRateLimitSignature(s)).toBe(true);
    }
  });

  it("does NOT match ordinary output", () => {
    expect(detectRateLimitSignature("")).toBe(false);
    expect(detectRateLimitSignature(undefined)).toBe(false);
    expect(detectRateLimitSignature("Running tests... 42 passed")).toBe(false);
    expect(detectRateLimitSignature("Compiling module rate_calculator.ts")).toBe(false);
  });

  it("does NOT false-positive on identifiers that merely contain the words", () => {
    // Tightened fingerprints: word-bounded "overloaded" and a "retry-after" that
    // must be header-shaped, so code identifiers / config keys don't trip it.
    expect(detectRateLimitSignature("calling overloaded_function() in queue")).toBe(false);
    expect(detectRateLimitSignature("config: { retry_after_ms: 0 }")).toBe(false);
  });
});

describe("abortableSleep", () => {
  it("rejects immediately with an AbortError when the signal is already fired", async () => {
    const ctrl = new AbortController();
    ctrl.abort();
    await expect(abortableSleep(1000, ctrl.signal)).rejects.toMatchObject({
      name: "AbortError",
    });
  });

  it("rejects with an AbortError when the signal fires during the wait", async () => {
    const ctrl = new AbortController();
    const p = abortableSleep(1000, ctrl.signal);
    ctrl.abort();
    await expect(p).rejects.toMatchObject({ name: "AbortError" });
  });

  it("resolves normally when never aborted", async () => {
    await expect(abortableSleep(1)).resolves.toBeUndefined();
  });
});
