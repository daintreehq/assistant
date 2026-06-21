import { test, expect, describe } from "bun:test";
import { testRender } from "@opentui/react/test-utils";
import { ControlRoom } from "../../src/ui/ControlRoom.js";
import type { DashboardState, TranscriptCell } from "../../src/ui/types.js";

// The native <markdown> renderable parses asynchronously (tree-sitter) off the render
// loop, so a bare flush() can return before the prose paints. Poll flush + settle until
// the expected text appears (or a generous ceiling), like tests/ui/markdown.test.tsx.
async function waitFor(
  t: {
    flush: () => Promise<void>;
    waitForVisualIdle: () => Promise<void>;
    captureCharFrame: () => string;
  },
  expected: string,
): Promise<string> {
  let frame = "";
  for (let i = 0; i < 60; i++) {
    await t.flush();
    await t.waitForVisualIdle();
    frame = t.captureCharFrame();
    if (frame.includes(expected)) return frame;
    await new Promise((r) => setTimeout(r, 50));
  }
  return frame;
}

const EMPTY_DASHBOARD: DashboardState = {
  mcp: { connected: true },
  workflowRuns: [],
  watchers: [],
  timers: [],
  inbox: [],
  audit: [],
};

/** An in-flight turn whose streamed body is far taller than the terminal. */
function tallTurn(lines: number): TranscriptCell {
  return {
    kind: "turn",
    id: "turn_busy",
    userText: "do a big thing",
    assistantText: Array.from(
      { length: lines },
      (_, i) => `streaming output line ${i + 1}`,
    ).join("\n"),
    streaming: true,
    activities: [],
    notes: [],
    state: "active",
    phase: "generating",
    phaseStartedAt: 0,
    ts: 0,
  };
}

describe("ControlRoom pins the status + composer", () => {
  // Regression for the user-reported bug: a long in-flight answer used to grow the
  // footer past the screen and clip the composer off the bottom, so you couldn't see
  // it or queue. With a bounded `rows` the conversation region clips and the chrome
  // stays pinned and visible.
  test("a turn taller than the terminal keeps the composer + status on screen", async () => {
    const ROWS = 14;
    const t = await testRender(
      <ControlRoom
        project="assistant"
        tier="operator"
        columns={80}
        rows={ROWS}
        connected
        transcript={[tallTurn(80)]}
        dashboard={EMPTY_DASHBOARD}
        busy
        stage="Generating"
        view="home"
        renderHeader={false}
        composerFocus
        now={0}
      />,
      { width: 80, height: ROWS },
    );
    await t.flush();
    await t.waitForVisualIdle();
    const frame = t.captureCharFrame();

    // The guarantee under test: the composer + its busy stage stay PINNED on screen no
    // matter how tall the in-flight turn is, so the input is always reachable and you
    // can read the status and queue — the bug where the input vanished mid-stream.
    expect(frame).toContain("commands"); // "/ commands" — the composer is visible
    expect(frame).toContain("ops"); // "^O inspect ops"
    expect(frame).toContain("Generating"); // the precise stage shows at the prompt
    // The conversation region still renders its head (the user card), so it isn't a
    // blank void. (A >screen in-flight markdown body can't paint in the clipped live
    // region — an OpenTUI renderable limit — but lands in full in native scrollback the
    // moment the turn seals. Progressive scrollback commit removes the clip entirely.)
    expect(frame).toContain("do a big thing");

    t.renderer.destroy?.();
  });

  // Without a `rows` budget (gallery/tests) nothing clips — the whole turn renders.
  test("renders the full turn when no row budget is given", async () => {
    const t = await testRender(
      <ControlRoom
        project="assistant"
        tier="operator"
        columns={80}
        connected
        transcript={[tallTurn(6)]}
        dashboard={EMPTY_DASHBOARD}
        busy
        stage="Generating"
        view="home"
        renderHeader={false}
        composerFocus
        now={0}
      />,
      { width: 80, height: 40 },
    );
    // Poll for line 1 (the async markdown stable block), not the always-present plain
    // pending line, so we don't return before the body has painted.
    const frame = await waitFor(t, "streaming output line 1");
    expect(frame).toContain("streaming output line 1");
    expect(frame).toContain("streaming output line 6");
    expect(frame).toContain("commands");

    t.renderer.destroy?.();
  });
});
