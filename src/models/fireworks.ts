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
import OpenAI from "openai";
import type { z } from "zod";
import type { AppConfig } from "../config.js";

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
}

export interface ChatResult {
  content: string;
  reasoning: string;
  toolCalls: ToolCallRequest[];
  finishReason: string;
  usage?: { promptTokens?: number; completionTokens?: number; totalTokens?: number };
}

export class FireworksUnavailableError extends Error {
  readonly code = "FIREWORKS_UNAVAILABLE";
  constructor(message: string) {
    super(message);
    this.name = "FireworksUnavailableError";
  }
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
    });
  }

  private guard(): void {
    if (this.cfg.offline) throw new FireworksUnavailableError("offline mode");
    if (!this.cfg.fireworksApiKey)
      throw new FireworksUnavailableError("FIREWORKS_API_KEY not set");
  }

  async chat(opts: ChatOptions): Promise<ChatResult> {
    this.guard();
    const resp = await this.client.chat.completions.create({
      model: opts.model,
      messages: toWireMessages(opts.messages) as never,
      tools: opts.tools as never,
      tool_choice: opts.toolChoice as never,
      temperature: opts.temperature ?? 0.3,
      max_tokens: opts.maxTokens,
      ...(opts.promptCacheKey
        ? ({ prompt_cache_key: opts.promptCacheKey } as Record<string, unknown>)
        : {}),
    });
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
          }
        : undefined,
    };
  }

  async chatStream(
    opts: ChatOptions,
    onToken?: (visible: string) => void,
  ): Promise<ChatResult> {
    this.guard();
    const stream = await this.client.chat.completions.create({
      model: opts.model,
      messages: toWireMessages(opts.messages) as never,
      tools: opts.tools as never,
      tool_choice: opts.toolChoice as never,
      temperature: opts.temperature ?? 0.3,
      max_tokens: opts.maxTokens,
      stream: true,
      ...(opts.promptCacheKey
        ? ({ prompt_cache_key: opts.promptCacheKey } as Record<string, unknown>)
        : {}),
    });

    const filter = new ThinkFilter();
    const toolAcc = new Map<number, { id: string; name: string; args: string }>();
    let finishReason = "stop";

    for await (const chunk of stream) {
      const choice = chunk.choices[0];
      if (!choice) continue;
      const delta = choice.delta;
      if (delta?.content) {
        const visible = filter.push(delta.content);
        if (visible && onToken) onToken(visible);
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
    if (tail && onToken) onToken(tail);

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
    };
  }

  /** Strict JSON-object completion validated against a Zod schema. */
  async json<S extends z.ZodTypeAny>(
    opts: Omit<ChatOptions, "tools" | "toolChoice">,
    schema: S,
  ): Promise<z.infer<S>> {
    this.guard();
    const resp = await this.client.chat.completions.create({
      model: opts.model,
      messages: toWireMessages(opts.messages) as never,
      temperature: opts.temperature ?? 0,
      max_tokens: opts.maxTokens,
      response_format: { type: "json_object" } as never,
    });
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

/** Pull the first balanced JSON object/array out of a string. */
function extractJson(s: string): string {
  const start = s.search(/[[{]/);
  if (start === -1) return s;
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
