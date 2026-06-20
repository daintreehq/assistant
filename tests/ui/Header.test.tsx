import type { ReactElement } from "react";
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
    // The system tier colors its name red, so "tier" and "system" land in separate
    // spans with an SGR reset between them — strip color before asserting the row.
    const frame = stripAnsi(
      render(<Header columns={60} version="0.1.0" tier="system" />).lastFrame() ?? "",
    );
    expect(frame).toContain("tier system"); // labelled, not a bare token
    expect(frame).toContain("full access"); // gloss explains what it grants
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

  it("does not cap the rule to the columns prop", () => {
    const frame = stripAnsi(
      render(<Header columns={60} version="0.1.0" />).lastFrame() ?? "",
    );
    const rule = frame.split("\n").find((line) => /^[─-]+$/.test(line));
    expect(rule).toBeDefined();
    expect(rule!.length).toBeGreaterThan(60);
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
