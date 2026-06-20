/**
 * Wiping the host terminal's scrollback on `/clear`.
 *
 * The inline cockpit renders into the terminal's MAIN screen buffer (not the
 * alternate buffer) so the host terminal's own scrollback / mouse wheel /
 * selection work natively. The flip side: `/clear` resetting the conversation
 * and remounting <Static> only clears what Ink owns — the committed cells that
 * already flowed into native scrollback stay scrolled-back, so the user can still
 * wheel up into the "cleared" conversation. To make the visual reset match the
 * logical one we ask the OS terminal itself to drop its scrollback.
 *
 * The sequence is the same three escapes a shell `clear` emits on xterm-class
 * terminals, in this order:
 *   - `\x1b[2J` — erase the visible viewport
 *   - `\x1b[3J` — erase the scrollback buffer (the one that matters here)
 *   - `\x1b[H`  — cursor home, so Ink's next paint starts cleanly at the top
 *
 * `\x1b[3J` is broadly supported (iTerm2, kitty, WezTerm, Alacritty, xterm,
 * Terminal.app); terminals that lack it silently ignore it, so no fallback is
 * needed. We deliberately do NOT touch the alternate buffer (`\x1b[?1049h`) —
 * that fights Ink's render model and destroys native scrollback (see CLAUDE.md).
 */

/** The host-terminal clear: erase viewport, erase scrollback, cursor home. */
export const HOST_TERMINAL_CLEAR = "\x1b[2J\x1b[3J\x1b[H";

/**
 * Emit the scrollback-wipe sequence, but only on a real TTY and never throwing.
 * Like the cockpit's other terminal side-channels (BEL, OSC title), a failed
 * write here must never take down the caller.
 */
export function clearHostTerminal(
  stdout: NodeJS.WriteStream | undefined,
): void {
  if (!stdout?.isTTY) return;
  try {
    stdout.write(HOST_TERMINAL_CLEAR);
  } catch {
    // A failed escape write must never break /clear.
  }
}
