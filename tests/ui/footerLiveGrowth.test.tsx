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
 * The live footer-growth regression. Unlike the golden ControlRoom tests, this wires the
 * REAL split-footer pipeline — useScrollbackTranscript + useFooterHeight + ControlRoom on
 * a split-footer renderer — and drives a streaming turn through it, so it exercises the
 * `useFooterHeight` measure/grow loop that the golden tests can't.
 *
 * It pins the bug where the footer froze at its idle height: useFooterHeight capped the
 * footer at `useTerminalDimensions().height`, which in split-footer mode IS the footer
 * height — so once it shrank to fit the idle composer it could never grow back, and a
 * running turn showed only its top few rows (no streamed response, one tool call, the
 * composer clipped off). The fix caps at the real `renderer.terminalHeight`.
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

describe("live footer grows to fit a streaming turn", () => {
  test("a tall in-flight turn shows the full response, every tool call, AND the composer", async () => {
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
    const idleFooter = fh();

    // A turn taller than the idle footer: 8 response lines + two tool calls.
    await act(async () =>
      drive!([
        activeTurn(
          Array.from({ length: 8 }, (_, i) => `response line ${i + 1}`).join("\n"),
          {
            activities: [
              { id: "a", name: "fs.list", label: "Listed", detail: ".", state: "done", startedAt: 0, endedAt: 7 },
              { id: "b", name: "fs.read", label: "Read", detail: "README.md", state: "active", startedAt: 0 },
            ],
          },
        ),
      ]),
    );
    await drain();
    const frame = t.captureCharFrame();

    // The footer grew past its idle height to fit the turn...
    expect(fh()).toBeGreaterThan(idleFooter);
    // ...and the whole turn is on screen: DAINTREE, the FULL response (last line, not
    // just the first), BOTH tool calls, and the composer.
    expect(frame).toContain("DAINTREE");
    expect(frame).toContain("response line 1");
    expect(frame).toContain("response line 8"); // not clipped to the top
    expect(frame).toContain("Listed");
    expect(frame).toContain("Read"); // the second tool call shows, not just the first
    expect(frame).toContain("commands"); // the composer is visible/usable

    t.renderer.destroy?.();
  });
});
