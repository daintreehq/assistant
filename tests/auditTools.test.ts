import { mkdtempSync, rmSync } from "node:fs";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { Db } from "../src/storage/db.js";
import type { AuditRecord } from "../src/schemas.js";
import {
  auditTools,
  auditToCsv,
  serializeAudit,
  parseAuditExportArgs,
  AUDIT_EXPORT_COLUMNS,
} from "../src/tools/auditTools.js";
import type { ToolContext } from "../src/tools/types.js";

function row(overrides: Partial<AuditRecord> = {}): AuditRecord {
  return {
    id: "aud_1",
    ts: 1000,
    actor: "main",
    toolName: "fs.read",
    argsJson: "{}",
    outcome: "ok",
    durationMs: 1,
    summary: "read",
    ...overrides,
  };
}

describe("auditToCsv (RFC 4180 serialization)", () => {
  it("emits a header row in the declared column order", () => {
    const csv = auditToCsv([]);
    expect(csv).toBe(AUDIT_EXPORT_COLUMNS.join(","));
  });

  it("renders nullable columns as empty fields", () => {
    const csv = auditToCsv([row()]);
    const dataLine = csv.split("\r\n")[1];
    // resultJson, grantSource, grantId are absent → trailing empty fields.
    expect(dataLine.endsWith(",,,")).toBe(true);
    expect(dataLine.startsWith("aud_1,1000,main,fs.read,{},ok,1,read,")).toBe(true);
  });

  it("quotes and escapes fields containing comma, quote, or newline", () => {
    const csv = auditToCsv([
      row({ summary: 'has "quote", and comma\nand newline' }),
    ]);
    const dataLine = csv.split("\r\n")[1];
    expect(dataLine).toContain('"has ""quote"", and comma\nand newline"');
  });

  it("uses CRLF line terminators", () => {
    const csv = auditToCsv([row(), row({ id: "aud_2" })]);
    expect(csv.split("\r\n")).toHaveLength(3);
  });
});

describe("serializeAudit", () => {
  it("produces parseable JSON for the json format", () => {
    const json = serializeAudit([row(), row({ id: "aud_2", ts: 2000 })], "json");
    const parsed = JSON.parse(json) as AuditRecord[];
    expect(parsed.map((r) => r.id)).toEqual(["aud_1", "aud_2"]);
  });

  it("delegates to the CSV serializer for the csv format", () => {
    expect(serializeAudit([row()], "csv")).toBe(auditToCsv([row()]));
  });
});

describe("parseAuditExportArgs", () => {
  it("parses a bare format", () => {
    expect(parseAuditExportArgs(["csv"])).toEqual({ format: "csv", filters: {} });
  });

  it("maps key=value filters including from/to/tool aliases", () => {
    const parsed = parseAuditExportArgs([
      "json",
      "actor=main",
      "tool=git.commit",
      "outcome=ok",
      "from=1000",
      "to=2000",
      "limit=10",
    ]);
    expect(parsed).toEqual({
      format: "json",
      filters: {
        actor: "main",
        toolName: "git.commit",
        outcome: "ok",
        tsFrom: 1000,
        tsTo: 2000,
        limit: 10,
      },
    });
  });

  it("rejects an invalid format", () => {
    const r = parseAuditExportArgs(["xml"]);
    expect("error" in r && r.error).toContain("Usage");
  });

  it("rejects an unknown filter key", () => {
    const r = parseAuditExportArgs(["csv", "bogus=1"]);
    expect("error" in r && r.error).toContain("Unknown filter");
  });

  it("rejects a non-integer time bound", () => {
    const r = parseAuditExportArgs(["csv", "from=soon"]);
    expect("error" in r && r.error).toContain("from must be an integer");
  });

  it("rejects an out-of-range limit", () => {
    const r = parseAuditExportArgs(["csv", "limit=99999"]);
    expect("error" in r && r.error).toContain("limit must be");
  });

  it("rejects an empty filter value (e.g. from=)", () => {
    expect("error" in parseAuditExportArgs(["json", "from="])).toBe(true);
    expect("error" in parseAuditExportArgs(["json", "to="])).toBe(true);
    expect("error" in parseAuditExportArgs(["json", "actor="])).toBe(true);
  });
});

describe("audit.export tool", () => {
  let dir: string;
  let db: Db;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), "audit-tool-"));
    db = new Db(join(dir, "state.db"));
    db.insertAudit({
      ts: 1000,
      actor: "main",
      toolName: "fs.read",
      argsJson: "{}",
      outcome: "ok",
      durationMs: 1,
      summary: "read",
    });
    db.insertAudit({
      ts: 2000,
      actor: "watcher",
      toolName: "git.commit",
      argsJson: "{}",
      outcome: "error",
      durationMs: 2,
      summary: "boom",
    });
  });

  afterEach(() => {
    db.close();
    rmSync(dir, { recursive: true, force: true });
  });

  const tool = auditTools.find((t) => t.name === "audit.export")!;

  it("is registered as a read-risk tool", () => {
    expect(tool).toBeDefined();
    expect(tool.risk).toBe("read");
  });

  it("exports filtered rows as CSV with a count", async () => {
    const ctx = { db } as unknown as ToolContext;
    const res = await tool.handler({ format: "csv", actor: "main" }, ctx);
    expect(res.ok).toBe(true);
    const result = res.result as { format: string; count: number; content: string };
    expect(result.format).toBe("csv");
    expect(result.count).toBe(1);
    expect(result.content.split("\r\n")[0]).toBe(AUDIT_EXPORT_COLUMNS.join(","));
    expect(result.content).toContain("fs.read");
    expect(result.content).not.toContain("git.commit");
  });

  it("exports as JSON honoring the limit", async () => {
    const ctx = { db } as unknown as ToolContext;
    const res = await tool.handler({ format: "json", limit: 1 }, ctx);
    const result = res.result as { count: number; content: string };
    expect(result.count).toBe(1);
    const parsed = JSON.parse(result.content) as AuditRecord[];
    expect(parsed).toHaveLength(1);
    expect(parsed[0].ts).toBe(2000); // newest first
  });
});
