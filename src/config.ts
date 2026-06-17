/**
 * Configuration loading for the Daintree Assistant CLI.
 *
 * Resolution order (highest priority first):
 *   1. Explicit overrides (CLI flags) passed to loadConfig().
 *   2. Environment variables (including those Daintree injects when it launches us).
 *   3. .env file in the project root (FIREWORKS_API_KEY etc.).
 *   4. Built-in defaults.
 */
import os from "node:os";
import path from "node:path";
import fs from "node:fs";
import dotenv from "dotenv";
import { Tier } from "./schemas.js";

export interface AppConfig {
  /** Working/project directory the CLI was launched in. */
  projectPath: string;
  /** Where CLI state (db, indexes, logs) lives — never inside the repo. */
  stateDir: string;
  dbPath: string;

  /** Fireworks. */
  fireworksApiKey: string;
  fireworksBaseUrl: string;
  largeModel: string;
  mediumModel: string;
  smallModel: string;

  /** Daintree MCP. */
  mcpUrl?: string;
  mcpToken?: string;
  projectId?: string;

  /** Permission tier the CLI operates at. */
  tier: Tier;

  /** When true, never actually call the network (used by tests / --offline). */
  offline: boolean;
}

export interface ConfigOverrides {
  projectPath?: string;
  stateDir?: string;
  mcpUrl?: string;
  mcpToken?: string;
  fireworksApiKey?: string;
  largeModel?: string;
  smallModel?: string;
  tier?: Tier;
  offline?: boolean;
}

export const DEFAULTS = {
  fireworksBaseUrl: "https://api.fireworks.ai/inference/v1",
  largeModel: "accounts/fireworks/models/minimax-m3",
  mediumModel: "accounts/fireworks/models/minimax-m3",
  smallModel: "accounts/fireworks/models/deepseek-v4-flash",
  defaultMcpUrl: "http://127.0.0.1:45454/mcp",
} as const;

function firstString(...vals: Array<string | undefined>): string | undefined {
  for (const v of vals) if (v && v.trim().length > 0) return v.trim();
  return undefined;
}

export function loadConfig(overrides: ConfigOverrides = {}): AppConfig {
  const projectPath = path.resolve(overrides.projectPath ?? process.cwd());

  // Load .env from the project root (does not override already-set env vars).
  const envPath = path.join(projectPath, ".env");
  if (fs.existsSync(envPath)) {
    dotenv.config({ path: envPath });
  }

  const stateDir =
    overrides.stateDir ??
    process.env.DAINTREE_ASSISTANT_STATE_DIR ??
    path.join(os.homedir(), ".daintree", "assistant-cli");

  fs.mkdirSync(stateDir, { recursive: true });

  const fireworksApiKey =
    firstString(overrides.fireworksApiKey, process.env.FIREWORKS_API_KEY) ?? "";

  const tier = Tier.safeParse(
    overrides.tier ?? process.env.DAINTREE_ASSISTANT_TIER ?? "operator",
  );

  return {
    projectPath,
    stateDir,
    dbPath: path.join(stateDir, "state.db"),
    fireworksApiKey,
    fireworksBaseUrl:
      firstString(process.env.FIREWORKS_BASE_URL) ?? DEFAULTS.fireworksBaseUrl,
    largeModel:
      firstString(overrides.largeModel, process.env.DAINTREE_LARGE_MODEL) ??
      DEFAULTS.largeModel,
    mediumModel:
      firstString(process.env.DAINTREE_MEDIUM_MODEL) ?? DEFAULTS.mediumModel,
    smallModel:
      firstString(overrides.smallModel, process.env.DAINTREE_SMALL_MODEL) ??
      DEFAULTS.smallModel,
    mcpUrl: firstString(overrides.mcpUrl, process.env.DAINTREE_MCP_URL),
    mcpToken: firstString(overrides.mcpToken, process.env.DAINTREE_MCP_TOKEN),
    projectId: firstString(process.env.DAINTREE_PROJECT_ID),
    tier: tier.success ? tier.data : "operator",
    offline: overrides.offline ?? process.env.DAINTREE_ASSISTANT_OFFLINE === "1",
  };
}

/** Human-readable, secret-redacted view of the config for /status and logging. */
export function describeConfig(cfg: AppConfig): Record<string, string> {
  const redact = (s?: string) =>
    s ? `${s.slice(0, 4)}…${s.slice(-2)} (${s.length})` : "(unset)";
  return {
    projectPath: cfg.projectPath,
    stateDir: cfg.stateDir,
    largeModel: cfg.largeModel,
    smallModel: cfg.smallModel,
    fireworksApiKey: redact(cfg.fireworksApiKey),
    mcpUrl: cfg.mcpUrl ?? "(unset → degraded local mode)",
    mcpToken: redact(cfg.mcpToken),
    tier: cfg.tier,
    offline: String(cfg.offline),
  };
}
