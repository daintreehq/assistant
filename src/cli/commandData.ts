/**
 * Slash commands as structured data. The Ink UI renders these as command cards
 * (and may switch a panel) instead of printing to stdout. The legacy REPL keeps
 * using commands.ts. Both share the same App accessors.
 */
import type { App } from "./app.js";
import { describeConfig } from "../config.js";
import { Tier } from "../schemas.js";

export type PanelKey = "watchers" | "inbox" | "timers" | "audit" | "help";

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
  "/compact                summarize + condense the conversation",
  "/doctor                 check MCP / config / project mapping",
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
            .map(
              (r) =>
                `${new Date(r.ts).toLocaleTimeString()} ${r.toolName.padEnd(22)} ${r.outcome} ${r.durationMs}ms — ${r.summary}`,
            )
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
      app.session.refreshSystemPrompt(app.promptContext());
      return {
        handled: true,
        title: "Permissions",
        text: `Tier set to ${parsed.data}.`,
      };
    }

    case "doctor": {
      const st = app.mcp.status();
      return {
        handled: true,
        title: "Doctor",
        text: [
          `project path   : ${app.config.projectPath}`,
          `state dir      : ${app.config.stateDir}`,
          `fireworks key  : ${app.config.fireworksApiKey ? "present" : "MISSING"}`,
          `mcp url        : ${app.config.mcpUrl ?? "(unset)"}`,
          `mcp token      : ${app.config.mcpToken ? "present" : "(unset)"}`,
          `mcp connection : ${st.connected ? "ok" : st.error ?? "not connected"}`,
          `tools loaded   : ${app.registry.list().length}`,
        ].join("\n"),
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
        app.session.injectNote(
          `Compacted summary of earlier conversation:\n${res.content}`,
        );
        return {
          handled: true,
          title: "Compact",
          text: "Conversation compacted into a summary note.",
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
