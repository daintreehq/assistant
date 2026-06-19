/**
 * Neutral text helpers shared by the agent loop, the legacy console renderer, and
 * the Ink UI. Kept dependency-free so non-UI modules never import terminal code.
 */

/** Truncate to at most `n` chars, appending an ellipsis when clipped. */
export function truncate(s: string, n: number): string {
  return s.length > n ? s.slice(0, n - 1) + "…" : s;
}

/**
 * Wrap `s` into visual lines no wider than `n` columns, preserving explicit
 * newlines as hard breaks. Words break on whitespace where possible; a single
 * word longer than `n` is hard-split across rows. Always returns at least one
 * line (possibly empty) so callers can render a 1:1 gutter alongside the result.
 */
export function wrapText(s: string, n: number): string[] {
  if (n <= 0) return [s];
  const out: string[] = [];
  for (const para of s.split("\n")) {
    if (para === "") {
      out.push("");
      continue;
    }
    let line = "";
    for (const word of para.split(" ")) {
      // A word that can't fit on a line of its own is hard-split into chunks.
      if (word.length > n) {
        if (line) {
          out.push(line);
          line = "";
        }
        let rest = word;
        while (rest.length > n) {
          out.push(rest.slice(0, n));
          rest = rest.slice(n);
        }
        line = rest;
        continue;
      }
      const candidate = line ? line + " " + word : word;
      if (candidate.length > n) {
        out.push(line);
        line = word;
      } else {
        line = candidate;
      }
    }
    out.push(line);
  }
  return out.length ? out : [""];
}

export type DisplayRow =
  | { kind: "text"; text: string }
  | { kind: "snip"; hidden: number };

/**
 * Collapse a long list of (already-wrapped) lines to a head/snip/tail view, the
 * way Claude Code shows a long block: the first `head` and last `tail` lines with
 * a single snip marker standing in for the `hidden` middle. Only collapses when it
 * actually saves rows — i.e. more than one line would be hidden — otherwise every
 * line is returned verbatim (collapsing to hide a single line buys nothing).
 */
export function collapseLines(
  lines: string[],
  head = 4,
  tail = 4,
): DisplayRow[] {
  if (lines.length <= head + tail + 1) {
    return lines.map((text): DisplayRow => ({ kind: "text", text }));
  }
  const hidden = lines.length - head - tail;
  return [
    ...lines.slice(0, head).map((text): DisplayRow => ({ kind: "text", text })),
    { kind: "snip", hidden },
    ...lines
      .slice(lines.length - tail)
      .map((text): DisplayRow => ({ kind: "text", text })),
  ];
}

/**
 * A centered `+N lines` rule for a collapsed block — the snip marker, doubling as
 * the horizontal divider. Padded out to `width` with `─`; if the label alone can't
 * fit, just the trimmed label is returned.
 */
export function snipRule(hidden: number, width: number): string {
  const label = ` +${hidden} lines `;
  if (label.length >= width) return label.trim();
  const dashes = width - label.length;
  const left = Math.floor(dashes / 2);
  return "─".repeat(left) + label + "─".repeat(dashes - left);
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
