/**
 * Fireworks AI client (OpenAI-compatible).
 *
 * Wraps the official `openai` SDK pointed at the Fireworks base URL. Provides:
 *   - chat()         non-streaming completion (returns text + tool calls)
 *   - chatStream()   streaming completion with a token callback
 *   - json()         strict JSON-object completion validated with a Zod schema
 *
 * Reasoning models can emit <think>…</think> in delta.content; ThinkFilter keeps
 * that out of user-facing output while preserving the final answer.
 */
import OpenAI, { APIUserAbortError } from "openai";
import type { z } from "zod";
import type { AppConfig } from "../config.js";
import {
  abortableSleep,
  isRetriableModelError,
  modelRetryDelayMs,
  retryModelCall,
  MODEL_RETRY_POLICY,
  MODEL_REQUEST_TIMEOUT_MS,
  MODEL_STREAM_TIMEOUT_MS,
} from "../reliability.js";

export interface ToolCallRequest {
  id: string;
  type: "function";
  function: { name: string; arguments: string };
}

export interface ChatMessage {
  role: "system" | "user" | "assistant" | "tool";
  content: string | null;
  tool_calls?: ToolCallRequest[];
  tool_call_id?: string;
  name?: string;
}

export interface ChatTool {
  type: "function";
  function: {
    name: string;
    description: string;
    parameters: Record<string, unknown>;
  };
}

export interface ChatOptions {
  model: string;
  messages: ChatMessage[];
  tools?: ChatTool[];
  toolChoice?: "auto" | "none" | "required";
  temperature?: number;
  maxTokens?: number;
  /** Cache a static system-prompt prefix on the Fireworks side. */
  promptCacheKey?: string;
  /**
   * Abort the in-flight request. Used by the UI's Escape-to-cancel path: when the
   * user cancels a turn, the signal fires and the in-flight call rejects. Honoured
   * by all three paths — streaming and the one-shot chat()/json() — so a cancel
   * that lands during a pre-turn auto-compact or recipe-selection call tears the
   * request down instead of running it to completion in the background.
   */
  signal?: AbortSignal;
}

export interface ChatResult {
  content: string;
  reasoning: string;
  toolCalls: ToolCallRequest[];
  finishReason: string;
  usage?: {
    promptTokens?: number;
    completionTokens?: number;
    totalTokens?: number;
    /** Cached prompt tokens (billed at a discount), when the provider reports them. */
    cachedTokens?: number;
  };
}

export class FireworksUnavailableError extends Error {
  readonly code = "FIREWORKS_UNAVAILABLE";
  constructor(message: string) {
    super(message);
    this.name = "FireworksUnavailableError";
  }
}

/**
 * A streaming turn the caller aborted (the UI's Escape-to-cancel). Distinct from a
 * model failure: the agent loop treats it as a clean stop (an info note, not a red
 * error) rather than surfacing it as a broken turn. Raised by chatStream() when the
 * abort signal fires, so the SDK's transport-level AbortError never leaks upward.
 */
export class CancelledError extends Error {
  readonly code = "CANCELLED";
  constructor(message = "Turn cancelled") {
    super(message);
    this.name = "CancelledError";
  }
}

/**
 * Whether an error thrown out of the OpenAI SDK is an abort. The SDK wraps a fired
 * AbortSignal as APIUserAbortError, but a raw `for await` interruption can surface
 * as a DOMException/Error named "AbortError" — accept both so cancellation is never
 * misclassified as a model error.
 */
function isAbortError(err: unknown): boolean {
  return (
    err instanceof APIUserAbortError ||
    (err instanceof Error && err.name === "AbortError")
  );
}

/** Incrementally separates <think>…</think> reasoning from visible content. */
export class ThinkFilter {
  private inThink = false;
  private buf = "";
  reasoning = "";
  visible = "";

  /** Push a content delta; returns the newly visible (non-think) text. */
  push(delta: string): string {
    this.buf += delta;
    let out = "";
    while (this.buf.length > 0) {
      if (!this.inThink) {
        const open = this.buf.indexOf("<think>");
        if (open === -1) {
          // Hold back a possible partial "<think>" tag at the tail.
          const safe = keepBack(this.buf, "<think>");
          out += this.buf.slice(0, safe);
          this.buf = this.buf.slice(safe);
          break;
        }
        out += this.buf.slice(0, open);
        this.buf = this.buf.slice(open + "<think>".length);
        this.inThink = true;
      } else {
        const close = this.buf.indexOf("</think>");
        if (close === -1) {
          const safe = keepBack(this.buf, "</think>");
          this.reasoning += this.buf.slice(0, safe);
          this.buf = this.buf.slice(safe);
          break;
        }
        this.reasoning += this.buf.slice(0, close);
        this.buf = this.buf.slice(close + "</think>".length);
        this.inThink = false;
      }
    }
    this.visible += out;
    return out;
  }

  /** Flush any held-back tail at end of stream. */
  end(): string {
    const rest = this.buf;
    this.buf = "";
    if (this.inThink) {
      this.reasoning += rest;
      return "";
    }
    this.visible += rest;
    return rest;
  }
}

/** Largest prefix length of `s` safe to emit without splitting a possible `tag`. */
function keepBack(s: string, tag: string): number {
  const maxOverlap = Math.min(tag.length - 1, s.length);
  for (let k = maxOverlap; k > 0; k--) {
    if (s.slice(s.length - k) === tag.slice(0, k)) return s.length - k;
  }
  return s.length;
}

export class FireworksClient {
  private client: OpenAI;
  private cfg: AppConfig;

  constructor(cfg: AppConfig) {
    this.cfg = cfg;
    this.client = new OpenAI({
      baseURL: cfg.fireworksBaseUrl,
      apiKey: cfg.fireworksApiKey || "missing-key",
      // Own all retry logic explicitly (see reliability.ts) instead of letting the
      // SDK silently retry — its default maxRetries:2 would stack on top of ours,
      // making the real attempt count unpredictable and ignoring our backoff.
      maxRetries: 0,
    });
  }

  private guard(): void {
    if (this.cfg.offline) throw new FireworksUnavailableError("offline mode");
    if (!this.cfg.fireworksApiKey)
      throw new FireworksUnavailableError("FIREWORKS_API_KEY not set");
  }

  /**
   * Build the SDK RequestOptions for one attempt: the turn's abort signal (so a
   * cancel tears the request down) plus a per-attempt `timeout` (so a hung attempt
   * is abandoned and retried). We pass `timeout` as a plain number rather than
   * combining signals per attempt — AbortSignal.any leaks listeners onto the long-
   * lived turn signal (Node #54614), so we let the SDK race its own timeout.
   */
  private requestOptions(
    signal: AbortSignal | undefined,
    timeoutMs: number,
  ): { signal?: AbortSignal; timeout: number } {
    return signal ? { signal, timeout: timeoutMs } : { timeout: timeoutMs };
  }

  async chat(opts: ChatOptions): Promise<ChatResult> {
    this.guard();
    const payload = {
      model: opts.model,
      messages: toWireMessages(opts.messages) as never,
      tools: opts.tools as never,
      tool_choice: opts.toolChoice as never,
      temperature: opts.temperature ?? 0.3,
      max_tokens: opts.maxTokens,
      ...(opts.promptCacheKey
        ? ({ prompt_cache_key: opts.promptCacheKey } as Record<string, unknown>)
        : {}),
    };
    let resp;
    try {
      // Retry transient 5xx / rate-limit / connection failures with backoff so a
      // single blip doesn't break the call; a per-attempt timeout abandons a hung
      // request. A user abort is never retried (isRetriableModelError excludes it).
      resp = await retryModelCall(
        () =>
          this.client.chat.completions.create(
            payload,
            this.requestOptions(opts.signal, MODEL_REQUEST_TIMEOUT_MS),
          ),
        { signal: opts.signal },
      );
    } catch (err) {
      // Normalise a user abort the same way chatStream does, so a cancel during a
      // pre-turn chat() (e.g. auto-compact summary) surfaces as CancelledError
      // rather than the SDK's transport-level abort representation.
      if (isAbortError(err)) throw new CancelledError();
      throw err;
    }
    const choice = resp.choices[0];
    const filter = new ThinkFilter();
    filter.push(choice.message.content ?? "");
    filter.end();
    return {
      content: filter.visible.trim(),
      reasoning: filter.reasoning.trim(),
      toolCalls: normalizeToolCalls(choice.message.tool_calls),
      finishReason: choice.finish_reason ?? "stop",
      usage: resp.usage
        ? {
            promptTokens: resp.usage.prompt_tokens,
            completionTokens: resp.usage.completion_tokens,
            totalTokens: resp.usage.total_tokens,
            cachedTokens: (
              resp.usage as { prompt_tokens_details?: { cached_tokens?: number } }
            ).prompt_tokens_details?.cached_tokens,
          }
        : undefined,
    };
  }

  async chatStream(
    opts: ChatOptions,
    onToken?: (visible: string) => void,
  ): Promise<ChatResult> {
    this.guard();
    const payload = {
      model: opts.model,
      messages: toWireMessages(opts.messages) as never,
      tools: opts.tools as never,
      tool_choice: opts.toolChoice as never,
      temperature: opts.temperature ?? 0.3,
      max_tokens: opts.maxTokens,
      // `as const` keeps this a literal `true` so the SDK's create() overload
      // resolves to the streaming (Stream<…>) return, not the union — a plain
      // const payload would widen it to `boolean` and break `for await`.
      stream: true as const,
      // Ask the streaming endpoint to emit a final usage-only chunk; without
      // this the OpenAI-compatible API reports no usage on streamed calls.
      stream_options: { include_usage: true },
      ...(opts.promptCacheKey
        ? ({ prompt_cache_key: opts.promptCacheKey } as Record<string, unknown>)
        : {}),
    };

    // The abort signal rides as the second RequestOptions arg; the SDK forwards it
    // to fetch so the connection is actually torn down, not just abandoned. A
    // per-attempt timeout abandons a stream that never produces tokens.
    //
    // Retry is PRE-TOKEN ONLY: a transient failure while acquiring the stream (or
    // before the first visible token reaches the caller) is retried with backoff,
    // but once any token has been emitted, retrying would duplicate output into the
    // immutable transcript (see CLAUDE.md <Static> invariant), so a later failure
    // propagates unchanged.
    let emitted = false;
    for (let attempt = 0; ; attempt++) {
      // Fresh accumulators per attempt — a retry restarts the stream from scratch.
      const filter = new ThinkFilter();
      const toolAcc = new Map<number, { id: string; name: string; args: string }>();
      let finishReason = "stop";
      let usage: ChatResult["usage"];
      try {
        const stream = await this.client.chat.completions.create(
          payload,
          this.requestOptions(opts.signal, MODEL_STREAM_TIMEOUT_MS),
        );

        for await (const chunk of stream) {
          // The usage-only chunk (sent because of stream_options.include_usage)
          // carries token counts and an empty `choices` array. Capture it whenever
          // present — some providers also attach usage to the final content chunk —
          // then fall through; the choice guard below skips the empty-choices case.
          if (chunk.usage) {
            usage = {
              promptTokens: chunk.usage.prompt_tokens,
              completionTokens: chunk.usage.completion_tokens,
              totalTokens: chunk.usage.total_tokens,
              cachedTokens: (
                chunk.usage as { prompt_tokens_details?: { cached_tokens?: number } }
              ).prompt_tokens_details?.cached_tokens,
            };
          }
          const choice = chunk.choices[0];
          if (!choice) continue;
          const delta = choice.delta;
          if (delta?.content) {
            const visible = filter.push(delta.content);
            if (visible) {
              // A token has reached the caller — past this point a failure can no
              // longer be retried without duplicating output.
              emitted = true;
              if (onToken) onToken(visible);
            }
          }
          if (delta?.tool_calls) {
            for (const tc of delta.tool_calls) {
              const idx = tc.index ?? 0;
              const cur = toolAcc.get(idx) ?? { id: "", name: "", args: "" };
              if (tc.id) cur.id = tc.id;
              if (tc.function?.name) cur.name = tc.function.name;
              if (tc.function?.arguments) cur.args += tc.function.arguments;
              toolAcc.set(idx, cur);
            }
          }
          if (choice.finish_reason) finishReason = choice.finish_reason;
        }
        const tail = filter.end();
        if (tail) {
          emitted = true;
          if (onToken) onToken(tail);
        }

        const toolCalls: ToolCallRequest[] = [...toolAcc.entries()]
          .sort((a, b) => a[0] - b[0])
          .map(([, v]) => ({
            id: v.id || `call_${Math.abs(hashString(v.name + v.args))}`,
            type: "function" as const,
            function: { name: v.name, arguments: v.args || "{}" },
          }))
          .filter((t) => t.function.name);

        return {
          content: filter.visible.trim(),
          reasoning: filter.reasoning.trim(),
          toolCalls,
          finishReason,
          usage,
        };
      } catch (err) {
        // A user-initiated abort is a clean cancellation, not a model failure —
        // normalise it to CancelledError so callers don't have to know about the
        // SDK's transport-level abort representation.
        if (isAbortError(err)) throw new CancelledError();
        // A cancel that landed concurrently with a transient error is still a
        // clean stop, not a model failure.
        if (opts.signal?.aborted) throw new CancelledError();
        // Only the pre-token window is retriable; after that a retry would
        // duplicate already-emitted output. Otherwise honour the bounded budget.
        if (
          emitted ||
          attempt >= MODEL_RETRY_POLICY.maxRetries ||
          !isRetriableModelError(err)
        ) {
          throw err;
        }
        try {
          await abortableSleep(modelRetryDelayMs(attempt, err), opts.signal);
        } catch {
          // Only abortableSleep's own AbortError reaches here — the turn was
          // cancelled mid-backoff. Normalise it to a clean cancellation so the
          // caller sees CancelledError, not a raw transport AbortError.
          throw new CancelledError();
        }
      }
    }
  }

  /** Strict JSON-object completion validated against a Zod schema. */
  async json<S extends z.ZodTypeAny>(
    opts: Omit<ChatOptions, "tools" | "toolChoice">,
    schema: S,
  ): Promise<z.infer<S>> {
    this.guard();
    const payload = {
      model: opts.model,
      messages: toWireMessages(opts.messages) as never,
      temperature: opts.temperature ?? 0,
      max_tokens: opts.maxTokens,
      response_format: { type: "json_object" } as never,
    };
    let resp;
    try {
      // Same bounded retry + per-attempt timeout as chat(): a transient 5xx on a
      // watcher classification or recipe-selection call now rides out instead of
      // collapsing the call to an "unknown" verdict on the first blip.
      resp = await retryModelCall(
        () =>
          this.client.chat.completions.create(
            payload,
            this.requestOptions(opts.signal, MODEL_REQUEST_TIMEOUT_MS),
          ),
        { signal: opts.signal },
      );
    } catch (err) {
      // A cancel during a pre-turn json() (e.g. recipe selection) is a clean abort,
      // not a model failure — normalise it like the other paths.
      if (isAbortError(err)) throw new CancelledError();
      throw err;
    }
    const raw = resp.choices[0].message.content ?? "{}";
    const cleaned = stripThink(raw);
    const json = JSON.parse(extractJson(cleaned));
    return schema.parse(json);
  }
}

/**
 * Reduce internal ChatMessages to exactly the fields the (OpenAI-compatible)
 * Fireworks API accepts. Notably: drop our `name` helper field on tool messages
 * and emit tool_calls as only {id,type,function} — extra fields cause a 400 on
 * replay.
 */
function toWireMessages(
  messages: ChatMessage[],
): Array<Record<string, unknown>> {
  return messages.map((m) => {
    if (m.role === "tool") {
      return { role: "tool", content: m.content ?? "", tool_call_id: m.tool_call_id };
    }
    if (m.role === "assistant") {
      const base: Record<string, unknown> = { role: "assistant", content: m.content };
      if (m.tool_calls?.length) {
        base.tool_calls = m.tool_calls.map((t) => ({
          id: t.id,
          type: "function",
          function: { name: t.function.name, arguments: t.function.arguments },
        }));
      }
      return base;
    }
    return { role: m.role, content: m.content ?? "" };
  });
}

function normalizeToolCalls(
  calls:
    | Array<{ id?: string; type?: string; function?: { name?: string; arguments?: string } }>
    | undefined,
): ToolCallRequest[] {
  if (!calls) return [];
  return calls
    .filter((c) => c.function?.name)
    .map((c) => ({
      id: c.id ?? `call_${Math.abs(hashString(c.function!.name!))}`,
      type: "function" as const,
      function: {
        name: c.function!.name!,
        arguments: c.function!.arguments ?? "{}",
      },
    }));
}

function stripThink(s: string): string {
  return s.replace(/<think>[\s\S]*?<\/think>/g, "").trim();
}

/**
 * Pull the first balanced JSON object/array out of a string, ignoring trailing
 * prose or stray `<think>` residue the provider may append. Scans with string-
 * and escape-awareness so braces inside string literals don't unbalance the
 * count. Falls back to the slice-from-first-bracket behavior if no balanced
 * span is found.
 */
export function extractJson(s: string): string {
  const start = s.search(/[[{]/);
  if (start === -1) return s;
  const open = s[start];
  const close = open === "{" ? "}" : "]";
  let depth = 0;
  let inString = false;
  let escaped = false;
  for (let i = start; i < s.length; i++) {
    const ch = s[i];
    if (inString) {
      if (escaped) escaped = false;
      else if (ch === "\\") escaped = true;
      else if (ch === '"') inString = false;
      continue;
    }
    if (ch === '"') inString = true;
    else if (ch === open) depth++;
    else if (ch === close) {
      depth--;
      if (depth === 0) return s.slice(start, i + 1);
    }
  }
  // Unbalanced — return from the first bracket and let JSON.parse report it.
  return s.slice(start);
}

function hashString(s: string): number {
  let h = 0;
  for (let i = 0; i < s.length; i++) {
    h = (h << 5) - h + s.charCodeAt(i);
    h |= 0;
  }
  return h;
}
