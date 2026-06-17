/**
 * Tool registry: holds every ToolDef, projects them to OpenAI function specs, and
 * dispatches calls through the safety policy with audit logging.
 */
import type { ChatTool } from "../models/fireworks.js";
import type { ToolResult } from "../schemas.js";
import { decide } from "../safety/policy.js";
import { assertNoFileEditTools } from "../safety/policy.js";
import { fail, type ToolContext, type ToolDef } from "./types.js";

/**
 * OpenAI (and the Fireworks OpenAI-compatible endpoint) constrains function
 * names to this pattern. Our internal tool names use dot notation (`fs.read`),
 * which the dot makes illegal — hence the wire-name alias layer below.
 */
const OPENAI_NAME_RE = /^[a-zA-Z0-9_-]{1,64}$/;

/** Sanitize an internal dotted tool name into an OpenAI-legal wire name. */
function toWireName(name: string): string {
  return name.replaceAll(".", "__");
}

export class ToolRegistry {
  private tools = new Map<string, ToolDef>();
  // Bidirectional alias maps between internal dotted names and OpenAI-legal
  // wire names, rebuilt on every toOpenAITools() projection so they always
  // reflect the most recent (possibly filtered) tool set sent to the model.
  private wireToInternal = new Map<string, string>();
  private internalToWire = new Map<string, string>();

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

  /**
   * Project tools to OpenAI function-calling specs (optionally a subset),
   * emitting OpenAI-legal wire names and recording the alias maps needed to
   * translate a model's tool call back to its internal dotted name. `filterNames`
   * are matched against internal names. Throws if a wire name is illegal or two
   * internal names collide on the same wire name — failing fast at projection
   * time rather than silently dispatching to the wrong tool.
   */
  toOpenAITools(filterNames?: string[]): ChatTool[] {
    const names = filterNames ? new Set(filterNames) : undefined;
    const selected = this.list().filter((t) => !names || names.has(t.name));

    const wireToInternal = new Map<string, string>();
    const internalToWire = new Map<string, string>();
    for (const t of selected) {
      const wire = toWireName(t.name);
      if (!OPENAI_NAME_RE.test(wire)) {
        throw new Error(
          `Tool '${t.name}' produces wire name '${wire}', which does not match ${OPENAI_NAME_RE}`,
        );
      }
      const existing = wireToInternal.get(wire);
      if (existing !== undefined && existing !== t.name) {
        throw new Error(
          `Wire-name collision: '${existing}' and '${t.name}' both map to '${wire}'`,
        );
      }
      wireToInternal.set(wire, t.name);
      internalToWire.set(t.name, wire);
    }
    this.wireToInternal = wireToInternal;
    this.internalToWire = internalToWire;

    return selected.map((t) => ({
      type: "function" as const,
      function: {
        name: internalToWire.get(t.name)!,
        description: t.description,
        parameters: t.parameters,
      },
    }));
  }

  /**
   * Translate an OpenAI wire name (as returned in a model tool call) back to the
   * internal dotted tool name. Returns undefined for an unknown wire name, so the
   * caller can decide the fallback. Only resolves names from the most recent
   * toOpenAITools() projection.
   */
  resolveWireName(wireName: string): string | undefined {
    return this.wireToInternal.get(wireName);
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
    // workflow) can NEVER run a confirm-required tool unattended — UNLESS a
    // scoped automation grant tied to that exact actor authorizes it. This is
    // what stops a benign local timer from later invoking a high-risk tool
    // unattended, while still allowing a user-minted, bounded follow-up.
    if (decision.needsConfirmation) {
      if (ctx.actor !== "main") {
        // A grant is scoped to a specific watcher/timer id, an allowlist of risk
        // classes or tool names, a TTL, and a remaining-uses counter. Consume one
        // use atomically; on success the call proceeds and is audited as
        // "grant_ok" so a grant-authorized mutation is distinguishable from an
        // interactive one.
        if (ctx.actorId) {
          const grant = ctx.db.consumeGrant(ctx.actorId, name, tool.risk, started);
          if (grant) {
            return this.runHandler(tool, name, args, ctx, started, "grant_ok");
          }
        }
        const res = fail(
          "CONFIRMATION_REQUIRED",
          `${name} (${tool.risk}) needs user confirmation and cannot be run by a non-interactive '${ctx.actor}' actor.`,
          { recoverable: false },
        );
        this.audit(ctx, name, args, res, started, "denied");
        // Surface the denial so the user can see their automation was blocked
        // and why. Low severity keeps it out of the proactive notifier, and a
        // stable dedupeKey (no tick-specific value) collapses repeated denials
        // of the same tool by the same actor into one count-bumped inbox row.
        // The actor id (when present) keeps distinct watchers/timers from
        // collapsing into one another's denial row.
        try {
          ctx.queue.publish({
            source: "system",
            severity: "info",
            title: `Autonomous action blocked: ${name}`,
            summary: res.summary,
            dedupeKey: `denied:${ctx.actor}:${ctx.actorId ? `${ctx.actorId}:` : ""}${name}`,
          });
        } catch {
          /* surfacing must never break a tool call */
        }
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
    return this.runHandler(tool, name, args, ctx, started);
  }

  /**
   * Invoke a tool handler and audit the result. `okOutcome` is the audit outcome
   * recorded on success — "ok" for a normal call, "grant_ok" when a scoped
   * automation grant authorized a non-interactive actor. A failing/throwing
   * handler always audits as "error" regardless (a grant-authorized failure is
   * still just an error). Never throws.
   */
  private async runHandler(
    tool: ToolDef,
    name: string,
    args: unknown,
    ctx: ToolContext,
    started: number,
    okOutcome: "ok" | "grant_ok" = "ok",
  ): Promise<ToolResult> {
    try {
      const res = await tool.handler(args, ctx);
      this.audit(ctx, name, args, res, started, res.ok ? okOutcome : "error");
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
    outcome: "ok" | "error" | "denied" | "dedup" | "grant_ok" = res.ok
      ? "ok"
      : "error",
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
