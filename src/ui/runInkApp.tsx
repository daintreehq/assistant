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
  try {
    await instance.waitUntilExit();
  } finally {
    await app.shutdown();
  }
}
