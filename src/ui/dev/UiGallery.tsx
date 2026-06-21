/**
 * A deterministic visual-development harness. The cockpit is an INLINE main-screen
 * surface now (OpenTUI, Claude Code model), so the gallery renders it the way the
 * real app does — the whole tree renders inline; the live region sits at the bottom.
 * Cycle widths with `w`; toggle the operations view with `o` and raw tool detail
 * with `x`; number keys pick a fixture. Switching fixtures remounts the cockpit
 * (a fresh `key`) so output starts clean. Nothing here touches the model, scheduler,
 * or MCP.
 *
 *   number keys = fixture state   w width   o ops   x detail   q quit
 */
import { useState } from "react";
import { TextAttributes } from "@opentui/core";
import { useKeyboard } from "@opentui/react";
import { ControlRoom, type View } from "../ControlRoom.js";
import { buildFixtures, FIXED_NOW } from "./fixtures.js";

// Biased to the real host width — a host side panel is the common surface. 80/120
// stay as wider-terminal checks.
const WIDTHS = [55, 58, 62, 65, 80, 120];

export function UiGallery({ exit }: { exit?: () => void }) {
  const quit = exit ?? (() => process.exit(0));
  const fixtures = buildFixtures();
  const [fi, setFi] = useState(1);
  const [wi, setWi] = useState(1); // default 58
  const [view, setView] = useState<View>("home");
  const [expanded, setExpanded] = useState(false);

  useKeyboard((e) => {
    const input = e.name ?? "";
    if (input === "q") return quit();
    if (input === "w") return setWi((w) => (w + 1) % WIDTHS.length);
    if (input === "o") return setView((v) => (v === "operations" ? "home" : "operations"));
    if (input === "x") return setExpanded((x) => !x);
    const n = Number(input);
    if (n >= 1 && n <= fixtures.length) setFi(n - 1);
  });

  const f = fixtures[fi];
  const columns = WIDTHS[wi];
  const effectiveView = view === "operations" ? "operations" : f.view;

  return (
    <box flexDirection="column">
      <box marginBottom={1}>
        <text attributes={TextAttributes.DIM}>
          gallery · {f.label} ({fi + 1}/{fixtures.length}) · {columns} cols ·{" "}
          keys: 1-{fixtures.length} state · w width · o ops · x detail · q quit
        </text>
      </box>
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
    </box>
  );
}
