import { render } from "ink";
import { createElement } from "react";
import type { App } from "../cli/app.js";
import { DaintreeInkApp } from "./DaintreeInkApp.js";

export interface InkAppOptions {
  /** Full-screen alternate buffer (restores the terminal on exit). */
  alternateScreen?: boolean;
}

export async function startInkApp(
  app: App,
  opts: InkAppOptions = {},
): Promise<void> {
  const instance = render(createElement(DaintreeInkApp, { app }), {
    // We handle Ctrl+C ourselves so shutdown drains the scheduler/MCP/DB.
    exitOnCtrlC: false,
    // Keep stray console output from corrupting the frame.
    patchConsole: true,
    alternateScreen: opts.alternateScreen !== false,
    kittyKeyboard: { mode: "auto" },
  });

  // Ink's fullscreen renderer only does a full clear when the terminal width
  // *decreases*; growing the terminal, changing only its height, or a terminal
  // reflow leaves the previous frame's right-edge columns on screen. Because
  // the next frame is repainted incrementally from a cursor the resize has
  // desynced, those stale rightmost characters (the last char of each
  // right-aligned line — DEGRADED → "D", MCP degraded → "d", divider rules)
  // are never erased and pile up down the right column. Force a clean frame on
  // every resize. prependListener runs this before Ink's own resize repaint so
  // we clear first, then it draws the new frame onto a blank screen.
  const stdout = process.stdout;
  const clearOnResize = () => instance.clear();
  stdout.prependListener("resize", clearOnResize);

  try {
    await instance.waitUntilExit();
  } finally {
    stdout.removeListener("resize", clearOnResize);
    await app.shutdown();
  }
}
