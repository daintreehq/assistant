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
  type ChatOptions,
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
    const model = this.modelFor(tier);
    this.logRequest("chat", tier, model, opts);
    try {
      const res = await this.fw.chat({ ...opts, model });
      this.logResponse("chat", tier, model, res);
      return res;
    } catch (err) {
      this.logError("chat", tier, model, err);
      throw err;
    }
  }

  async stream(
    tier: ModelTier,
    opts: Omit<ChatOptions, "model">,
    onToken?: (t: string) => void,
  ): Promise<ChatResult> {
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
    const model = this.modelFor(tier);
    this.logRequest("json", tier, model, opts);
    try {
      const res = await this.fw.json({ ...opts, model }, schema);
      logDebug(this.cfg, "model.response", { kind: "json", tier, model, result: res });
      return res;
    } catch (err) {
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
      messages: opts.messages,
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
