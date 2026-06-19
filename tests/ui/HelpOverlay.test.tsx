import { render } from "ink-testing-library";
import { HelpOverlay } from "../../src/ui/components/HelpOverlay.js";
import { overlayEntries } from "../../src/commandRegistry.js";

describe("HelpOverlay", () => {
  it("renders every registry command's syntax (issue #50: was missing /models, /help)", () => {
    const { lastFrame } = render(<HelpOverlay width={72} />);
    const frame = lastFrame() ?? "";
    for (const [syntax] of overlayEntries()) {
      expect(frame).toContain(syntax);
    }
    // The two commands the original overlay dropped, called out explicitly.
    expect(frame).toContain("/models");
    expect(frame).toContain("/help");
  });

  it("keeps each command's description on one line at the default width", () => {
    // The overlay's right column is ~46 chars at width=72; descriptions must fit
    // so a registered command renders as a single, untruncated row.
    const { lastFrame } = render(<HelpOverlay width={72} />);
    const lines = (lastFrame() ?? "").split("\n");
    for (const [syntax, help] of overlayEntries()) {
      const row = lines.find((l) => l.includes(syntax));
      expect(row).toBeDefined();
      expect(row).toContain(help);
    }
  });
});
