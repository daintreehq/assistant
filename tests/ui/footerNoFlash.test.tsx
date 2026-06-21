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
 * The "no flash while streaming" regression. The flash the user saw is structural: in
 * split-footer mode OpenTUI does a FULL-SCREEN repaint whenever `forceFullRepaintRequested`
 * is true at render time (renderNative), and applyScreenMode sets that flag true on EVERY
 * footer resize. Our footer resizes as the turn streams (it grows to fit), so without the
 * fix every streamed token forced a full repaint — the flash. useFooterHeight now suppresses
 * the forced repaint on a GROW (it only needs it on a shrink, to clear vacated rows).
 *
 * We measure it directly: spy on `forceFullRepaintRequested` and count how many frames read
 * it as true (= a full repaint = a flash). Across many incremental streamed steps it must
 * stay tiny — NOT one-per-token.
 */

const tick = (ms = 8) => new Promise((r) => setTimeout(r, ms));

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

function streamingTurn(text: string): TurnCell {
  return {
    kind: "turn",
    id: "t1",
    userText: "what is this project?",
    assistantText: text,
    streaming: true,
    activities: [],
    notes: [],
    state: "active",
    phase: "generating",
    phaseStartedAt: 0,
    ts: 0,
  };
}

let drive: ((t: TranscriptCell[]) => void) | null = null;

function Cockpit() {
  const renderer = useRenderer();
  const { width: columns, height: rows } = useTerminalDimensions();
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
      composerFocus
      now={0}
    />
  );
}

describe("the live footer does not flash (force-repaint) on every streamed token", () => {
  test("incremental streaming forces a full repaint only a handful of times, not per token", async () => {
    const t = await testRender(<Cockpit />, OPTS);

    // Spy on the full-repaint flag: every frame that reads it `true` is a full repaint.
    const r = t.renderer as unknown as { forceFullRepaintRequested: boolean };
    let value = r.forceFullRepaintRequested ?? false;
    let fullRepaints = 0;
    Object.defineProperty(r, "forceFullRepaintRequested", {
      configurable: true,
      get() {
        if (value) fullRepaints += 1;
        return value;
      },
      set(v: boolean) {
        value = v;
      },
    });

    const drain = async () => {
      for (let i = 0; i < 6; i++) {
        await t.flush();
        await (
          t as unknown as { waitForVisualIdle?: () => Promise<void> }
        ).waitForVisualIdle?.();
        await tick();
      }
    };
    await drain();

    // Stream the response one short step at a time, the way real tokens arrive — each step
    // grows the footer by ~a line.
    const STEPS = 8;
    fullRepaints = 0; // count only the streaming phase
    let text = "";
    for (let i = 0; i < STEPS; i++) {
      text += `response line ${i + 1}\n`;
      await act(async () => drive!([streamingTurn(text)]));
      await drain();
    }

    const frame = t.captureCharFrame();
    // Sanity: it actually grew and rendered the tail + composer (not a frozen footer).
    expect(frame).toContain(`response line ${STEPS}`);
    expect(frame).toContain("commands");

    // The point: NOT a full repaint per streamed step. Allow a small constant for the
    // occasional genuine reflow, but it must be far below one-per-step.
    expect(fullRepaints).toBeLessThan(STEPS / 2);

    t.renderer.destroy?.();
  });
});
