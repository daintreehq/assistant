import { render } from "ink-testing-library";
import { ControlRoom, type View } from "../../src/ui/ControlRoom.js";
import type { PanelKey } from "../../src/cli/commandData.js";
import { buildFixtures, FIXED_NOW } from "../../src/ui/dev/fixtures.js";

const fixtures = buildFixtures();
const byKey = (label: string) => fixtures.find((f) => f.label === label)!;

function frameFor(
  label: string,
  columns: number,
  rows = 32,
  over: { view?: View; activePanel?: PanelKey | null } = {},
): string {
  const f = byKey(label);
  const { lastFrame } = render(
    <ControlRoom
      project="assistant"
      tier="operator"
      columns={columns}
      rows={rows}
      connected={f.connected}
      transcript={f.transcript}
      dashboard={f.dashboard}
      previews={f.previews}
      busy={f.busy}
      stage={f.stage}
      queueDepth={f.queueDepth}
      view={over.view ?? f.view}
      activePanel={over.activePanel}
      pending={f.pending}
      now={FIXED_NOW}
      composerFocus={false}
    />,
  );
  return lastFrame() ?? "";
}

// The cockpit is one inline column at every width now (the old sidebar/standard/
// wide banding is gone). 58 is a typical host side panel; 120 is a wide terminal.
const WIDTHS = [58, 80, 120];

/** Strip SGR color codes so we measure the VISIBLE width of a rendered row. */
const ANSI = /\[[0-9;]*m/g;
const visibleWidth = (line: string): number => line.replace(ANSI, "").length;

/**
 * Width below every live value in the oscillation. We commit the `<Static>` region
 * (the one-time header, plus any already-finished turns) at THIS narrow width on
 * the first render — modelling "the history scrolled past while the pane was
 * narrow". Ink's `<Static>` emits each item exactly once and never repaints it, so
 * those rows are EXEMPT from the orphan bug (they print once and flow into native
 * scrollback). Seeding them narrow keeps them out of the width assertion, which
 * then measures only the repainting region — the part that actually re-renders and
 * can orphan.
 */
const STATIC_SEED_COLUMNS = 40;

const renderControlRoom = (
  label: string,
  columns: number,
  over: { view?: View; activePanel?: PanelKey | null },
) => {
  const f = byKey(label);
  return (
    <ControlRoom
      project="assistant"
      tier="system"
      columns={columns}
      connected={f.connected}
      transcript={f.transcript}
      dashboard={f.dashboard}
      sessionUsage={f.sessionUsage}
      previews={f.previews}
      busy={f.busy}
      stage={f.stage}
      queueDepth={f.queueDepth}
      view={over.view ?? f.view}
      activePanel={over.activePanel}
      pending={f.pending}
      now={FIXED_NOW}
      composerFocus={false}
    />
  );
};

/**
 * Render a fixture with the LIVE terminal width (`stdout.columns`) decoupled from
 * the `columns` PROP — the exact #138 lag. Ink's yoga layout sizes `width="100%"`
 * children against the live terminal on every relayout, but the `columns` prop we
 * thread into the tree comes from `useWindowSize()`, which updates a render tick
 * later. So during a pane show/hide the prop trails the real width, and any box in
 * the repainting region sized from that prop can momentarily be WIDER than the live
 * terminal — its row wraps and orphans a stale copy into scrollback. We reproduce
 * the trailing prop by overriding the headless stdout's hardcoded `columns` getter,
 * then assert the repainting region never out-runs the live width.
 *
 * (The literal duplicated-row symptom is a terminal-scrollback artifact of Ink's
 * cursor-up erase math and is not reproducible in the headless debug renderer; the
 * reproducible ROOT CAUSE is the over-wide line, which is what we guard here.)
 */
function liveFrame(
  label: string,
  propColumns: number,
  liveColumns: number,
  over: { view?: View; activePanel?: PanelKey | null } = {},
): string {
  // First render at the narrow seed so the <Static> region commits (once) at a
  // width that fits every live value below — keeping the exempt header/history out
  // of the assertion.
  const { rerender, stdout, lastFrame } = render(
    renderControlRoom(label, STATIC_SEED_COLUMNS, over),
  );
  // Pin the headless stdout (hardcoded columns=100) to the live width and relayout
  // with the prop now LAGGING ahead of it — the repainting region re-renders, the
  // already-committed <Static> rows do not.
  Object.defineProperty(stdout, "columns", {
    value: liveColumns,
    configurable: true,
  });
  rerender(renderControlRoom(label, propColumns, over));
  return lastFrame() ?? "";
}

// A pane animation oscillates the live width; the prop trails by one step. The
// shrink half (prop > live) is the dangerous direction — a prop-sized box exceeds
// the just-shrunk terminal. We cover both halves; the grow half (prop < live) is
// trivially safe but asserted anyway as a guard against the inverse regression.
const OSCILLATION: Array<{ prop: number; live: number }> = [
  { prop: 80, live: 76 }, // just shrank, prop still 80
  { prop: 76, live: 72 }, // shrank again
  { prop: 72, live: 76 }, // grew back, prop still 72
  { prop: 76, live: 80 }, // grew again
  { prop: 80, live: 58 }, // a hard jump to a narrow pane, prop way ahead
];

describe("ControlRoom resize oscillation — no row out-runs the live width (#138)", () => {
  for (const fixture of ["idle", "active", "approval"] as const) {
    it.each(OSCILLATION)(
      `${fixture}: every live row fits the terminal when prop lags (prop=$prop, live=$live)`,
      ({ prop, live }) => {
        const frame = liveFrame(fixture, prop, live);
        const overflowing = frame
          .split("\n")
          .filter((line) => visibleWidth(line) > live);
        expect(overflowing).toEqual([]);
      },
    );
  }

  it.each(OSCILLATION)(
    "help overlay fits the terminal when prop lags (prop=$prop, live=$live)",
    ({ prop, live }) => {
      const frame = liveFrame("idle", prop, live, { view: "help" });
      const overflowing = frame
        .split("\n")
        .filter((line) => visibleWidth(line) > live);
      expect(overflowing).toEqual([]);
    },
  );

  it("the status line stays a single row across the whole oscillation", () => {
    // Regression for the stacked-status-line symptom: whatever the prop/live skew,
    // the idle status content ("Standing by") renders exactly once per frame — a
    // duplicated row would mean a wrapped overflow leaked a second copy.
    for (const { prop, live } of OSCILLATION) {
      const frame = liveFrame("idle", prop, live);
      const count = frame.split("\n").filter((l) => l.includes("Standing by"))
        .length;
      expect(count).toBe(1);
    }
  });
});

describe("ControlRoom inline cockpit (golden frames)", () => {
  it.each(WIDTHS)("prints the masthead + composer, idle reads as standing by (%i cols)", (w) => {
    const frame = frameFor("idle", w);
    expect(frame).toContain("Daintree Assistant"); // one-time header banner (scrolls away)
    expect(frame).toContain("MCP"); // connection lives in the status line
    expect(frame).toContain("Standing by"); // what is it doing?
    expect(frame).toContain("commands"); // composer hint: / opens the palette
    expect(frame).toContain("ops"); // composer hint: ^O inspects operations
  });

  it.each(WIDTHS)("an active run renders the turn and the supervised agent (%i cols)", (w) => {
    const frame = frameFor("active", w);
    expect(frame).toContain("Daintree Assistant");
    expect(frame).toContain("YOU"); // the human turn (live tail)
    expect(frame).toContain("DAINTREE"); // the assistant turn
    expect(frame).toContain("term_8"); // the active agent surfaced in the status line
  });

  it.each(WIDTHS)("attention is surfaced in the status line (%i cols)", (w) => {
    const frame = frameFor("attention", w);
    // The most-urgent agent (needs input) is promoted into the one-line status,
    // and the attention chip carries the unresolved count — the failure detail
    // itself is reachable in the on-demand operations view.
    expect(frame).toContain("NEEDS INPUT");
    expect(frame).toContain("term_12");
    expect(frame).toContain("!2"); // unresolved-attention count chip
  });

  it.each(WIDTHS)("approval shows a risk-specific sheet above the composer (%i cols)", (w) => {
    const frame = frameFor("approval", w);
    expect(frame).toContain("Push branch to origin?");
    expect(frame).toContain("approve");
  });

  it.each(WIDTHS)("degraded is unmistakable (%i cols)", (w) => {
    expect(frameFor("degraded", w)).toContain("DEGRADED");
  });

  it("threads the queued follow-up count into the composer (#95)", () => {
    // The "active" fixture queues two follow-ups; the count must reach the
    // Composer busy indicator. Guards against a dropped queueDepth prop.
    expect(frameFor("active", 120, 32)).toContain("2 queued");
    // The idle fixture has nothing queued — no hint.
    expect(frameFor("idle", 120, 32)).not.toContain("queued");
  });
});

describe("ControlRoom turn rendering", () => {
  it.each(WIDTHS)("renders the human turn as a distinct card, no box (%i cols)", (w) => {
    const frame = frameFor("active", w);
    expect(frame).toContain("YOU"); // quiet who-said-what label
    expect(frame).toContain("▏"); // the human's left accent bar marks the turn
    expect(frame).not.toMatch(/[╭┌].*[╮┐]/); // no box — the redesign drops it
    expect(frame).toContain("DAINTREE");
  });

  it("keeps user and Daintree distinguishable without color", () => {
    const prev = process.env.DAINTREE_THEME;
    process.env.DAINTREE_THEME = "none";
    try {
      const frame = frameFor("active", 58, 40);
      expect(frame).toContain("YOU"); // label + bar glyph both survive color strip
      expect(frame).toContain("▏");
      expect(frame).toContain("◆ DAINTREE");
    } finally {
      if (prev === undefined) delete process.env.DAINTREE_THEME;
      else process.env.DAINTREE_THEME = prev;
    }
  });
});

describe("ControlRoom operations view (on-demand, replaces the composer)", () => {
  it("shows the full ops deck and a return hint when view=operations", () => {
    const frame = frameFor("active", 80, 32, { view: "operations" });
    // The active fixture has watchers (AGENTS), timers (SCHEDULED) and audit (RECENT).
    expect(frame).toContain("NOW");
    expect(frame).toContain("SCHEDULED");
    expect(frame).toContain("Esc to return");
  });

  it("replaces the composer with the panel but keeps the run + status line visible", () => {
    const frame = frameFor("active", 80, 32, { view: "operations" });
    // Only the composer swaps out for the panel — the in-flight turn and the
    // status line still repaint at the bottom of the stream.
    expect(frame).not.toContain("Ask Daintree to supervise"); // composer placeholder is gone
    expect(frame).toContain("DAINTREE"); // the active turn is still shown
    expect(frame).toContain("term_8"); // status line still surfaces the agent
  });

  it("a pending confirmation overrides an open panel (approval is never hidden)", () => {
    // The approval fixture has a pending confirm; even asked to open operations,
    // the sheet must surface and the composer (not the deck) stays present.
    const frame = frameFor("approval", 80, 32, { view: "operations" });
    expect(frame).toContain("Push branch to origin?");
    expect(frame).not.toContain("Esc to return"); // the ops panel yielded
  });

  it("focuses one section when a panel command set activePanel", () => {
    const frame = frameFor("active", 80, 32, {
      view: "operations",
      activePanel: "timers",
    });
    expect(frame).toContain("SCHEDULED");
    // The deck is filtered to the requested panel — other sections are absent.
    expect(frame).not.toContain("AGENTS");
    expect(frame).not.toContain("RECENT");
    expect(frame).not.toContain("NOW");
  });
});
