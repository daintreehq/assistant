import { render } from "ink-testing-library";
import { Header } from "../../src/ui/components/Header.js";

describe("Header", () => {
  it("renders the brand signature, project basename and tier", () => {
    const { lastFrame } = render(
      <Header project="assistant" tier="operator" connected />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("DAINTREE"); // brand mark
    expect(frame).toContain("assistant"); // project basename
    expect(frame).toContain("OPERATOR"); // tier
  });

  it("shows a live connection badge", () => {
    expect(
      render(<Header project="x" tier="operator" connected />).lastFrame() ?? "",
    ).toContain("CONNECTED");
    expect(
      render(<Header project="x" tier="operator" connected={false} />).lastFrame() ??
        "",
    ).toContain("DEGRADED");
  });

  it("names the active run when one is supplied", () => {
    const { lastFrame } = render(
      <Header
        project="x"
        tier="operator"
        connected
        runTitle="repair watcher tests"
      />,
    );
    expect(lastFrame() ?? "").toContain("repair watcher tests");
  });
});
