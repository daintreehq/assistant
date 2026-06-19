import { Box } from "ink";
import { render } from "ink-testing-library";
import { ActivityTree } from "../../src/ui/components/ActivityTree.js";
import { glyphs, ui } from "../../src/ui/theme.js";
import type { ActivityItem } from "../../src/ui/types.js";

/** Strip ANSI so assertions read the plain glyphs even in colour. */
const plain = (s: string | undefined) => (s ?? "").replace(/\x1b\[[0-9;]*m/g, "");

function done(over: Partial<ActivityItem>): ActivityItem {
  return {
    id: "x",
    name: "tool",
    label: "Did",
    state: "done",
    startedAt: 0,
    endedAt: 5,
    ...over,
  };
}

describe("ActivityTree", () => {
  it("uses the square last-branch glyph, not the rounded arc", () => {
    // The arc ╰ (U+2570) is substituted at a different width by many fonts and
    // breaks column alignment with ├; the square └ is its universal sibling.
    // Lock the theme value directly so this holds regardless of ASCII fallback.
    expect(ui.glyph.lastBranch).toBe("└─");
    expect(ui.glyph.lastBranch).not.toContain("╰");

    const set = glyphs();
    const acts = [
      done({ id: "a", label: "First" }),
      done({ id: "b", label: "Last" }),
    ];
    const frame = plain(render(<ActivityTree activities={acts} width={72} />).lastFrame());
    expect(frame).toContain(set.branch);
    expect(frame).toContain(set.lastBranch);
    expect(frame).not.toContain("╰"); // the arc never reaches the screen
  });

  it("truncates a long detail so it never collides with the duration", () => {
    // A long label ("Checked status") + long summary used to overflow the row;
    // `space-between` then left zero gap before the timing ("…http1ms").
    // Constrain the layout to 64 cols so the jam would actually reproduce.
    const acts = [
      done({
        id: "b",
        label: "Checked status",
        summary: "Daintree MCP connected via streamable-http (12 tools).",
        endedAt: 1,
      }),
    ];
    const frame = plain(
      render(
        <Box width={64}>
          <ActivityTree activities={acts} width={64} />
        </Box>,
      ).lastFrame(),
    );
    const row = frame.split("\n").find((l) => l.includes("Checked status")) ?? "";
    expect(row).toContain("…"); // detail was truncated to fit
    expect(row).toContain("1ms"); // duration still present
    expect(row).not.toMatch(/\S1ms/); // and not jammed against the detail
  });
});
