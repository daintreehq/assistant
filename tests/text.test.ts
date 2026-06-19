import { collapseLines, snipRule, wrapText } from "../src/utils/text.js";

describe("wrapText", () => {
  it("returns the line unchanged when it fits", () => {
    expect(wrapText("hello world", 80)).toEqual(["hello world"]);
  });

  it("breaks on whitespace at the width budget", () => {
    expect(wrapText("alpha beta gamma", 11)).toEqual(["alpha beta", "gamma"]);
  });

  it("preserves explicit newlines as hard breaks, including blank lines", () => {
    expect(wrapText("a\n\nb", 10)).toEqual(["a", "", "b"]);
  });

  it("hard-splits a single word longer than the width", () => {
    expect(wrapText("abcdefghij", 4)).toEqual(["abcd", "efgh", "ij"]);
  });

  it("always returns at least one (possibly empty) line", () => {
    expect(wrapText("", 10)).toEqual([""]);
  });

  it("preserves leading indentation (split would otherwise eat it)", () => {
    expect(wrapText("    code", 80)).toEqual(["    code"]);
  });

  it("keeps indentation on the first row only when the line wraps", () => {
    expect(wrapText("  alpha beta gamma", 11)).toEqual(["  alpha beta", "gamma"]);
  });

  it("preserves an all-whitespace line instead of flattening it", () => {
    expect(wrapText("a\n   \nb", 10)).toEqual(["a", "   ", "b"]);
  });
});

describe("collapseLines", () => {
  it("returns every line as text when short enough to not save rows", () => {
    const lines = ["1", "2", "3", "4", "5", "6", "7", "8", "9"];
    const rows = collapseLines(lines);
    expect(rows).toHaveLength(9);
    expect(rows.every((r) => r.kind === "text")).toBe(true);
  });

  it("collapses to head + snip + tail when more than one line would hide", () => {
    const lines = Array.from({ length: 20 }, (_, i) => String(i));
    const rows = collapseLines(lines);
    // 4 head + 1 snip + 4 tail
    expect(rows).toHaveLength(9);
    expect(rows.slice(0, 4).map((r) => (r.kind === "text" ? r.text : ""))).toEqual(
      ["0", "1", "2", "3"],
    );
    expect(rows[4]).toEqual({ kind: "snip", hidden: 12 });
    expect(rows.slice(5).map((r) => (r.kind === "text" ? r.text : ""))).toEqual(
      ["16", "17", "18", "19"],
    );
  });

  it("honors custom head/tail sizes", () => {
    const lines = Array.from({ length: 10 }, (_, i) => String(i));
    const rows = collapseLines(lines, 2, 2);
    expect(rows).toHaveLength(5);
    expect(rows[2]).toEqual({ kind: "snip", hidden: 6 });
  });
});

describe("snipRule", () => {
  it("centers the +N lines label padded with dashes to the width", () => {
    const rule = snipRule(12, 20);
    expect(rule).toHaveLength(20);
    expect(rule).toContain("+12 lines");
    expect(rule.startsWith("─")).toBe(true);
    expect(rule.endsWith("─")).toBe(true);
  });

  it("spans the full width when the label fits exactly (no trim)", () => {
    const label = " +12 lines ";
    const rule = snipRule(12, label.length);
    expect(rule).toBe(label);
    expect(rule).toHaveLength(label.length);
  });

  it("falls back to the trimmed label when it cannot fit", () => {
    expect(snipRule(3, 4)).toBe("+3 lines");
  });
});
