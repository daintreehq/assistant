import os from "node:os";
import path from "node:path";
import fs from "node:fs";
import { describe, it, expect, beforeEach, afterEach } from "vitest";
import {
  loadProjectInstructions,
  PROJECT_INSTRUCTIONS_FILENAME,
  PROJECT_INSTRUCTIONS_MAX_BYTES,
} from "../src/projectInstructions.js";

let dir: string;
const file = () => path.join(dir, PROJECT_INSTRUCTIONS_FILENAME);

beforeEach(() => {
  dir = fs.mkdtempSync(path.join(os.tmpdir(), "daintree-instructions-"));
});

afterEach(() => {
  fs.rmSync(dir, { recursive: true, force: true });
});

describe("loadProjectInstructions", () => {
  it("returns nothing when the file is absent (the normal case)", async () => {
    const res = await loadProjectInstructions(dir);
    expect(res.content).toBeUndefined();
    expect(res.warning).toBeUndefined();
  });

  it("loads and trims the content when present", async () => {
    fs.writeFileSync(file(), "\n  # Norms\nUse make check.\n  \n");
    const res = await loadProjectInstructions(dir);
    expect(res.content).toBe("# Norms\nUse make check.");
    expect(res.warning).toBeUndefined();
  });

  it("treats an empty file as no instructions", async () => {
    fs.writeFileSync(file(), "");
    const res = await loadProjectInstructions(dir);
    expect(res.content).toBeUndefined();
    expect(res.warning).toBeUndefined();
  });

  it("treats a whitespace-only file as no instructions", async () => {
    fs.writeFileSync(file(), "   \n\t\n  ");
    const res = await loadProjectInstructions(dir);
    expect(res.content).toBeUndefined();
    expect(res.warning).toBeUndefined();
  });

  it("silently skips when the path is a directory, not a file", async () => {
    fs.mkdirSync(file());
    const res = await loadProjectInstructions(dir);
    expect(res.content).toBeUndefined();
    expect(res.warning).toBeUndefined();
  });

  it("loads a file exactly at the size cap", async () => {
    // A single line of 'a' characters totalling the cap; trim() leaves it intact.
    fs.writeFileSync(file(), "a".repeat(PROJECT_INSTRUCTIONS_MAX_BYTES));
    const res = await loadProjectInstructions(dir);
    expect(res.content).toHaveLength(PROJECT_INSTRUCTIONS_MAX_BYTES);
    expect(res.warning).toBeUndefined();
  });

  it("skips with a warning when the file exceeds the size cap", async () => {
    fs.writeFileSync(file(), "a".repeat(PROJECT_INSTRUCTIONS_MAX_BYTES + 1));
    const res = await loadProjectInstructions(dir);
    expect(res.content).toBeUndefined();
    expect(res.warning).toContain(PROJECT_INSTRUCTIONS_FILENAME);
    expect(res.warning).toContain("limit");
  });

  it("caps on UTF-8 byte length, not character count", async () => {
    // Each "é" is 2 UTF-8 bytes; half-the-cap-plus-one characters overruns the
    // byte cap even though the character count is well under it.
    fs.writeFileSync(file(), "é".repeat(PROJECT_INSTRUCTIONS_MAX_BYTES / 2 + 1));
    const res = await loadProjectInstructions(dir);
    expect(res.content).toBeUndefined();
    expect(res.warning).toContain("limit");
  });

  it("resolves the file against the given project path, not cwd", async () => {
    fs.writeFileSync(file(), "scoped to this dir");
    // A sibling dir with no instruction file must yield nothing.
    const other = fs.mkdtempSync(path.join(os.tmpdir(), "daintree-other-"));
    try {
      expect((await loadProjectInstructions(other)).content).toBeUndefined();
      expect((await loadProjectInstructions(dir)).content).toBe("scoped to this dir");
    } finally {
      fs.rmSync(other, { recursive: true, force: true });
    }
  });
});
