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

/**
 * Usable content width inside a single-column sidebar of `columns` cells, after
 * reserving `chrome` cells for the surrounding border + padding. Never returns
 * less than 20 so rows stay legible even in survival-mode widths.
 */
export function cellBudget(columns: number, chrome = 4): number {
  return Math.max(20, columns - chrome);
}

/**
 * Truncate `s` to fit a column-budgeted width, reserving `reserved` cells for a
 * symbol/badge/age that shares the row. Always leaves room for at least a few
 * characters of the value itself.
 */
export function fit(s: string, width: number, reserved = 0): string {
  return truncate(s, Math.max(4, width - reserved));
}
