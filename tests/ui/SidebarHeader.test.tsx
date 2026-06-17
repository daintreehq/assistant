import { render } from "ink-testing-library";
import { SidebarHeader } from "../../src/ui/sidebar/SidebarHeader.js";
import type { HeaderStatus } from "../../src/ui/sidebar/model.js";

function status(over: Partial<HeaderStatus>): HeaderStatus {
  return {
    live: true,
    liveLabel: "live",
    project: "assistant",
    mcpOk: true,
    tier: "op",
    watcherCount: 4,
    attentionCount: 2,
    ...over,
  };
}

describe("SidebarHeader", () => {
  it("renders the two-line capsule: identity + live + compact status", () => {
    const { lastFrame } = render(<SidebarHeader status={status({})} />);
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Daintree");
    expect(frame).toContain("live");
    expect(frame).toContain("assistant");
    expect(frame).toContain("op");
    expect(frame).toContain("4w");
    expect(frame).toContain("2!");
  });

  it("shows degraded when not live", () => {
    const { lastFrame } = render(
      <SidebarHeader status={status({ live: false, liveLabel: "degraded", mcpOk: false })} />,
    );
    expect(lastFrame() ?? "").toContain("degraded");
  });
});
