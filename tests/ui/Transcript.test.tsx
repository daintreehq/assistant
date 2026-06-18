import { render } from "ink-testing-library";
import { Transcript } from "../../src/ui/components/Transcript.js";
import type { TranscriptCell } from "../../src/ui/types.js";

const FIXED = 1_700_000_000_000;

function stripAnsi(s: string): string {
  return s.replace(/\x1b\[[0-9;]*m/g, "");
}

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

  it("renders the run as USER/DAINTREE markers and a branch tree of verbs", () => {
    const frame =
      render(
        <Transcript cells={activeRun()} height={20} width={72} now={FIXED} />,
      ).lastFrame() ?? "";
    expect(frame).toContain("USER");
    expect(frame).toContain("DAINTREE");
    expect(frame).not.toContain("assistant"); // no role label
    // Human verbs, not raw fn() syntax or JSON.
    expect(frame).toContain("Inspected");
    expect(frame).toContain("Watching");
    expect(frame).not.toContain("watcher.terminal.create(");
    expect(frame).not.toContain('"query"');
    expect(stripAnsi(frame)).toMatch(
      /USER\n│ Fix the watcher tests\.\n\n\n◆ DAINTREE/,
    );
    // Branch grammar + a settled duration.
    expect(frame).toMatch(/[├╰]/);
    expect(frame).toContain("180ms");
  });

  it("reveals raw args/result only in expanded detail mode", () => {
    const frame =
      render(
        <Transcript cells={activeRun()} height={30} width={72} now={FIXED} expanded />,
      ).lastFrame() ?? "";
    expect(frame).toContain("args");
    expect(frame).toContain('"query"');
    expect(frame).toContain("result");
  });

  it("renders the whole stream without a windowed viewport", () => {
    // The host terminal owns scrollback now, so nothing is clipped to a fixed
    // height — the full active turn (top to bottom) is present in one frame.
    const frame =
      render(<Transcript cells={activeRun()} width={72} now={FIXED} />)
        .lastFrame() ?? "";
    expect(frame).toContain("Fix the watcher tests");
    expect(frame).toContain("Inspected");
    expect(frame).toContain("Watching tests running");
  });

  it("clips the live region to liveHeight (tail), keeping the latest lines", () => {
    // A long in-flight turn must not let the repainting frame overflow the
    // viewport (Ink would wipe scrollback). Only the tail renders inline; the
    // full turn lands in scrollback once it commits.
    const long: TranscriptCell = {
      kind: "turn",
      id: "tlong",
      userText: "kick off",
      assistantText: Array.from({ length: 40 }, (_, i) => `LINE${i + 1}`).join("\n"),
      streaming: true,
      state: "active",
      ts: FIXED,
      notes: [],
      activities: [],
    };
    const frame =
      render(
        <Transcript cells={[long]} width={72} now={FIXED} liveHeight={5} />,
      ).lastFrame() ?? "";
    expect(frame).toContain("LINE40"); // newest is visible
    expect(frame).not.toContain("LINE01"); // oldest is clipped out of the live tail
    expect(frame).not.toContain("USER"); // header scrolled past too
  });

  it("commits finalized turns above the live tail", () => {
    // A completed turn is committed (rendered once) and a trailing active turn
    // is the live tail; both are present, completed first.
    const cells = activeRun();
    const completed: TranscriptCell = {
      ...(cells[0] as TranscriptCell & { kind: "turn" }),
      id: "t0",
      state: "complete",
      userText: "Earlier question",
      assistantText: "Earlier answer",
      activities: [],
    };
    const frame =
      render(
        <Transcript cells={[completed, ...cells]} width={72} now={FIXED} />,
      ).lastFrame() ?? "";
    expect(frame).toContain("Earlier question");
    expect(frame.indexOf("Earlier question")).toBeLessThan(
      frame.indexOf("Fix the watcher tests"),
    );
  });
});
