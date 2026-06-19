import { render } from "ink-testing-library";
import { ControlRoom } from "../../src/ui/ControlRoom.js";
import { buildFixtures, FIXED_NOW } from "../../src/ui/dev/fixtures.js";

const fixtures = buildFixtures();
const byKey = (label: string) => fixtures.find((f) => f.label === label)!;

function frameFor(label: string, columns: number, rows = 24): string {
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
      view={f.view}
      pending={f.pending}
      now={FIXED_NOW}
      composerFocus={false}
    />,
  );
  return lastFrame() ?? "";
}

// The sidebar (55–65) is the design target; 80/120 are wider-layout regressions.
const SIDEBAR_WIDTHS = [55, 58, 62, 65];
const WIDTHS = [58, 80, 120];

describe("ControlRoom golden frames (deterministic fixtures)", () => {
  it.each(WIDTHS)("idle reads as connected + standing by at %i cols", (w) => {
    const frame = frameFor("idle", w, 32);
    expect(frame).toContain("Daintree Assistant"); // the masthead wordmark
    expect(frame).toContain("MCP"); // connection lives in the status line now
    expect(frame).toContain("Standing by"); // what is it doing?
    expect(frame).toContain("ops"); // what key reveals more?
  });

  it.each(WIDTHS)("active run shows the supervised work at %i cols", (w) => {
    const frame = frameFor("active", w, 32);
    expect(frame).toContain("Daintree Assistant"); // the masthead wordmark
    expect(frame).toContain("term_8"); // which operation is active?
    expect(frame).toMatch(/Watching|WORKING|WATCHING/); // what is Daintree doing?
  });

  it.each(WIDTHS)("attention surfaces the urgent title at %i cols", (w) => {
    const frame = frameFor("attention", w, 32);
    expect(frame).toContain("term_8"); // the urgent agent is visible
    expect(frame.toLowerCase()).toContain("fail"); // failure is communicated
  });

  it.each(WIDTHS)("approval shows a risk-specific sheet at %i cols", (w) => {
    const frame = frameFor("approval", w, 32);
    expect(frame).toContain("Push branch to origin?");
    expect(frame).toContain("approve");
  });

  it.each(WIDTHS)("degraded is unmistakable at %i cols", (w) => {
    const frame = frameFor("degraded", w, 32);
    expect(frame).toContain("DEGRADED");
  });

  it("renders a quiet ops rail only at wide widths", () => {
    expect(frameFor("active", 120, 32)).toContain("NOW");
    // The sidebar carries NOW itself; the wide-only NEXT rail label is absent.
    expect(frameFor("active", 58, 32)).not.toContain("NEXT");
  });
});

describe("ControlRoom sidebar (55–65 cols, the primary surface)", () => {
  it.each(SIDEBAR_WIDTHS)(
    "shows operations before recent transcript at %i cols",
    (w) => {
      const frame = frameFor("active", w, 36);
      expect(frame).toContain("NOW");
      expect(frame).toContain("WATCHING");
      expect(frame).toContain("TIMERS");
      expect(frame).toContain("RECENT");
      // The product is operations-first: monitoring precedes chat history.
      expect(frame.indexOf("WATCHING")).toBeLessThan(frame.indexOf("RECENT"));
    },
  );

  it.each(SIDEBAR_WIDTHS)(
    "renders user messages as a distinct card at %i cols",
    (w) => {
      // 40 rows, not 36: the masthead is two rows taller than the old identity
      // bar, so the Daintree response label needs a realistic terminal height to
      // sit inside the visible transcript window on the narrow sidebar.
      const frame = frameFor("active", w, 40);
      expect(frame).toContain("YOU");
      expect(frame).toMatch(/[╭┌].*[╮┐]/); // a boxed card around the prompt
      expect(frame).toContain("DAINTREE");
    },
  );

  it("focuses one section when a panel command set activePanel", () => {
    const f = byKey("active"); // has watchers (AGENTS), timers (SCHEDULED), audit (RECENT)
    const frame =
      render(
        <ControlRoom
          project="assistant"
          tier="operator"
          columns={80}
          rows={32}
          connected={f.connected}
          transcript={f.transcript}
          dashboard={f.dashboard}
          previews={f.previews}
          busy={f.busy}
          stage={f.stage}
          view="operations"
          activePanel="timers"
          pending={f.pending}
          now={FIXED_NOW}
          composerFocus={false}
        />,
      ).lastFrame() ?? "";
    expect(frame).toContain("SCHEDULED");
    // The deck is filtered to the requested panel — other sections are absent.
    expect(frame).not.toContain("AGENTS");
    expect(frame).not.toContain("RECENT");
    expect(frame).not.toContain("NOW");
  });

  it("keeps user and Daintree distinguishable without color", () => {
    const prev = process.env.DAINTREE_THEME;
    process.env.DAINTREE_THEME = "none";
    try {
      const frame = frameFor("active", 58, 40);
      expect(frame).toContain("YOU");
      expect(frame).toContain("◆ DAINTREE");
      expect(frame).toContain("WATCHING");
    } finally {
      if (prev === undefined) delete process.env.DAINTREE_THEME;
      else process.env.DAINTREE_THEME = prev;
    }
  });
});
