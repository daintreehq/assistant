/**
 * Out-of-band attention signalling for the cockpit.
 *
 * When a supervised agent surfaces a fresh actionable event the user may have
 * switched focus to another window — the in-band attention banner only
 * helps if they're looking at the terminal. This hook adds two terminal-native
 * cues that survive a focus change:
 *
 *   - a BEL (`\x07`) on each fresh attention batch the scheduler delivers, so the
 *     terminal flashes / dings even when backgrounded, and
 *   - an OSC 2 title badge (`Daintree ⚠ N`) tracking the live inbox count, so a
 *     glance at the tab/window title shows whether anything is waiting.
 *
 * Both are gated on `stdout.isTTY` and wrapped so a failed write can never crash
 * the cockpit — these are passive side-channels, like the audit log. We do NOT
 * implement terminal focus-reporting (`\x1b[?1004h`) for v1: it requires raw
 * stdin parsing that fights Ink's own input handling, and the issue names it
 * optional. Desktop-notification escapes (OSC 9 / 777) are likewise out of scope.
 *
 * Why a BEL on the bridge event rather than on an inbox-count increment: the
 * scheduler stamps `notifiedAt` and delivers each fresh batch exactly once via
 * the `attention` bridge event, so that is the authoritative "new events just
 * arrived" signal. The count (capped, polled once a second) can stay flat while
 * new events replace resolved ones, which would miss a ding.
 */
import { useEffect } from "react";
import { useStdout } from "ink";
import type { UiBridge } from "../bridge.js";

const BEL = "\x07";

/** An OSC 2 "set window title" escape. Ignored by terminals that don't support it. */
function osc2(title: string): string {
  return `\x1b]2;${title}\x07`;
}

/** Write a raw escape only on a real TTY; never throw out of a side-channel. */
function safeWrite(
  stdout: NodeJS.WriteStream | undefined,
  chunk: string,
): void {
  if (!stdout?.isTTY) return;
  try {
    // The managed Ink stream: BEL and OSC title escapes emit no printable chars
    // and don't move the cursor, so they don't corrupt the rendered frame.
    stdout.write(chunk);
  } catch {
    // A failed escape write must never take down the cockpit.
  }
}

export function useAttentionSignal({
  bridge,
  inboxCount,
}: {
  bridge: UiBridge;
  inboxCount: number;
}): void {
  const { stdout } = useStdout();

  // Ding once per fresh attention batch the scheduler delivers.
  useEffect(() => {
    return bridge.subscribe((event) => {
      if (event.type === "attention" && event.events.length > 0) {
        safeWrite(stdout, BEL);
      }
    });
  }, [bridge, stdout]);

  // Mirror the unresolved inbox count into the window/tab title.
  useEffect(() => {
    safeWrite(
      stdout,
      osc2(inboxCount > 0 ? `Daintree ⚠ ${inboxCount}` : "Daintree"),
    );
  }, [inboxCount, stdout]);

  // Leave a clean title behind when the cockpit exits.
  useEffect(() => {
    return () => safeWrite(stdout, osc2("Daintree"));
  }, [stdout]);
}
