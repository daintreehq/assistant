import { test, expect, describe } from "bun:test";
import { act, useState } from "react";
import { testRender } from "@opentui/react/test-utils";
import { ApprovalSheet } from "../../src/ui/components/ApprovalSheet.js";
import type { PendingConfirm } from "../../src/ui/types.js";

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
  test("leads with the consequence, keeps the tool name as a dim secondary label", async () => {
    const t = await testRender(
      <ApprovalSheet
        pending={pending("git.push", "external", {
          consequence: "Pushes your branch to the remote, visible to collaborators.",
        })}
        onResolve={() => {}}
      />,
      { width: 72, height: 16 },
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain("Push branch to origin?");
    expect(frame).toContain("affects");
    expect(frame).toContain("Pushes your branch to the remote");
    // The tool name stays visible (dim) so the sheet is never a black box.
    expect(frame).toContain("git.push");
    expect(frame).toContain("approve");
    expect(frame).toContain("decline");
  });

  test("never renders the raw risk class as a labelled field", async () => {
    const t = await testRender(
      <ApprovalSheet pending={pending("git.push", "external")} onResolve={() => {}} />,
      { width: 72, height: 16 },
    );
    await t.flush();
    const frame = t.captureCharFrame();
    // The old `risk  external` row is gone — consequence language replaces it.
    expect(frame).not.toMatch(/risk\s+external/);
  });

  test("falls back to a per-risk consequence when the tool gives none", async () => {
    const t = await testRender(
      <ApprovalSheet pending={pending("daintree.call", "system")} onResolve={() => {}} />,
      { width: 72, height: 16 },
    );
    await t.flush();
    expect(t.captureCharFrame()).toContain("system-level action");
  });

  test("falls back when the consequence is blank rather than rendering an empty line", async () => {
    const t = await testRender(
      <ApprovalSheet
        pending={pending("daintree.call", "system", { consequence: "   " })}
        onResolve={() => {}}
      />,
      { width: 72, height: 16 },
    );
    await t.flush();
    expect(t.captureCharFrame()).toContain("system-level action");
  });

  test("renders a non-empty consequence for every risk class", async () => {
    const RISKS = ["read", "local", "ui", "terminal", "project", "git", "external", "system"];
    for (const risk of RISKS) {
      const t = await testRender(
        <ApprovalSheet pending={pending("some.tool", risk)} onResolve={() => {}} />,
        { width: 72, height: 16 },
      );
      await t.flush();
      const frame = t.captureCharFrame();
      const affects = (frame.split("\n").find((l) => l.includes("affects")) ?? "").trim();
      // The affects row must carry prose, never just the bare risk-class word.
      expect(affects, risk).toContain("affects");
      expect(affects.replace("affects", "").trim().length, risk).toBeGreaterThan(0);
      expect(affects.replace("affects", "").trim(), risk).not.toBe(risk);
      t.renderer.destroy?.();
    }
  });

  test("hides the raw reason and args until V is pressed, then reveals them", async () => {
    const t = await testRender(
      <ApprovalSheet pending={pending("git.push", "external")} onResolve={() => {}} />,
      { width: 72, height: 16 },
    );
    await t.flush();
    // Collapsed by default: the LLM-facing summary and args are not shown.
    expect(t.captureCharFrame()).not.toContain("ready for review");
    expect(t.captureCharFrame()).not.toContain("fix/x");

    act(() => {
      t.mockInput.pressKey("v");
    });
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain("ready for review");
    expect(frame).toContain("fix/x");
  });

  test("collapses the inspect panel when a new request takes the sheet", async () => {
    // The OpenTUI test harness mounts a single tree (no Ink-style `rerender`), so
    // drive the prop change through a stateful wrapper that swaps `pending.id`,
    // exercising the same render-time reset path as a live re-render.
    let swap: () => void = () => {};
    function Harness() {
      const [id, setId] = useState("cfm_1");
      swap = () => setId("cfm_2");
      return (
        <ApprovalSheet
          pending={pending("git.push", "external", { id })}
          onResolve={() => {}}
        />
      );
    }

    const t = await testRender(<Harness />, { width: 72, height: 16 });
    await t.flush();
    act(() => {
      t.mockInput.pressKey("v");
    });
    await t.flush();
    expect(t.captureCharFrame()).toContain("ready for review");

    // Swap to a new request id; the render-time reset must collapse the panel so
    // even the first frame of the new request hides the previous expanded args.
    act(() => {
      swap();
    });
    await t.flush();
    expect(t.captureCharFrame()).not.toContain("ready for review");
  });

  test("titles a terminal-input request distinctly", async () => {
    const t = await testRender(
      <ApprovalSheet pending={pending("terminal.sendInput", "terminal")} onResolve={() => {}} />,
      { width: 72, height: 16 },
    );
    await t.flush();
    expect(t.captureCharFrame()).toContain("Send input to terminal?");
  });

  test("approves on y and declines on n", async () => {
    let a: boolean | undefined;
    const t1 = await testRender(
      <ApprovalSheet pending={pending("git.push", "external")} onResolve={(v) => (a = v)} />,
      { width: 72, height: 16 },
    );
    await t1.flush();
    act(() => {
      t1.mockInput.pressKey("y");
    });
    await t1.flush();
    expect(a).toBe(true);

    let b: boolean | undefined;
    const t2 = await testRender(
      <ApprovalSheet pending={pending("git.push", "external")} onResolve={(v) => (b = v)} />,
      { width: 72, height: 16 },
    );
    await t2.flush();
    act(() => {
      t2.mockInput.pressKey("n");
    });
    await t2.flush();
    expect(b).toBe(false);
  });
});
