import { render } from "ink-testing-library";
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
  it("shows an empty hint when there are no cells", () => {
    expect(
      render(<Transcript cells={[]} height={10} width={72} />).lastFrame() ?? "",
    ).toContain("Ask Daintree");
  });

  it("renders the run as YOU/DAINTREE markers and a branch tree of verbs", () => {
    const frame =
      render(
        <Transcript cells={activeRun()} height={20} width={72} now={FIXED} />,
      ).lastFrame() ?? "";
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

  it("reveals raw args/result only in expanded detail mode", () => {
    const frame =
      render(
        <Transcript cells={activeRun()} height={30} width={72} now={FIXED} expanded />,
      ).lastFrame() ?? "";
    expect(frame).toContain("fs.search args:");
    expect(frame).toContain("result:");
  });

  // A plain short turn, ~5 estimated rows: one fits in height=6, two don't.
  function turn(id: string, user: string, assistant: string): TranscriptCell {
    return {
      kind: "turn",
      id,
      userText: user,
      assistantText: assistant,
      streaming: false,
      state: "complete",
      ts: FIXED,
      notes: [],
      activities: [],
    };
  }

  function threeTurns(): TranscriptCell[] {
    return [
      turn("a", "First question.", "First answer."),
      turn("b", "Second question.", "Second answer."),
      turn("c", "Third question.", "Third answer."),
    ];
  }

  it("anchors on the newest turn and counts the older ones above it", () => {
    const frame =
      render(
        <Transcript cells={threeTurns()} height={6} width={72} now={FIXED} />,
      ).lastFrame() ?? "";
    expect(frame).toContain("Third question.");
    expect(frame).not.toContain("First question.");
    expect(frame).toContain("↑ 2 older turns");
  });

  it("pages back to older turns with scrollOffset", () => {
    const frame =
      render(
        <Transcript
          cells={threeTurns()}
          height={6}
          width={72}
          now={FIXED}
          scrollOffset={1}
        />,
      ).lastFrame() ?? "";
    expect(frame).toContain("Second question.");
    expect(frame).not.toContain("Third question.");
    // One older turn ("a") still sits above the window.
    expect(frame).toContain("↑ 1 older turn");
  });

  it("clamps an out-of-range scrollOffset to the oldest turn without throwing", () => {
    const frame =
      render(
        <Transcript
          cells={threeTurns()}
          height={6}
          width={72}
          now={FIXED}
          scrollOffset={99}
        />,
      ).lastFrame() ?? "";
    expect(frame).toContain("First question.");
    // Nothing is older than the oldest, so no indicator.
    expect(frame).not.toContain("older turn");
  });

  it("collapses an oversized newest turn to a compact summary instead of clipping", () => {
    const long = "word ".repeat(400).trim(); // wraps far past a 6-row viewport
    const cells: TranscriptCell[] = [turn("big", "Tell me everything.", long)];
    const frame =
      render(
        <Transcript cells={cells} height={6} width={72} now={FIXED} />,
      ).lastFrame() ?? "";
    // Markers survive so "who said what" still reads...
    expect(frame).toContain("YOU");
    expect(frame).toContain("DAINTREE");
    // ...with the scroll hint, and crucially NO torn round-border card.
    expect(frame).toContain("truncated");
    expect(frame).not.toContain("╭");
  });
});
