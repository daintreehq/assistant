import type { ReactElement } from "react";
import { render } from "ink-testing-library";
import { Header } from "../../src/ui/components/Header.js";

describe("Header", () => {
  it("renders the product wordmark and version", () => {
    const { lastFrame } = render(<Header columns={60} version="0.1.0" />);
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Daintree assistant"); // brand wordmark
    expect(frame).toContain("v0.1.0"); // version beside the name
  });

  it("drops the operational badges from the masthead", () => {
    const frame = render(<Header columns={60} version="0.1.0" />).lastFrame() ?? "";
    // Tier + the MCP connection badge now live in the StatusLine, not here.
    expect(frame).not.toContain("CONNECTED");
    expect(frame).not.toContain("DEGRADED");
    expect(frame).not.toContain("OPERATOR");
  });

  it("closes the box with a full-width rule", () => {
    const frame = render(<Header columns={60} version="0.1.0" />).lastFrame() ?? "";
    expect(frame).toMatch(/[─-]{10,}/); // a long horizontal rule
  });

  it("names the active run when one is supplied", () => {
    const { lastFrame } = render(
      <Header columns={60} version="0.1.0" runTitle="repair watcher tests" />,
    );
    expect(lastFrame() ?? "").toContain("repair watcher tests");
  });

  it("surfaces the debug log on its own line when active", () => {
    const frame =
      render(
        <Header
          columns={60}
          version="0.1.0"
          logging
          logFile="/tmp/daintree.log"
        />,
      ).lastFrame() ?? "";
    expect(frame).toContain("LOG");
    expect(frame).toContain("/tmp/daintree.log");
  });

  // The rendered row count is the contract behind ControlRoom's headerH budget
  // (6 + runTitle + logging). If the layout drifts, this guard fails before the
  // body silently overlaps the header on short terminals.
  it("renders a stable row count matching the headerH budget", () => {
    const rows = (el: ReactElement) =>
      (render(el).lastFrame() ?? "").split("\n").length;
    expect(rows(<Header columns={60} version="0.1.0" />)).toBe(6);
    expect(rows(<Header columns={60} version="0.1.0" logging logFile="/t.log" />)).toBe(7);
    expect(rows(<Header columns={60} version="0.1.0" runTitle="busy" />)).toBe(7);
    expect(
      rows(
        <Header columns={60} version="0.1.0" runTitle="busy" logging logFile="/t.log" />,
      ),
    ).toBe(8);
  });

  it("falls back to ASCII glyphs when unicode is disabled", () => {
    const prev = process.env.DAINTREE_ASCII;
    process.env.DAINTREE_ASCII = "1";
    try {
      const frame =
        render(
          <Header columns={60} version="0.1.0" logging logFile="/tmp/t.log" />,
        ).lastFrame() ?? "";
      expect(frame).toContain("Daintree assistant");
      expect(frame).toContain("/^\\"); // ASCII canopy, not the block logo
      expect(frame).toMatch(/-{10,}/); // ASCII rule (hyphens, not box-drawing)
      expect(frame).not.toContain("▟"); // no unicode block glyph leaks through
      expect(frame).not.toContain("·"); // log separator uses the ASCII bullet
    } finally {
      if (prev === undefined) delete process.env.DAINTREE_ASCII;
      else process.env.DAINTREE_ASCII = prev;
    }
  });
});
