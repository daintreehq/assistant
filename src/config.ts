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
import { createHash } from "node:crypto";
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
  windowId?: string;

  /** Permission tier the CLI operates at. */
  tier: Tier;

  /** When true, never actually call the network (used by tests / --offline). */
  offline: boolean;
}

export interface ConfigOverrides {
  projectPath?: string;
  stateDir?: string;
  projectId?: string;
  windowId?: string;
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

/**
 * Map an arbitrary project (or window) identifier to a single safe directory
 * name for use under the state root.
 *
 * The result is a human-readable slug plus a short SHA-256 suffix of the raw
 * input. The slug aids debugging; the hash guarantees that two distinct inputs
 * can never collide after slugging/truncation, and that path-traversal inputs
 * (e.g. `../../evil`) collapse to a single harmless segment.
 *
 * @internal
 */
export function projectIdToDir(rawId: string): string {
  const hash = createHash("sha256").update(rawId).digest("hex").slice(0, 8);
  const slug = rawId
    .toLowerCase()
    .replace(/[^a-z0-9_-]/g, "-")
    .replace(/-+/g, "-")
    .replace(/^-|-$/g, "")
    .slice(0, 40)
    .replace(/-$/, ""); // truncation may leave a trailing dash
  return slug ? `${slug}-${hash}` : hash;
}

export function loadConfig(overrides: ConfigOverrides = {}): AppConfig {
  const projectPath = path.resolve(overrides.projectPath ?? process.cwd());

  // Load .env from the project root (does not override already-set env vars).
  const envPath = path.join(projectPath, ".env");
  if (fs.existsSync(envPath)) {
    dotenv.config({ path: envPath });
  }

  const projectId = firstString(
    overrides.projectId,
    process.env.DAINTREE_PROJECT_ID,
  );
  const windowId = firstString(
    overrides.windowId,
    process.env.DAINTREE_WINDOW_ID,
  );

  // State location precedence (highest first):
  //   1. explicit override / DAINTREE_ASSISTANT_STATE_DIR — caller decides.
  //   2. per-project subdirectory derived from DAINTREE_PROJECT_ID, so
  //      concurrent Daintree projects never share a state.db (issue #4).
  //   3. legacy flat path — backward compatible for callers with no project id.
  // Window-level isolation is deliberately deferred (see issue #5); windowId is
  // read and surfaced on the config but does not yet affect the path.
  const stateRoot = path.join(os.homedir(), ".daintree", "assistant-cli");
  const stateDir =
    firstString(overrides.stateDir, process.env.DAINTREE_ASSISTANT_STATE_DIR) ??
    (projectId ? path.join(stateRoot, projectIdToDir(projectId)) : stateRoot);

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
    projectId,
    windowId,
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
    projectId: cfg.projectId ?? "(unset)",
    windowId: cfg.windowId ?? "(unset)",
    largeModel: cfg.largeModel,
    smallModel: cfg.smallModel,
    fireworksApiKey: redact(cfg.fireworksApiKey),
    mcpUrl: cfg.mcpUrl ?? "(unset → degraded local mode)",
    mcpToken: redact(cfg.mcpToken),
    tier: cfg.tier,
    offline: String(cfg.offline),
  };
}
