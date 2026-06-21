import { test, expect, describe } from "bun:test";
import { testRender } from "@opentui/react/test-utils";
import { Transcript } from "../../src/ui/components/Transcript.js";
import type { TranscriptCell } from "../../src/ui/types.js";

const FIXED = 1_700_000_000_000;

function activeRun(): TranscriptCell[] {
  return [
    {
      kind: "turn",
      id: "t1",
      userText: "Fix the watcher tests.",
      assistantText: "I'll delegate and supervise.",
      streaming: false,
      state: "active",
      ts: FIXED - 20000,
      notes: [],
      activities: [
        {
          id: "c1",
          name: "fs.search",
          label: "Inspected",
          detail: "tests/ui",
          args: { query: "watcher" },
          summary: "8 matches",
          state: "done",
          startedAt: FIXED - 20000,
          endedAt: FIXED - 20000 + 180,
        },
        {
          id: "c3",
          name: "watcher.terminal.create",
          label: "Watching",
          detail: "tests running",
          args: { goal: "wait" },
          state: "active",
          startedAt: FIXED - 18000,
        },
      ],
    },
  ];
}

describe("Transcript", () => {
  test("shows an empty hint when there are no cells", async () => {
    const t = await testRender(
      <Transcript cells={[]} height={10} width={72} />,
      { width: 72, height: 10 },
    );
    await t.flush();
    expect(t.captureCharFrame()).toContain("Ask Daintree");
  });

  test("renders the run as YOU/DAINTREE markers and a branch tree of verbs", async () => {
    const t = await testRender(
      <Transcript cells={activeRun()} height={20} width={72} now={FIXED} />,
      { width: 72, height: 20 },
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain("YOU"); // quiet who-said-what label
    expect(frame).toContain("▏"); // the human's left accent bar marks the turn
    expect(frame).toContain("DAINTREE");
    expect(frame).not.toContain("assistant"); // no role label
    // Human verbs, not raw fn() syntax or JSON.
    expect(frame).toContain("Inspected");
    expect(frame).toContain("Watching");
    expect(frame).not.toContain("watcher.terminal.create(");
    expect(frame).not.toContain('"query"');
    // Branch grammar + a settled duration.
    expect(frame).toMatch(/[├└]/);
    expect(frame).toContain("180ms");
  });

  test("reveals raw args/result only in expanded detail mode", async () => {
    const t = await testRender(
      <Transcript cells={activeRun()} height={30} width={72} now={FIXED} expanded />,
      { width: 72, height: 30 },
    );
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain("fs.search args:");
    expect(frame).toContain("result:");
  });
});
