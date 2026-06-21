/**
 * Load the assistant recipe library from `recipes/*.md` content files.
 *
 * Each recipe is a single markdown file: a small YAML-ish frontmatter block of
 * metadata (id, title, version, the two headers, tags, risk, requiredTools)
 * followed by the procedural body. Authoring one is "drop a file in `recipes/`"
 * — see docs/RECIPES.md. Loading happens once, synchronously, at registry
 * construction (the registry stays sync), and every file is validated through the
 * `Recipe` Zod schema, so a malformed recipe fails loudly at boot with its
 * filename rather than silently shipping a broken runbook.
 *
 * This is the on-disk implementation of the swappable recipe seam (see source.ts):
 * a future hosted recipe service would replace this loader without touching any
 * caller. We deliberately parse a *small, fixed* subset of YAML by hand rather
 * than add a dependency — the frontmatter shape is ours to constrain.
 */
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { Recipe } from "./types.js";

/** Env override for the recipes directory (absolute path). Mostly for tests. */
const RECIPES_DIR_ENV = "DAINTREE_ASSISTANT_RECIPES_DIR";

/**
 * Find the project's `recipes/` directory by walking up from this module to the
 * NEAREST `package.json` (our package root) and resolving `recipes/` beside it.
 * This resolves the same way whether we run from source (`src/recipes/…` under
 * bun/tsx/vitest) or from the bundle (`dist/index.js` under node) — both live
 * inside the package root. We stop at the first package.json rather than climbing
 * further, so we can never accidentally adopt an ancestor/consumer's `recipes/`
 * dir: if our own package root has no `recipes/`, that's an error, not a reason to
 * keep walking. Returns undefined when no package root or no `recipes/` is found.
 */
function findRecipesDir(startDir: string): string | undefined {
  let dir = startDir;
  for (;;) {
    if (fs.existsSync(path.join(dir, "package.json"))) {
      const recipes = path.join(dir, "recipes");
      return fs.existsSync(recipes) && fs.statSync(recipes).isDirectory()
        ? recipes
        : undefined;
    }
    const parent = path.dirname(dir);
    if (parent === dir) return undefined; // reached filesystem root
    dir = parent;
  }
}

/** Resolve the directory recipe files live in (env override wins). Throws if none. */
export function resolveRecipesDir(): string {
  const override = process.env[RECIPES_DIR_ENV]?.trim();
  if (override) return override;
  const here = path.dirname(fileURLToPath(import.meta.url));
  const found = findRecipesDir(here);
  if (!found) {
    throw new Error(
      `Could not locate the recipes/ directory (searched up from ${here}). ` +
        `Set ${RECIPES_DIR_ENV} to override.`,
    );
  }
  return found;
}

/** Coerce a single scalar token from frontmatter into string | number | boolean. */
function coerceScalar(raw: string): string | number | boolean {
  const v = raw.trim();
  // Strip a single layer of matching surrounding quotes.
  if (
    (v.startsWith('"') && v.endsWith('"')) ||
    (v.startsWith("'") && v.endsWith("'"))
  ) {
    return v.slice(1, -1);
  }
  if (v === "true") return true;
  if (v === "false") return false;
  // Plain integers only — versions like "0.2.0" stay strings (they have dots).
  if (/^-?\d+$/.test(v)) return Number(v);
  return v;
}

/**
 * Parse the small YAML subset we allow in recipe frontmatter:
 *   - `key: scalar`            (string / int / bool; quotes optional)
 *   - `key: [a, b, c]`         inline array
 *   - `key:` then `  - item`   block-list array (preferred for long lists)
 * Anything else is rejected so a typo can't silently produce a half-parsed recipe.
 */
function parseFrontmatter(
  block: string,
  filename: string,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  const lines = block.split("\n");
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (line.trim() === "") continue;
    if (/^\s+/.test(line)) {
      // A stray indented line that wasn't consumed as a block-list item below.
      throw new Error(`${filename}: unexpected indented line: "${line}"`);
    }
    const m = /^([A-Za-z0-9_]+):\s?(.*)$/.exec(line);
    if (!m) throw new Error(`${filename}: malformed frontmatter line: "${line}"`);
    const key = m[1];
    const rest = m[2];
    // Reject duplicate keys: a second `requiredTools:` (etc.) would silently clobber
    // the first, which could under-declare tools with no error. Fail loudly instead.
    if (Object.prototype.hasOwnProperty.call(out, key)) {
      throw new Error(`${filename}: duplicate frontmatter key: "${key}"`);
    }
    if (rest === "") {
      // Block list: consume following `  - item` lines (if any).
      const items: Array<string | number | boolean> = [];
      while (i + 1 < lines.length && /^\s*-\s+/.test(lines[i + 1])) {
        items.push(coerceScalar(lines[i + 1].replace(/^\s*-\s+/, "")));
        i++;
      }
      out[key] = items; // empty array when no items followed
    } else if (rest.startsWith("[") && rest.endsWith("]")) {
      // Inline array; empty `[]` yields []. Empty items (a trailing comma) are
      // dropped so `[a, b,]` doesn't smuggle in a "" entry.
      const inner = rest.slice(1, -1).trim();
      out[key] =
        inner === ""
          ? []
          : inner
              .split(",")
              .map((s) => coerceScalar(s))
              .filter((s) => s !== "");
    } else {
      out[key] = coerceScalar(rest);
    }
  }
  return out;
}

/**
 * Parse one recipe markdown file into a validated Recipe. The frontmatter supplies
 * the metadata; the markdown beneath the closing `---` is the recipe body. Throws
 * (with the filename) on a missing/garbled frontmatter block or a schema violation.
 */
export function parseRecipeFile(text: string, filename: string): Recipe {
  // Tolerate a leading BOM and any blank lines before the opening fence.
  const normalized = text.replace(/^﻿/, "").replace(/^(?:[ \t]*\r?\n)+/, "");
  const fence = /^---[ \t]*\r?\n([\s\S]*?)\r?\n---[ \t]*\r?\n?([\s\S]*)$/.exec(
    normalized,
  );
  if (!fence) {
    throw new Error(
      `${filename}: missing YAML frontmatter (a recipe must open with a --- … --- block).`,
    );
  }
  const meta = parseFrontmatter(fence[1], filename);
  const body = fence[2].trim();
  try {
    return Recipe.parse({ ...meta, body });
  } catch (e) {
    throw new Error(
      `${filename}: invalid recipe — ${e instanceof Error ? e.message : String(e)}`,
    );
  }
}

/** True for files we treat as recipe content (skip READMEs and dot/underscore files). */
function isRecipeFile(name: string): boolean {
  if (!name.toLowerCase().endsWith(".md")) return false;
  if (name.startsWith(".") || name.startsWith("_")) return false;
  if (name.toLowerCase() === "readme.md") return false;
  return true;
}

/**
 * Load and validate every recipe file in `dir` (default: the resolved recipes
 * directory), sorted by filename for a deterministic order. Each file is parsed
 * and validated; one bad file aborts the whole load so the problem is impossible
 * to miss.
 */
export function loadRecipesFromDir(dir: string = resolveRecipesDir()): Recipe[] {
  const names = fs.readdirSync(dir).filter(isRecipeFile).sort();
  return names.map((name) => {
    const full = path.join(dir, name);
    return parseRecipeFile(fs.readFileSync(full, "utf8"), name);
  });
}
