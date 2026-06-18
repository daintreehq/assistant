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
}

const HELP_TEXT = [
  "/status                 Daintree connection, project, models, tier",
  "/inbox [sev]            queued watcher/timer events (info|attention|urgent)",
  "/tools [query]          list/search available tools",
  "/timers                 scheduled timers",
  "/watchers               active watchers",
  "/audit [n]              recent tool calls (default 15)",
  "/models                 model routing",
  "/permissions [tier]     show or set tier (supervisor|operator|system)",
  "/recipes [sub]          loaded | reload | load <id…> | clear",
  "/compact                summarize + condense the conversation",
  "/doctor                 check MCP / config / project mapping (with fixes)",
  "/reconnect              retry the Daintree MCP connection",
  "/help                   this help",
  "/quit                   exit",
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

    default:
      return {
        handled: true,
        title: "Unknown command",
        text: `Unknown command /${cmd}. Try /help.`,
      };
  }
}
