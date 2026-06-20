/**
 * Status-style rows in Ink's repainting region must be conservative in the main
 * screen buffer. When the host terminal shrinks, it reflows already-printed rows
 * before Ink receives resize and erases by its old logical line count. Keeping
 * compact status/dynamic chrome below Daintree's narrow pane width prevents those
 * rows from gaining extra physical rows and being orphaned into scrollback.
 */
export const LIVE_CHROME_MAX_WIDTH = 56;
