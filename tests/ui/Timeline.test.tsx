import { render } from "ink-testing-library";
import { Timeline } from "../../src/ui/components/Timeline.js";
import type { TimelineItem } from "../../src/ui/types.js";

describe("Timeline", () => {
  it("shows an empty hint when there are no items", () => {
    const { lastFrame } = render(<Timeline items={[]} height={10} />);
    expect(lastFrame() ?? "").toContain("Ask Daintree");
  });

  it("renders user, assistant, and tool rows", () => {
    const items: TimelineItem[] = [
      { id: "u1", kind: "user", text: "watch the build", ts: 0 },
      { id: "a1", kind: "assistant", text: "On it.", ts: 0 },
      {
        id: "t1",
        kind: "tool",
        name: "agentTask.spawnForEdits",
        args: {},
        ok: true,
        summary: "spawned term_1",
        ts: 0,
      },
    ];
    const frame = render(<Timeline items={items} height={20} />).lastFrame() ?? "";
    expect(frame).toContain("watch the build");
    expect(frame).toContain("On it.");
    expect(frame).toContain("agentTask.spawnForEdits");
    expect(frame).toContain("spawned term_1");
    expect(frame).toContain("ok");
  });

  it("shows a running tool as 'running'", () => {
    const items: TimelineItem[] = [
      { id: "t1", kind: "tool", name: "fs.read", args: {}, ts: 0 },
    ];
    expect(
      render(<Timeline items={items} height={10} />).lastFrame() ?? "",
    ).toContain("running");
  });
});
