/**
 * Transient-failure resilience helpers (issue #123).
 *
 * A single 5xx, a dropped socket, or a momentarily hung request should not kill a
 * turn or wedge a watcher tick. This module centralises the small, dependency-free
 * primitives that make calls resilient:
 *   - bounded exponential backoff with FULL jitter (avoids retry thundering herds);
 *   - retryable-error predicates for the OpenAI SDK and the MCP SDK (so a user
 *     cancel or a 4xx is never retried, only genuine transient failures);
 *   - an abortable sleep (a backoff wait still tears down on the turn's cancel);
 *   - a rate-limit OUTPUT-signature detector the watcher uses to flag a terminal
 *     whose agent is being throttled by its own model provider.
 *
 * Everything here is pure / built-ins only, so it is unit-testable without a model
 * or a live MCP connection. Per-call timeouts are expressed as plain numbers and
 * handed to each SDK's own RequestOptions.timeout — we deliberately do NOT build
 * combined AbortSignals per attempt (AbortSignal.any leaks listeners onto a long-
 * lived turn signal — Node #54614), letting each SDK race its own timeout instead.
 */
import { APIError, APIUserAbortError } from "openai";
import { McpError, ErrorCode } from "@modelcontextprotocol/sdk/types.js";

/** A bounded exponential-backoff retry budget. `maxRetries` is the number of
 *  ADDITIONAL attempts after the first (so 3 ⇒ up to 4 total tries). */
export interface RetryPolicy {
  maxRetries: number;
  baseDelayMs: number;
  maxDelayMs: number;
}

/** Retry budget for Fireworks model calls. Three retries, ~0.5s→10s with jitter —
 *  enough to ride out a brief 5xx blip or a connection reset without making a
 *  failing provider feel hung. */
export const MODEL_RETRY_POLICY: RetryPolicy = {
  maxRetries: 3,
  baseDelayMs: 500,
  maxDelayMs: 10_000,
};

/** Per-attempt timeout for one-shot model calls (chat/json). A response that
 *  hasn't arrived in this window is treated as a hung attempt and retried. */
export const MODEL_REQUEST_TIMEOUT_MS = 60_000;

/** Per-attempt timeout for streaming model calls. Generous on purpose: it is a
 *  backstop against a truly hung stream, NOT a cap on a legitimately long one —
 *  killing a slow-but-progressing turn would be worse than the hang it guards. */
export const MODEL_STREAM_TIMEOUT_MS = 300_000;

/** Retry budget + per-attempt timeout for read-only MCP calls (watcher reads).
 *  Smaller than the model budget — a watcher tick must stay snappy — and applied
 *  ONLY to idempotent reads, never to mutating tools (a retried mutation could
 *  double-apply). */
export const MCP_READ_RETRY_POLICY: RetryPolicy = {
  maxRetries: 2,
  baseDelayMs: 250,
  maxDelayMs: 2_000,
};
export const MCP_READ_TIMEOUT_MS = 20_000;

/** Cap on an honoured Retry-After so a pathological header value can't park a
 *  turn for minutes; beyond this we fall back to ordinary jittered backoff. */
const MAX_RETRY_AFTER_MS = 30_000;

/** Largest tail window the watcher scans for a rate-limit signature. Bounded so a
 *  stale rate-limit line buried deep in scrollback can't keep re-flagging a
 *  terminal that has since moved on — only RECENT output counts as "throttled". */
export const RATE_LIMIT_TAIL_WINDOW = 1500;

/**
 * Full-jitter exponential backoff: a uniform random delay in
 * `[0, min(maxMs, baseMs * 2^attempt)]`. Full (not equal/decorrelated) jitter is
 * the simplest choice that still spreads concurrent retriers across the window.
 * `attempt` is 0-based (the first retry is attempt 0).
 */
export function fullJitterDelay(
  attempt: number,
  baseMs: number,
  maxMs: number,
): number {
  const ceiling = Math.min(maxMs, baseMs * 2 ** Math.max(0, attempt));
  return Math.floor(Math.random() * (ceiling + 1));
}

/** Whether an error out of the OpenAI SDK is a user-initiated abort. The SDK wraps
 *  a fired AbortSignal as APIUserAbortError, but a raw `for await` interruption can
 *  surface as an Error named "AbortError" — accept both so a cancel is never
 *  retried or misread as a transient model failure. */
function isModelAbort(err: unknown): boolean {
  return (
    err instanceof APIUserAbortError ||
    (err instanceof Error && err.name === "AbortError")
  );
}

/**
 * Whether a model-call error is worth retrying: a 429 (rate limit), a 5xx, or a
 * connection-level failure (APIConnectionError/Timeout, whose `.status` is
 * undefined). A user abort and any other 4xx (bad request, auth) are NOT
 * retriable — retrying them just wastes the budget and delays the real failure.
 */
export function isRetriableModelError(err: unknown): boolean {
  if (isModelAbort(err)) return false;
  if (err instanceof APIError) {
    const status = (err as APIError).status;
    if (status === undefined) return true; // connection error / timeout
    return status === 429 || status >= 500;
  }
  return false;
}

/** Specifically a 429 rate-limit error — distinguished from a plain 5xx so the
 *  backoff can honour a Retry-After header when the provider sends one. */
export function isRateLimitModelError(err: unknown): boolean {
  return err instanceof APIError && (err as APIError).status === 429;
}

/**
 * Parse a Retry-After value (ms or HTTP-date) from a header bag, tolerating both a
 * `Headers`-like object (`.get`) and a plain lowercased record (what the OpenAI
 * SDK attaches to `APIError.headers`). Prefers the non-standard `retry-after-ms`
 * when present. Returns undefined when nothing parseable is found.
 */
export function parseRetryAfterMs(headers: unknown): number | undefined {
  if (!headers || typeof headers !== "object") return undefined;
  const h = headers as Record<string, unknown> & { get?: (k: string) => unknown };
  const read = (k: string): string | undefined => {
    const v =
      typeof h.get === "function" ? h.get(k) : h[k] ?? h[k.toLowerCase()];
    return v == null ? undefined : String(v);
  };
  const ms = read("retry-after-ms");
  if (ms !== undefined) {
    const n = Number(ms);
    if (Number.isFinite(n) && n >= 0) return n;
  }
  const ra = read("retry-after");
  if (ra !== undefined) {
    const secs = Number(ra);
    if (Number.isFinite(secs) && secs >= 0) return secs * 1000;
    const when = Date.parse(ra);
    if (!Number.isNaN(when)) return Math.max(0, when - Date.now());
  }
  return undefined;
}

/** The delay before retrying a failed model attempt: an honoured (capped)
 *  Retry-After on a 429, otherwise full-jitter exponential backoff. */
export function modelRetryDelayMs(
  attempt: number,
  err: unknown,
  policy: RetryPolicy = MODEL_RETRY_POLICY,
): number {
  if (isRateLimitModelError(err)) {
    const ra = parseRetryAfterMs((err as { headers?: unknown }).headers);
    if (ra !== undefined) return Math.min(ra, MAX_RETRY_AFTER_MS);
  }
  return fullJitterDelay(attempt, policy.baseDelayMs, policy.maxDelayMs);
}

/** A sleep that resolves after `ms`, or rejects with an AbortError-named error the
 *  moment `signal` fires — so a backoff wait between attempts is itself
 *  cancellable. Removes its listener on both paths (no leak on the turn signal). */
export function abortableSleep(ms: number, signal?: AbortSignal): Promise<void> {
  return new Promise<void>((resolve, reject) => {
    if (signal?.aborted) {
      reject(makeAbortError());
      return;
    }
    let onAbort: (() => void) | undefined;
    const timer = setTimeout(() => {
      if (onAbort && signal) signal.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    if (signal) {
      onAbort = () => {
        clearTimeout(timer);
        signal.removeEventListener("abort", onAbort!);
        reject(makeAbortError());
      };
      signal.addEventListener("abort", onAbort, { once: true });
    }
  });
}

function makeAbortError(): Error {
  const e = new Error("The operation was aborted");
  e.name = "AbortError";
  return e;
}

/**
 * Retry a one-shot model call (chat/json) under `policy`, sleeping with backoff
 * between attempts. Stops immediately on a non-retriable error or a fired signal;
 * the final attempt's error propagates so the caller can normalise an abort to its
 * own CancelledError. The per-attempt timeout is baked into `attempt` by the
 * caller (via the SDK's RequestOptions.timeout) — each attempt is a fresh request.
 *
 * NOTE: deliberately NOT used for streaming. Once a stream has emitted a token to
 * the caller, re-running it would duplicate output into the immutable transcript,
 * so chatStream owns a bespoke loop that only retries BEFORE the first token.
 */
export async function retryModelCall<T>(
  attempt: () => Promise<T>,
  opts: { policy?: RetryPolicy; signal?: AbortSignal } = {},
): Promise<T> {
  const policy = opts.policy ?? MODEL_RETRY_POLICY;
  for (let i = 0; ; i++) {
    try {
      return await attempt();
    } catch (err) {
      if (i >= policy.maxRetries) throw err;
      if (opts.signal?.aborted) throw err;
      if (!isRetriableModelError(err)) throw err;
      await abortableSleep(modelRetryDelayMs(i, err, policy), opts.signal);
    }
  }
}

/**
 * Whether an MCP error is a transient transport hiccup worth retrying: a request
 * timeout (the SDK's -32001) or a closed connection (-32000), plus connection-
 * level transport errors (`fetch failed`, ECONNRESET, …). A JSON-RPC application
 * error (a tool that genuinely failed) is NOT retriable — only the transport is.
 */
export function isRetriableMcpError(err: unknown): boolean {
  if (err instanceof McpError) {
    return (
      err.code === ErrorCode.RequestTimeout ||
      err.code === ErrorCode.ConnectionClosed
    );
  }
  const msg = err instanceof Error ? err.message : String(err);
  return /fetch failed|ECONNRESET|ETIMEDOUT|ECONNREFUSED|socket hang up|network error|timed out/i.test(
    msg,
  );
}

/** Output fingerprints that mean an agent is being throttled by its model
 *  provider: HTTP 429/529, quota/rate-limit phrasing, retry-after, "overloaded".
 *  Intentionally broad (Anthropic/OpenAI/generic CLI wording) — this is interim
 *  local detection from stdout, pending an authoritative Daintree-side signal. */
const RATE_LIMIT_SIGNATURE =
  /\b(?:429|529)\b|too many requests|rate[ _-]?limit(?:ed|ing|s)?|quota (?:exceeded|exhausted)|insufficient[_ ]quota|retry[ -]?after\b|resource (?:exhausted|temporarily unavailable)|server is temporarily limiting|\boverloaded\b|you(?:'ve| have) hit your limit|exceed (?:your|the) .{0,40}rate limit/i;

/** Whether recent terminal output shows a rate-limit / API-throttle signature.
 *  Pass a bounded RECENT slice (see RATE_LIMIT_TAIL_WINDOW) — scanning deep
 *  scrollback would re-flag a terminal that has already recovered. */
export function detectRateLimitSignature(text: string | undefined): boolean {
  if (!text) return false;
  return RATE_LIMIT_SIGNATURE.test(text);
}
