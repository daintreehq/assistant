import type { ReactElement } from "react";
import chalk from "chalk";
import { render } from "ink-testing-library";
import { Header } from "../../src/ui/components/Header.js";

describe("Header", () => {
  // Styling splits the wordmark and version into separate <Text> spans, so the
  // rendered frame carries ANSI escapes between them; strip those before asserting
  // they sit on the same row.
  const stripAnsi = (s: string) => s.replace(/\[[0-9;]*m/g, "");

  it("renders the product wordmark and version on one line", () => {
    const { lastFrame } = render(<Header columns={60} version="0.1.0" />);
    const frame = stripAnsi(lastFrame() ?? "");
    expect(frame).toContain("Daintree Assistant"); // brand wordmark, capital A
    expect(frame).toContain("v0.1.0"); // version beside the name
    // Wordmark and version share a row (Claude-Code style).
    expect(frame).toMatch(/Daintree Assistant\s+v0\.1\.0/);
  });

  it("shows the project name beneath the wordmark", () => {
    const frame =
      render(
        <Header columns={60} version="0.1.0" project="assistant" />,
      ).lastFrame() ?? "";
    expect(frame).toContain("assistant");
  });

  it("drops the MCP connection badge from the masthead", () => {
    const frame = render(<Header columns={60} version="0.1.0" />).lastFrame() ?? "";
    // The live MCP link is by-exception status that stays in the StatusLine.
    expect(frame).not.toContain("CONNECTED");
    expect(frame).not.toContain("DEGRADED");
  });

  it("shows the permission tier with a plain-English gloss", () => {
    // The tier name sits in its own <Text> span (dim, color escalates by exception),
    // so "tier" and "system" land in separate spans with an SGR reset between them —
    // strip color before asserting the row.
    const frame = stripAnsi(
      render(<Header columns={60} version="0.1.0" tier="system" />).lastFrame() ?? "",
    );
    expect(frame).toContain("tier system"); // labelled, not a bare token
    expect(frame).toContain("full access"); // gloss explains what it grants
  });

  // ui.color.danger is the #FB7185 truecolor, emitted by Ink as the SGR
  // 38;2;251;113;133. Asserting its presence/absence is how we prove the tier is
  // quiet at rest and red only by exception. Ink emits color only when chalk's color
  // level is non-zero; a non-TTY CI run defaults it to 0 and would strip every code,
  // so pin it to truecolor for these tests (and restore it after) to make the color
  // assertions deterministic across local and CI.
  describe("tier color escalation", () => {
    const DANGER_SGR = "38;2;251;113;133";
    let prevLevel: typeof chalk.level;
    beforeEach(() => {
      prevLevel = chalk.level;
      chalk.level = 3;
    });
    afterEach(() => {
      chalk.level = prevLevel;
    });

    it("keeps the system tier quiet at rest, not alarm-red", () => {
      // At rest no destructive action is pending, so the `system` tier must NOT carry
      // the danger color — a steady red capsule is alarm fatigue. The tier word is
      // still present; only its color is muted to dim.
      const frame =
        render(<Header columns={60} version="0.1.0" tier="system" />).lastFrame() ?? "";
      expect(stripAnsi(frame)).toContain("tier system"); // still rendered
      expect(frame).not.toContain(DANGER_SGR); // no red anywhere at rest
    });

    it("escalates the tier to danger color when a destructive action is pending", () => {
      // Red is reserved for the moment it earns attention: a git/system confirmation
      // in flight. The controller passes destructivePending and the tier turns red.
      const frame =
        render(
          <Header columns={60} version="0.1.0" tier="system" destructivePending />,
        ).lastFrame() ?? "";
      expect(stripAnsi(frame)).toContain("tier system");
      expect(frame).toContain(DANGER_SGR); // danger color present on the tier
    });
  });

  it("glosses each tier so the level is self-explaining", () => {
    const op =
      render(<Header columns={60} version="0.1.0" tier="operator" />).lastFrame() ?? "";
    expect(op).toContain("operator");
    expect(op).toContain("terminals");
    const sup =
      render(<Header columns={60} version="0.1.0" tier="supervisor" />).lastFrame() ?? "";
    expect(sup).toContain("supervisor");
    expect(sup).toContain("read & UI only");
  });

  it("omits the tier line entirely when no tier is supplied", () => {
    const frame = render(<Header columns={60} version="0.1.0" />).lastFrame() ?? "";
    expect(frame).not.toContain("tier ");
  });

  it("always closes the header with a full-width rule", () => {
    // The rule is always present now (it closes the header off from the body),
    // not just when logging is active.
    const frame = render(<Header columns={60} version="0.1.0" />).lastFrame() ?? "";
    expect(frame).toMatch(/[─-]{10,}/);
  });

  it("sizes the rule to the columns prop", () => {
    // The masthead commits to <Static> (prints once, never repaints), so the rule
    // takes the explicit cockpit width it is handed rather than yoga-filling the
    // live region — a flex "100%" rule collapses to content width inside a Static
    // item, which is what shrank the masthead rule to the prose width.
    const frame = stripAnsi(
      render(<Header columns={60} version="0.1.0" />).lastFrame() ?? "",
    );
    const rule = frame.split("\n").find((line) => /^[─-]+$/.test(line));
    expect(rule).toBeDefined();
    expect(rule!.length).toBe(60);
  });

  it("places the rule directly under the project and a blank row below it", () => {
    const frame = stripAnsi(
      render(
        <Header columns={60} version="0.1.0" project="Daintree Assistant" />,
      ).lastFrame() ?? "",
    );
    const lines = frame.split("\n");
    expect(lines[0]).toContain("Daintree Assistant v0.1.0");
    expect(lines[1]).toBe("Daintree Assistant");
    expect(lines[2]).toMatch(/^[─-]+$/);
    expect(lines[3]).toBe("");
  });

  it("shows no emoji or brand mark — plain text only", () => {
    const frame = render(<Header columns={60} version="0.1.0" />).lastFrame() ?? "";
    expect(frame).not.toMatch(/\p{Extended_Pictographic}/u);
  });

  // Regression (#138): the header commits to <Static>, where Ink lays each item out
  // in an isolated tree with no parent width — so `width="100%"` collapsed to content
  // width and the `wrap="truncate"` rows (notably the long log path) had no bound and
  // physically wrapped. A numeric root width gives truncate a real bound. Lock it in:
  // at a narrow width EVERY masthead row must fit `columns`, the rule is exactly
  // `columns`, and the long log path truncates with an ellipsis instead of wrapping.
  it("truncates every masthead row to the column bound (no wrapping)", () => {
    const COLS = 22;
    const frame = stripAnsi(
      render(
        <Header
          columns={COLS}
          version="0.1.0"
          project="a-very-long-project-name-that-overflows"
          tier="system"
          logging
          logFile="/Users/gpriday/.daintree/logs/2026-06-20-ses_02f0965b.log"
        />,
      ).lastFrame() ?? "",
    );
    const rows = frame.split("\n");
    for (const row of rows) {
      expect(row.length).toBeLessThanOrEqual(COLS);
    }
    // The full-width rule fills exactly the column bound (no more, no less).
    expect(rows).toContain("─".repeat(COLS));
    // The over-long log path is clipped with the truncation ellipsis, not wrapped
    // onto a second physical row.
    expect(frame).toContain("…");
    expect(frame).not.toContain("ses_02f0965b.log");
  });

  it("names the active run when one is supplied", () => {
    const { lastFrame } = render(
      <Header columns={60} version="0.1.0" runTitle="repair watcher tests" />,
    );
    expect(lastFrame() ?? "").toContain("repair watcher tests");
  });

  it("surfaces the debug log under a rule when active", () => {
    const frame =
      render(
        <Header
          columns={60}
          version="0.1.0"
          logging
          logFile="/tmp/daintree.log"
        />,
      ).lastFrame() ?? "";
    expect(frame).toMatch(/[─-]{10,}/); // the rule that opens the log section
    expect(frame).toContain("logging"); // spelled out, not "LO"
    expect(frame).toContain("/tmp/daintree.log");
  });

  // Guards the header's rendered row count. The header owns the blank line BELOW
  // its rule so debug logging and the first transcript row never sit flush against
  // the masthead separator.
  it("renders a stable row count", () => {
    const rows = (el: ReactElement) =>
      (render(el).lastFrame() ?? "").split("\n").length;
    // wordmark (1) + rule + blank below rule = 3.
    expect(rows(<Header columns={60} version="0.1.0" />)).toBe(3);
    // + the logging line after the blank row = 4.
    expect(rows(<Header columns={60} version="0.1.0" logging logFile="/t.log" />)).toBe(4);
    // wordmark + project + run subtitle = 3 text rows + rule + blank = 5.
    expect(
      rows(<Header columns={60} version="0.1.0" project="p" runTitle="busy" />),
    ).toBe(5);
  });

  it("keeps an ASCII rule and bullet when unicode is disabled", () => {
    const prev = process.env.DAINTREE_ASCII;
    process.env.DAINTREE_ASCII = "1";
    try {
      const frame =
        render(
          <Header
            columns={60}
            version="0.1.0"
            tier="system"
            logging
            logFile="/tmp/t.log"
          />,
        ).lastFrame() ?? "";
      expect(frame).toContain("Daintree Assistant");
      expect(frame).toContain("full access"); // tier gloss still rendered
      expect(frame).toMatch(/-{10,}/); // ASCII rule (hyphens, not box-drawing)
      // Neither the log separator NOR the tier gloss may emit a Unicode bullet.
      expect(frame).not.toContain("·");
    } finally {
      if (prev === undefined) delete process.env.DAINTREE_ASCII;
      else process.env.DAINTREE_ASCII = prev;
    }
  });
});
