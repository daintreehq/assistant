/**
 * fsTools — read-only project filesystem access.
 *
 * These tools NEVER write, edit, or delete. They list, read, and text-search
 * files under the project root only. Every path is resolved with
 * resolveInsideProject so traversal outside the project is impossible.
 */
import { z } from "zod";
import { ok, fail, type ToolDef } from "./types.js";
import { resolveInsideProject, isSensitivePath, isSensitiveSegment } from "../safety/policy.js";
import fs from "node:fs/promises";
import path from "node:path";

/** Directory names skipped by every recursive walk. */
const SKIP_DIRS = new Set([
  ".git",
  "node_modules",
  "dist",
  "build",
  "coverage",
  ".next",
  ".turbo",
  ".cache",
  "vendor",
]);

/** Default ceiling for a single file read / per-file search scan (bytes). */
const DEFAULT_MAX_BYTES = 200_000;
/** Files larger than this are skipped entirely by fs.search. */
const SEARCH_MAX_FILE_BYTES = 1_000_000;

/**
 * Heuristic binary sniff: a NUL byte in the first chunk, or a high ratio of
 * non-text control bytes, means we should not treat the buffer as UTF-8 text.
 */
function looksBinary(buf: Buffer): boolean {
  const n = Math.min(buf.length, 4096);
  let suspicious = 0;
  for (let i = 0; i < n; i++) {
    const b = buf[i];
    if (b === 0) return true;
    // Allow tab(9) newline(10) CR(13) and the printable range; count the rest.
    if (b < 9 || (b > 13 && b < 32)) suspicious++;
  }
  return n > 0 && suspicious / n > 0.3;
}

const ListArgs = z.object({
  path: z.string().optional().describe("Directory relative to project root."),
  depth: z
    .number()
    .int()
    .positive()
    .max(10)
    .optional()
    .describe("How many directory levels to descend (default 1)."),
});

const ReadArgs = z.object({
  path: z.string().describe("Path relative to the project root."),
  maxBytes: z.number().int().positive().max(200_000).optional(),
});

const SearchArgs = z.object({
  query: z.string().min(1).describe("Substring to search for in file contents."),
  glob: z
    .string()
    .optional()
    .describe("Optional filename suffix/extension filter, e.g. \".ts\"."),
  maxResults: z
    .number()
    .int()
    .positive()
    .max(500)
    .optional()
    .describe("Maximum number of matches to return (default 50)."),
});

interface WalkEntry {
  rel: string;
  abs: string;
}

/**
 * Pure-JS recursive walk of files under `root`, skipping SKIP_DIRS. Returns
 * relative + absolute paths of regular files. `maxDepth` undefined = unlimited.
 */
async function walkFiles(
  root: string,
  maxDepth?: number,
): Promise<WalkEntry[]> {
  const out: WalkEntry[] = [];
  async function recurse(dirAbs: string, dirRel: string, depth: number): Promise<void> {
    let dirents;
    try {
      dirents = await fs.readdir(dirAbs, { withFileTypes: true });
    } catch {
      return;
    }
    for (const dirent of dirents) {
      if (dirent.isDirectory() && SKIP_DIRS.has(dirent.name)) continue;
      // Never descend into credential stores (.ssh, .aws, .env dirs, …). Prune
      // them at walk time so their paths are never enumerated into memory — the
      // post-hoc isSensitivePath filter in fs.search is a second layer, not the
      // first. Guard isSymbolicLink too: on POSIX a symlink named .ssh reports
      // isDirectory() === false, so the name check alone would miss it.
      if (
        (dirent.isDirectory() || dirent.isSymbolicLink()) &&
        isSensitiveSegment(dirent.name.toLowerCase())
      )
        continue;
      const childAbs = path.join(dirAbs, dirent.name);
      const childRel = dirRel ? path.join(dirRel, dirent.name) : dirent.name;
      if (dirent.isDirectory()) {
        if (maxDepth === undefined || depth + 1 < maxDepth) {
          await recurse(childAbs, childRel, depth + 1);
        }
      } else if (dirent.isFile()) {
        out.push({ rel: childRel, abs: childAbs });
      }
    }
  }
  await recurse(root, "", 0);
  return out;
}

export const fsTools: ToolDef[] = [
  {
    name: "fs.list",
    description:
      "List directory entries under the project root (read-only). Skips .git, node_modules, and dist.",
    risk: "read",
    schema: ListArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        path: {
          type: "string",
          description: "Directory relative to project root (default root).",
        },
        depth: {
          type: "number",
          description: "How many directory levels to descend (default 1).",
        },
      },
      required: [],
    },
    async handler(args, ctx) {
      const rel = args.path ?? ".";
      const depth = args.depth ?? 1;
      try {
        // Refuse to list a credential store directly, mirroring fs.read. An
        // empty success could be misread as "directory is empty"; a refusal is
        // unambiguous.
        if (isSensitivePath(rel)) {
          return fail(
            "FS_SENSITIVE",
            `Refusing to list ${rel}: it looks like a sensitive credential directory. Ask the user for only the specific files you need.`,
            { recoverable: false },
          );
        }
        const abs = resolveInsideProject(ctx.projectPath, rel);
        // Re-check the symlink-resolved target: a project-local symlink with a
        // benign name (e.g. cloud -> .aws) would pass the lexical check above
        // but still expose a credential store on descent. resolveInsideProject
        // already guarantees containment; this guards content sensitivity.
        let realAbs = abs;
        try {
          realAbs = await fs.realpath(abs);
        } catch {
          /* path may not exist; the readdir below reports it */
        }
        if (isSensitivePath(realAbs)) {
          return fail(
            "FS_SENSITIVE",
            `Refusing to list ${rel}: it resolves to a sensitive credential directory. Ask the user for only the specific files you need.`,
            { recoverable: false },
          );
        }
        const entries: Array<{ name: string; type: "file" | "dir" }> = [];
        async function recurse(
          dirAbs: string,
          dirRel: string,
          level: number,
        ): Promise<void> {
          let dirents;
          try {
            dirents = await fs.readdir(dirAbs, { withFileTypes: true });
          } catch {
            return;
          }
          for (const dirent of dirents) {
            const isDir = dirent.isDirectory();
            if (isDir && SKIP_DIRS.has(dirent.name)) continue;
            // Omit credential dirs from the listing entirely (not just skip
            // descent): surfacing `.ssh` as a dir entry still leaks that the
            // store exists into the model's context.
            if (
              (isDir || dirent.isSymbolicLink()) &&
              isSensitiveSegment(dirent.name.toLowerCase())
            )
              continue;
            if (!isDir && !dirent.isFile()) continue;
            const childRel = dirRel
              ? `${dirRel}/${dirent.name}`
              : dirent.name;
            entries.push({ name: childRel, type: isDir ? "dir" : "file" });
            if (isDir && level + 1 < depth) {
              await recurse(path.join(dirAbs, dirent.name), childRel, level + 1);
            }
          }
        }
        await recurse(abs, "", 0);
        entries.sort((a, b) => a.name.localeCompare(b.name));
        return ok(`Listed ${entries.length} entr${entries.length === 1 ? "y" : "ies"} under ${rel}.`, {
          path: rel,
          depth,
          entries,
        });
      } catch (e) {
        return fail(
          "FS_LIST",
          `Could not list ${rel}: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "fs.read",
    description: "Read a UTF-8 text file from the project (read-only).",
    risk: "read",
    schema: ReadArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        path: { type: "string", description: "Path relative to project root." },
        maxBytes: { type: "number", description: "Max bytes to read." },
      },
      required: ["path"],
    },
    async handler(args, ctx) {
      try {
        if (isSensitivePath(args.path)) {
          return fail(
            "FS_SENSITIVE",
            `Refusing to read ${args.path}: it looks like a secrets file (env file, private key, or credential store). Reading it could persist secrets into the audit log and conversation history. Ask the user to share only the specific values you need.`,
            { recoverable: false },
          );
        }
        const abs = resolveInsideProject(ctx.projectPath, args.path);
        // Re-check the symlink-resolved target: a project-local symlink could
        // point at a secret (e.g. notes.txt -> .env) and slip past the lexical
        // check above. resolveInsideProject already guarantees containment.
        let real = abs;
        try {
          real = await fs.realpath(abs);
        } catch {
          /* file may not exist yet; the open below reports it */
        }
        if (isSensitivePath(real)) {
          return fail(
            "FS_SENSITIVE",
            `Refusing to read ${args.path}: it resolves to a secrets file. Ask the user to share only the specific values you need.`,
            { recoverable: false },
          );
        }
        const limit = Math.min(args.maxBytes ?? DEFAULT_MAX_BYTES, DEFAULT_MAX_BYTES);
        // Byte-aware read: open and read at most `limit` bytes so maxBytes is a
        // true byte cap and a huge file never loads fully into memory.
        const handle = await fs.open(abs, "r");
        try {
          const stat = await handle.stat();
          if (!stat.isFile()) {
            return fail("FS_READ", `Not a regular file: ${args.path}`, { recoverable: false });
          }
          const toRead = Math.min(stat.size, limit);
          const buf = Buffer.alloc(toRead);
          const { bytesRead } = await handle.read(buf, 0, toRead, 0);
          const slice = buf.subarray(0, bytesRead);
          if (looksBinary(slice)) {
            return fail(
              "FS_BINARY",
              `Refusing to read ${args.path}: it appears to be a binary file.`,
              { recoverable: false },
            );
          }
          const content = slice.toString("utf8");
          const truncated = stat.size > toRead;
          return ok(
            `Read ${args.path} (${bytesRead} bytes${truncated ? `, truncated from ${stat.size}` : ""}).`,
            { path: args.path, content, bytes: bytesRead, truncated },
          );
        } finally {
          await handle.close();
        }
      } catch (e) {
        return fail(
          "FS_READ",
          `Could not read ${args.path}: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "fs.search",
    description:
      "Text-search file contents across the project (read-only). Recursive pure-JS walk skipping .git, node_modules, and dist.",
    risk: "read",
    schema: SearchArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        query: {
          type: "string",
          description: "Substring to search for in file contents.",
        },
        glob: {
          type: "string",
          description: "Optional filename suffix/extension filter, e.g. \".ts\".",
        },
        maxResults: {
          type: "number",
          description: "Maximum number of matches to return (default 50).",
        },
      },
      required: ["query"],
    },
    async handler(args, ctx) {
      const max = args.maxResults ?? 50;
      try {
        const root = resolveInsideProject(ctx.projectPath, ".");
        const files = await walkFiles(root);
        const matches: Array<{ file: string; line: number; text: string }> = [];
        const needle = args.query;
        const suffix = args.glob;
        for (const file of files) {
          if (matches.length >= max) break;
          if (suffix && !file.rel.endsWith(suffix)) continue;
          // Never scan secrets, and never load very large or binary files.
          if (isSensitivePath(file.rel)) continue;
          let content: string;
          try {
            const stat = await fs.stat(file.abs);
            if (!stat.isFile() || stat.size > SEARCH_MAX_FILE_BYTES) continue;
            const buf = await fs.readFile(file.abs);
            if (looksBinary(buf)) continue;
            content = buf.toString("utf8");
          } catch {
            continue;
          }
          const lines = content.split("\n");
          for (let i = 0; i < lines.length; i++) {
            if (lines[i].includes(needle)) {
              matches.push({
                file: file.rel,
                line: i + 1,
                text: lines[i].trim().slice(0, 300),
              });
              if (matches.length >= max) break;
            }
          }
        }
        const capped = matches.length >= max;
        return ok(
          `Found ${matches.length}${capped ? "+" : ""} match${matches.length === 1 ? "" : "es"} for "${args.query}".`,
          { query: args.query, glob: suffix, capped, matches },
        );
      } catch (e) {
        return fail(
          "FS_SEARCH",
          `Search failed for "${args.query}": ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
];
