import { test, expect, describe } from "bun:test";
import { testRender } from "@opentui/react/test-utils";
import { ActivityTree } from "../../src/ui/components/ActivityTree.js";
import { glyphs, ui } from "../../src/ui/theme.js";
import type { ActivityItem } from "../../src/ui/types.js";

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
  test("uses the square last-branch glyph, not the rounded arc", async () => {
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
    const t = await testRender(<ActivityTree activities={acts} width={72} />, {
      width: 72,
      height: 12,
    });
    await t.flush();
    const frame = t.captureCharFrame();
    expect(frame).toContain(set.branch);
    expect(frame).toContain(set.lastBranch);
    expect(frame).not.toContain("╰"); // the arc never reaches the screen
  });

  test("truncates a long detail so it never collides with the duration", async () => {
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
    const t = await testRender(
      <box width={64}>
        <ActivityTree activities={acts} width={64} />
      </box>,
      { width: 64, height: 12 },
    );
    await t.flush();
    const frame = t.captureCharFrame();
    const row = frame.split("\n").find((l) => l.includes("Checked status")) ?? "";
    expect(row).toContain("…"); // detail was truncated to fit
    expect(row).toContain("1ms"); // duration still present
    expect(row).not.toMatch(/\S1ms/); // and not jammed against the detail
  });
});
