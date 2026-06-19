/**
 * Model router: maps the small/medium/large abstraction onto concrete Fireworks
 * model ids and the FireworksClient. The large model owns the main thread; the
 * small model does cheap repeated work (watchers, summaries, classification).
 * For v1 the medium tier routes to the large model.
 */
import type { AppConfig } from "../config.js";
import type { ModelTier } from "../schemas.js";
import {
  FireworksClient,
  CancelledError,
  ImageInputNotSupportedError,
  hasImageContent,
  type ChatOptions,
  type ChatContentPart,
  type ChatResult,
  type ChatTool,
} from "./fireworks.js";
import { logDebug } from "../debugLog.js";
import type { z } from "zod";

export class ModelRouter {
  readonly fw: FireworksClient;
  private cfg: AppConfig;

  constructor(cfg: AppConfig, fw?: FireworksClient) {
    this.cfg = cfg;
    this.fw = fw ?? new FireworksClient(cfg);
  }

  /**
   * Reject image content on any tier but `large`. Only the large model
   * (minimax-m3) is vision-capable; `small` is text-only and `medium` routes
   * through, so an image bound for either would either 400 at the provider or be
   * silently dropped. Gate on tier semantics (the stable contract), not the
   * resolved model id, and fail before any wire call so the error is a clear
   * local one. Called at the top of every model path.
   */
  private assertImageTier(tier: ModelTier, messages: ChatOptions["messages"]): void {
    if (tier !== "large" && hasImageContent(messages)) {
      throw new ImageInputNotSupportedError(
        `Image inputs require the large tier; got "${tier}" (only the large model is vision-capable).`,
      );
    }
  }

  modelFor(tier: ModelTier): string {
    switch (tier) {
      case "small":
        return this.cfg.smallModel;
      case "medium":
        return this.cfg.mediumModel;
      case "large":
      default:
        return this.cfg.largeModel;
    }
  }

  async chat(
    tier: ModelTier,
    opts: Omit<ChatOptions, "model">,
  ): Promise<ChatResult> {
    this.assertImageTier(tier, opts.messages);
    const model = this.modelFor(tier);
    this.logRequest("chat", tier, model, opts);
    try {
      const res = await this.fw.chat({ ...opts, model });
      this.logResponse("chat", tier, model, res);
      return res;
    } catch (err) {
      // A cancelled turn is a clean user abort, not a model error — trace it under
      // a distinct key so it doesn't read as a failure (mirrors stream()).
      if (err instanceof CancelledError) {
        logDebug(this.cfg, "model.cancelled", { kind: "chat", tier, model });
        throw err;
      }
      this.logError("chat", tier, model, err);
      throw err;
    }
  }

  async stream(
    tier: ModelTier,
    opts: Omit<ChatOptions, "model">,
    onToken?: (t: string) => void,
  ): Promise<ChatResult> {
    this.assertImageTier(tier, opts.messages);
    const model = this.modelFor(tier);
    this.logRequest("stream", tier, model, opts);
    try {
      const res = await this.fw.chatStream({ ...opts, model }, onToken);
      this.logResponse("stream", tier, model, res);
      return res;
    } catch (err) {
      // A cancelled turn is a clean user abort, not a model error — trace it under
      // a distinct key so it doesn't pollute the error log or read as a failure.
      if (err instanceof CancelledError) {
        logDebug(this.cfg, "model.cancelled", { kind: "stream", tier, model });
        throw err;
      }
      this.logError("stream", tier, model, err);
      throw err;
    }
  }

  async json<S extends z.ZodTypeAny>(
    tier: ModelTier,
    opts: Omit<ChatOptions, "model" | "tools" | "toolChoice">,
    schema: S,
  ): Promise<z.infer<S>> {
    this.assertImageTier(tier, opts.messages);
    const model = this.modelFor(tier);
    this.logRequest("json", tier, model, opts);
    try {
      const res = await this.fw.json({ ...opts, model }, schema);
      logDebug(this.cfg, "model.response", { kind: "json", tier, model, result: res });
      return res;
    } catch (err) {
      if (err instanceof CancelledError) {
        logDebug(this.cfg, "model.cancelled", { kind: "json", tier, model });
        throw err;
      }
      this.logError("json", tier, model, err);
      throw err;
    }
  }

  /** Trace a model call that threw, so the debug log shows failures, not just the
   *  request that preceded them. Re-throwing is the caller's job. */
  private logError(kind: string, tier: ModelTier, model: string, err: unknown): void {
    logDebug(this.cfg, "model.error", {
      kind,
      tier,
      model,
      error: err instanceof Error ? err.message : String(err),
    });
  }

  /** Trace an outgoing model request (full message array included). Tool specs are
   *  static, so we log just their names to keep the trace readable. */
  private logRequest(
    kind: string,
    tier: ModelTier,
    model: string,
    opts: { messages: ChatOptions["messages"]; tools?: ChatTool[]; toolChoice?: unknown; temperature?: number },
  ): void {
    logDebug(this.cfg, "model.request", {
      kind,
      tier,
      model,
      temperature: opts.temperature,
      toolChoice: opts.toolChoice,
      toolNames: opts.tools?.map((t) => t.function?.name),
      messages: opts.messages.map(redactImageData),
    });
  }

  private logResponse(
    kind: string,
    tier: ModelTier,
    model: string,
    res: ChatResult,
  ): void {
    logDebug(this.cfg, "model.response", {
      kind,
      tier,
      model,
      finishReason: res.finishReason,
      usage: res.usage,
      toolCalls: res.toolCalls,
      content: res.content,
    });
  }

  describe(): Record<string, string> {
    return {
      large: this.cfg.largeModel,
      medium: this.cfg.mediumModel,
      small: this.cfg.smallModel,
    };
  }
}

/**
 * Replace base64 image-data URIs in a message's content parts with a short size
 * marker before the message is written to the debug log. A captured screenshot
 * is hundreds of KB of base64 — logging it verbatim would bloat the trace and
 * leak the raw bytes. Text parts and string content pass through untouched.
 */
function redactImageData(m: ChatOptions["messages"][number]): ChatOptions["messages"][number] {
  if (!Array.isArray(m.content)) return m;
  const parts: ChatContentPart[] = m.content.map((part) => {
    if (part.type !== "image_url" || !part.image_url.url.startsWith("data:")) return part;
    const kb = Math.ceil((part.image_url.url.length * 3) / 4 / 1024);
    return { type: "image_url", image_url: { url: `<redacted base64 ~${kb}kb>` } };
  });
  return { ...m, content: parts };
}
