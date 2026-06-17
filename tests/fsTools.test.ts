import { describe, it, expect, beforeAll, afterAll } from "vitest";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fsTools } from "../src/tools/fsTools.js";
import type { ToolContext } from "../src/tools/types.js";

const FILE_TEXT = "hello daintree\nfind-me-needle here\nthird line\n";

let tempDir: string;
let ctx: ToolContext;

function tool(name: string) {
  const def = fsTools.find((t) => t.name === name);
  if (!def) throw new Error(`tool ${name} not found`);
  return def;
}

beforeAll(async () => {
  tempDir = await fs.mkdtemp(path.join(os.tmpdir(), "fsTools-test-"));
  await fs.writeFile(path.join(tempDir, "readme.txt"), FILE_TEXT, "utf8");
  await fs.mkdir(path.join(tempDir, "sub"), { recursive: true });
  await fs.writeFile(path.join(tempDir, "sub", "nested.txt"), "nested body\n", "utf8");
  ctx = { projectPath: tempDir } as unknown as ToolContext;
});

afterAll(async () => {
  await fs.rm(tempDir, { recursive: true, force: true });
});

describe("fsTools (#10)", () => {
  it("fs.read returns the file content", async () => {
    const res = await tool("fs.read").handler({ path: "readme.txt" }, ctx);
    expect(res.ok).toBe(true);
    expect((res.result as { content: string }).content).toBe(FILE_TEXT);
  });

  it("fs.read honours maxBytes by slicing the content", async () => {
    const res = await tool("fs.read").handler({ path: "readme.txt", maxBytes: 5 }, ctx);
    expect(res.ok).toBe(true);
    expect((res.result as { content: string }).content).toBe("hello");
  });

  it("fs.list lists entries under the project root", async () => {
    const res = await tool("fs.list").handler({}, ctx);
    expect(res.ok).toBe(true);
    const names = (res.result as { entries: Array<{ name: string; type: string }> }).entries;
    expect(names.find((e) => e.name === "readme.txt")?.type).toBe("file");
    expect(names.find((e) => e.name === "sub")?.type).toBe("dir");
  });

  it("fs.list descends with depth", async () => {
    const res = await tool("fs.list").handler({ depth: 2 }, ctx);
    expect(res.ok).toBe(true);
    const entries = (res.result as { entries: Array<{ name: string }> }).entries;
    expect(entries.some((e) => e.name === "sub/nested.txt")).toBe(true);
  });

  it("fs.search finds the text in file contents", async () => {
    const res = await tool("fs.search").handler({ query: "find-me-needle" }, ctx);
    expect(res.ok).toBe(true);
    const matches = (res.result as { matches: Array<{ file: string; line: number; text: string }> }).matches;
    expect(matches.length).toBeGreaterThanOrEqual(1);
    const hit = matches.find((m) => m.file === "readme.txt");
    expect(hit).toBeDefined();
    expect(hit!.line).toBe(2);
    expect(hit!.text).toContain("find-me-needle");
  });

  it("fs.search respects the glob suffix filter", async () => {
    const res = await tool("fs.search").handler({ query: "body", glob: ".txt" }, ctx);
    expect(res.ok).toBe(true);
    const matches = (res.result as { matches: Array<{ file: string }> }).matches;
    expect(matches.some((m) => m.file === "sub/nested.txt")).toBe(true);
  });

  it("fs.read blocks path traversal outside the project (ok:false)", async () => {
    const res = await tool("fs.read").handler({ path: "../outside" }, ctx);
    expect(res.ok).toBe(false);
    if (!res.ok) {
      expect(res.error.code).toBe("FS_READ");
    }
  });
});
