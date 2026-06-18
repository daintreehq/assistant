import { render } from "ink-testing-library";
import { AttentionBanner } from "../../src/ui/components/AttentionBanner.js";

const event = (severity: string, title: string) =>
  ({ id: title, severity, title }) as any;

describe("AttentionBanner", () => {
  it("renders nothing when the inbox is empty (no empty state)", () => {
    const { lastFrame } = render(<AttentionBanner events={[]} />);
    expect((lastFrame() ?? "").trim()).toBe("");
  });

  it("names the most urgent event and rolls up the rest", () => {
    const { lastFrame } = render(
      <AttentionBanner
        events={[
          event("error", "Tests failed in term_8"),
          event("attention", "Branch ready"),
        ]}
      />,
    );
    const frame = lastFrame() ?? "";
    // The title beats a bare count.
    expect(frame).toContain("Tests failed in term_8");
    expect(frame).toContain("1 more");
    expect(frame).toContain("^O inspect");
  });

  it("omits the rollup for a single item", () => {
    const { lastFrame } = render(
      <AttentionBanner events={[event("attention", "Needs input")]} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Needs input");
    expect(frame).not.toContain("more");
  });
});
