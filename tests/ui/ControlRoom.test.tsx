import { test, expect, describe } from "bun:test";
import { testRender } from "@opentui/react/test-utils";
import { ControlRoom, type View } from "../../src/ui/ControlRoom.js";
import type { PanelKey } from "../../src/cli/commandData.js";
import { buildFixtures, FIXED_NOW } from "../../src/ui/dev/fixtures.js";
import type { TranscriptCell } from "../../src/ui/types.js";

const fixtures = buildFixtures();
const byKey = (label: string) => fixtures.find((f) => f.label === label)!;

/**
 * Render a fixture through OpenTUI's headless renderer and return the plain-text
 * frame. The OpenTUI build renders in `main-screen` with NO Ink `<Static>`: the
 * whole tree (Header → every transcript cell → ApprovalSheet → StatusLine → the
 * Composer or an on-demand view) is live in one column, so we assert against the
 * full rendered frame. Give it a tall height (40) so nothing is clipped.
 *
 * captureCharFrame() is already plain text — no strip-ansi needed; assert with
 * `.toContain` / regex like the old `lastFrame()`.
 */
async function frameFor(
  label: string,
  columns: number,
  _rows = 40,
  over: { view?: View; activePanel?: PanelKey | null } = {},
): Promise<string> {
  const f = byKey(label);
  const t = await testRender(
    <ControlRoom
      project="assistant"
      tier="operator"
      columns={columns}
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
    { width: columns, height: 40 },
  );
  await t.flush();
  return t.captureCharFrame();
}

// The cockpit is one inline column at every width now (the old sidebar/standard/
// wide banding is gone). 58 is a typical host side panel; 120 is a wide terminal.
const WIDTHS = [58, 80, 120];

describe("ControlRoom inline cockpit (golden frames)", () => {
  test.each(WIDTHS)("prints the masthead + composer, quiet when idle (%i cols)", async (w) => {
    const frame = await frameFor("idle", w);
    expect(frame).toContain("Daintree Assistant"); // live masthead
    expect(frame).toContain("tier operator"); // permission tier reads in the masthead
    expect(frame).not.toContain("Standing by"); // silence already means idle
    expect(frame).toContain("commands"); // composer hint: / opens the palette
    expect(frame).toContain("ops"); // composer hint: ^O inspects operations
  });

  test.each(WIDTHS)("an active run renders the turn and the supervised agent (%i cols)", async (w) => {
    const frame = await frameFor("active", w);
    expect(frame).toContain("Daintree Assistant");
    expect(frame).toContain("YOU"); // the human turn (live tail)
    expect(frame).toContain("DAINTREE"); // the assistant turn
    expect(frame).toContain("term_8"); // the active agent surfaced in the status line
  });

  test.each(WIDTHS)("attention is surfaced in the status line (%i cols)", async (w) => {
    const frame = await frameFor("attention", w);
    // The most-urgent agent (needs input) is promoted into the one-line status,
    // and the attention chip carries the unresolved count — the failure detail
    // itself is reachable in the on-demand operations view.
    expect(frame).toContain("NEEDS INPUT");
    expect(frame).toContain("term_12");
    expect(frame).toContain("!2"); // unresolved-attention count chip
  });

  test.each(WIDTHS)("approval shows a risk-specific sheet above the composer (%i cols)", async (w) => {
    const frame = await frameFor("approval", w);
    expect(frame).toContain("Push branch to origin?");
    expect(frame).toContain("approve");
  });

  test.each(WIDTHS)("degraded is unmistakable (%i cols)", async (w) => {
    expect(await frameFor("degraded", w)).toContain("DEGRADED");
  });

  test("threads the queued follow-up count into the composer (#95)", async () => {
    // The "active" fixture queues two follow-ups; the count must reach the
    // Composer busy indicator. Guards against a dropped queueDepth prop.
    expect(await frameFor("active", 120)).toContain("2 queued");
    // The idle fixture has nothing queued — no hint.
    expect(await frameFor("idle", 120)).not.toContain("queued");
  });
});

describe("ControlRoom transcript cells render in order", () => {
  test("the header sits above the transcript, which sits above the live chrome", async () => {
    // The OpenTUI build has no <Static> split: the masthead is just the top of the
    // live tree and the conversation grows beneath it. Assert that vertical order on
    // the full frame rather than any committed-vs-live boundary.
    const f = byKey("idle");
    const t = await testRender(
      <ControlRoom
        project="assistant"
        tier="operator"
        columns={120}
        connected={f.connected}
        dashboard={f.dashboard}
        busy={f.busy}
        stage={f.stage}
        view="home"
        pending={null}
        now={FIXED_NOW}
        composerFocus={false}
        transcript={[
          {
            kind: "note",
            id: "note_connected",
            level: "info",
            text: "Connected to Daintree MCP.",
            ts: FIXED_NOW,
          },
        ]}
      />,
      { width: 120, height: 40 },
    );
    await t.flush();
    const lines = t.captureCharFrame().split("\n");
    const headerLine = lines.findIndex((line) => line.includes("Daintree Assistant"));
    const noteLine = lines.findIndex((line) => line.includes("Connected to Daintree MCP."));
    const composerLine = lines.findIndex((line) =>
      line.includes("Ask Daintree to supervise"),
    );
    expect(headerLine).toBeGreaterThanOrEqual(0);
    expect(noteLine).toBeGreaterThan(headerLine); // transcript below the masthead
    expect(composerLine).toBeGreaterThan(noteLine); // live chrome below the transcript
  });
});

describe("ControlRoom turn rendering", () => {
  test.each(WIDTHS)("renders the human turn as a distinct card, no box (%i cols)", async (w) => {
    const frame = await frameFor("active", w);
    expect(frame).toContain("YOU"); // quiet who-said-what label
    expect(frame).toContain("▏"); // the human's left accent bar marks the turn
    expect(frame).not.toMatch(/[╭┌].*[╮┐]/); // no box — the redesign drops it
    expect(frame).toContain("DAINTREE");
  });

  test("keeps user and Daintree distinguishable without color", async () => {
    const prev = process.env.DAINTREE_THEME;
    process.env.DAINTREE_THEME = "none";
    try {
      const frame = await frameFor("active", 58, 40);
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
  test("shows the full ops deck and a return hint when view=operations", async () => {
    const frame = await frameFor("active", 80, 32, { view: "operations" });
    // The active fixture has watchers (AGENTS), timers (SCHEDULED) and audit (RECENT).
    expect(frame).toContain("NOW");
    expect(frame).toContain("SCHEDULED");
    expect(frame).toContain("Esc to return");
  });

  test("replaces the composer with the panel but keeps the run + status line visible", async () => {
    const frame = await frameFor("active", 80, 32, { view: "operations" });
    // Only the composer swaps out for the panel — the in-flight turn and the
    // status line still repaint at the bottom of the stream.
    expect(frame).not.toContain("Ask Daintree to supervise"); // composer placeholder is gone
    expect(frame).toContain("DAINTREE"); // the active turn is still shown
    expect(frame).toContain("term_8"); // status line still surfaces the agent
  });

  test("a pending confirmation overrides an open panel (approval is never hidden)", async () => {
    // The approval fixture has a pending confirm; even asked to open operations,
    // the sheet must surface and the composer (not the deck) stays present.
    const frame = await frameFor("approval", 80, 32, { view: "operations" });
    expect(frame).toContain("Push branch to origin?");
    expect(frame).not.toContain("Esc to return"); // the ops panel yielded
  });

  test("focuses one section when a panel command set activePanel", async () => {
    const frame = await frameFor("active", 80, 32, {
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
  // boolean. ui.color.danger is #FB7185 → RGB(251,113,133). The color is no longer an
  // SGR escape in the plain-text frame; OpenTUI exposes per-span color via
  // captureSpans(), so we read the foreground of the `system` run on the tier line.
  describe("tier escalation by pending risk class (#154)", () => {
    const DANGER: [number, number, number] = [251, 113, 133];
    // Whether the danger color appears on the tier word itself. The tier line carries
    // two "system" runs (the tier capsule, and the literal "system" inside the gloss
    // "full access (git, system)"); only the capsule may turn red, so we look for the
    // danger fg among the tier-line spans rather than pinning one span.
    const tierLineHasDanger = async (risk: string | null): Promise<boolean> => {
      const f = byKey("approval"); // a fixture with a pending confirm
      const pending =
        risk === null
          ? null
          : { ...f.pending!, request: { ...f.pending!.request, risk } as any };
      const t = await testRender(
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
        { width: 80, height: 40 },
      );
      await t.flush();
      for (const line of t.captureSpans().lines) {
        const text = line.spans.map((s) => s.text).join("");
        if (!(text.includes("tier ") && text.includes("system"))) continue;
        for (const span of line.spans) {
          const [r, g, b] = span.fg.toInts();
          if (r === DANGER[0] && g === DANGER[1] && b === DANGER[2]) return true;
        }
      }
      return false;
    };

    test("colors the system tier red while a git/system confirm is pending", async () => {
      expect(await tierLineHasDanger("git")).toBe(true);
      expect(await tierLineHasDanger("system")).toBe(true);
    });

    test("keeps the tier quiet for a non-destructive (external) confirm", async () => {
      expect(await tierLineHasDanger("external")).toBe(false);
    });

    test("keeps the tier quiet when nothing is pending", async () => {
      expect(await tierLineHasDanger(null)).toBe(false);
    });
  });
});

// Regression (#138): the cockpit insets content by one column on each side — the
// right by `reservedColumns` (the autowrap/scrollbar gutter), the left by LEFT_PAD
// (=1). The only full-width rules left are the composer's top and bottom (the masthead
// has no rule now); both flex-fill the live region, so they equal
// `columns - reservedColumns - 1` (= chromeWidth).
describe("ControlRoom reserved-column gutter (#138)", () => {
  const ruleWidths = (frame: string): number[] =>
    frame
      .split("\n")
      .map((line) => line.trim())
      .filter((line) => /^[─-]+$/.test(line) && line.length > 1)
      .map((line) => line.length);

  test.each([
    [1, 118],
    [2, 117],
    [3, 116],
  ])(
    "sizes the composer rules to columns - reserved(%i) - 1",
    async (reserved, expectedWidth) => {
      const f = byKey("idle");
      const COLS = 120;
      const t = await testRender(
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
          now={FIXED_NOW}
          composerFocus={false}
        />,
        { width: COLS, height: 40 },
      );
      await t.flush();
      const widths = ruleWidths(t.captureCharFrame());
      // Three full-width rules in the idle home view: the header-band rule below the
      // masthead, plus the composer's top and bottom.
      expect(widths).toHaveLength(3);
      // All land at the same inset column (columns - reserved - LEFT_PAD = chromeWidth).
      for (const w of widths) expect(w).toBe(expectedWidth);
    },
  );

  test("a full-width rule closes the header band (below the masthead)", async () => {
    // The Header COMPONENT itself emits no rule (a committed rule would wrap on a
    // narrow host resize — see Header.test); ControlRoom draws the band rule instead,
    // live, just below the masthead. The native renderer reflows it cleanly, so it's
    // safe to render full-width. Assert a rule appears within a couple rows of the
    // wordmark, not only down at the composer.
    const frame = await frameFor("idle", 120);
    const lines = frame.split("\n").map((l) => l.trim());
    const headerIdx = lines.findIndex((l) => l.includes("Daintree Assistant"));
    const firstRuleIdx = lines.findIndex(
      (l) => /^[─-]+$/.test(l) && l.length > 1,
    );
    expect(firstRuleIdx).toBeGreaterThan(headerIdx);
    // The header-band rule sits close under the masthead block (tier line included),
    // not all the way down at the composer.
    expect(firstRuleIdx).toBeLessThanOrEqual(headerIdx + 4);
  });
});
