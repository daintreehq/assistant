/**
 * Full-fidelity debug trace — a pre-release observability aid.
 *
 * When `config.debugLog` is on (env `DAINTREE_ASSISTANT_DEBUG_LOG=1`), EVERYTHING
 * the assistant does is appended to a per-session human-readable file:
 *
 *     <logDir>/<YYYY-MM-DD>-<sessionId>.log   (logDir defaults to ~/.daintree/logs)
 *
 * That means every model request and response (full message arrays included),
 * every tool/function call with its arguments and result, and the whole watcher
 * lifecycle. These logs are intentionally large and complete — values are NOT
 * truncated — so you can reconstruct a session end to end.
 *
 * Each process gets its OWN dated+id file (no shared `debug.log`, no rotation): a
 * new instance never clobbers a previous run's log. {@link startDebugLog} opens the
 * file once at boot, writes a `session.start` header naming the project/tier/models,
 * and returns the path so the caller can announce "logging to <file>". As part of
 * boot it also deletes any log older than {@link MAX_LOG_AGE_MS} (7 days).
 *
 * The log directory is GLOBAL, not per-project, so one `tail -f` covers every
 * session regardless of which project it was bound to.
 *
 * Logging is a no-op when disabled, and it NEVER throws into the caller: a write
 * failure warns once on stderr and is otherwise swallowed, because losing a debug
 * line must never take down a model call, a tool dispatch, or a watcher check.
 */
import fs from "node:fs";
import path from "node:path";
import type { AppConfig } from "./config.js";

/** Logs older than this (by mtime) are deleted at boot. */
export const MAX_LOG_AGE_MS = 7 * 24 * 60 * 60 * 1000;

/** A field rendered inline (on the header line) must be a scalar this short. */
const INLINE_MAX = 120;

let warnedOnce = false;
/** The file the current process writes to, once logging has started. */
let activeLogPath: string | undefined;

/** Just the config fields the logger needs — keeps call sites and tests light. */
export type DebugLogConfig = Pick<AppConfig, "debugLog" | "logDir">;

function isoNow(): string {
  try {
    return new Date().toISOString();
  } catch {
    return String(Date.now());
  }
}

/** Session log filename: `<YYYY-MM-DD>-<id>.log`. */
function sessionLogFileName(id: string): string {
  const date = isoNow().slice(0, 10);
  const safeId = id.replace(/[^\w.-]/g, "") || "session";
  return `${date}-${safeId}.log`;
}

function randomId(): string {
  return Math.random().toString(36).slice(2, 10);
}

/**
 * The file `logDebug` should append to within `logDir`. Reuses the session file
 * opened by {@link startDebugLog}; if logging wrote before boot (e.g. a direct
 * `logDebug` in a test) or the active path belongs to a different `logDir`, it
 * lazily opens a fresh session file so writes still coalesce into one file.
 */
function resolveTarget(logDir: string): string {
  if (activeLogPath && path.dirname(activeLogPath) === logDir) return activeLogPath;
  activeLogPath = path.join(logDir, sessionLogFileName(randomId()));
  return activeLogPath;
}

/** The log file the current process is writing to, once logging has started. */
export function currentDebugLogPath(): string | undefined {
  return activeLogPath;
}

function safeStringify(v: unknown, indent?: number): string {
  try {
    return JSON.stringify(v, null, indent) ?? String(v);
  } catch {
    return String(v);
  }
}

/** A short, single-line scalar that can sit on the header line as `key=value`. */
function isInlineScalar(v: unknown): boolean {
  if (v === null || v === undefined) return true;
  if (typeof v === "number" || typeof v === "boolean") return true;
  if (typeof v === "string") return v.length <= INLINE_MAX && !v.includes("\n");
  return false;
}

/** Render a large/structured value as an indented block (full, untruncated). */
function blockValue(v: unknown): string {
  const s = typeof v === "string" ? v : safeStringify(v, 2);
  return s
    .split("\n")
    .map((l) => `    ${l}`)
    .join("\n");
}

function warnOnce(err: unknown): void {
  if (warnedOnce) return;
  warnedOnce = true;
  console.error(
    `[debugLog] write failed (logging disabled for this run): ${
      err instanceof Error ? err.message : String(err)
    }`,
  );
}

/**
 * Append one event to the current session log. No-op unless `cfg.debugLog` is on.
 * `event` is a short dotted name (e.g. "model.request", "tool.call",
 * "watcher.stop"); `fields` carries the structured payload. Short scalars render
 * inline on the header line; objects, arrays, and multi-line strings render as
 * indented blocks below it — full and untruncated.
 */
export function logDebug(
  cfg: DebugLogConfig | undefined,
  event: string,
  fields: Record<string, unknown> = {},
): void {
  // Tolerate a missing/partial config — logging must never break the caller.
  if (!cfg?.debugLog || !cfg.logDir) return;
  const ts = isoNow();
  try {
    fs.mkdirSync(cfg.logDir, { recursive: true });
    const target = resolveTarget(cfg.logDir);

    const inline: string[] = [];
    const blocks: string[] = [];
    for (const [k, v] of Object.entries(fields)) {
      if (v === undefined) continue; // omit absent fields — they're just noise
      if (isInlineScalar(v)) inline.push(`${k}=${v === null ? "null" : String(v)}`);
      else blocks.push(`  ${k}:\n${blockValue(v)}`);
    }

    let out = `${ts}  ${event}${inline.length ? `  ${inline.join(" ")}` : ""}\n`;
    if (blocks.length) out += `${blocks.join("\n")}\n`;
    fs.appendFileSync(target, out);
  } catch (err) {
    warnOnce(err);
  }
}

/**
 * Begin a debug-log session at assistant startup. No-op (returns undefined) when
 * logging is off. Otherwise:
 *   1. delete any log older than {@link MAX_LOG_AGE_MS} (7 days);
 *   2. open a fresh `<date>-<sessionId>.log`;
 *   3. write a `session.start` header naming the project/tier/models/MCP;
 * and return the file path so the caller can announce "logging to <path>". Call
 * once per process, after config is loaded.
 */
export function startDebugLog(cfg: AppConfig, sessionId?: string): string | undefined {
  if (!cfg.debugLog || !cfg.logDir) return undefined;
  pruneOldLogs(cfg.logDir);
  activeLogPath = path.join(cfg.logDir, sessionLogFileName(sessionId ?? randomId()));
  logDebug(cfg, "session.start", {
    project: cfg.projectPath,
    projectId: cfg.projectId,
    windowId: cfg.windowId,
    tier: cfg.tier,
    largeModel: cfg.largeModel,
    mediumModel: cfg.mediumModel,
    smallModel: cfg.smallModel,
    mcpUrl: cfg.mcpUrl ?? "(unset)",
    offline: cfg.offline,
    autoApprove: cfg.autoApprove,
    stateDir: cfg.stateDir,
    logDir: cfg.logDir,
    pid: process.pid,
    node: process.version,
  });
  return activeLogPath;
}

/** Matches our session log filenames (`<YYYY-MM-DD>-<id>.log`) so pruning never
 *  touches unrelated `*.log` files if logDir points at a shared directory. */
const SESSION_LOG_RE = /^\d{4}-\d{2}-\d{2}-.+\.log$/;

/** Delete our own session logs in `dir` older than {@link MAX_LOG_AGE_MS} by mtime.
 *  Never throws; only files matching {@link SESSION_LOG_RE} are eligible. */
function pruneOldLogs(dir: string): void {
  let entries: string[];
  try {
    entries = fs.readdirSync(dir);
  } catch {
    return; // directory doesn't exist yet — nothing to prune.
  }
  const cutoff = Date.now() - MAX_LOG_AGE_MS;
  for (const f of entries) {
    if (!SESSION_LOG_RE.test(f)) continue;
    const p = path.join(dir, f);
    try {
      if (fs.statSync(p).mtimeMs < cutoff) fs.unlinkSync(p);
    } catch {
      // Best-effort cleanup — a failure to stat/remove one file is harmless.
    }
  }
}
