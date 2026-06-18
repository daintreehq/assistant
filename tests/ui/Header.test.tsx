import { render } from "ink-testing-library";
import { Header } from "../../src/ui/components/Header.js";

const app = {
  config: { projectPath: "/Users/x/Projects/assistant", tier: "operator" },
} as any;

describe("Header", () => {
  it("renders the brand, project basename and tier as one calm identity line", () => {
    const { lastFrame } = render(<Header app={app} />);
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Daintree");
    expect(frame).toContain("assistant"); // project basename
    expect(frame).toContain("operator");
  });
});
