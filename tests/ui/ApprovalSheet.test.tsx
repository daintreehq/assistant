import { render } from "ink-testing-library";
import { ApprovalSheet } from "../../src/ui/components/ApprovalSheet.js";
import type { PendingConfirm } from "../../src/ui/types.js";

const tick = () => new Promise((r) => setTimeout(r, 20));

function pending(
  toolName: string,
  risk: string,
  extra: { id?: string; consequence?: string } = {},
): PendingConfirm {
  return {
    id: extra.id ?? "cfm_1",
    request: {
      toolName,
      risk: risk as any,
      summary: "the branch is ready for review",
      consequence: extra.consequence,
      args: { branch: "fix/x", remote: "origin" },
    },
    resolve: () => {},
  };
}

describe("ApprovalSheet", () => {
  it("leads with the consequence, keeps the tool name as a dim secondary label", () => {
    const { lastFrame } = render(
      <ApprovalSheet
        pending={pending("git.push", "external", {
          consequence: "Pushes your branch to the remote, visible to collaborators.",
        })}
        onResolve={() => {}}
      />,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Push branch to origin?");
    expect(frame).toContain("affects");
    expect(frame).toContain("Pushes your branch to the remote");
    // The tool name stays visible (dim) so the sheet is never a black box.
    expect(frame).toContain("git.push");
    expect(frame).toContain("approve");
    expect(frame).toContain("decline");
  });

  it("never renders the raw risk class as a labelled field", () => {
    const { lastFrame } = render(
      <ApprovalSheet pending={pending("git.push", "external")} onResolve={() => {}} />,
    );
    const frame = lastFrame() ?? "";
    // The old `risk  external` row is gone — consequence language replaces it.
    expect(frame).not.toMatch(/risk\s+external/);
  });

  it("falls back to a per-risk consequence when the tool gives none", () => {
    const { lastFrame } = render(
      <ApprovalSheet pending={pending("daintree.call", "system")} onResolve={() => {}} />,
    );
    expect(lastFrame() ?? "").toContain("system-level action");
  });

  it("hides the raw reason and args until V is pressed, then reveals them", async () => {
    const { lastFrame, stdin } = render(
      <ApprovalSheet pending={pending("git.push", "external")} onResolve={() => {}} />,
    );
    // Collapsed by default: the LLM-facing summary and args are not shown.
    expect(lastFrame() ?? "").not.toContain("ready for review");
    expect(lastFrame() ?? "").not.toContain("fix/x");

    stdin.write("v");
    await tick();
    const frame = lastFrame() ?? "";
    expect(frame).toContain("ready for review");
    expect(frame).toContain("fix/x");
  });

  it("collapses the inspect panel when a new request takes the sheet", async () => {
    const { lastFrame, stdin, rerender } = render(
      <ApprovalSheet
        pending={pending("git.push", "external", { id: "cfm_1" })}
        onResolve={() => {}}
      />,
    );
    stdin.write("v");
    await tick();
    expect(lastFrame() ?? "").toContain("ready for review");

    rerender(
      <ApprovalSheet
        pending={pending("git.push", "external", { id: "cfm_2" })}
        onResolve={() => {}}
      />,
    );
    await tick();
    // A fresh prompt must not inherit the previous one's expanded view.
    expect(lastFrame() ?? "").not.toContain("ready for review");
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
