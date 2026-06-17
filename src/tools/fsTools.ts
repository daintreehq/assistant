/**
 * fsTools — read-only project filesystem access.
 *
 * These tools NEVER write, edit, or delete. They list, read, and text-search
 * files under the project root only. Every path is resolved with
 * resolveInsideProject so traversal outside the project is impossible.
 */
import { z } from "zod";
import { ok, fail, type ToolDef } from "./types.js";
import { resolveInsideProject } from "../safety/policy.js";
import fs from "node:fs/promises";
import path from "node:path";

/** Directory names skipped by every recursive walk. */
const SKIP_DIRS = new Set([".git", "node_modules", "dist"]);

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
    readOnly: true,
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
        const abs = resolveInsideProject(ctx.projectPath, rel);
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
    readOnly: true,
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
        const abs = resolveInsideProject(ctx.projectPath, args.path);
        const buf = await fs.readFile(abs, "utf8");
        const sliced = args.maxBytes ? buf.slice(0, args.maxBytes) : buf;
        return ok(`Read ${args.path} (${sliced.length} chars).`, {
          path: args.path,
          content: sliced,
        });
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
    readOnly: true,
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
          let content: string;
          try {
            content = await fs.readFile(file.abs, "utf8");
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
