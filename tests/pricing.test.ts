import { describe, it, expect } from "vitest";
import { estimateCostUsd } from "../src/models/pricing.js";

describe("estimateCostUsd", () => {
  it("prices the large model (minimax-m3) at its input/output rates", () => {
    // 1M input @ $0.30 + 1M output @ $1.20 = $1.50
    expect(estimateCostUsd("minimax-m3", 1_000_000, 1_000_000)).toBeCloseTo(1.5, 6);
  });

  it("matches versioned ids by prefix", () => {
    expect(estimateCostUsd("deepseek-v3-0324", 1_000_000, 0)).toBeCloseTo(0.56, 6);
  });

  it("strips a Fireworks account path before matching", () => {
    expect(
      estimateCostUsd("accounts/fireworks/models/minimax-m3", 1_000_000, 0),
    ).toBeCloseTo(0.3, 6);
  });

  it("discounts cached prompt tokens by half", () => {
    // 1M prompt tokens, all cached → 0.30 * 0.5 = $0.15
    expect(estimateCostUsd("minimax-m3", 1_000_000, 0, 1_000_000)).toBeCloseTo(
      0.15,
      6,
    );
  });

  it("clamps cached tokens to the prompt total", () => {
    // cachedTokens exceeding promptTokens must not produce a negative fresh cost.
    expect(estimateCostUsd("minimax-m3", 100, 0, 1_000)).toBeCloseTo(
      (100 * 0.3 * 0.5) / 1_000_000,
      9,
    );
  });

  it("returns undefined for an unknown model", () => {
    expect(estimateCostUsd("some-unknown-model", 1000, 1000)).toBeUndefined();
  });
});
