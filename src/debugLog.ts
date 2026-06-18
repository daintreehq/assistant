/**
 * Full-fidelity debug trace — a pre-release observability aid.
 *
 * When `config.debugLog` is on (env `DAINTREE_ASSISTANT_DEBUG_LOG=1`), EVERYTHING
 * the assistant does is appended to a single human-readable file:
 *
 *     <projectPath>/logs/debug.log
 *
 * That means every model request and response (full message arrays included),
 * every tool/function call with its arguments and result, and the whole watcher
 * lifecycle. These logs are intentionally large and complete — values are NOT
 * truncated — so you can reconstruct a session end to end.
 *
 * The `logs/` folder is git-ignored. The intent is interaction-by-interaction
 * debugging: each assistant start rotates the current `debug.log` out to a
 * timestamped archive (see {@link rotateDebugLog}) so a fresh run begins with a
 * clean file, while the previous {@link MAX_ARCHIVES} runs stay around.
 *
 * Logging is a no-op when disabled, and it NEVER throws into the caller: a write
 * failure warns once on stderr and is otherwise swallowed, because losing a debug
 * line must never take down a model call, a tool dispatch, or a watcher check.
 */
import fs from "node:fs";
import path from "node:path";
import type { AppConfig } from "./config.js";

/** Folder (under the project root) that holds the debug log + its archives. */
export const DEBUG_LOG_DIRNAME = "logs";
/** The single live human-readable log file. */
export const DEBUG_LOG_FILE = "debug.log";
/** How many rotated archives to retain (the "previous N" runs). */
export const MAX_ARCHIVES = 20;

/** A field rendered inline (on the header line) must be a scalar this short. */
const INLINE_MAX = 120;

let warnedOnce = false;

/** Just the config fields the logger needs — keeps call sites and tests light. */
export type DebugLogConfig = Pick<AppConfig, "debugLog" | "projectPath">;

/** Absolute path to the project-local `logs/` directory. */
function logDir(projectPath: string): string {
  return path.join(projectPath, DEBUG_LOG_DIRNAME);
}

function isoNow(): string {
  try {
    return new Date().toISOString();
  } catch {
    return String(Date.now());
  }
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
 * Append one event to `<projectPath>/logs/debug.log`. No-op unless `cfg.debugLog`
 * is on. `event` is a short dotted name (e.g. "model.request", "tool.call",
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
  if (!cfg?.debugLog || !cfg.projectPath) return;
  const ts = isoNow();
  try {
    const dir = logDir(cfg.projectPath);
    fs.mkdirSync(dir, { recursive: true });

    const inline: string[] = [];
    const blocks: string[] = [];
    for (const [k, v] of Object.entries(fields)) {
      if (v === undefined) continue; // omit absent fields — they're just noise
      if (isInlineScalar(v)) inline.push(`${k}=${v === null ? "null" : String(v)}`);
      else blocks.push(`  ${k}:\n${blockValue(v)}`);
    }

    let out = `${ts}  ${event}${inline.length ? `  ${inline.join(" ")}` : ""}\n`;
    if (blocks.length) out += `${blocks.join("\n")}\n`;
    fs.appendFileSync(path.join(dir, DEBUG_LOG_FILE), out);
  } catch (err) {
    warnOnce(err);
  }
}

/**
 * Rotate the debug log at assistant startup: rename the current `debug.log` to a
 * timestamped archive (`debug-<iso>.log`) so the new run starts clean, then prune
 * archives down to the most recent {@link MAX_ARCHIVES}.
 *
 * No-op when logging is disabled or there is no existing `debug.log` (it never
 * creates the directory itself — that is the logger's job on first write), so a
 * fresh checkout and the test suite never touch the project tree. Never throws.
 */
export function rotateDebugLog(cfg: DebugLogConfig | undefined): void {
  if (!cfg?.debugLog || !cfg.projectPath) return;
  try {
    const dir = logDir(cfg.projectPath);
    const live = path.join(dir, DEBUG_LOG_FILE);
    if (fs.existsSync(live)) {
      const stamp = isoNow().replace(/[:.]/g, "-");
      fs.renameSync(live, path.join(dir, `debug-${stamp}.log`));
    }
    pruneArchives(dir);
  } catch (err) {
    warnOnce(err);
  }
}

/** Keep only the newest {@link MAX_ARCHIVES} `debug-*.log` files. */
function pruneArchives(dir: string): void {
  let entries: string[];
  try {
    entries = fs.readdirSync(dir);
  } catch {
    return; // directory doesn't exist yet — nothing to prune.
  }
  // Names embed an ISO timestamp, so a descending lexicographic sort is newest-first.
  const archives = entries
    .filter((f) => /^debug-.*\.log$/.test(f))
    .sort()
    .reverse();
  for (const stale of archives.slice(MAX_ARCHIVES)) {
    try {
      fs.unlinkSync(path.join(dir, stale));
    } catch {
      // Best-effort cleanup — a failure to remove one stale archive is harmless.
    }
  }
}
