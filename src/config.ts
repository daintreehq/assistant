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
import { fileURLToPath } from "node:url";
import { createHash } from "node:crypto";
import dotenv from "dotenv";
import { Tier } from "./schemas.js";

export interface AppConfig {
  /** Working/project directory the CLI was launched in. */
  projectPath: string;
  /** Where CLI state (db, indexes) lives — never inside the repo. */
  stateDir: string;
  dbPath: string;
  /**
   * Where the debug log lives. GLOBAL across all projects (default
   * `~/.daintree/logs`), so a single tail covers every session regardless of which
   * project it was bound to. Override with DAINTREE_ASSISTANT_LOG_DIR.
   */
  logDir: string;

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
  /**
   * Window/session id Daintree injects (DAINTREE_WINDOW_ID) so a CLI bound to
   * one Daintree window can be told apart from another. Env-only — Daintree
   * sets it; the CLI never accepts it as a flag. See docs/DAINTREE_MCP.md.
   */
  windowId?: string;

  /** Permission tier the CLI operates at. */
  tier: Tier;

  /**
   * When true, the assistant skips its OWN per-action confirm sheet for the
   * interactive `main` actor — mutating tools run without a Y/N prompt. The
   * capability {@link tier} is the only remaining safeguard (it still gates what
   * is permitted at all). Driven by `DAINTREE_ASSISTANT_AUTO_APPROVE=1`, which
   * Daintree injects when the user enables "bypass permissions" for the
   * assistant. Non-interactive actors (timer/watcher/workflow) are unaffected —
   * they always need a scoped grant regardless of this flag.
   */
  autoApprove: boolean;

  /** When true, never actually call the network (used by tests / --offline). */
  offline: boolean;

  /**
   * When true, append a full-fidelity trace of EVERYTHING — every model request and
   * response, every tool/function call with its args and result, and the whole
   * watcher lifecycle — to a per-session `<date>-<sessionId>.log` under {@link logDir}
   * (global, default `~/.daintree/logs`). Each start opens its own file (never
   * clobbering a prior run), writes a `session.start` header (project, tier, models,
   * MCP), and deletes logs older than 7 days. Intentionally large; a pre-release
   * debugging aid, off by default. Driven by `DAINTREE_ASSISTANT_DEBUG_LOG=1` (also
   * settable in `.env`).
   */
  debugLog: boolean;
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
  autoApprove?: boolean;
  offline?: boolean;
  debugLog?: boolean;
  logDir?: string;
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
 * Resolve the `.env` that ships next to the assistant itself, by walking up from
 * this module to the nearest package.json. In dev that is the repo root; in a
 * `tsup` build it is the parent of `dist/`. Used as a low-precedence fallback so
 * dev/debug flags set beside the assistant apply regardless of which project's
 * cwd the session is bound to. Best-effort — returns undefined if nothing resolves.
 */
function assistantOwnEnvPath(): string | undefined {
  try {
    let dir = path.dirname(fileURLToPath(import.meta.url));
    for (let i = 0; i < 8; i++) {
      if (fs.existsSync(path.join(dir, "package.json"))) {
        return path.join(dir, ".env");
      }
      const parent = path.dirname(dir);
      if (parent === dir) break; // reached the filesystem root
      dir = parent;
    }
  } catch {
    /* best-effort — never block config load on this */
  }
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

  // Snapshot the REAL process environment BEFORE loading any .env file. Security-
  // sensitive controls (permission tier, auto-approve, offline) are read ONLY from
  // here or from explicit overrides — never from a loaded .env. The bound project
  // is arbitrary/untrusted code; without this, a repo-local `.env` could set
  // DAINTREE_ASSISTANT_AUTO_APPROVE=1 or _TIER=system and silently escalate the
  // assistant into running mutations unattended.
  const trustedEnv: Record<string, string | undefined> = { ...process.env };

  // Load .env from the project root (does not override already-set env vars).
  const envPath = path.join(projectPath, ".env");
  if (fs.existsSync(envPath)) {
    dotenv.config({ path: envPath });
  }
  // Also load the assistant's OWN package .env as a lower-precedence fallback.
  // When Daintree embeds us, projectPath is the *bound project's* cwd, so a flag
  // set next to the assistant (e.g. DAINTREE_ASSISTANT_DEBUG_LOG) would otherwise
  // never be read. dotenv never overrides an already-set var, so the real env and
  // the project .env above still win; this only fills gaps. A no-op in a published
  // install (no .env ships there) and when it resolves to the same project .env.
  const ownEnvPath = assistantOwnEnvPath();
  if (ownEnvPath && ownEnvPath !== envPath && fs.existsSync(ownEnvPath)) {
    dotenv.config({ path: ownEnvPath });
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
    // Trusted-env only: a bound project's .env must not be able to redirect where
    // the transcript/audit DB is written (same exfiltration class as the log dir).
    firstString(overrides.stateDir, trustedEnv.DAINTREE_ASSISTANT_STATE_DIR) ??
    (projectId ? path.join(stateRoot, projectIdToDir(projectId)) : stateRoot);

  fs.mkdirSync(stateDir, { recursive: true });

  const fireworksApiKey =
    firstString(overrides.fireworksApiKey, process.env.FIREWORKS_API_KEY) ?? "";

  // The Daintree Assistant is the workspace's own first-class orchestrator, so it
  // defaults to the highest tier — full access to every Daintree action. The
  // confirmation matrix (ALWAYS_CONFIRM in safety/policy.ts) still gates mutating
  // actions, so "system by default" grants reach, not unattended execution. Lower
  // it explicitly via `--tier` / DAINTREE_ASSISTANT_TIER (trusted env only — see
  // trustedEnv). Fail CLOSED: if a tier was explicitly given but is invalid, drop
  // to the least-privileged tier rather than silently defaulting to system.
  const rawTier = overrides.tier ?? trustedEnv.DAINTREE_ASSISTANT_TIER;
  const tierParsed = Tier.safeParse(rawTier ?? "system");
  const tier = tierParsed.success ? tierParsed.data : "supervisor";

  // Debug log dir is GLOBAL (not per-project), so one tail spans every session.
  // Resolve to an absolute, normalized path so a relative or trailing-slash
  // override doesn't break per-session file targeting in debugLog.ts. Read the
  // override from the TRUSTED env only: a bound project's .env must not be able to
  // redirect the full-fidelity log into a path it can read (exfiltration).
  const logDir = path.resolve(
    firstString(overrides.logDir, trustedEnv.DAINTREE_ASSISTANT_LOG_DIR) ??
      path.join(os.homedir(), ".daintree", "logs"),
  );

  return {
    projectPath,
    stateDir,
    dbPath: path.join(stateDir, "state.db"),
    logDir,
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
    tier,
    // Permission/exec controls come from the TRUSTED env only (see trustedEnv) —
    // never from a bound project's .env, which could otherwise escalate us.
    autoApprove:
      overrides.autoApprove ?? trustedEnv.DAINTREE_ASSISTANT_AUTO_APPROVE === "1",
    offline: overrides.offline ?? trustedEnv.DAINTREE_ASSISTANT_OFFLINE === "1",
    // debugLog may come from the assistant's own (trusted) .env fallback, so read
    // the merged env. It's safe because logs can only ever go to the trusted-only
    // logDir above — a project .env can at worst enable logging into the user's own
    // home dir, never redirect it.
    debugLog:
      overrides.debugLog ?? process.env.DAINTREE_ASSISTANT_DEBUG_LOG === "1",
  };
}

/** Human-readable, secret-redacted view of the config for /status and logging. */
export function describeConfig(cfg: AppConfig): Record<string, string> {
  const redact = (s?: string) =>
    s ? `${s.slice(0, 4)}…${s.slice(-2)} (${s.length})` : "(unset)";
  return {
    projectPath: cfg.projectPath,
    stateDir: cfg.stateDir,
    logDir: cfg.logDir,
    projectId: cfg.projectId ?? "(unset)",
    windowId: cfg.windowId ?? "(unset)",
    largeModel: cfg.largeModel,
    smallModel: cfg.smallModel,
    fireworksApiKey: redact(cfg.fireworksApiKey),
    mcpUrl: cfg.mcpUrl ?? "(unset → degraded local mode)",
    mcpToken: redact(cfg.mcpToken),
    tier: cfg.tier,
    autoApprove: String(cfg.autoApprove),
    offline: String(cfg.offline),
    debugLog: String(cfg.debugLog),
  };
}
