import { render } from "ink";
import { createElement } from "react";
import type { App } from "../cli/app.js";
import { DaintreeInkApp } from "./DaintreeInkApp.js";

export interface InkAppOptions {
  /**
   * Opt INTO the full-screen alternate buffer. Default is inline rendering so
   * the cockpit shares the host terminal's native scrollback — inside Daintree
   * (and any normal terminal) the wheel/trackpad scrolls history for free, the
   * same way Claude Code's agent panes do. The alternate buffer has no
   * scrollback, so it's now strictly opt-in (`--alt-screen`).
   */
  alternateScreen?: boolean;
}

export async function startInkApp(
  app: App,
  opts: InkAppOptions = {},
): Promise<void> {
  const alternateScreen = opts.alternateScreen === true;
  const instance = render(createElement(DaintreeInkApp, { app }), {
    // We handle Ctrl+C ourselves so shutdown drains the scheduler/MCP/DB.
    exitOnCtrlC: false,
    // Keep stray console output from corrupting the frame.
    patchConsole: true,
    alternateScreen,
    kittyKeyboard: { mode: "auto" },
  });

  // The resize-clear below is an alternate-screen-only workaround: Ink's
  // fullscreen renderer only does a full clear when the terminal width
  // *decreases*; growing the terminal, changing only its height, or a terminal
  // reflow leaves the previous frame's right-edge columns on screen, piling up
  // stale rightmost characters down the right column. In inline mode the
  // finished history lives in <Static> (already committed to scrollback) and
  // only the small live region repaints, so there's nothing stale to clear —
  // calling instance.clear() there would wipe rows we don't own.
  const stdout = process.stdout;
  const clearOnResize = () => instance.clear();
  if (alternateScreen) stdout.prependListener("resize", clearOnResize);

  try {
    await instance.waitUntilExit();
  } finally {
    if (alternateScreen) stdout.removeListener("resize", clearOnResize);
    await app.shutdown();
  }
}
