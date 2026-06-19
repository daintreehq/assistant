/**
 * Audit export tool: filter the durable audit log and serialize the result as
 * JSON or CSV. Read-only — it only queries `audit_log` via `Db.queryAudit` and
 * returns the serialized content in the tool result; it never touches the
 * filesystem, so it sits at `risk: "read"` and respects the no-file-edit
 * invariant. The caller (the model, or a CLI user via `/audit export`) decides
 * what to do with the returned string.
 */
import { z } from "zod";
import { ok, type ToolDef } from "./types.js";
import type { AuditRecord } from "../schemas.js";
import type { AuditFilters } from "../storage/db.js";

/** Column order for CSV/JSON export — mirrors the `audit_log` table declaration. */
export const AUDIT_EXPORT_COLUMNS = [
  "id",
  "ts",
  "actor",
  "toolName",
  "argsJson",
  "outcome",
  "durationMs",
  "summary",
  "resultJson",
  "grantSource",
  "grantId",
] as const;

/**
 * Escape a single CSV field per RFC 4180: wrap in double quotes when the value
 * contains a comma, double quote, CR or LF, and double any embedded quote.
 * `null`/`undefined` (nullable columns like `resultJson`) render as empty.
 */
function csvField(value: unknown): string {
  const s = value == null ? "" : String(value);
  return /[",\r\n]/.test(s) ? `"${s.replaceAll('"', '""')}"` : s;
}

/** Serialize audit rows to RFC 4180 CSV with a header row (CRLF line endings). */
export function auditToCsv(rows: AuditRecord[]): string {
  const lines = [AUDIT_EXPORT_COLUMNS.join(",")];
  for (const row of rows) {
    const r = row as unknown as Record<string, unknown>;
    lines.push(AUDIT_EXPORT_COLUMNS.map((col) => csvField(r[col])).join(","));
  }
  return lines.join("\r\n");
}

/** Serialize audit rows in the requested format. */
export function serializeAudit(
  rows: AuditRecord[],
  format: "json" | "csv",
): string {
  return format === "csv" ? auditToCsv(rows) : JSON.stringify(rows, null, 2);
}

/**
 * Parse the CLI form `export <json|csv> [key=value …]` shared by both `/audit`
 * surfaces (Ink + classic REPL). `tokens` is everything after the `export`
 * keyword. Accepted keys: actor, tool/toolName, outcome, from/tsFrom, to/tsTo,
 * limit. Returns the validated format + filters, or an `error` string to show.
 */
export function parseAuditExportArgs(
  tokens: string[],
): { format: "json" | "csv"; filters: AuditFilters } | { error: string } {
  const format = tokens[0];
  if (format !== "json" && format !== "csv") {
    return { error: "Usage: /audit export <json|csv> [actor=… tool=… outcome=… from=<ms> to=<ms> limit=<n>]" };
  }
  const filters: AuditFilters = {};
  for (const tok of tokens.slice(1)) {
    const eq = tok.indexOf("=");
    if (eq <= 0) return { error: `Bad filter '${tok}'. Use key=value (e.g. actor=main).` };
    const key = tok.slice(0, eq);
    const value = tok.slice(eq + 1);
    switch (key) {
      case "actor":
        filters.actor = value;
        break;
      case "tool":
      case "toolName":
        filters.toolName = value;
        break;
      case "outcome":
        filters.outcome = value;
        break;
      case "from":
      case "tsFrom": {
        const n = Number(value);
        if (!Number.isInteger(n)) return { error: `from must be an integer (Unix ms), got '${value}'.` };
        filters.tsFrom = n;
        break;
      }
      case "to":
      case "tsTo": {
        const n = Number(value);
        if (!Number.isInteger(n)) return { error: `to must be an integer (Unix ms), got '${value}'.` };
        filters.tsTo = n;
        break;
      }
      case "limit": {
        const n = Number(value);
        if (!Number.isInteger(n) || n < 1 || n > 5000) {
          return { error: `limit must be an integer 1–5000, got '${value}'.` };
        }
        filters.limit = n;
        break;
      }
      default:
        return { error: `Unknown filter '${key}'. Use actor, tool, outcome, from, to, limit.` };
    }
  }
  return { format, filters };
}

const ExportArgs = z.object({
  format: z
    .enum(["json", "csv"])
    .describe("Output format for the exported rows."),
  actor: z
    .string()
    .optional()
    .describe("Filter by actor (main, watcher, timer, workflow, system)."),
  toolName: z
    .string()
    .optional()
    .describe("Filter by tool name (exact match, e.g. fs.read)."),
  outcome: z
    .string()
    .optional()
    .describe("Filter by outcome (ok, error, denied, dedup, grant_ok)."),
  tsFrom: z
    .number()
    .int()
    .optional()
    .describe("Inclusive start of the time range, Unix epoch milliseconds."),
  tsTo: z
    .number()
    .int()
    .optional()
    .describe("Inclusive end of the time range, Unix epoch milliseconds."),
  limit: z
    .number()
    .int()
    .min(1)
    .max(5000)
    .optional()
    .default(200)
    .describe("Maximum rows to return (newest first). Default 200, max 5000."),
});
type ExportArgs = z.infer<typeof ExportArgs>;

export const auditTools: ToolDef[] = [
  {
    name: "audit.export",
    description:
      "Export the audit log as JSON or CSV with optional filters (actor, toolName, outcome, and a tsFrom/tsTo time range in Unix ms). Filters are AND-combined; rows are returned newest-first and bounded by `limit`. Read-only — returns the serialized content as a string for the caller to save or inspect.",
    risk: "read",
    readOnly: true,
    schema: ExportArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        format: {
          type: "string",
          enum: ["json", "csv"],
          description: "Output format for the exported rows.",
        },
        actor: {
          type: "string",
          description: "Filter by actor (main, watcher, timer, workflow, system).",
        },
        toolName: {
          type: "string",
          description: "Filter by tool name (exact match, e.g. fs.read).",
        },
        outcome: {
          type: "string",
          description: "Filter by outcome (ok, error, denied, dedup, grant_ok).",
        },
        tsFrom: {
          type: "number",
          description: "Inclusive start of the time range, Unix epoch milliseconds.",
        },
        tsTo: {
          type: "number",
          description: "Inclusive end of the time range, Unix epoch milliseconds.",
        },
        limit: {
          type: "number",
          description: "Maximum rows to return (newest first). Default 200, max 5000.",
        },
      },
      required: ["format"],
    },
    async handler(args: ExportArgs, ctx) {
      const filters: AuditFilters = {
        actor: args.actor,
        toolName: args.toolName,
        outcome: args.outcome,
        tsFrom: args.tsFrom,
        tsTo: args.tsTo,
        limit: args.limit,
      };
      const rows = ctx.db.queryAudit(filters);
      const content = serializeAudit(rows, args.format);
      return ok(
        `Exported ${rows.length} audit row${rows.length === 1 ? "" : "s"} as ${args.format.toUpperCase()}.`,
        { format: args.format, count: rows.length, content },
      );
    },
  },
];
