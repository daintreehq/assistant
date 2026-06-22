import { test, expect, describe } from "bun:test";
import { act, useRef, useState } from "react";
import { useRenderer, useTerminalDimensions } from "@opentui/react";
import { testRender } from "@opentui/react/test-utils";
import type { BoxRenderable } from "@opentui/core";
import { useScrollbackTranscript } from "../../src/ui/hooks/useScrollbackTranscript.js";
import { useFooterHeight } from "../../src/ui/hooks/useFooterHeight.js";
import { ControlRoom } from "../../src/ui/ControlRoom.js";
import type {
  DashboardState,
  TranscriptCell,
  TurnCell,
} from "../../src/ui/types.js";

/**
 * The live footer-STABILITY regression. Unlike the golden ControlRoom tests, this wires the
 * REAL split-footer pipeline — useScrollbackTranscript + useFooterHeight + ControlRoom on a
 * split-footer renderer — and streams a turn through it, so it exercises the
 * `useFooterHeight` measure loop the golden tests can't.
 *
 * It pins the streaming-flash fix: an active turn renders inside a FIXED-height pane
 * (liveMaxRows, see TurnCellView), so once the footer reaches its busy height it stays
 * CONSTANT as more tokens stream — no per-token resize, which is what forced a full
 * split-footer repaint (the flash). The turn shows its TAIL (most recent lines) plus every
 * tool call and the composer; the full styled turn lands in scrollback on seal.
 */

const tick = (ms = 12) => new Promise((r) => setTimeout(r, ms));

const OPTS = {
  width: 80,
  height: 24,
  screenMode: "split-footer",
  externalOutputMode: "capture-stdout",
  footerHeight: 24,
} as const;

const DASH: DashboardState = {
  mcp: { connected: true },
  workflowRuns: [],
  watchers: [],
  timers: [],
  inbox: [],
  audit: [],
};

function activeTurn(text: string, over: Partial<TurnCell> = {}): TurnCell {
  return {
    kind: "turn",
    id: "t1",
    userText: "what is this project?",
    assistantText: text,
    streaming: true,
    activities: [],
    notes: [],
    state: "active",
    phase: text ? "generating" : "analyzing",
    phaseStartedAt: 0,
    ts: 0,
    ...over,
  };
}

let drive: ((t: TranscriptCell[]) => void) | null = null;

function Cockpit() {
  const renderer = useRenderer();
  const { width: columns, height: rows } = useTerminalDimensions();
  const terminalRows =
    (renderer as unknown as { terminalHeight?: number }).terminalHeight ?? rows;
  const liveMaxRows = Math.max(4, Math.min(16, Math.floor(terminalRows * 0.4)));
  const [transcript, setT] = useState<TranscriptCell[]>([]);
  drive = setT;
  const rootRef = useRef<BoxRenderable | null>(null);
  const { liveCells, commitSlot } = useScrollbackTranscript({
    renderer,
    transcript,
    header: null,
    width: 78,
  });
  useFooterHeight(renderer, rootRef, rows);
  return (
    <ControlRoom
      project="help"
      tier="operator"
      columns={columns}
      rows={rows}
      connected
      transcript={liveCells}
      dashboard={DASH}
      busy
      stage="Generating"
      view="home"
      renderHeader={false}
      footerSlot={commitSlot}
      rootRef={rootRef}
      liveMaxRows={liveMaxRows}
      composerFocus
      now={0}
    />
  );
}

describe("live footer stays fixed while a turn streams", () => {
  test("the footer holds a constant height across streamed steps and shows the tail + tools + composer", async () => {
    const t = await testRender(<Cockpit />, OPTS);
    const fh = () =>
      (t.renderer as unknown as { footerHeight: number }).footerHeight;
    // Drain queued frames so the (intentionally lagged) per-frame measurement converges.
    const drain = async () => {
      for (let i = 0; i < 8; i++) {
        await t.flush();
        await (
          t as unknown as { waitForVisualIdle?: () => Promise<void> }
        ).waitForVisualIdle?.();
        await tick();
      }
    };
    await drain();

    const tools = [
      { id: "a", name: "fs.list", label: "Listed", detail: ".", state: "done" as const, startedAt: 0, endedAt: 7 },
      { id: "b", name: "fs.read", label: "Read", detail: "README.md", state: "active" as const, startedAt: 0 },
    ];

    // Stream the response one line at a time; record the footer height after each step.
    const heights: number[] = [];
    for (let n = 1; n <= 8; n++) {
      const text = Array.from({ length: n }, (_, i) => `response line ${i + 1}`).join("\n");
      await act(async () => drive!([activeTurn(text, { activities: n >= 3 ? tools : [] })]));
      await drain();
      heights.push(fh());
    }

    // Once it reaches its busy height the footer NEVER resizes again as more streams in —
    // that invariance is the flash fix (a resize forces a full split-footer repaint). The
    // tail (last few steps) must be perfectly flat.
    const tail = heights.slice(3);
    expect(new Set(tail).size).toBe(1);

    const frame = t.captureCharFrame();
    // The latest line, both tool calls, and the composer are all on screen (tail view).
    expect(frame).toContain("DAINTREE");
    expect(frame).toContain("response line 8"); // most recent line is visible
    expect(frame).toContain("Listed");
    expect(frame).toContain("Read"); // the second tool call shows, not just the first
    expect(frame).toContain("commands"); // the composer is visible/usable

    t.renderer.destroy?.();
  });
});
