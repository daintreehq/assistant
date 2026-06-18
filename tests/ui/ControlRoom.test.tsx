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
      view={f.view}
      pending={f.pending}
      now={FIXED_NOW}
      composerFocus={false}
    />,
  );
  return lastFrame() ?? "";
}

const WIDTHS = [52, 80, 120];

describe("ControlRoom golden frames (deterministic fixtures)", () => {
  it.each(WIDTHS)("idle reads as connected + standing by at %i cols", (w) => {
    const frame = frameFor("idle", w);
    expect(frame).toContain("DAINTREE"); // is this Daintree?
    expect(frame).toContain("CONNECTED"); // is it connected?
    expect(frame).toContain("Standing by"); // what is it doing?
    expect(frame).toContain("operations"); // what key reveals more?
  });

  it.each(WIDTHS)("active run shows the supervised work at %i cols", (w) => {
    const frame = frameFor("active", w);
    expect(frame).toContain("DAINTREE");
    expect(frame).toContain("term_8"); // which operation is active?
    expect(frame).toMatch(/Watching|WORKING/); // what is Daintree doing?
  });

  it.each(WIDTHS)("attention surfaces the urgent title at %i cols", (w) => {
    const frame = frameFor("attention", w);
    expect(frame).toContain("term_8"); // the urgent agent is visible
    expect(frame.toLowerCase()).toContain("fail"); // failure is communicated
  });

  it.each(WIDTHS)("approval shows a risk-specific sheet at %i cols", (w) => {
    const frame = frameFor("approval", w);
    expect(frame).toContain("Push branch to origin?");
    expect(frame).toContain("approve");
  });

  it.each(WIDTHS)("degraded is unmistakable at %i cols", (w) => {
    const frame = frameFor("degraded", w);
    expect(frame).toContain("DEGRADED");
  });

  it("renders a quiet ops rail only at wide widths", () => {
    expect(frameFor("active", 120)).toContain("NOW");
    // Narrow has no rail; the current-operation strip carries NOW instead.
    expect(frameFor("active", 52)).not.toContain("NEXT");
  });
});
