import { render } from "ink";
import { createElement } from "react";
import type { App } from "../cli/app.js";
import { DaintreeInkApp } from "./DaintreeInkApp.js";

export interface InkAppOptions {
  /**
   * Reserved. The cockpit is an INLINE surface (Claude Code style): it renders
   * into the terminal's main screen buffer so native scrollback / mouse wheel /
   * selection work, and commits completed turns to scrollback via <Static>. We do
   * NOT use the alternate screen buffer — a pinned full-screen frame is mutually
   * exclusive with native scrollback. Kept for call-site compatibility only.
   */
  alternateScreen?: boolean;
}

export async function startInkApp(app: App): Promise<void> {
  const instance = render(createElement(DaintreeInkApp, { app }), {
    // We handle Ctrl+C ourselves so shutdown drains the scheduler/MCP/DB.
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
