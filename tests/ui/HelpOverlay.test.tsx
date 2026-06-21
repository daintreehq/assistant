import { test, expect, describe } from "bun:test";
import { testRender } from "@opentui/react/test-utils";
import { HelpOverlay } from "../../src/ui/components/HelpOverlay.js";
import { overlayEntries } from "../../src/commandRegistry.js";

describe("HelpOverlay", () => {
  test("renders every registry command's syntax (issue #50: was missing /models, /help)", async () => {
    const t = await testRender(<HelpOverlay width={72} />, {
      width: 80,
      height: 60,
    });
    await t.flush();
    const frame = t.captureCharFrame();
    for (const [syntax] of overlayEntries()) {
      expect(frame).toContain(syntax);
    }
    // The two commands the original overlay dropped, called out explicitly.
    expect(frame).toContain("/models");
    expect(frame).toContain("/help");
  });

  test("keeps each command's description on one line at the default width", async () => {
    // The overlay's right column is ~46 chars at width=72; descriptions must fit
    // so a registered command renders as a single, untruncated row.
    const t = await testRender(<HelpOverlay width={72} />, {
      width: 80,
      height: 60,
    });
    await t.flush();
    const lines = t.captureCharFrame().split("\n");
    for (const [syntax, help] of overlayEntries()) {
      const row = lines.find((l) => l.includes(syntax));
      expect(row).toBeDefined();
      expect(row).toContain(help);
    }
  });
});
