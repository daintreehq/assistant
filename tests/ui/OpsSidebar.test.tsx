import { render } from "ink-testing-library";
import { OpsSidebar } from "../../src/ui/components/OpsSidebar.js";
import type { DashboardState } from "../../src/ui/types.js";

// useTerminalPreview only touches app.mcp.isConnected/callTool.
const app = { mcp: { isConnected: () => false, callTool: async () => ({}) } } as any;

function dash(over: Partial<DashboardState> = {}): DashboardState {
  return {
    mcp: { connected: false } as any,
    watchers: [],
    timers: [],
    inbox: [],
    audit: [],
    ...over,
  };
}

describe("OpsSidebar", () => {
  it("renders every deck section", () => {
    const { lastFrame, unmount } = render(
      <OpsSidebar app={app} dashboard={dash()} height={30} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Operations Deck");
    expect(frame).toContain("Watchers");
    expect(frame).toContain("Terminals");
    expect(frame).toContain("Inbox");
    expect(frame).toContain("Timers");
    expect(frame).toContain("Audit");
    unmount(); // clears the useTerminalPreview poll interval
  });
});
