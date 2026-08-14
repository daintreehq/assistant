package ui

import "github.com/daintreehq/assistant/internal/redact"

// redact.go scrubs secret-looking values out of tool args BEFORE they render in the
// approval sheet (argsBlock) or the expanded ^X activity row (compactArgs).
//
// The patterns themselves live in internal/redact, shared with the debug log and the
// durable audit rows. They used to be duplicated here, which is the wrong shape for a
// security control: three copies drift, and the copy that falls behind is invisible until
// the day it matters. It also meant the UI could not benefit from RegisterSecret — the
// exact-value protection for this process's own API key and MCP token.
//
// Why the display path needs redaction at all, given the tool layer already refuses to
// READ credential-bearing files (safety.IsSensitivePath): the ARGS reach the screen
// verbatim. A terminal.sendCommand of `export TOKEN=sk-…`, a daintree.call carrying an
// Authorization header, a git remote with an inline token. And the cockpit renders on the
// terminal's NORMAL screen buffer, so anything shown persists in the host's native
// scrollback long after the session and is never cleared.
func redactArgs(s string) string { return redact.String(s) }
