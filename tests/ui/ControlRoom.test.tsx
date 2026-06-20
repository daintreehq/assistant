import chalk from "chalk";
import { render } from "ink-testing-library";
import { ControlRoom, type View } from "../../src/ui/ControlRoom.js";
import type { PanelKey } from "../../src/cli/commandData.js";
import { buildFixtures, FIXED_NOW } from "../../src/ui/dev/fixtures.js";
import { LIVE_CHROME_MAX_WIDTH } from "../../src/ui/liveChrome.js";
import type { TranscriptCell } from "../../src/ui/types.js";

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
 * (any already-finished turns) at THIS narrow width on
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
  over: {
    view?: View;
    activePanel?: PanelKey | null;
    expanded?: boolean;
    transcript?: TranscriptCell[];
    staticKey?: number;
  },
) => {
  const f = byKey(label);
  return (
    <ControlRoom
      project="assistant"
      tier="system"
      staticKey={over.staticKey ?? 0}
      columns={columns}
      connected={f.connected}
      transcript={over.transcript ?? f.transcript}
      dashboard={f.dashboard}
      sessionUsage={f.sessionUsage}
      previews={f.previews}
      busy={f.busy}
      stage={f.stage}
      queueDepth={f.queueDepth}
      view={over.view ?? f.view}
      activePanel={over.activePanel}
      expanded={over.expanded ?? false}
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
  over: {
    view?: View;
    activePanel?: PanelKey | null;
    expanded?: boolean;
    transcript?: TranscriptCell[];
  } = {},
): string {
  // First render at the narrow seed so the <Static> region commits (once) at a
  // width that fits every live value below — keeping exempt history out of the
  // assertion.
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
          .filter((line) => visibleWidth(line) >= live);
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
        .filter((line) => visibleWidth(line) >= live);
      expect(overflowing).toEqual([]);
    },
  );

  it.each(OSCILLATION)(
    "operations deck fits the terminal when prop lags (prop=$prop, live=$live)",
    ({ prop, live }) => {
      // The "active" fixture carries watchers/timers/audit, so every operations
      // section (Now/Attention/Agents/Scheduled/Recent) renders and is measured.
      const frame = liveFrame("active", prop, live, { view: "operations" });
      const overflowing = frame
        .split("\n")
        .filter((line) => visibleWidth(line) >= live);
      expect(overflowing).toEqual([]);
    },
  );

  it.each(OSCILLATION)(
    "expanded activity tree fits the terminal when prop lags (prop=$prop, live=$live)",
    ({ prop, live }) => {
      // ^X expands raw args/result rows in the live turn — the widest live content.
      const frame = liveFrame("active", prop, live, { expanded: true });
      const overflowing = frame
        .split("\n")
        .filter((line) => visibleWidth(line) >= live);
      expect(overflowing).toEqual([]);
    },
  );

  it("the status line stays a single row across the whole oscillation", () => {
    // Regression for the stacked-status-line symptom: whatever the prop/live skew,
    // the idle status content (the context gauge "CTX 8%") renders exactly once per
    // frame — a duplicated row would mean a wrapped overflow leaked a second copy.
    for (const { prop, live } of OSCILLATION) {
      const frame = liveFrame("idle", prop, live);
      const count = frame.split("\n").filter((l) => l.includes("CTX 8%")).length;
      expect(count).toBe(1);
    }
  });

  it("idle chrome is already narrow-pane safe before a shrink", () => {
    // The prior regression only checked the frame AFTER a shrink. The real
    // terminal reflows the OLD wide frame first, so full-width status/footer rows
    // can gain physical rows before Ink erases them and leave the idle gauge orphaned
    // above the new frame. Full-width composer rules are visual separators and are
    // allowed to match the transcript width.
    const frame = liveFrame("idle", 80, 80);
    const unsafe = frame.split("\n").filter((line) => {
      const plain = line.replace(ANSI, "");
      const isIdleChrome =
        plain.includes("CTX") ||
        plain.includes("Ask Daintree") ||
        plain.includes("commands") ||
        /^agents \d+/.test(plain.trim());
      return isIdleChrome && visibleWidth(line) > LIVE_CHROME_MAX_WIDTH;
    });
    expect(unsafe).toEqual([]);
  });

  it("active/busy chrome is already narrow-pane safe before a shrink", () => {
    const frame = liveFrame("active", 80, 80);
    const unsafe = frame.split("\n").filter((line) => {
      const plain = line.replace(ANSI, "");
      const isBusyChrome =
        plain.includes("WORKING") ||
        plain.includes("Ask Daintree") ||
        plain.includes("queued") ||
        plain.includes("commands") ||
        /^agents \d+/.test(plain.trim());
      return isBusyChrome && visibleWidth(line) > LIVE_CHROME_MAX_WIDTH;
    });
    expect(unsafe).toEqual([]);
  });

  it("masthead rule spans the full cockpit width, not the prose cap", () => {
    // The masthead rule is a flex `<Divider />` in the repainting region, so it
    // yoga-fills the live width (stdout - gutter), past the ≤100 prose cap. The
    // headless stdout is a fixed 100 cols, so widen it to 120 and re-render so the
    // flex rule re-measures.
    const { rerender, stdout, lastFrame } = render(
      renderControlRoom("idle", 120, { staticKey: 0 }),
    );
    Object.defineProperty(stdout, "columns", { value: 120, configurable: true });
    rerender(renderControlRoom("idle", 120, { staticKey: 0 }));
    const firstRule = (lastFrame() ?? "")
      .split("\n")
      .find((line) => /^[─-]+$/.test(line.replace(ANSI, "").trim()));
    expect(firstRule).toBeDefined();
    // Exactly the full chrome width (cols - gutter = 119), not merely "> the 100 prose
    // cap": an exact assertion proves a full-width rule spans the cockpit.
    expect(visibleWidth(firstRule!)).toBe(119);
  });

  it("reflows the masthead rule when the live width changes (no stale-width copy)", () => {
    // The whole point of moving the masthead into the repainting region: its rule
    // re-measures on resize. Drive the live width via stdout (yoga reads stdout for
    // the flex rules) and assert the FIRST rule — the masthead's — tracks it, with no
    // stale wider rule and exactly one masthead row per frame.
    const mastheadRows = (frame: string) =>
      frame
        .split("\n")
        .map((l) => l.replace(ANSI, ""))
        .filter((l) => l.includes("Daintree Assistant"));
    const firstRuleWidth = (frame: string) => {
      const rule = frame
        .split("\n")
        .map((l) => l.replace(ANSI, ""))
        .find((l) => /^[─-]+$/.test(l.trim()));
      return rule ? rule.trim().length : -1;
    };
    const allRuleWidths = (frame: string) =>
      frame
        .split("\n")
        .map((l) => l.replace(ANSI, ""))
        .filter((l) => /^[─-]+$/.test(l.trim()))
        .map((l) => l.trim().length);

    const { rerender, stdout, lastFrame } = render(
      renderControlRoom("idle", 80, { staticKey: 0 }),
    );
    Object.defineProperty(stdout, "columns", { value: 80, configurable: true });
    rerender(renderControlRoom("idle", 80, { staticKey: 0 }));
    expect(mastheadRows(lastFrame() ?? "")).toHaveLength(1);
    expect(firstRuleWidth(lastFrame() ?? "")).toBe(79); // 80 - gutter(1)

    // Shrink the live width: the masthead rule must follow, leaving no 79-wide copy.
    Object.defineProperty(stdout, "columns", { value: 58, configurable: true });
    rerender(renderControlRoom("idle", 80, { staticKey: 0 }));
    expect(mastheadRows(lastFrame() ?? "")).toHaveLength(1);
    expect(firstRuleWidth(lastFrame() ?? "")).toBe(57);
    expect(allRuleWidths(lastFrame() ?? "")).not.toContain(79);

    // Grow it back: the rule re-expands.
    Object.defineProperty(stdout, "columns", { value: 80, configurable: true });
    rerender(renderControlRoom("idle", 80, { staticKey: 0 }));
    expect(firstRuleWidth(lastFrame() ?? "")).toBe(79);
  });

  it("orders the masthead below history/turn and above the input chrome", () => {
    // Layout contract after the move: committed history (and any in-flight turn) sit
    // ABOVE the masthead, which sits ABOVE the status line and composer. Distinct
    // marker text on each band lets us assert the vertical order in one frame.
    const frame = liveFrame("idle", 80, 120, {
      transcript: [
        {
          kind: "note",
          id: "note_hist",
          level: "info",
          text: "HISTORY_MARKER note",
          ts: FIXED_NOW,
        },
      ],
    });
    const lines = frame.split("\n").map((l) => l.replace(ANSI, ""));
    const at = (needle: string) => lines.findIndex((l) => l.includes(needle));
    const history = at("HISTORY_MARKER");
    const masthead = at("Daintree Assistant");
    const composer = at("Ask Daintree to supervise");
    expect(history).toBeGreaterThanOrEqual(0);
    expect(masthead).toBeGreaterThan(history); // history above masthead
    expect(composer).toBeGreaterThan(masthead); // masthead above the input
  });

  it("renders the masthead below committed history (it lives in the repainting region)", () => {
    // The masthead moved OUT of <Static> into the live region so its rule reflows on
    // resize. Consequence (accepted): committed history — including the startup
    // "Connected" note — now sits ABOVE the masthead, which settles just above the
    // input rather than pinned at the very top.
    const frame = liveFrame("idle", 80, 120, {
      transcript: [
        {
          kind: "note",
          id: "note_connected",
          level: "info",
          text: "Connected to Daintree MCP.",
          ts: FIXED_NOW,
        },
      ],
    });
    const lines = frame.split("\n").map((line) => line.replace(ANSI, ""));
    const headerLine = lines.findIndex((line) =>
      line.includes("Daintree Assistant"),
    );
    const noteLine = lines.findIndex((line) =>
      line.includes("Connected to Daintree MCP."),
    );
    expect(noteLine).toBeGreaterThanOrEqual(0);
    expect(headerLine).toBeGreaterThan(noteLine);
  });
});

describe("ControlRoom inline cockpit (golden frames)", () => {
  it.each(WIDTHS)("prints the masthead + composer, quiet when idle (%i cols)", (w) => {
    const frame = frameFor("idle", w);
    expect(frame).toContain("Daintree Assistant"); // live masthead
    expect(frame).toContain("tier operator"); // permission tier reads in the masthead
    expect(frame).not.toContain("Standing by"); // silence already means idle
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

  // Wire-through of the destructivePending escalation (#154): ControlRoom owns the
  // risk-class decision (git/system are destructive) and the Header just renders the
  // boolean. ui.color.danger is #FB7185 → SGR 38;2;251;113;133. Scope the assertion
  // to the TIER line so unrelated red elsewhere (an approval sheet, an error badge)
  // can't mask the signal we're proving.
  describe("tier escalation by pending risk class (#154)", () => {
    const DANGER = "38;2;251;113;133";
    // Ink emits the danger SGR only when chalk's color level is non-zero; pin it to
    // truecolor (restoring after) so the assertion is deterministic on a non-TTY CI.
    let prevLevel: typeof chalk.level;
    beforeEach(() => {
      prevLevel = chalk.level;
      chalk.level = 3;
    });
    afterEach(() => {
      chalk.level = prevLevel;
    });
    const tierLineHasDanger = (risk: string | null) => {
      const f = byKey("approval"); // a fixture with a pending confirm
      const pending =
        risk === null
          ? null
          : { ...f.pending!, request: { ...f.pending!.request, risk } as any };
      const { lastFrame } = render(
        <ControlRoom
          project="assistant"
          tier="system"
          columns={80}
          connected={f.connected}
          transcript={f.transcript}
          dashboard={f.dashboard}
          busy={f.busy}
          stage={f.stage}
          view="home"
          pending={pending}
          now={FIXED_NOW}
          composerFocus={false}
        />,
      );
      const tierLine = (lastFrame() ?? "")
        .split("\n")
        .find((l) => l.includes("tier ") && l.includes("system"));
      return (tierLine ?? "").includes(DANGER);
    };

    it("colors the system tier red while a git/system confirm is pending", () => {
      expect(tierLineHasDanger("git")).toBe(true);
      expect(tierLineHasDanger("system")).toBe(true);
    });

    it("keeps the tier quiet for a non-destructive (external) confirm", () => {
      expect(tierLineHasDanger("external")).toBe(false);
    });

    it("keeps the tier quiet when nothing is pending", () => {
      expect(tierLineHasDanger(null)).toBe(false);
    });
  });
});

// Regression (#138): `reservedColumns` is the single right-edge gutter that keeps
// every glyph clear of the terminal autowrap column AND of a host overlay scrollbar
// painted over the rightmost cells. The masthead rule and the composer rules are now
// ALL flex `<Divider />` inside the same root that has paddingRight={gutter}, so they
// resolve to the SAME width instead of one rule stopping short of the others. Prove
// every full-width rule equals `columns - reservedColumns`.
describe("ControlRoom reserved-column gutter (#138)", () => {
  const ruleWidths = (frame: string): number[] =>
    frame
      .split("\n")
      .map((line) => line.replace(ANSI, ""))
      .filter((line) => /^[─-]+$/.test(line.trim()) && line.trim().length > 1)
      .map((line) => line.trim().length);

  it.each([
    [1, 119],
    [2, 118],
    [3, 117],
  ])(
    "sizes the masthead AND composer rules to columns-%i",
    (reserved, expectedWidth) => {
      const f = byKey("idle");
      const COLS = 120;
      // All rules are flex now, so they yoga-fill stdout - paddingRight(gutter). The
      // headless stdout is a fixed 100 cols, so widen it to the prop width and
      // re-render so the flex rules re-measure at the wide width.
      const { rerender, stdout, lastFrame } = render(
        <ControlRoom
          project="assistant"
          tier="system"
          columns={COLS}
          reservedColumns={reserved}
          connected={f.connected}
          transcript={[]}
          dashboard={f.dashboard}
          busy={false}
          stage=""
          view="home"
          staticKey={0}
          now={FIXED_NOW}
          composerFocus={false}
        />,
      );
      Object.defineProperty(stdout, "columns", { value: COLS, configurable: true });
      rerender(
        <ControlRoom
          project="assistant"
          tier="system"
          columns={COLS}
          reservedColumns={reserved}
          connected={f.connected}
          transcript={[]}
          dashboard={f.dashboard}
          busy={false}
          stage=""
          view="home"
          staticKey={0}
          now={FIXED_NOW}
          composerFocus={false}
        />,
      );
      const widths = ruleWidths(lastFrame() ?? "");
      // Exactly three full-width rules in the idle home view: the masthead rule plus
      // the composer's top and bottom rules. Asserting the count (not just ">= 2")
      // proves the masthead rule is among them — a regressed masthead can't hide.
      expect(widths).toHaveLength(3);
      // Every full-width rule lands at the same gutter-shy column (masthead + live
      // rules converge), proving the single `reservedColumns` gutter drives them all.
      for (const w of widths) expect(w).toBe(expectedWidth);
    },
  );
});
