/**
 * A deterministic visual-development harness. Switch fixture states with number
 * keys and simulated widths with `w`; toggle the operations view with `o` and
 * raw tool detail with `x`. Nothing here touches the model, scheduler, or MCP.
 *
 *   1 idle   2 active   3 attention   4 approval   5 degraded
 *   w cycle width (52 / 80 / 120)   o operations   x detail   q quit
 */
import { useState } from "react";
import { Box, Text, useApp, useInput } from "ink";
import { ControlRoom, type View } from "../ControlRoom.js";
import { buildFixtures, FIXED_NOW } from "./fixtures.js";

const WIDTHS = [52, 80, 120];
const ROWS = 24;

export function UiGallery() {
  const { exit } = useApp();
  const fixtures = buildFixtures();
  const [fi, setFi] = useState(1);
  const [wi, setWi] = useState(1);
  const [view, setView] = useState<View>("home");
  const [expanded, setExpanded] = useState(false);

  useInput((input) => {
    if (input === "q") return exit();
    if (input === "w") return setWi((w) => (w + 1) % WIDTHS.length);
    if (input === "o") return setView((v) => (v === "operations" ? "home" : "operations"));
    if (input === "x") return setExpanded((e) => !e);
    const n = Number(input);
    if (n >= 1 && n <= fixtures.length) setFi(n - 1);
  });

  const f = fixtures[fi];
  const columns = WIDTHS[wi];
  const effectiveView = view === "operations" ? "operations" : f.view;

  return (
    <Box flexDirection="column">
      <Box marginBottom={1}>
        <Text dimColor>
          gallery · {f.label} ({fi + 1}/{fixtures.length}) · {columns}×{ROWS} ·{" "}
          keys: 1-5 state · w width · o ops · x detail · q quit
        </Text>
      </Box>
      <Box
        width={columns}
        height={ROWS}
        borderStyle="round"
        borderColor="gray"
      >
        <ControlRoom
          project="assistant-main"
          tier="operator"
          columns={columns}
          rows={ROWS}
          connected={f.connected}
          transcript={f.transcript}
          dashboard={f.dashboard}
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
