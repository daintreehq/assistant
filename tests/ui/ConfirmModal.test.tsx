import { Box } from "ink";
import { render } from "ink-testing-library";
import { ConfirmModal } from "../../src/ui/components/ConfirmModal.js";
import type { PendingConfirm } from "../../src/ui/types.js";

const tick = () => new Promise((r) => setTimeout(r, 20));

function pending(resolve: (v: boolean) => void): PendingConfirm {
  return {
    id: "cfm_1",
    request: {
      toolName: "git.commit",
      risk: "git",
      summary: "commit staged changes",
      args: { message: "wip" },
    },
    resolve,
  };
}

describe("ConfirmModal", () => {
  it("renders the pending request", () => {
    // Absolute positioning needs a sized ancestor (the app provides one).
    const { lastFrame } = render(
      <Box height={12} width={70} position="relative">
        <ConfirmModal pending={pending(() => {})} onResolve={() => {}} />
      </Box>,
    );
    const frame = lastFrame() ?? "";
    expect(frame).toContain("Confirm action");
    expect(frame).toContain("git.commit");
    expect(frame).toContain("approve");
  });

  it("approves on 'y'", async () => {
    let result: boolean | undefined;
    const { stdin } = render(
      <ConfirmModal pending={pending(() => {})} onResolve={(v) => (result = v)} />,
    );
    stdin.write("y");
    await tick();
    expect(result).toBe(true);
  });

  it("declines on 'n'", async () => {
    let result: boolean | undefined;
    const { stdin } = render(
      <ConfirmModal pending={pending(() => {})} onResolve={(v) => (result = v)} />,
    );
    stdin.write("n");
    await tick();
    expect(result).toBe(false);
  });
});
