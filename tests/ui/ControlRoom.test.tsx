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
