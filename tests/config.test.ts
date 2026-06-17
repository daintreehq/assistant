import os from "node:os";
import path from "node:path";
import fs from "node:fs";
import { describe, it, expect, vi, afterEach } from "vitest";
import {
  loadConfig,
  describeConfig,
  projectIdToDir,
  DEFAULTS,
} from "../src/config.js";

const stateDir = path.join(os.tmpdir(), "daintree-config-test");

describe("loadConfig", () => {
  it("applies overrides (mcpUrl, tier, smallModel)", () => {
    const cfg = loadConfig({
      stateDir,
      mcpUrl: "http://example.test/mcp",
      tier: "supervisor",
      smallModel: "accounts/custom/models/tiny",
    });

    expect(cfg.mcpUrl).toBe("http://example.test/mcp");
    expect(cfg.tier).toBe("supervisor");
    expect(cfg.smallModel).toBe("accounts/custom/models/tiny");
    expect(cfg.stateDir).toBe(stateDir);
  });

  it("falls back to defaults for largeModel", () => {
    const cfg = loadConfig({ stateDir });

    expect(cfg.largeModel).toBe("accounts/fireworks/models/minimax-m3");
    expect(cfg.largeModel).toBe(DEFAULTS.largeModel);
  });

  describe("DAINTREE_WINDOW_ID", () => {
    const prev = process.env.DAINTREE_WINDOW_ID;
    afterEach(() => {
      if (prev === undefined) delete process.env.DAINTREE_WINDOW_ID;
      else process.env.DAINTREE_WINDOW_ID = prev;
    });

    it("reads DAINTREE_WINDOW_ID from the environment", () => {
      process.env.DAINTREE_WINDOW_ID = "win-42";
      expect(loadConfig({ stateDir }).windowId).toBe("win-42");
    });

    it("leaves windowId unset when the env var is absent", () => {
      delete process.env.DAINTREE_WINDOW_ID;
      expect(loadConfig({ stateDir }).windowId).toBeUndefined();
    });
  });
});

describe("describeConfig", () => {
  it("redacts fireworksApiKey", () => {
    const rawKey = "fw-secret-1234567890";
    const cfg = loadConfig({ stateDir, fireworksApiKey: rawKey });
    const described = describeConfig(cfg);

    expect(cfg.fireworksApiKey).toBe(rawKey);
    expect(described.fireworksApiKey).not.toBe(rawKey);
    expect(described.fireworksApiKey).not.toContain(rawKey);
  });

  it("surfaces windowId (Daintree-injected, non-secret)", () => {
    const described = describeConfig(loadConfig({ stateDir }));
    expect(described).toHaveProperty("windowId");
    expect(described).toHaveProperty("projectId");
  });
});

describe("projectIdToDir", () => {
  it("produces a slug plus an 8-char hash suffix", () => {
    const dir = projectIdToDir("My Project 42");
    expect(dir).toMatch(/^my-project-42-[0-9a-f]{8}$/);
  });

  it("returns the bare hash when the slug is empty", () => {
    const dir = projectIdToDir("!!!");
    expect(dir).toMatch(/^[0-9a-f]{8}$/);
  });

  it("collapses path-traversal input to a single safe segment", () => {
    const dir = projectIdToDir("../../etc/passwd");
    expect(dir).not.toContain("/");
    expect(dir).not.toContain("..");
    expect(dir).toMatch(/^[a-z0-9_-]+$/);
  });

  it("yields distinct dirs for inputs that slug identically", () => {
    // Both slug to the same prefix; the hash suffix must keep them apart.
    const a = projectIdToDir("Project A!!!");
    const b = projectIdToDir("Project A???");
    expect(a).not.toBe(b);
  });

  it("is deterministic for the same input", () => {
    expect(projectIdToDir("acme-web")).toBe(projectIdToDir("acme-web"));
  });

  it("bounds the directory name length", () => {
    const dir = projectIdToDir("x".repeat(200));
    // slug capped at 40 + "-" + 8 hex chars
    expect(dir.length).toBeLessThanOrEqual(49);
  });

  it("never leaves a trailing dash before the hash", () => {
    // A truncation boundary that would otherwise land on a dash.
    const dir = projectIdToDir(`${"a".repeat(40)}-tail`);
    expect(dir).not.toContain("--");
    expect(dir).toMatch(/^[a-z0-9_-]+-[0-9a-f]{8}$/);
  });
});

describe("per-project state isolation (issue #4)", () => {
  const createdRoots: string[] = [];

  function withHome(): string {
    const home = fs.mkdtempSync(path.join(os.tmpdir(), "daintree-home-"));
    createdRoots.push(home);
    vi.stubEnv("HOME", home);
    vi.stubEnv("USERPROFILE", home);
    return home;
  }

  afterEach(() => {
    vi.unstubAllEnvs();
    for (const root of createdRoots.splice(0)) {
      fs.rmSync(root, { recursive: true, force: true });
    }
  });

  it("uses the flat legacy path when no project id is set", () => {
    const home = withHome();
    vi.stubEnv("DAINTREE_PROJECT_ID", "");

    const cfg = loadConfig();

    const flat = path.join(home, ".daintree", "assistant-cli");
    expect(cfg.stateDir).toBe(flat);
    expect(cfg.dbPath).toBe(path.join(flat, "state.db"));
  });

  it("derives a per-project subdirectory from DAINTREE_PROJECT_ID", () => {
    const home = withHome();
    vi.stubEnv("DAINTREE_PROJECT_ID", "alpha");

    const cfg = loadConfig();

    const flat = path.join(home, ".daintree", "assistant-cli");
    expect(cfg.stateDir).not.toBe(flat);
    expect(cfg.stateDir).toBe(path.join(flat, projectIdToDir("alpha")));
    expect(cfg.projectId).toBe("alpha");
  });

  it("isolates two different projects into different db files", () => {
    const home = withHome();

    vi.stubEnv("DAINTREE_PROJECT_ID", "alpha");
    const a = loadConfig();

    vi.stubEnv("DAINTREE_PROJECT_ID", "beta");
    const b = loadConfig();

    const flatDb = path.join(home, ".daintree", "assistant-cli", "state.db");
    expect(a.dbPath).not.toBe(b.dbPath);
    expect(a.dbPath).not.toBe(flatDb);
    expect(b.dbPath).not.toBe(flatDb);
  });

  it("maps the same project id to the same db file", () => {
    withHome();

    vi.stubEnv("DAINTREE_PROJECT_ID", "alpha");
    const a = loadConfig();
    const b = loadConfig();

    expect(a.dbPath).toBe(b.dbPath);
  });

  it("lets overrides.stateDir win over the project id", () => {
    withHome();
    vi.stubEnv("DAINTREE_PROJECT_ID", "alpha");

    const cfg = loadConfig({ stateDir });

    expect(cfg.stateDir).toBe(stateDir);
  });

  it("lets DAINTREE_ASSISTANT_STATE_DIR win over the project id", () => {
    withHome();
    vi.stubEnv("DAINTREE_PROJECT_ID", "alpha");
    vi.stubEnv("DAINTREE_ASSISTANT_STATE_DIR", stateDir);

    const cfg = loadConfig();

    expect(cfg.stateDir).toBe(stateDir);
  });

  it("supports project id injected via overrides", () => {
    const home = withHome();

    const cfg = loadConfig({ projectId: "gamma" });

    const flat = path.join(home, ".daintree", "assistant-cli");
    expect(cfg.stateDir).toBe(path.join(flat, projectIdToDir("gamma")));
    expect(cfg.projectId).toBe("gamma");
  });

  it("reads windowId but does not yet branch the path on it", () => {
    const home = withHome();
    vi.stubEnv("DAINTREE_PROJECT_ID", "alpha");
    vi.stubEnv("DAINTREE_WINDOW_ID", "win-1");

    const cfg = loadConfig();

    const flat = path.join(home, ".daintree", "assistant-cli");
    expect(cfg.windowId).toBe("win-1");
    // Window isolation is deferred (issue #5): path stays project-scoped.
    expect(cfg.stateDir).toBe(path.join(flat, projectIdToDir("alpha")));
  });

  it("falls through to the per-project path when state-dir env is blank", () => {
    const home = withHome();
    vi.stubEnv("DAINTREE_PROJECT_ID", "alpha");
    vi.stubEnv("DAINTREE_ASSISTANT_STATE_DIR", "   ");

    const cfg = loadConfig();

    const flat = path.join(home, ".daintree", "assistant-cli");
    expect(cfg.stateDir).toBe(path.join(flat, projectIdToDir("alpha")));
  });

  it("falls through to the per-project path when overrides.stateDir is blank", () => {
    const home = withHome();
    vi.stubEnv("DAINTREE_PROJECT_ID", "alpha");

    const cfg = loadConfig({ stateDir: "" });

    const flat = path.join(home, ".daintree", "assistant-cli");
    expect(cfg.stateDir).toBe(path.join(flat, projectIdToDir("alpha")));
  });

  it("keeps a traversal-style project id inside the state root", () => {
    const home = withHome();
    vi.stubEnv("DAINTREE_PROJECT_ID", "../../escape");

    const cfg = loadConfig();

    const flat = path.join(home, ".daintree", "assistant-cli");
    expect(cfg.stateDir.startsWith(flat + path.sep)).toBe(true);
    expect(cfg.stateDir).not.toContain("..");
  });
});
