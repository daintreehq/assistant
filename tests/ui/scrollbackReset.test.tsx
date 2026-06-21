import { test, expect, describe } from "bun:test";
import { act, useState } from "react";
import { useRenderer } from "@opentui/react";
import { testRender } from "@opentui/react/test-utils";
import {
  useScrollbackTranscript,
  type ScrollbackHeader,
} from "../../src/ui/hooks/useScrollbackTranscript.js";
import type { TranscriptCell } from "../../src/ui/types.js";

const tick = (ms = 30) => new Promise((r) => setTimeout(r, ms));

// Scrollback commits require split-footer + capture-stdout (the APIs throw otherwise).
const OPTS = {
  width: 80,
  height: 24,
  screenMode: "split-footer",
  externalOutputMode: "capture-stdout",
  footerHeight: 24,
} as const;

function sealedTurn(id: string, assistant: string): TranscriptCell {
  return {
    kind: "turn",
    id,
    userText: "",
    assistantText: assistant,
    streaming: false,
    activities: [],
    notes: [],
    state: "complete",
    ts: 0,
  };
}

let drive: ((s: { transcript: TranscriptCell[]; resetKey: number }) => void) | null =
  null;

function Harness({
  initial,
}: {
  initial: { transcript: TranscriptCell[]; resetKey: number };
}) {
  const renderer = useRenderer();
  const [s, setS] = useState(initial);
  drive = setS;
  const header: ScrollbackHeader = {
    node: <text>MASTHEAD_MARK</text>,
    fallbackText: "MASTHEAD_MARK",
  };
  const { liveCells, commitSlot } = useScrollbackTranscript({
    renderer,
    transcript: s.transcript,
    header,
    width: 70,
    resetKey: s.resetKey,
  });
  return (
    <box>
      {commitSlot}
      <text>live={liveCells.length}</text>
    </box>
  );
}

async function drain(t: {
  flush: () => Promise<void>;
  externalOutput: { takeText: () => string };
}): Promise<string> {
  let acc = "";
  for (let i = 0; i < 8; i++) {
    await t.flush();
    await tick();
    acc += t.externalOutput.takeText();
  }
  return acc;
}

describe("useScrollbackTranscript /clear reset", () => {
  // Regression for the equal-length clear: `/clear` replaces the transcript and drops
  // a fresh confirmation card, so the new length can equal the old committed count. A
  // length-only reset would treat the new card as already committed (lost) and skip
  // re-committing the masthead; the explicit resetKey must fix both.
  test("re-commits masthead + the new card when the transcript is replaced at equal length", async () => {
    const t = await testRender(
      <Harness initial={{ transcript: [sealedTurn("a", "first-answer")], resetKey: 0 }} />,
      OPTS,
    );
    const before = await drain(t);
    expect(before).toContain("MASTHEAD_MARK");
    expect(before).toContain("first-answer");

    // /clear: replace the single committed cell with a NEW one (same length 1) and bump
    // the reset key — exactly the controller's transcript:clear + command:add + nonce.
    await act(async () => {
      drive!({ transcript: [sealedTurn("b", "cleared-card")], resetKey: 1 });
    });
    const after = await drain(t);
    expect(after).toContain("MASTHEAD_MARK"); // masthead re-committed on top
    expect(after).toContain("cleared-card"); // the new card committed, not lost

    t.renderer.destroy?.();
  });
});
