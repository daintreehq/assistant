import { createCliRenderer } from "@opentui/core";
import { createRoot } from "@opentui/react";
import type { App } from "../cli/app.js";
import { DaintreeApp } from "./DaintreeApp.js";

/**
 * Boot the cockpit on OpenTUI's native renderer.
 *
 * INLINE / main-screen (Claude Code model), NEVER the alternate screen: we render
 * into the terminal's MAIN buffer so the host terminal (xterm in Daintree) owns
 * scrolling — the mouse wheel scrolls wherever it hovers, selection and copy/paste
 * work, and on resize the native (Zig) renderer reflows the tree cleanly. The
 * alternate screen would disable all of that, so `screenMode` is `"main-screen"` and
 * must stay that way. `useMouse: false` keeps OpenTUI from capturing the wheel — the
 * HOST owns scroll. `exitOnCtrlC: false` because the shell owns Ctrl-C shutdown.
 */
export async function startCockpit(app: App): Promise<void> {
  const renderer = await createCliRenderer({
    screenMode: "main-screen",
    exitOnCtrlC: false,
    useMouse: false,
    targetFps: 30,
    // We own process-signal teardown (below) so it routes through the SAME path as
    // ^C / `/quit` and actually shuts the runtime down. Left to OpenTUI, the default
    // signal handler only calls `renderer.destroy()` and skips `app.shutdown()`,
    // orphaning the scheduler / MCP / DB.
    exitSignals: [],
  });
  const root = createRoot(renderer);

  await new Promise<void>((resolve) => {
    let exiting = false;
    // Tear down the renderer (restores the terminal) and resolve so the caller can
    // shut the runtime down. Idempotent: ^C, `/quit`, and a process signal all route
    // here. A SECOND signal force-exits in case shutdown itself wedges.
    const exit = () => {
      if (exiting) return;
      exiting = true;
      process.off("SIGINT", onSignal);
      process.off("SIGTERM", onSignal);
      try {
        root.unmount();
      } catch {
        /* renderer already gone */
      }
      try {
        renderer.destroy();
      } catch {
        /* already destroyed */
      }
      resolve();
    };
    const onSignal = () => {
      if (exiting) process.exit(130);
      exit();
    };
    process.on("SIGINT", onSignal);
    process.on("SIGTERM", onSignal);
    root.render(<DaintreeApp app={app} exit={exit} />);
  });

  await app.shutdown();
}
