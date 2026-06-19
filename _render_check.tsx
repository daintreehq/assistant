import { render } from "ink-testing-library";
import React from "react";
import { ControlRoom } from "./src/ui/ControlRoom.js";
import { buildFixtures, FIXED_NOW } from "./src/ui/dev/fixtures.js";
const f = buildFixtures().find((x) => x.label === "active")!;
const { lastFrame } = render(React.createElement(ControlRoom, {
  project: "assistant", tier: "system", columns: 72, rows: 32,
  connected: f.connected, transcript: f.transcript, dashboard: f.dashboard,
  previews: f.previews, busy: f.busy, stage: f.stage, queueDepth: f.queueDepth,
  view: f.view, activePanel: null, pending: f.pending, now: FIXED_NOW, composerFocus: false,
} as any));
const out = (lastFrame() ?? "").split("\n").map((l) => {
  const p = l.replace(/\x1b\[[0-9;]*m/g, "");
  return `|${p}| (${[...p].length})`;
}).join("\n");
console.log(out);
