/**
 * Pure parsers for Daintree MCP tool results.
 *
 * The MCP CallToolResult spec (SEP-2200) makes `structuredContent` OPTIONAL: a
 * server MUST populate the `content[]` blocks but MAY omit `structuredContent`.
 * Daintree's terminal tools return their payload only in the text content blocks
 * (flattened into `McpCallResult.text`), never in `structuredContent`. Readers
 * that looked at `structuredContent` alone therefore got empty data on every
 * call — silently mis-detecting terminal state and rendering blank previews.
 *
 * These helpers read `structuredContent` first, then fall back to the text body,
 * so a call site works regardless of which field the server populated. They are
 * pure and never throw — a missing/garbled source yields the empty result.
 */

/** The subset of an MCP result these parsers read. */
export interface ParsableMcpResult {
  structuredContent?: unknown;
  text?: string;
}

/**
 * Extract a named array (e.g. `terminals`) from an MCP result, merging entries
 * found in BOTH `structuredContent[field]` and a JSON-encoded `text` body —
 * either may be the one the server populated. Mirrors `readTerminalList`'s
 * merged-parse path. Returns `[]` when neither source yields a recognizable
 * array. Never throws (non-JSON `text` is ignored).
 *
 * Order: structuredContent entries first, then text entries. In practice Daintree
 * populates exactly one source, so the order only matters if both ever carry the
 * same id — callers that key by id (e.g. a Map in readStatuses) then take the
 * LAST occurrence (text), while a `.find()` takes the FIRST (structuredContent).
 * Don't rely on the merged order for conflicting duplicates.
 */
export function parseMcpArray(res: ParsableMcpResult, field: string): unknown[] {
  const entries: unknown[] = [];
  const sc = (res.structuredContent ?? {}) as Record<string, unknown>;
  if (Array.isArray(sc[field])) entries.push(...(sc[field] as unknown[]));
  const text = res.text;
  if (typeof text === "string" && text.trim()) {
    try {
      const parsed = JSON.parse(text) as unknown;
      if (parsed && typeof parsed === "object") {
        const val = (parsed as Record<string, unknown>)[field];
        if (Array.isArray(val)) entries.push(...val);
      }
    } catch {
      /* not JSON — ignore this source */
    }
  }
  return entries;
}

/**
 * Read a string field from an MCP result, falling back to the raw `text` body
 * when the structured field is absent. Used for terminal scrollback, which
 * Daintree delivers as a RAW string in `content[].text` (not a JSON document) —
 * so the fallback is the raw `text`, never `JSON.parse`. Mirrors the
 * `contextTools.ts` pattern. Returns `undefined` when neither source provides a
 * string, so callers can distinguish "no content" from an empty string.
 */
export function parseMcpString(
  res: ParsableMcpResult,
  field: string,
): string | undefined {
  const sc = (res.structuredContent ?? {}) as Record<string, unknown>;
  if (typeof sc[field] === "string") return sc[field] as string;
  if (typeof res.text === "string") return res.text;
  return undefined;
}
