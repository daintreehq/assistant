/**
 * A deterministic visual-development harness. The sidebar (55–65 cols) is the
 * canonical surface, so the width cycle is biased there and defaults to 58. Cycle
 * widths with `w`, heights with `h`; toggle the operations view with `o` and raw
 * tool detail with `x`. Nothing here touches the model, scheduler, or MCP.
 *
 *   number keys = fixture state   w width   h height   o ops   x detail   q quit
 */
import { useState } from "react";
import { Box, Text, useApp, useInput } from "ink";
import { ControlRoom, type View } from "../ControlRoom.js";
import { buildFixtures, FIXED_NOW } from "./fixtures.js";

// Biased to the real host width — the sidebar is the design target, not an
// equal third of the range. 80/120 stay as wider-layout regression checks.
const WIDTHS = [55, 58, 62, 65, 80, 120];
// Daintree sidebars are "quite a few characters high" — test realistic heights.
const ROWS = [24, 32, 40, 48];

export function UiGallery() {
  const { exit } = useApp();
  const fixtures = buildFixtures();
  const [fi, setFi] = useState(1);
  const [wi, setWi] = useState(1); // default 58
  const [ri, setRi] = useState(1); // default 32
  const [view, setView] = useState<View>("home");
  const [expanded, setExpanded] = useState(false);

  useInput((input) => {
    if (input === "q") return exit();
    if (input === "w") return setWi((w) => (w + 1) % WIDTHS.length);
    if (input === "h") return setRi((r) => (r + 1) % ROWS.length);
    if (input === "o") return setView((v) => (v === "operations" ? "home" : "operations"));
    if (input === "x") return setExpanded((e) => !e);
    const n = Number(input);
    if (n >= 1 && n <= fixtures.length) setFi(n - 1);
  });

  const f = fixtures[fi];
  const columns = WIDTHS[wi];
  const rows = ROWS[ri];
  const effectiveView = view === "operations" ? "operations" : f.view;

  return (
    <Box flexDirection="column">
      <Box marginBottom={1}>
        <Text dimColor>
          gallery · {f.label} ({fi + 1}/{fixtures.length}) · {columns}×{rows} ·{" "}
          keys: 1-{fixtures.length} state · w width · h height · o ops · x detail · q quit
        </Text>
      </Box>
      <Box
        width={columns}
        height={rows}
        borderStyle="round"
        borderColor="gray"
      >
        <ControlRoom
          project="assistant"
          tier="operator"
          columns={columns}
          rows={rows}
          connected={f.connected}
          transcript={f.transcript}
          dashboard={f.dashboard}
          sessionUsage={f.sessionUsage}
          previews={f.previews}
          busy={f.busy}
          stage={f.stage}
          view={effectiveView}
          expanded={expanded}
          pending={f.pending}
          now={FIXED_NOW}
          composerFocus={false}
        />
      </Box>
    </Box>
  );
}
