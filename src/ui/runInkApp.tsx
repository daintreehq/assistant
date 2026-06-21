import { render } from "ink";
import { createElement } from "react";
import type { App } from "../cli/app.js";
import { DaintreeInkApp } from "./DaintreeInkApp.js";

export interface InkAppOptions {
  /**
   * Reserved for call-site compatibility — the cockpit is ALWAYS an inline surface
   * (Claude Code model) and never takes the alternate screen. We render into the
   * terminal's MAIN buffer and commit completed turns to native scrollback via
   * `<Static>`, so the host terminal (xterm in Daintree) owns scrolling: the mouse
   * wheel scrolls wherever it hovers, selection and copy/paste work, and on resize
   * the host reflows the scrollback while Ink only repaints the small live region at
   * the bottom. The alternate screen would disable all of that, so we never use it.
   */
  alternateScreen?: boolean;
}

export async function startInkApp(app: App): Promise<void> {
  const instance = render(createElement(DaintreeInkApp, { app }), {
    // INLINE / main buffer (no alternateScreen) so xterm keeps native scrollback +
    // hover scroll. We deliberately do NOT monkeypatch Ink's resize/erase: the host
    // reflows committed scrollback natively (the Claude Code approach). Resize stays
    // clean as long as live-region lines never WRAP (they truncate) and nothing
    // commits a full-width rule to scrollback (the host would wrap it on shrink).
    exitOnCtrlC: false,
    // Keep stray console output from corrupting the live region.
    patchConsole: true,
    kittyKeyboard: { mode: "auto" },
  });

  try {
    await instance.waitUntilExit();
  } finally {
    await app.shutdown();
  }
}
