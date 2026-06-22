/**
 * Standalone e2e cockpit driver — boots the REAL OpenTUI split-footer renderer
 * (the same screenMode/externalOutputMode runApp uses) writing to the REAL
 * process.stdout, renders the live footer (ControlRoom + useScrollbackTranscript +
 * useFooterHeight), and drives a scripted streaming turn on a timer.
 *
 * It is meant to run under a PTY (see pty_run.py) so the native renderer emits the
 * actual ANSI escape stream a terminal would see — including the `\x1b[<n>S`
 * scrollUp the split-footer footer-resize emits. That stream is what the Python
 * runner scans to count flashes (a footer resize per streamed line = the bug) and
 * reconstructs with pyte to check nothing garbled. The OpenTUI `testRender` harness
 * can't see any of that (it reads the logical buffer, not the escape output), which
 * is exactly why the streaming flash went undetected before.
 *
 * Scenario is selectable via DAINTREE_E2E_SCENARIO:
 *   stream   (default) — a multi-line response streams in token-by-token + 2 tools
 *
 * Timing is driven by DAINTREE_E2E_STEP_MS (default 40ms) between steps; the program
 * exits cleanly (renderer.destroy) once the turn seals and a short settle elapses.
 */
import { createCliRenderer, type BoxRenderable } from "@opentui/core";
import { createRoot, useRenderer, useTerminalDimensions } from "@opentui/react";
import { useEffect, useRef, useState } from "react";
import { ControlRoom } from "../../../src/ui/ControlRoom.js";
import { useScrollbackTranscript } from "../../../src/ui/hooks/useScrollbackTranscript.js";
import { useFooterHeight } from "../../../src/ui/hooks/useFooterHeight.js";
import type {
  ActivityItem,
  DashboardState,
  TranscriptCell,
  TurnCell,
} from "../../../src/ui/types.js";

const STEP_MS = Number(process.env.DAINTREE_E2E_STEP_MS ?? 40);

const DASH: DashboardState = {
  mcp: { connected: true },
  workflowRuns: [],
  watchers: [],
  timers: [],
  inbox: [],
  audit: [],
};

// A representative multi-line response (the kind that grew the footer line-by-line).
const RESPONSE_LINES = [
  "This project is Daintree's local operations officer.",
  "It plans operations, spawns visible agent terminals, and supervises them.",
  "It is NOT a code editor — edits go through a spawned Daintree agent.",
  "Three model tiers route work: large, small, and medium.",
  "The cockpit renders on OpenTUI in split-footer mode.",
  "Finished turns commit to the host terminal's native scrollback.",
  "The live footer holds only the in-flight turn, status, and composer.",
  "Timers and watchers tick only while the assistant is open.",
];

function baseTurn(over: Partial<TurnCell> = {}): TurnCell {
  return {
    kind: "turn",
    id: "t1",
    userText: "what is this project?",
    assistantText: "",
    streaming: true,
    activities: [],
    notes: [],
    state: "active",
    phase: "analyzing",
    phaseStartedAt: 0,
    ts: 0,
    ...over,
  };
}

const TOOLS: ActivityItem[] = [
  { id: "a", name: "fs.list", label: "Listed", detail: ".", state: "done", startedAt: 0, endedAt: 7 },
  { id: "b", name: "fs.read", label: "Read", detail: "README.md", state: "done", startedAt: 0, endedAt: 12 },
];

function Cockpit({ onDone }: { onDone: () => void }) {
  const renderer = useRenderer();
  const { width: columns, height: rows } = useTerminalDimensions();
  const terminalRows =
    (renderer as unknown as { terminalHeight?: number }).terminalHeight ?? rows;
  const liveMaxRows = Math.max(4, Math.min(16, Math.floor(terminalRows * 0.4)));
  const [transcript, setTranscript] = useState<TranscriptCell[]>([]);
  const rootRef = useRef<BoxRenderable | null>(null);
  const { liveCells, commitSlot } = useScrollbackTranscript({
    renderer,
    transcript,
    header: null,
    width: 78,
  });
  useFooterHeight(renderer, rootRef, rows);

  // Instrument footerHeight: each CHANGE during streaming is one forced full footer
  // repaint = one visible flash. The runner counts these between PHASE stream and
  // PHASE seal. (Harness-only — never in production code.)
  useEffect(() => {
    let last = -1;
    const poll = setInterval(() => {
      const fh = (renderer as unknown as { footerHeight: number }).footerHeight;
      if (fh !== last) {
        last = fh;
        process.stderr.write(`FH ${fh}\n`);
      }
    }, 8);
    return () => clearInterval(poll);
  }, [renderer]);

  // Drive the scripted turn: stream lines one at a time, attach tools, then seal.
  useEffect(() => {
    let line = 0;
    let cancelled = false;
    const text: string[] = [];
    const advance = () => {
      if (cancelled) return;
      if (line < RESPONSE_LINES.length) {
        if (line === 0) process.stderr.write("PHASE stream\n");
        text.push(RESPONSE_LINES[line]!);
        line += 1;
        setTranscript([
          baseTurn({
            assistantText: text.join("\n"),
            phase: "generating",
            activities: line >= 4 ? TOOLS : [],
          }),
        ]);
        setTimeout(advance, STEP_MS);
        return;
      }
      // NOSEAL: hold the ACTIVE streaming turn on screen (don't finalize) so the runner
      // can snapshot the live footer mid-stream (fixed pane + activities + composer).
      if (process.env.DAINTREE_E2E_NOSEAL === "1") {
        process.stderr.write("PHASE seal\n");
        process.stderr.write("E2E_SEALED\n");
        if (process.env.DAINTREE_E2E_HOLD === "1") setTimeout(() => {}, 60_000);
        else onDone();
        return;
      }
      // Seal: the turn finalizes and commits to scrollback.
      setTranscript([
        baseTurn({
          assistantText: text.join("\n"),
          streaming: false,
          state: "complete",
          phase: "complete",
          activities: TOOLS,
        }),
      ]);
      // Signal the runner that the turn has sealed and the screen is at peak content.
      // In HOLD mode we do NOT tear the renderer down (teardown clears the screen and
      // ruins the pyte reconstruction) — we let the runner snapshot then kill us.
      setTimeout(() => {
        if (cancelled) return;
        process.stderr.write("PHASE seal\n");
        process.stderr.write("E2E_SEALED\n");
        if (process.env.DAINTREE_E2E_HOLD === "1") {
          setTimeout(() => {}, 60_000); // keep alive for the runner to snapshot
        } else {
          onDone();
        }
      }, 250);
    };
    const start = setTimeout(advance, STEP_MS);
    return () => {
      cancelled = true;
      clearTimeout(start);
    };
  }, [onDone]);

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

async function main() {
  const renderer = await createCliRenderer({
    screenMode: "split-footer",
    footerHeight: Math.max(1, process.stdout.rows ?? 24),
    externalOutputMode: "capture-stdout",
    exitOnCtrlC: false,
    useMouse: false,
    targetFps: 30,
    exitSignals: [],
  });
  const root = createRoot(renderer);
  let done = false;
  const finish = () => {
    if (done) return;
    done = true;
    try {
      root.unmount();
    } catch {
      /* already gone */
    }
    try {
      renderer.destroy();
    } catch {
      /* already destroyed */
    }
    // Give the terminal a beat to flush the restore sequences, then exit.
    setTimeout(() => process.exit(0), 60);
  };
  root.render(<Cockpit onDone={finish} />);
}

void main();
