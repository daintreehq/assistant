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
  type ChatOptions,
  type ChatResult,
} from "./fireworks.js";
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

  chat(tier: ModelTier, opts: Omit<ChatOptions, "model">): Promise<ChatResult> {
    return this.fw.chat({ ...opts, model: this.modelFor(tier) });
  }

  stream(
    tier: ModelTier,
    opts: Omit<ChatOptions, "model">,
    onToken?: (t: string) => void,
  ): Promise<ChatResult> {
    return this.fw.chatStream({ ...opts, model: this.modelFor(tier) }, onToken);
  }

  json<S extends z.ZodTypeAny>(
    tier: ModelTier,
    opts: Omit<ChatOptions, "model" | "tools" | "toolChoice">,
    schema: S,
  ): Promise<z.infer<S>> {
    return this.fw.json({ ...opts, model: this.modelFor(tier) }, schema);
  }

  describe(): Record<string, string> {
    return {
      large: this.cfg.largeModel,
      medium: this.cfg.mediumModel,
      small: this.cfg.smallModel,
    };
  }
}
