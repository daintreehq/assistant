/**
 * Tool registry: holds every ToolDef, projects them to OpenAI function specs, and
 * dispatches calls through the safety policy with audit logging.
 */
import type { ChatTool } from "../models/fireworks.js";
import type { ToolResult } from "../schemas.js";
import { decide } from "../safety/policy.js";
import { assertNoFileEditTools } from "../safety/policy.js";
import { fail, type ToolContext, type ToolDef } from "./types.js";

export class ToolRegistry {
  private tools = new Map<string, ToolDef>();

  register(tool: ToolDef): void {
    if (this.tools.has(tool.name)) {
      throw new Error(`Duplicate tool registration: ${tool.name}`);
    }
    this.tools.set(tool.name, tool);
  }

  registerAll(tools: ToolDef[]): void {
    for (const t of tools) this.register(t);
  }

  get(name: string): ToolDef | undefined {
    return this.tools.get(name);
  }

  list(): ToolDef[] {
    return [...this.tools.values()];
  }

  /** Enforce the no-file-edit invariant across everything registered. */
  assertSafe(): void {
    assertNoFileEditTools([...this.tools.keys()]);
  }

  /** Project tools to OpenAI function-calling specs (optionally a subset). */
  toOpenAITools(filterNames?: string[]): ChatTool[] {
    const names = filterNames ? new Set(filterNames) : undefined;
    return this.list()
      .filter((t) => !names || names.has(t.name))
      .map((t) => ({
        type: "function" as const,
        function: {
          name: t.name,
          description: t.description,
          parameters: t.parameters,
        },
      }));
  }

  /**
   * Dispatch a tool call. Applies tier gating + confirmation, runs the handler,
   * and records an audit row. Never throws — failures come back as ToolResult.
   */
  async dispatch(
    name: string,
    rawArgs: unknown,
    ctx: ToolContext,
  ): Promise<ToolResult> {
    const started = Date.now();
    const tool = this.tools.get(name);
    if (!tool) {
      const res = fail("UNKNOWN_TOOL", `No such tool: ${name}`, {
        recoverable: false,
      });
      this.audit(ctx, name, rawArgs, res, started);
      return res;
    }

    // Validate args.
    let args = rawArgs ?? {};
    if (tool.schema) {
      const parsed = tool.schema.safeParse(args);
      if (!parsed.success) {
        const res = fail(
          "INVALID_ARGS",
          `Invalid arguments for ${name}: ${parsed.error.issues
            .map((i) => `${i.path.join(".")}: ${i.message}`)
            .join("; ")}`,
          { recoverable: true, details: parsed.error.issues },
        );
        this.audit(ctx, name, rawArgs, res, started);
        return res;
      }
      args = parsed.data;
    }

    // Safety policy.
    const decision = decide(tool.risk, ctx.config.tier);
    if (!decision.allowed) {
      const res = fail("TIER_DENIED", decision.reason ?? "denied", {
        recoverable: false,
      });
      this.audit(ctx, name, args, res, started, "denied");
      return res;
    }

    // Confirmation for mutating actions. Non-interactive actors (timer, watcher,
    // workflow) can NEVER run a confirm-required tool — this is what stops a
    // benign local timer from later invoking a high-risk tool unattended.
    if (decision.needsConfirmation) {
      if (ctx.actor !== "main") {
        const res = fail(
          "CONFIRMATION_REQUIRED",
          `${name} (${tool.risk}) needs user confirmation and cannot be run by a non-interactive '${ctx.actor}' actor.`,
          { recoverable: false },
        );
        this.audit(ctx, name, args, res, started, "denied");
        return res;
      }
      let approved = false;
      try {
        approved = await ctx.confirm({
          toolName: name,
          risk: tool.risk,
          summary: tool.description,
          args,
        });
      } catch {
        approved = false; // a failed prompt is a decline, never an approval
      }
      if (!approved) {
        const res = fail("USER_DECLINED", `User declined ${name}.`, {
          recoverable: true,
        });
        this.audit(ctx, name, args, res, started, "denied");
        return res;
      }
    }

    // Run.
    try {
      const res = await tool.handler(args, ctx);
      this.audit(ctx, name, args, res, started, res.ok ? "ok" : "error");
      return res;
    } catch (err) {
      const message = err instanceof Error ? err.message : String(err);
      const res = fail("TOOL_THREW", message, { recoverable: true });
      this.audit(ctx, name, args, res, started, "error");
      return res;
    }
  }

  private audit(
    ctx: ToolContext,
    name: string,
    args: unknown,
    res: ToolResult,
    started: number,
    outcome: "ok" | "error" | "denied" | "dedup" = res.ok ? "ok" : "error",
  ): void {
    try {
      const row = ctx.db.insertAudit({
        actor: ctx.actor,
        toolName: name,
        argsJson: capJson(safeJson(args)),
        outcome,
        durationMs: Date.now() - started,
        summary: res.summary,
        resultJson:
          res.result !== undefined ? capJson(safeJson(res.result)) : undefined,
      });
      res.auditId = row.id;
    } catch {
      /* auditing must never break a tool call */
    }
  }
}

/** Largest serialized args/result we persist to the audit log. */
const MAX_AUDIT_JSON = 4000;

function safeJson(v: unknown): string {
  try {
    return JSON.stringify(v) ?? "null";
  } catch {
    return '"<unserializable>"';
  }
}

/**
 * Bound the size of audited JSON. Tool results can contain large file contents
 * or terminal scrollback; the audit log keeps a redacted preview plus the byte
 * count rather than the full blob, so a single read can't bloat the DB.
 */
function capJson(s: string): string {
  if (s.length <= MAX_AUDIT_JSON) return s;
  return JSON.stringify({
    truncated: true,
    bytes: Buffer.byteLength(s, "utf8"),
    // Leave headroom for the wrapper + JSON escaping so the stored row stays
    // near the cap rather than ballooning past it.
    preview: s.slice(0, MAX_AUDIT_JSON - 200),
  });
}
