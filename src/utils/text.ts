/**
 * Neutral text helpers shared by the agent loop, the legacy console renderer, and
 * the Ink UI. Kept dependency-free so non-UI modules never import terminal code.
 */

/** Truncate to at most `n` chars, appending an ellipsis when clipped. */
export function truncate(s: string, n: number): string {
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}

/** Compact a tool's args to a single short JSON-ish line for display. */
export function compactArgs(args: unknown, max = 120): string {
  if (args === undefined || args === null) return "";
  try {
    const s = JSON.stringify(args);
    return truncate(s ?? "", max);
  } catch {
    return "…";
  }
}
