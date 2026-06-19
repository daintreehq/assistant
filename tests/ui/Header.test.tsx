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

  it("drops the operational badges from the masthead", () => {
    const frame = render(<Header columns={60} version="0.1.0" />).lastFrame() ?? "";
    // Tier + the MCP connection badge now live in the StatusLine, not here.
    expect(frame).not.toContain("CONNECTED");
    expect(frame).not.toContain("DEGRADED");
    expect(frame).not.toContain("OPERATOR");
  });

  it("always closes the header with a full-width rule", () => {
    // The rule is always present now (it closes the header off from the body),
    // not just when logging is active.
    const frame = render(<Header columns={60} version="0.1.0" />).lastFrame() ?? "";
    expect(frame).toMatch(/[─-]{10,}/);
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

  // Guards the header's rendered row count. The header owns the blank line ABOVE
  // its rule (marginTop) but NOT a trailing blank — the first transcript cell owns
  // the gap below the header via its own marginTop, so the two never double up.
  it("renders a stable row count", () => {
    const rows = (el: ReactElement) =>
      (render(el).lastFrame() ?? "").split("\n").length;
    // wordmark (1) + blank above rule + rule = 3.
    expect(rows(<Header columns={60} version="0.1.0" />)).toBe(3);
    // + the logging line under the rule = 4.
    expect(rows(<Header columns={60} version="0.1.0" logging logFile="/t.log" />)).toBe(4);
    // wordmark + project + run subtitle = 3 text rows + blank + rule = 5.
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
          <Header columns={60} version="0.1.0" logging logFile="/tmp/t.log" />,
        ).lastFrame() ?? "";
      expect(frame).toContain("Daintree Assistant");
      expect(frame).toMatch(/-{10,}/); // ASCII rule (hyphens, not box-drawing)
      expect(frame).not.toContain("·"); // log separator uses the ASCII bullet
    } finally {
      if (prev === undefined) delete process.env.DAINTREE_ASCII;
      else process.env.DAINTREE_ASCII = prev;
    }
  });
});
