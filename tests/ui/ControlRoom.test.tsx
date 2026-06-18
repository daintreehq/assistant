import { render } from "ink-testing-library";
import { ControlRoom } from "../../src/ui/ControlRoom.js";
import { buildFixtures, FIXED_NOW } from "../../src/ui/dev/fixtures.js";

const fixtures = buildFixtures();
const byKey = (label: string) => fixtures.find((f) => f.label === label)!;

function frameFor(label: string, columns: number, rows = 24): string {
  const f = byKey(label);
  const { lastFrame } = render(
    <ControlRoom
      project="assistant-main"
      tier="operator"
      columns={columns}
      rows={rows}
      connected={f.connected}
      transcript={f.transcript}
      dashboard={f.dashboard}
      previews={f.previews}
      busy={f.busy}
      stage={f.stage}
      pending={f.pending}
      now={FIXED_NOW}
      composerFocus={false}
    />,
  );
  return lastFrame() ?? "";
}

const SIDEBAR_WIDTHS = [55, 58, 62, 65];
const WIDTHS = [58, 80, 120];

describe("ControlRoom stream frames (deterministic fixtures)", () => {
  it.each(WIDTHS)("idle starts with the Daintree intro block at %i cols", (w) => {
    const frame = frameFor("idle", w, 32);
    expect(frame).toContain("assistant");
    expect(frame).toContain("MCP CONNECTED");
    expect(frame).toContain("Standing by");
    expect(frame).toContain("scroll the terminal"); // native scrollback hint
    expect(frame).toContain("Ask Daintree...");
  });

  it.each(WIDTHS)("active work reads as a vertical ledger at %i cols", (w) => {
    const frame = frameFor("active", w, 32);
    expect(frame).toContain("WORK");
    expect(frame).toContain("term_8");
    expect(frame).toContain("USER");
    expect(frame).toContain("◆ DAINTREE");
    expect(frame).toMatch(/Watching|WATCHING/);
  });

  it.each(WIDTHS)("attention surfaces the urgent title at %i cols", (w) => {
    const frame = frameFor("attention", w, 32);
    expect(frame).toContain("ATTENTION");
    expect(frame).toContain("term_8");
    expect(frame.toLowerCase()).toContain("fail");
  });

  it.each(WIDTHS)("approval keeps the decision sheet above the composer at %i cols", (w) => {
    const frame = frameFor("approval", w, 32);
    expect(frame).toContain("Push branch to origin?");
    expect(frame).toContain("approve");
    expect(frame).toContain("Ask Daintree...");
  });

  it.each(WIDTHS)("degraded is unmistakable at %i cols", (w) => {
    const frame = frameFor("degraded", w, 32);
    expect(frame).toContain("MCP DEGRADED");
    expect(frame).toContain("DEGRADED");
  });

  it("never renders a wide right rail on the home surface", () => {
    const frame = frameFor("active", 120, 32);
    expect(frame).not.toContain("NOW");
    expect(frame).not.toContain("AGENTS");
    expect(frame).toContain("USER");
  });
});

describe("ControlRoom sidebar (55-65 cols, the primary surface)", () => {
  it.each(SIDEBAR_WIDTHS)(
    "places the intro/work block before the chronological turn at %i cols",
    (w) => {
      const frame = frameFor("active", w, 36);
      const work = frame.indexOf("WORK");
      const user = frame.indexOf("USER");
      const daintreeReply = frame.indexOf("◆ DAINTREE", user);
      expect(work).toBeLessThan(user);
      expect(user).toBeLessThan(daintreeReply);
      expect(daintreeReply).toBeLessThan(frame.indexOf("Watching", daintreeReply));
    },
  );

  it.each(SIDEBAR_WIDTHS)(
    "represents user input as a quoted stream row, not a space-heavy card at %i cols",
    (w) => {
      const frame = frameFor("active", w, 36);
      expect(frame).toContain("USER");
      expect(frame).toContain("│");
      expect(frame).toContain("Fix the watcher tests");
      expect(frame).toMatch(/─{8,}/);
      expect(frame).not.toMatch(/[╭┌].*[╮┐]/);
    },
  );

  it("keeps user and Daintree distinguishable with ASCII glyphs", () => {
    const prev = process.env.DAINTREE_ASCII;
    process.env.DAINTREE_ASCII = "1";
    try {
      const frame = frameFor("active", 58, 36);
      expect(frame).toContain("USER");
      expect(frame).toContain("# DAINTREE");
      expect(frame).toContain("Watching");
    } finally {
      if (prev === undefined) delete process.env.DAINTREE_ASCII;
      else process.env.DAINTREE_ASCII = prev;
    }
  });
});
