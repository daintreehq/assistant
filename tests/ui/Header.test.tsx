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
});
