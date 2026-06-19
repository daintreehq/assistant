/**
 * Project-level instruction loading.
 *
 * Teams encode repo-local norms (build commands, do-not-touch paths, preferred
 * workflows, conventions) in a `DAINTREE.md` at the project root. We read it once
 * at startup and fold the content into the DYNAMIC prompt layer (message[1] via
 * {@link MainPromptContext.projectInstructions}) — never the cached base prefix —
 * so the prompt cache survives unchanged.
 *
 * The read is best-effort, mirroring the debug-log init philosophy: a missing or
 * unreadable file must NEVER block startup. Absence is the normal case for most
 * repos; an oversized or unreadable file returns a non-fatal warning the caller
 * surfaces however it surfaces other startup notices (render.warn / stderr).
 */
import path from "node:path";
import { stat, readFile } from "node:fs/promises";

/** The per-repo instruction file, read from the project root. */
export const PROJECT_INSTRUCTIONS_FILENAME = "DAINTREE.md";

/**
 * Hard cap on the instruction file. Large enough for rich team norms, small
 * enough that it can't blow the model's context or bias the cache. Files over
 * this size are skipped with a warning rather than truncated — silent truncation
 * would drop the team's instructions mid-sentence.
 */
export const PROJECT_INSTRUCTIONS_MAX_BYTES = 16 * 1024;

export interface ProjectInstructionsResult {
  /** The loaded instruction text (trimmed of surrounding whitespace). Absent when
   *  there's nothing to inject (no file, empty/whitespace-only, or skipped). */
  content?: string;
  /** A non-fatal warning to surface (oversized file, unreadable file). */
  warning?: string;
}

/**
 * Read `DAINTREE.md` from `projectPath`, if present. Never throws.
 *
 * Resolution and guards:
 *   - resolved against `projectPath` (the authoritative project root), not cwd
 *   - missing file / not a regular file → silently skip (the normal state)
 *   - larger than {@link PROJECT_INSTRUCTIONS_MAX_BYTES} → warn + skip
 *   - empty or whitespace-only → no content (no section gets rendered)
 *   - any other I/O error (e.g. EACCES) → warn + skip
 */
export async function loadProjectInstructions(
  projectPath: string,
): Promise<ProjectInstructionsResult> {
  const file = path.resolve(projectPath, PROJECT_INSTRUCTIONS_FILENAME);
  try {
    const info = await stat(file);
    if (!info.isFile()) return {};
    if (info.size > PROJECT_INSTRUCTIONS_MAX_BYTES) {
      return {
        warning: `Skipping ${PROJECT_INSTRUCTIONS_FILENAME}: ${info.size} bytes exceeds the ${PROJECT_INSTRUCTIONS_MAX_BYTES}-byte limit.`,
      };
    }
    const raw = await readFile(file, "utf8");
    // Re-check the actual byte length after reading: stat() and readFile() are two
    // syscalls, so a file that grew between them (or whose multibyte content the
    // stat size under-counts) must not slip past the cap. Belt-and-suspenders with
    // the stat() guard above.
    if (Buffer.byteLength(raw, "utf8") > PROJECT_INSTRUCTIONS_MAX_BYTES) {
      return {
        warning: `Skipping ${PROJECT_INSTRUCTIONS_FILENAME}: exceeds the ${PROJECT_INSTRUCTIONS_MAX_BYTES}-byte limit.`,
      };
    }
    const trimmed = raw.trim();
    if (trimmed.length === 0) return {};
    return { content: trimmed };
  } catch (err) {
    // ENOENT is the normal "no instructions for this repo" case — silent.
    if ((err as NodeJS.ErrnoException)?.code === "ENOENT") return {};
    const message = err instanceof Error ? err.message : String(err);
    return {
      warning: `Could not read ${PROJECT_INSTRUCTIONS_FILENAME}: ${message}`,
    };
  }
}
