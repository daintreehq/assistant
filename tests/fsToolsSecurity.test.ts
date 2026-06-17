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
