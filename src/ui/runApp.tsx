import { createCliRenderer } from "@opentui/core";
import { createRoot } from "@opentui/react";
import type { App } from "../cli/app.js";
import { DaintreeApp } from "./DaintreeApp.js";

/**
 * Boot the cockpit on OpenTUI's native renderer in SPLIT-FOOTER mode.
 *
 * The Claude-Code inline model: a growing transcript that lives in the terminal's
 * native scrollback (the host owns the wheel, the scrollbar, selection and
 * copy/paste) plus a small LIVE FOOTER pinned at the bottom for the in-flight turn,
 * the status line and the composer.
 *
 * Why split-footer and NOT main-screen: OpenTUI's `main-screen` renders the whole
 * React tree into a FIXED viewport and repaints it in place — it does NOT spill
 * overflow into native scrollback. The instant the tree grew taller than the
 * terminal the layout math overflowed and the text garbled/interleaved (the bug this
 * replaces). `split-footer` is the mode built for this shape: `renderer.root` (our
 * React tree) is the bottom footer of `footerHeight` rows, and finished content is
 * COMMITTED to scrollback above it via `createScrollbackSurface().commitRows` (see
 * `scrollback.tsx`). That is the true equivalent of Ink's `<Static>`: each finished
 * turn prints once, becomes real terminal scrollback, and scrolls up and away — the
 * header included. The alternate screen is still forbidden (it would kill the host's
 * wheel/selection); `useMouse:false` keeps OpenTUI off the wheel.
 *
 * `footerHeight` is seeded from the current terminal height (so the boot splash and
 * the first frame are never clipped) and then tracked down to the live region's
 * measured height by `DaintreeApp` once the cockpit is up — see `useFooterHeight`.
 */
export async function startCockpit(app: App): Promise<void> {
  const renderer = await createCliRenderer({
    screenMode: "split-footer",
    // Seed the footer at the full terminal height: nothing is committed yet, so the
    // splash/first frame should own the whole screen. DaintreeApp shrinks this to the
    // live region's real height once booted, which is what frees the rows above the
    // footer for committed scrollback.
    footerHeight: Math.max(1, process.stdout.rows ?? 24),
    // REQUIRED for the scrollback APIs: `createScrollbackSurface` / `writeToScrollback`
    // both throw unless externalOutputMode is "capture-stdout" (it routes committed
    // rows — and any stray stdout — into scrollback above the footer). Without it every
    // commit in scrollback.tsx would fail and finished turns would be silently lost.
    externalOutputMode: "capture-stdout",
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
