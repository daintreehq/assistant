import { render } from "ink-testing-library";
import { ApprovalSheet } from "../../src/ui/components/ApprovalSheet.js";
import type { PendingConfirm } from "../../src/ui/types.js";

const tick = () => new Promise((r) => setTimeout(r, 20));

function pending(toolName: string, risk: string): PendingConfirm {
  return {
    id: "cfm_1",
    request: {
      toolName,
      risk: risk as any,
      summary: "the branch is ready for review",
      args: { branch: "fix/x", remote: "origin" },
    },
    resolve: () => {},
  };
}

describe("ApprovalSheet", () => {
  it("uses a risk-specific title and states action/risk/reason", () => {
    const { lastFrame } = render(
      <ApprovalSheet pending={pending("git.push", "external")} onResolve={() => {}} />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Push branch to origin?");
    expect(frame).toContain("git.push");
    expect(frame).toContain("ready for review");
    expect(frame).toContain("approve");
    expect(frame).toContain("decline");
  });

  it("titles a terminal-input request distinctly", () => {
    const { lastFrame } = render(
      <ApprovalSheet pending={pending("terminal.sendInput", "terminal")} onResolve={() => {}} />,
    );
    expect(lastFrame() ?? "").toContain("Send input to terminal?");
  });

  it("approves on y and declines on n", async () => {
    let a: boolean | undefined;
    const { stdin } = render(
      <ApprovalSheet pending={pending("git.push", "external")} onResolve={(v) => (a = v)} />,
    );
    stdin.write("y");
    await tick();
    expect(a).toBe(true);

    let b: boolean | undefined;
    const r2 = render(
      <ApprovalSheet pending={pending("git.push", "external")} onResolve={(v) => (b = v)} />,
    );
    r2.stdin.write("n");
    await tick();
    expect(b).toBe(false);
  });
});
