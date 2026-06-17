import os from "node:os";
import path from "node:path";
import { describe, it, expect } from "vitest";
import { loadConfig, describeConfig, DEFAULTS } from "../src/config.js";

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
});
