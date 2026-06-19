/**
 * Slash commands as structured data. The Ink UI renders these as command cards
 * (and may switch a panel) instead of printing to stdout. The legacy REPL keeps
 * using commands.ts. Both share the same App accessors.
 */
import path from "node:path";
import fs from "node:fs";
import type { App } from "./app.js";
import { describeConfig } from "../config.js";
import { Tier } from "../schemas.js";
import type {
  AuditRecord,
  RunEventRecord,
  RunSummaryRecord,
} from "../schemas.js";
import { parseAuditExportArgs, serializeAudit } from "../tools/auditTools.js";
import { helpLines } from "../commandRegistry.js";

/** Bound an MCP call so a stalled server can't hang a diagnostic command. */
function withTimeout<T>(p: Promise<T>, ms: number, what: string): Promise<T> {
  return Promise.race([
    p,
    new Promise<T>((_, reject) => {
      const t = setTimeout(
        () => reject(new Error(`${what} timed out after ${ms}ms`)),
        ms,
      );
      t.unref();
    }),
  ]);
}

export type PanelKey = "watchers" | "inbox" | "timers" | "audit" | "help";

/** Parse a run-event payload defensively; a malformed row must never throw. */
function parseEventPayload(payload?: string): Record<string, unknown> {
  if (!payload) return {};
  try {
    const v = JSON.parse(payload);
    return v && typeof v === "object" ? (v as Record<string, unknown>) : {};
  } catch {
    return {};
  }
}

/** One-line, length-bounded preview of a tool call's arguments. */
function previewArgs(args: unknown): string {
  if (args === undefined || args === null) return "";
  let s: string;
  try {
    s = typeof args === "string" ? args : JSON.stringify(args);
  } catch {
    return "";
  }
  if (!s || s === "{}") return "";
  return s.length > 120 ? `${s.slice(0, 117)}…` : s;
}

/** Indent every line of a (possibly multi-line) block by `pad` spaces. */
function indent(text: string, pad = "    "): string {
  return text
    .split("\n")
    .map((l) => `${pad}${l}`)
    .join("\n");
}

/**
 * Render a single run's event log as a replayable plain-text timeline, shared by
 * the Ink command card and the REPL so both surfaces read identically. Events are
 * assumed to arrive in `seq` order (as `Db.listRunEvents` returns them). Tool
 * results are enriched from `auditRows` (matched by `auditId`) for outcome/duration
 * when the call reached dispatch; calls that didn't leave an audit row fall back to
 * the event's own summary.
 */
export function formatRunTimeline(
  events: RunEventRecord[],
  auditRows: AuditRecord[],
): string {
  if (events.length === 0) return "(no events found for this run)";
  const auditById = new Map(auditRows.map((r) => [r.id, r]));
  const lines: string[] = [];
  for (const ev of events) {
    const p = parseEventPayload(ev.payload);
    // An oversized row is stored as a `{truncated, bytes, preview}` wrapper (see
    // serializePayload), so its `content`/`reasoning`/`summary` fields are gone.
    // Surface the truncation + preview rather than rendering an empty entry.
    if (p.truncated === true) {
      lines.push(`… [truncated ${ev.type} — ${p.bytes ?? "?"} bytes]`);
      const preview = String(p.preview ?? "").trim();
      if (preview) lines.push(indent(preview));
      continue;
    }
    switch (ev.type) {
      case "assistant:start":
        lines.push("▸ assistant");
        break;
      case "assistant:content": {
        const content = String(p.content ?? "").trim();
        if (content) lines.push(indent(content));
        break;
      }
      case "assistant:end": {
        const reasoning = String(p.reasoning ?? "").trim();
        if (reasoning) {
          lines.push("  reasoning:");
          lines.push(indent(reasoning, "      "));
        }
        const content = String(p.content ?? "").trim();
        if (content) lines.push(indent(content));
        break;
      }
      case "assistant:cancelled": {
        const content = String(p.content ?? "").trim();
        lines.push(`■ cancelled${content ? ":" : ""}`);
        if (content) lines.push(indent(content));
        break;
      }
      case "tool:call": {
        const args = previewArgs(p.args);
        lines.push(`→ tool ${String(p.name ?? "?")}${args ? ` ${args}` : ""}`);
        break;
      }
      case "tool:result": {
        const audit =
          typeof p.auditId === "string" ? auditById.get(p.auditId) : undefined;
        const ok = p.ok === true;
        const mark = ok ? "✓" : "✗";
        const meta = audit
          ? `${audit.outcome}, ${audit.durationMs}ms`
          : ok
            ? "ok"
            : "error";
        lines.push(`${mark} tool ${String(p.name ?? "?")} (${meta})`);
        const summary = String(p.summary ?? "").trim();
        if (summary) lines.push(indent(summary));
        break;
      }
      case "error":
        lines.push(`⚠ error: ${String(p.message ?? "").trim()}`);
        break;
      case "info":
        lines.push(`· ${String(p.message ?? "").trim()}`);
        break;
      default:
        // Unknown event type — surface it rather than silently dropping a row.
        lines.push(`· ${ev.type}`);
        break;
    }
  }
  return lines.join("\n");
}

/** A one-line-per-run listing for no-argument `/explain` (run discovery). */
export function formatRunList(runs: RunSummaryRecord[]): string {
  if (runs.length === 0) {
    return "(no runs recorded yet — runs are logged as the assistant works)";
  }
  return runs
    .map(
      (r) =>
        `${r.runId.padEnd(16)} ${new Date(r.firstTs).toLocaleString()}  ${r.eventCount} event${r.eventCount === 1 ? "" : "s"}`,
    )
    .join("\n");
}

export interface DoctorCheck {
  label: string;
  ok: boolean;
  detail: string;
  /** Suggested remedy when the check failed. */
  fix?: string;
}

/**
 * Actionable environment diagnosis shared by the Ink UI and the REPL. Attempts a
 * safe reconnect when credentials exist but the connection is down, then reports
 * each prerequisite with a concrete fix for the ones that fail.
 */
export async function runDoctor(app: App): Promise<DoctorCheck[]> {
  const cfg = app.config;
  if (!app.mcp.isConnected() && cfg.mcpUrl && cfg.mcpToken) {
    try {
      await app.reconnectMcp();
    } catch {
      /* failure is reported by the connection check below */
    }
  }
  const st = app.mcp.status();
  const checks: DoctorCheck[] = [];
  const need = (v: string | undefined, env: string) =>
    v ? undefined : `set ${env}`;

  checks.push({
    label: "fireworks key",
    ok: !!cfg.fireworksApiKey,
    detail: cfg.fireworksApiKey ? "present" : "MISSING",
    fix: need(cfg.fireworksApiKey, "FIREWORKS_API_KEY in .env or the environment"),
  });
  checks.push({ label: "large model", ok: !!cfg.largeModel, detail: cfg.largeModel || "(unset)", fix: need(cfg.largeModel, "DAINTREE_LARGE_MODEL") });
  checks.push({ label: "small model", ok: !!cfg.smallModel, detail: cfg.smallModel || "(unset)", fix: need(cfg.smallModel, "DAINTREE_SMALL_MODEL") });
  checks.push({ label: "mcp url", ok: !!cfg.mcpUrl, detail: cfg.mcpUrl ?? "(unset)", fix: need(cfg.mcpUrl, "DAINTREE_MCP_URL to Daintree's MCP endpoint") });
  checks.push({ label: "mcp token", ok: !!cfg.mcpToken, detail: cfg.mcpToken ? "present" : "(unset)", fix: need(cfg.mcpToken, "DAINTREE_MCP_TOKEN") });
  checks.push({
    label: "mcp connection",
    ok: st.connected,
    detail: st.connected ? `ok (${st.transport})` : st.error ?? "not connected",
    fix: st.connected ? undefined : "start Daintree, then run /reconnect",
  });
  checks.push({
    label: "mcp tools",
    ok: st.connected && (st.toolCount ?? 0) > 0,
    detail: st.connected ? `${st.toolCount ?? 0} tools` : "unavailable",
    fix:
      st.connected && !(st.toolCount ?? 0)
        ? "connected but no tools listed; run /reconnect"
        : undefined,
  });

  // Live functional probe: list/connection can be "up" while the token lacks the
  // tier to actually call a tool. actions.getContext is workbench tier (read-only,
  // no confirmation), so it verifies end-to-end access without mutating anything.
  if (st.connected) {
    const probeTool = "actions.getContext";
    try {
      const advertised = await withTimeout(app.mcp.listTools(), 5_000, "listTools");
      if (!advertised.some((t) => t.name === probeTool)) {
        checks.push({
          label: "mcp probe",
          ok: false,
          detail: `${probeTool} not advertised — workbench tier may be unavailable`,
          fix: "verify the MCP token grants at least workbench tier",
        });
      } else {
        const startedAt = Date.now();
        const res = await withTimeout(app.mcp.callTool(probeTool, {}), 5_000, probeTool);
        const ms = Date.now() - startedAt;
        checks.push({
          label: "mcp probe",
          ok: !res.isError,
          detail: res.isError
            ? `${probeTool} returned an error: ${res.text || "(no detail)"}`
            : `${probeTool} ok (${ms}ms)`,
          fix: res.isError ? "check Daintree tier/permissions; run /reconnect" : undefined,
        });
      }
    } catch (e) {
      // A throw here is a live connection/transport failure (or timeout) — NOT a
      // tier issue — so report it as such rather than "tool not advertised".
      checks.push({
        label: "mcp probe",
        ok: false,
        detail: `probe failed: ${e instanceof Error ? e.message : String(e)}`,
        fix: "connection may be stale; run /reconnect",
      });
    }
  }

  let writable = false;
  let writeErr = "";
  try {
    const probe = path.join(cfg.stateDir, ".doctor-probe");
    fs.writeFileSync(probe, "ok");
    fs.unlinkSync(probe);
    writable = true;
  } catch (e) {
    writeErr = e instanceof Error ? e.message : String(e);
  }
  checks.push({
    label: "state writable",
    ok: writable,
    detail: writable ? cfg.stateDir : `not writable: ${writeErr}`,
    fix: writable ? undefined : "ensure the state dir is writable or set DAINTREE_ASSISTANT_STATE_DIR",
  });

  let projOk = false;
  try {
    projOk = fs.statSync(cfg.projectPath).isDirectory();
  } catch {
    /* projOk stays false */
  }
  checks.push({
    label: "project path",
    ok: projOk,
    detail: cfg.projectPath,
    fix: projOk ? undefined : "pass --project <dir> or run from the project root",
  });
  if (st.connected && st.driftToolNames?.length) {
    const names = st.driftToolNames;
    checks.push({
      label: "mcp drift",
      ok: true,
      detail: `${names.length} documented tool(s) not advertised at this tier/plugin config: ${names.join(", ")}`,
    });
  }
  checks.push({ label: "tier", ok: true, detail: cfg.tier });
  checks.push({ label: "tools loaded", ok: app.registry.list().length > 0, detail: String(app.registry.list().length) });
  return checks;
}

function formatDoctor(checks: DoctorCheck[]): string {
  return checks
    .map((c) => {
      const mark = c.ok ? "✓" : "✗";
      const fix = !c.ok && c.fix ? `  → ${c.fix}` : "";
      return `${mark} ${c.label.padEnd(16)}: ${c.detail}${fix}`;
    })
    .join("\n");
}

export interface UiCommandResult {
  handled: boolean;
  quit?: boolean;
  title?: string;
  text?: string;
  switchPanel?: PanelKey;
  /**
   * Set by `/clear`: the conversation state was reset to its initial controls, so
   * the controller should also wipe the in-flight transcript and remount `<Static>`
   * (committed scrollback already in the host terminal stays — same as shell clear).
   */
  clearTranscript?: boolean;
}

const HELP_TEXT = [
  ...helpLines(),
  "",
  "Keys: ? help · ^O toggle ops deck · ^C exit. Anything else goes to the assistant.",
].join("\n");

export async function handleUiCommand(
  line: string,
  app: App,
): Promise<UiCommandResult> {
  const [cmd, ...rest] = line.slice(1).trim().split(/\s+/);
  const arg = rest.join(" ");

  switch (cmd) {
    case "quit":
    case "exit":
    case "q":
      return { handled: true, quit: true };

    case "help":
    case "?":
      return { handled: true, switchPanel: "help", title: "Help", text: HELP_TEXT };

    case "status": {
      const d = describeConfig(app.config);
      const st = app.mcp.status();
      return {
        handled: true,
        title: "Status",
        text: [
          `Daintree MCP: ${
            st.connected
              ? `connected (${st.transport}, ${st.toolCount ?? "?"} tools)`
              : `disconnected — ${st.error ?? "no url/token"}`
          }`,
          ...Object.entries(d).map(([k, v]) => `${k}: ${v}`),
        ].join("\n"),
      };
    }

    case "inbox": {
      const sev = (["info", "attention", "urgent", "blocked"].includes(arg)
        ? arg
        : undefined) as "info" | "attention" | "urgent" | "blocked" | undefined;
      const events = app.queue.digest({ severityAtLeast: sev, maxItems: 30 });
      return {
        handled: true,
        switchPanel: "inbox",
        title: `Inbox (${events.length})`,
        text: app.queue.format(events),
      };
    }

    case "tools": {
      const all = app.registry.list();
      const q = arg.toLowerCase();
      const matches = q
        ? all.filter(
            (t) =>
              t.name.toLowerCase().includes(q) ||
              t.description.toLowerCase().includes(q),
          )
        : all;
      return {
        handled: true,
        title: `Tools (${matches.length}/${all.length})`,
        text: matches
          .map((t) => `${t.name.padEnd(26)} [${t.risk}] ${t.description}`)
          .join("\n"),
      };
    }

    case "timers": {
      const timers = app.db.listTimers("scheduled");
      return {
        handled: true,
        switchPanel: "timers",
        title: `Timers (${timers.length})`,
        text:
          timers
            .map(
              (t) =>
                `${t.id}  ${new Date(t.fireAt).toLocaleString()} — ${t.title} (${t.payloadType})`,
            )
            .join("\n") || "(none)",
      };
    }

    case "watchers": {
      const watchers = app.db.listWatchers("active");
      return {
        handled: true,
        switchPanel: "watchers",
        title: `Watchers (${watchers.length})`,
        text:
          watchers
            .map(
              (w) =>
                `${w.id}  ${w.title} — ${w.goal} [${w.lastClassification ?? "pending"}]`,
            )
            .join("\n") || "(none)",
      };
    }

    case "audit": {
      // `/audit export <json|csv> [filters]` returns the serialized export as the
      // panel text; plain `/audit [n]` keeps the existing recent-calls listing.
      if (rest[0] === "export") {
        const parsed = parseAuditExportArgs(rest.slice(1));
        if ("error" in parsed) {
          return { handled: true, switchPanel: "audit", title: "Audit export", text: parsed.error };
        }
        const rows = app.db.queryAudit(parsed.filters);
        const content = serializeAudit(rows, parsed.format);
        return {
          handled: true,
          switchPanel: "audit",
          title: `Audit export (${parsed.format}, ${rows.length} row${rows.length === 1 ? "" : "s"})`,
          text: content || "(none)",
        };
      }
      const n = Number(arg) || 15;
      const rows = app.db.listAudit(n);
      return {
        handled: true,
        switchPanel: "audit",
        title: `Audit (last ${rows.length})`,
        text:
          rows
            .map((r) => {
              // Tag grant-authorized calls with the grant's provenance so a local
              // grant is distinguishable from a (future) Daintree session grant.
              const outcome =
                r.outcome === "grant_ok" && r.grantSource
                  ? `grant_ok[${r.grantSource}]`
                  : r.outcome;
              return `${new Date(r.ts).toLocaleTimeString()} ${r.toolName.padEnd(22)} ${outcome} ${r.durationMs}ms — ${r.summary}`;
            })
            .join("\n") || "(none)",
      };
    }

    case "explain": {
      // No id → list recent runs so the user can pick one. The operations view is
      // a live dashboard, not a text pane, so this routes through the transcript
      // command card (no switchPanel) like /tools and /models.
      if (!arg) {
        const runs = app.db.listRuns(10);
        return {
          handled: true,
          title: `Explain — recent runs (${runs.length})`,
          text: `${formatRunList(runs)}\n\n/explain <runId> to replay one.`,
        };
      }
      const runId = arg;
      const events = app.db.listRunEvents(runId);
      if (events.length === 0) {
        return {
          handled: true,
          title: "Explain",
          text: `No events found for run '${runId}'. Use /explain to list recent runs.`,
        };
      }
      const auditRows = app.db.listAuditByRunId(runId);
      return {
        handled: true,
        title: `Explain ${runId} (${events.length} events)`,
        text: formatRunTimeline(events, auditRows),
      };
    }

    case "models": {
      const m = app.router.describe();
      return {
        handled: true,
        title: "Models",
        text: Object.entries(m)
          .map(([k, v]) => `${k.padEnd(7)}: ${v}`)
          .join("\n"),
      };
    }

    case "permissions": {
      if (!arg) {
        return {
          handled: true,
          title: "Permissions",
          text: [
            `Current tier: ${app.config.tier}`,
            "supervisor = read-only · operator = +spawn/create · system = +git/destructive",
          ].join("\n"),
        };
      }
      const parsed = Tier.safeParse(arg);
      if (!parsed.success) {
        return {
          handled: true,
          title: "Permissions",
          text: `Unknown tier '${arg}'. Use supervisor | operator | system.`,
        };
      }
      app.config.tier = parsed.data;
      app.session.refreshRuntimeContext(app.promptContext());
      return {
        handled: true,
        title: "Permissions",
        text: `Tier set to ${parsed.data}.`,
      };
    }

    case "recipes": {
      const sub = rest[0];
      if (!sub) {
        const all = app.recipes.list();
        return {
          handled: true,
          title: `Recipes (${all.length})`,
          text:
            all
              .map((r) => `${r.id}  [${r.risk}]  ${r.title} — ${r.summary}`)
              .join("\n") +
            "\n\n/recipes loaded | reload | load <id…> | clear",
        };
      }
      if (sub === "loaded") {
        return {
          handled: true,
          title: "Recipes",
          text: app.session.describeRecipes(),
        };
      }
      if (sub === "clear") {
        app.session.setRecipes([]);
        return { handled: true, title: "Recipes", text: "Cleared loaded recipes." };
      }
      if (sub === "load") {
        const ids = rest.slice(1);
        if (ids.length === 0) {
          return {
            handled: true,
            title: "Recipes",
            text: "Usage: /recipes load <id> [<id>…]",
          };
        }
        const known = ids.filter((id) => app.recipes.has(id));
        const unknown = ids.filter((id) => !app.recipes.has(id));
        if (known.length === 0) {
          return {
            handled: true,
            title: "Recipes",
            text: `No known recipe ids given; loaded recipes unchanged.${unknown.length ? ` Unknown: ${unknown.join(", ")}` : ""}`,
          };
        }
        app.session.setRecipes(known);
        const note = unknown.length
          ? `Unknown id(s) ignored: ${unknown.join(", ")}\n`
          : "";
        return {
          handled: true,
          title: "Recipes",
          text: note + app.session.describeRecipes(),
        };
      }
      if (sub === "reload") {
        try {
          const ok = await app.session.forceRecipeRefresh();
          return {
            handled: true,
            title: "Recipes",
            text: `${ok ? "Recipe selection refreshed." : "Selector unavailable; kept existing recipes."}\n${app.session.describeRecipes()}`,
          };
        } catch (e) {
          return {
            handled: true,
            title: "Recipes",
            text: `Recipe refresh failed: ${e instanceof Error ? e.message : String(e)}`,
          };
        }
      }
      return {
        handled: true,
        title: "Recipes",
        text: "Usage: /recipes [loaded|reload|load <id…>|clear]",
      };
    }

    case "doctor": {
      const checks = await runDoctor(app);
      return { handled: true, title: "Doctor", text: formatDoctor(checks) };
    }

    case "reconnect": {
      await app.reconnectMcp();
      const st = app.mcp.status();
      return {
        handled: true,
        title: "Reconnect",
        text: st.connected
          ? `Reconnected to Daintree MCP (${st.transport}, ${st.toolCount ?? "?"} tools).`
          : `Still not connected — ${st.error ?? "no url/token"}.`,
      };
    }

    case "compact": {
      try {
        const msgs = app.session
          .getMessages()
          .filter((m) => m.role !== "system");
        const transcript = msgs
          .map((m) => `${m.role}: ${m.content ?? "[tool call]"}`)
          .join("\n")
          .slice(0, 12000);
        const res = await app.router.chat("small", {
          messages: [
            {
              role: "system",
              content:
                "Summarize this assistant session into a tight brief: goals, decisions, open watchers/timers, and next steps. <= 200 words.",
            },
            { role: "user", content: transcript || "(empty)" },
          ],
          maxTokens: 400,
        });
        app.session.compact(res.content);
        return {
          handled: true,
          title: "Compact",
          text: "Conversation compacted — earlier turns replaced with a summary.",
        };
      } catch (e) {
        return {
          handled: true,
          title: "Compact",
          text: `Compaction failed: ${e instanceof Error ? e.message : String(e)}`,
        };
      }
    }

    case "clear": {
      app.session.clear();
      return {
        handled: true,
        title: "Clear",
        text: "Conversation cleared — starting fresh.",
        clearTranscript: true,
      };
    }

    default:
      return {
        handled: true,
        title: "Unknown command",
        text: `Unknown command /${cmd}. Try /help.`,
      };
  }
}
