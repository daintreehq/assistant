import { render } from "ink-testing-library";
import { StatusLine } from "../../src/ui/components/StatusLine.js";
import type { DashboardState } from "../../src/ui/types.js";

function dash(over: Partial<DashboardState> = {}): DashboardState {
  return {
    mcp: { connected: true } as any,
    watchers: [],
    timers: [],
    inbox: [],
    audit: [],
    ...over,
  };
}

const watcher = (id: string) => ({ id, title: id }) as any;
const event = (severity: string) => ({ id: severity, severity }) as any;
const timer = (id: string) => ({ id }) as any;

describe("StatusLine", () => {
  it("collapses to a single calm idle token when nothing is active", () => {
    const { lastFrame } = render(<StatusLine dashboard={dash()} />);
    const frame = lastFrame() ?? "";
    expect(frame).toContain("watching");
    // No count chips at all — the whole point is to kill the "0 0 0" clutter.
    expect(frame).not.toContain("›"); // terminal chip glyph
    expect(frame).not.toContain("⏱"); // timer chip glyph
    expect(frame).not.toContain("!"); // attention chip glyph
  });

  it("shows only non-zero count chips", () => {
    const { lastFrame } = render(
      <StatusLine
        dashboard={dash({ watchers: [watcher("a"), watcher("b")], timers: [timer("t")] })}
      />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("› 2"); // two terminals
    expect(frame).toContain("⏱ 1"); // one timer
    expect(frame).not.toContain("watching"); // idle token gone once active
    expect(frame).not.toContain("!"); // no inbox chip when inbox empty
  });

  it("renders the attention chip when the inbox is non-empty", () => {
    const { lastFrame } = render(
      <StatusLine dashboard={dash({ inbox: [event("error"), event("attention")] })} />,
    );
    expect(lastFrame() ?? "").toContain("! 2");
  });

  it("flags a degraded MCP connection", () => {
    const { lastFrame } = render(
      <StatusLine dashboard={dash({ mcp: { connected: false } as any })} />,
    );
    expect(lastFrame() ?? "").toContain("degraded");
  });
});
