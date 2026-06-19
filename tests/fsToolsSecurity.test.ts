import { describe, it, expect, beforeAll, afterAll } from "vitest";
import fs from "node:fs/promises";
import os from "node:os";
import path from "node:path";
import { fsTools } from "../src/tools/fsTools.js";
import { isSensitivePath } from "../src/safety/policy.js";
import type { ToolContext } from "../src/tools/types.js";

let tempDir: string;
let ctx: ToolContext;

function tool(name: string) {
  const def = fsTools.find((t) => t.name === name);
  if (!def) throw new Error(`tool ${name} not found`);
  return def;
}

beforeAll(async () => {
  tempDir = await fs.mkdtemp(path.join(os.tmpdir(), "fsSec-test-"));
  await fs.writeFile(path.join(tempDir, ".env"), "FIREWORKS_API_KEY=fw_secret_value\n", "utf8");
  await fs.writeFile(path.join(tempDir, "server.key"), "-----BEGIN PRIVATE KEY-----\nsecret\n", "utf8");
  await fs.writeFile(path.join(tempDir, "app.ts"), "const apiKey = readEnv();\n", "utf8");
  // A "binary" file with NUL bytes.
  await fs.writeFile(path.join(tempDir, "blob.bin"), Buffer.from([0x00, 0x01, 0x02, 0x00, 0x42]));
  // Credential stores nested under the project root. Each holds a file with a
  // unique marker so a search/list that descended into them would surface it.
  await fs.mkdir(path.join(tempDir, ".ssh"), { recursive: true });
  await fs.writeFile(path.join(tempDir, ".ssh", "id_ed25519"), "SSH_MARKER_aaa\n", "utf8");
  await fs.mkdir(path.join(tempDir, "nested", ".aws"), { recursive: true });
  await fs.writeFile(
    path.join(tempDir, "nested", ".aws", "credentials"),
    "AWS_MARKER_bbb\n",
    "utf8",
  );
  await fs.mkdir(path.join(tempDir, ".env.local"), { recursive: true });
  await fs.writeFile(path.join(tempDir, ".env.local", "secret.txt"), "ENV_MARKER_ccc\n", "utf8");
  ctx = { projectPath: tempDir } as unknown as ToolContext;
});

afterAll(async () => {
  await fs.rm(tempDir, { recursive: true, force: true });
});

describe("isSensitivePath (#1)", () => {
  it("flags env files, keys, and ssh/aws dirs", () => {
    expect(isSensitivePath(".env")).toBe(true);
    expect(isSensitivePath("config/.env.production")).toBe(true);
    expect(isSensitivePath("server.key")).toBe(true);
    expect(isSensitivePath("certs/cert.pem")).toBe(true);
    expect(isSensitivePath(".ssh/id_ed25519")).toBe(true);
    expect(isSensitivePath("home/.aws/credentials")).toBe(true);
  });
  it("catches segment/suffix/case variants the basename-only check missed", () => {
    expect(isSensitivePath("config/prod.env")).toBe(true);
    expect(isSensitivePath("nested/.env/x")).toBe(true);
    expect(isSensitivePath("FOO.ENV")).toBe(true);
    expect(isSensitivePath("Server.KEY")).toBe(true);
  });
  it("does not flag ordinary source files", () => {
    expect(isSensitivePath("src/app.ts")).toBe(false);
    expect(isSensitivePath("README.md")).toBe(false);
    expect(isSensitivePath("environment.ts")).toBe(false);
  });
});

describe("fs.read secret + binary guards (#1, #13)", () => {
  it("refuses to read a .env file so secrets never reach the audit log", async () => {
    const res = await tool("fs.read").handler({ path: ".env" }, ctx);
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("FS_SENSITIVE");
  });

  it("refuses to read a private key", async () => {
    const res = await tool("fs.read").handler({ path: "server.key" }, ctx);
    expect(res.ok).toBe(false);
  });

  it("refuses to read a binary file", async () => {
    const res = await tool("fs.read").handler({ path: "blob.bin" }, ctx);
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("FS_BINARY");
  });

  it("still reads normal source files", async () => {
    const res = await tool("fs.read").handler({ path: "app.ts" }, ctx);
    expect(res.ok).toBe(true);
    expect((res.result as { content: string }).content).toContain("readEnv");
  });
});

describe("fs.search skips secrets and binaries (#1, #13)", () => {
  it("never returns matches from a .env even when the query would match", async () => {
    const res = await tool("fs.search").handler({ query: "FIREWORKS_API_KEY" }, ctx);
    expect(res.ok).toBe(true);
    const matches = (res.result as { matches: Array<{ file: string }> }).matches;
    expect(matches.some((m) => m.file === ".env")).toBe(false);
  });
});

describe("fs.search does not descend into credential dirs (#122)", () => {
  it.each([
    ["SSH_MARKER_aaa", ".ssh/id_ed25519"],
    ["AWS_MARKER_bbb", "nested/.aws/credentials"],
    ["ENV_MARKER_ccc", ".env.local/secret.txt"],
  ])("finds no match for %s inside %s", async (marker) => {
    const res = await tool("fs.search").handler({ query: marker }, ctx);
    expect(res.ok).toBe(true);
    const matches = (res.result as { matches: Array<{ file: string }> }).matches;
    expect(matches).toHaveLength(0);
  });
});

describe("fs.list omits credential dirs (#122)", () => {
  it("does not surface .ssh, .aws, or .env.local at any depth", async () => {
    const res = await tool("fs.list").handler({ depth: 10 }, ctx);
    expect(res.ok).toBe(true);
    const entries = (res.result as { entries: Array<{ name: string }> }).entries;
    for (const e of entries) {
      const segs = e.name.split("/");
      expect(segs).not.toContain(".ssh");
      expect(segs).not.toContain(".aws");
      expect(segs).not.toContain(".env.local");
    }
  });

  it("refuses to list a credential dir directly with FS_SENSITIVE", async () => {
    const res = await tool("fs.list").handler({ path: ".ssh" }, ctx);
    expect(res.ok).toBe(false);
    if (!res.ok) expect(res.error.code).toBe("FS_SENSITIVE");
  });

  it("still lists ordinary nested directories", async () => {
    const res = await tool("fs.list").handler({ path: "nested", depth: 1 }, ctx);
    // `nested` itself is fine; only its .aws child is pruned.
    expect(res.ok).toBe(true);
    const entries = (res.result as { entries: Array<{ name: string }> }).entries;
    expect(entries.some((e) => e.name === ".aws")).toBe(false);
  });
});
