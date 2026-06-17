import { render } from "ink-testing-library";
import { AttentionSection } from "../../src/ui/sidebar/AttentionSection.js";
import type { AttentionRow } from "../../src/ui/sidebar/model.js";

const row: AttentionRow = {
  id: "a1",
  symbol: "!",
  color: "yellow",
  title: "tests failed",
  evidence: "parser.spec.ts failed",
  related: "terminal term_3a",
  actions: "open · summarize · resolve",
};

describe("AttentionSection", () => {
  it("shows a calm empty state when nothing needs the human", () => {
    const { lastFrame } = render(<AttentionSection rows={[]} />);
    expect(lastFrame() ?? "").toContain("nothing needs you");
  });

  it("renders title, evidence and actions when comfortable", () => {
    const { lastFrame } = render(<AttentionSection rows={[row]} density="comfortable" />);
    const frame = lastFrame() ?? "";
    expect(frame).toContain("tests failed");
    expect(frame).toContain("parser.spec.ts failed");
    expect(frame).toContain("open · summarize · resolve");
  });

  it("collapses to one line in dense mode", () => {
    const { lastFrame } = render(<AttentionSection rows={[row]} density="dense" />);
    const frame = lastFrame() ?? "";
    expect(frame).toContain("tests failed");
    // Dense drops the multi-line evidence/actions block.
    expect(frame).not.toContain("parser.spec.ts failed");
  });
});
