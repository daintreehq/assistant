/**
 * A deterministic visual-development harness. The cockpit is an INLINE surface
 * now, so the gallery renders it the way the real app does — completed turns
 * commit to native scrollback (via <Static>), the live region sits at the bottom.
 * Cycle widths with `w`; toggle the operations view with `o` and raw tool detail
 * with `x`; number keys pick a fixture. Switching fixtures remounts the cockpit
 * (a fresh `key`) so committed output starts clean instead of piling up. Nothing
 * here touches the model, scheduler, or MCP.
 *
 *   number keys = fixture state   w width   o ops   x detail   q quit
 */
import { useState } from "react";
import { Box, Text, useApp, useInput } from "ink";
import { ControlRoom, type View } from "../ControlRoom.js";
import { buildFixtures, FIXED_NOW } from "./fixtures.js";

// Biased to the real host width — a host side panel is the common surface. 80/120
// stay as wider-terminal checks.
const WIDTHS = [55, 58, 62, 65, 80, 120];

export function UiGallery() {
  const { exit } = useApp();
  const fixtures = buildFixtures();
  const [fi, setFi] = useState(1);
  const [wi, setWi] = useState(1); // default 58
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
          gallery · {f.label} ({fi + 1}/{fixtures.length}) · {columns} cols ·{" "}
          keys: 1-{fixtures.length} state · w width · o ops · x detail · q quit
        </Text>
      </Box>
      <ControlRoom
        key={`${fi}:${columns}:${effectiveView}`}
        project="assistant"
        tier="operator"
        columns={columns}
        connected={f.connected}
        transcript={f.transcript}
        dashboard={f.dashboard}
        sessionUsage={f.sessionUsage}
        previews={f.previews}
        busy={f.busy}
        stage={f.stage}
        queueDepth={f.queueDepth}
        view={effectiveView}
        expanded={expanded}
        pending={f.pending}
        now={FIXED_NOW}
        composerFocus={false}
      />
    </Box>
  );
}
