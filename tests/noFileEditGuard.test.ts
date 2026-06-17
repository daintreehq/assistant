import fs from "node:fs";
import os from "node:os";
import path from "node:path";
import {
  assertNoFileEditTools,
  isForbiddenToolName,
  resolveInsideProject,
  FileEditAttemptError,
} from "../src/safety/policy.js";
import { buildAllTools } from "../src/tools/index.js";

describe("no-file-edit guard", () => {
  describe("assertNoFileEditTools", () => {
    it("throws on fs.write", () => {
      expect(() => assertNoFileEditTools(["fs.write"])).toThrow(
        FileEditAttemptError,
      );
    });

    it("throws on apply_patch", () => {
      expect(() => assertNoFileEditTools(["apply_patch"])).toThrow(
        FileEditAttemptError,
      );
    });

    it("names the offenders in the error", () => {
      expect(() =>
        assertNoFileEditTools(["fs.read", "apply_patch"]),
      ).toThrow(/apply_patch/);
    });

    it("does not throw for the real tool registry", () => {
      const names = buildAllTools().map((t) => t.name);
      expect(names.length).toBeGreaterThan(0);
      expect(() => assertNoFileEditTools(names)).not.toThrow();
    });
  });

  describe("isForbiddenToolName", () => {
    it("is true for mutating fragments", () => {
      expect(isForbiddenToolName("fs.write")).toBe(true);
      expect(isForbiddenToolName("apply_patch")).toBe(true);
      expect(isForbiddenToolName("edit_file")).toBe(true);
      expect(isForbiddenToolName("FS.WRITE")).toBe(true);
    });

    it("is false for read-only / unrelated names", () => {
      expect(isForbiddenToolName("fs.read")).toBe(false);
      expect(isForbiddenToolName("fs.list")).toBe(false);
      expect(isForbiddenToolName("queue.publish")).toBe(false);
      expect(isForbiddenToolName("")).toBe(false);
    });
  });

  describe("resolveInsideProject", () => {
    const root = "/tmp/project";

    it("blocks traversal with ../escape", () => {
      expect(() => resolveInsideProject(root, "../escape")).toThrow(
        FileEditAttemptError,
      );
    });

    it("allows a path inside the project", () => {
      expect(resolveInsideProject(root, "src/index.ts")).toBe(
        "/tmp/project/src/index.ts",
      );
    });

    it("blocks reads through a repo-local symlink that escapes the project", () => {
      const proj = fs.mkdtempSync(path.join(os.tmpdir(), "dt-proj-"));
      const outside = fs.mkdtempSync(path.join(os.tmpdir(), "dt-out-"));
      fs.writeFileSync(path.join(outside, "secret.txt"), "top secret");
      const link = path.join(proj, "escape");
      fs.symlinkSync(outside, link);
      try {
        // Lexically inside the project, but the symlink resolves outside it.
        expect(() => resolveInsideProject(proj, "escape/secret.txt")).toThrow(
          FileEditAttemptError,
        );
        // A real file genuinely inside the project still resolves fine.
        fs.writeFileSync(path.join(proj, "ok.txt"), "fine");
        expect(resolveInsideProject(proj, "ok.txt")).toBe(path.join(proj, "ok.txt"));
      } finally {
        fs.rmSync(proj, { recursive: true, force: true });
        fs.rmSync(outside, { recursive: true, force: true });
      }
    });
  });
});
