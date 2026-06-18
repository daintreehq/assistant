import { render } from "ink-testing-library";
import { AttentionBanner } from "../../src/ui/components/AttentionBanner.js";

const event = (severity: string) => ({ id: severity, severity }) as any;

describe("AttentionBanner", () => {
  it("renders nothing when the inbox is empty (no empty state)", () => {
    const { lastFrame } = render(<AttentionBanner events={[]} />);
    expect((lastFrame() ?? "").trim()).toBe("");
  });

  it("summarizes the queue with a count and an ops hint", () => {
    const { lastFrame } = render(
      <AttentionBanner events={[event("error"), event("attention")]} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("2 items need attention");
    expect(frame).toContain("^O ops");
  });

  it("uses the singular form for one item", () => {
    const { lastFrame } = render(<AttentionBanner events={[event("attention")]} />);
    expect(lastFrame() ?? "").toContain("1 item needs attention");
  });
});
