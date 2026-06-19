/**
 * Static, dependency-free pricing for the Fireworks models the router maps to,
 * used to turn per-turn token counts into a rough running session cost in the
 * cockpit. Rates are USD per million tokens, current for Fireworks serverless as
 * of 2026-06; they drift over time and are intentionally approximate — the
 * cockpit shows cost as a coarse signal, not an invoice. An unknown model returns
 * `undefined` so the UI can distinguish "no rate" from a genuine $0.000.
 */

interface ModelRate {
  /** USD per million input (prompt) tokens. */
  inputPerM: number;
  /** USD per million output (completion) tokens. */
  outputPerM: number;
}

/**
 * Keyed by model-id PREFIX so versioned ids (e.g. `deepseek-v3-0324`) match their
 * base family. Longest matching prefix wins, so a more specific id can override a
 * broader family entry if one is ever added.
 */
const RATES: Array<{ prefix: string; rate: ModelRate }> = [
  { prefix: "minimax-m3", rate: { inputPerM: 0.3, outputPerM: 1.2 } },
  { prefix: "deepseek-v4", rate: { inputPerM: 0.56, outputPerM: 1.68 } },
  { prefix: "deepseek-v3", rate: { inputPerM: 0.56, outputPerM: 1.68 } },
];

/** Cached prompt tokens bill at half rate (Fireworks prompt-cache discount). */
const CACHED_INPUT_DISCOUNT = 0.5;

/**
 * The bare model id: strip any `accounts/<x>/models/<id>` Fireworks path so both
 * pricing and the cockpit display work with the short, human-readable id (e.g.
 * `minimax-m3`) rather than the full 36-char account path.
 */
export function bareModelId(model: string): string {
  return model.includes("/") ? model.slice(model.lastIndexOf("/") + 1) : model;
}

function rateFor(model: string): ModelRate | undefined {
  const bare = bareModelId(model.toLowerCase());
  let best: { len: number; rate: ModelRate } | undefined;
  for (const { prefix, rate } of RATES) {
    if (bare.startsWith(prefix) && (!best || prefix.length > best.len)) {
      best = { len: prefix.length, rate };
    }
  }
  return best?.rate;
}

/**
 * Estimate the USD cost of one model call. `cachedTokens` (a subset of
 * `promptTokens`) is billed at the cache discount. Returns `undefined` for a model
 * with no known rate so callers can show "$?" rather than a misleading $0.000.
 */
export function estimateCostUsd(
  model: string,
  promptTokens: number,
  completionTokens: number,
  cachedTokens = 0,
): number | undefined {
  const rate = rateFor(model);
  if (!rate) return undefined;
  const cached = Math.max(0, Math.min(cachedTokens, promptTokens));
  const freshInput = promptTokens - cached;
  const inputCost =
    (freshInput * rate.inputPerM +
      cached * rate.inputPerM * CACHED_INPUT_DISCOUNT) /
    1_000_000;
  const outputCost = (completionTokens * rate.outputPerM) / 1_000_000;
  return inputCost + outputCost;
}
