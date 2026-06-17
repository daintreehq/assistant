import { render } from "ink-testing-library";
import { Header } from "../../src/ui/components/Header.js";
import type { DashboardState } from "../../src/ui/types.js";

const app = {
  config: { projectPath: "/Users/x/Projects/assistant", tier: "operator" },
} as any;

function dash(over: Partial<DashboardState>): DashboardState {
  return {
    mcp: { connected: true } as any,
    watchers: [],
    timers: [],
    inbox: [],
    audit: [],
    ...over,
  };
}

describe("Header", () => {
  it("renders title, project, tier and connected state", () => {
    const { lastFrame } = render(<Header app={app} dashboard={dash({})} />);
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Daintree Assistant");
    expect(frame).toContain("assistant"); // project basename
    expect(frame).toContain("operator");
    expect(frame).toContain("connected");
  });

  it("shows degraded when MCP is down", () => {
    const { lastFrame } = render(
      <Header app={app} dashboard={dash({ mcp: { connected: false } as any })} />,
    );
    expect(lastFrame() ?? "").toContain("degraded");
  });
});
