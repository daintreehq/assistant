/**
 * Slash command handling for the REPL. Commands are local, fast, and never call
 * the large model except /compact.
 */
import type { App } from "./app.js";
import { render, c } from "./render.js";
import { describeConfig } from "../config.js";
import { Tier } from "../schemas.js";

export interface CommandResult {
  handled: boolean;
  quit?: boolean;
}

const HELP = `${c.bold("Commands")}
  /status                 Daintree connection, project, models, tier
  /inbox [sev]            queued watcher/timer events (sev: info|attention|urgent)
  /tools [query]          list/search available tools
  /timers                 scheduled timers
  /watchers               active watchers
  /audit [n]              recent tool calls (default 15)
  /models                 model routing
  /recipes [sub]          assistant recipes (loaded|reload|load <id…>|clear)
  /permissions [tier]     show or set tier (supervisor|operator|system)
  /compact                summarize + reset the conversation
  /doctor                 check MCP / config / project mapping
  /help                   this help
  /quit                   exit

Anything else is sent to the assistant.`;

export async function handleSlashCommand(
  line: string,
  app: App,
): Promise<CommandResult> {
  const [cmd, ...rest] = line.slice(1).trim().split(/\s+/);
  const arg = rest.join(" ");

  switch (cmd) {
    case "quit":
    case "exit":
    case "q":
      return { handled: true, quit: true };

    case "help":
    case "?":
      render.line(HELP);
      return { handled: true };

    case "status": {
      const d = describeConfig(app.config);
      const st = app.mcp.status();
      render.line(c.bold("\nStatus"));
      render.line(
        `  Daintree MCP : ${st.connected ? c.green(`connected (${st.transport}, ${st.toolCount ?? "?"} tools)`) : c.yellow(`disconnected — ${st.error ?? "no url/token"}`)}`,
      );
      for (const [k, v] of Object.entries(d)) render.line(`  ${k.padEnd(13)}: ${v}`);
      return { handled: true };
    }

    case "inbox": {
      const sev = (["info", "attention", "urgent", "blocked"].includes(arg)
        ? arg
        : undefined) as "info" | "attention" | "urgent" | "blocked" | undefined;
      const events = app.queue.digest({ severityAtLeast: sev, maxItems: 30 });
      render.line(c.bold(`\nInbox (${events.length})`));
      render.line(app.queue.format(events));
      return { handled: true };
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
      render.line(c.bold(`\nTools (${matches.length}/${all.length})`));
      for (const t of matches) {
        render.line(`  ${c.cyan(t.name.padEnd(26))} ${c.gray(`[${t.risk}]`)} ${t.description}`);
      }
      return { handled: true };
    }

    case "timers": {
      const timers = app.db.listTimers("scheduled");
      render.line(c.bold(`\nTimers (${timers.length})`));
      for (const t of timers) {
        render.line(
          `  ${c.cyan(t.id)} ${new Date(t.fireAt).toLocaleString()} — ${t.title} ${c.gray(`(${t.payloadType})`)}`,
        );
      }
      if (!timers.length) render.line(c.gray("  (none)"));
      return { handled: true };
    }

    case "watchers": {
      const watchers = app.db.listWatchers("active");
      render.line(c.bold(`\nWatchers (${watchers.length})`));
      for (const w of watchers) {
        render.line(
          `  ${c.cyan(w.id)} ${w.title} — ${c.gray(w.goal)} ${c.gray(`[${w.lastClassification ?? "pending"}]`)}`,
        );
      }
      if (!watchers.length) render.line(c.gray("  (none)"));
      return { handled: true };
    }

    case "audit": {
      const n = Number(arg) || 15;
      const rows = app.db.listAudit(n);
      render.line(c.bold(`\nAudit (last ${rows.length})`));
      for (const r of rows) {
        const mark = r.outcome === "ok" ? c.green("ok") : r.outcome === "denied" ? c.yellow("denied") : c.red(r.outcome);
        render.line(
          `  ${c.gray(new Date(r.ts).toLocaleTimeString())} ${r.toolName.padEnd(22)} ${mark} ${c.gray(`${r.durationMs}ms`)} — ${r.summary}`,
        );
      }
      return { handled: true };
    }

    case "models": {
      const m = app.router.describe();
      render.line(c.bold("\nModels"));
      for (const [k, v] of Object.entries(m)) render.line(`  ${k.padEnd(7)}: ${v}`);
      return { handled: true };
    }

    case "permissions": {
      if (arg) {
        const parsed = Tier.safeParse(arg);
        if (!parsed.success) {
          render.error(`Unknown tier '${arg}'. Use supervisor | operator | system.`);
          return { handled: true };
        }
        app.config.tier = parsed.data;
        app.session.refreshRuntimeContext(app.promptContext());
        render.success(`Tier set to ${parsed.data}.`);
      } else {
        render.line(`\nCurrent tier: ${c.bold(app.config.tier)}`);
        render.line(c.gray("  supervisor = read-only · operator = +spawn/create · system = +git/destructive"));
      }
      return { handled: true };
    }

    case "doctor": {
      render.line(c.bold("\nDoctor"));
      render.line(`  project path   : ${app.config.projectPath}`);
      render.line(`  state dir      : ${app.config.stateDir}`);
      render.line(`  fireworks key  : ${app.config.fireworksApiKey ? c.green("present") : c.red("MISSING")}`);
      render.line(`  mcp url        : ${app.config.mcpUrl ?? c.yellow("(unset)")}`);
      render.line(`  mcp token      : ${app.config.mcpToken ? c.green("present") : c.yellow("(unset)")}`);
      const st = app.mcp.status();
      render.line(`  mcp connection : ${st.connected ? c.green("ok") : c.yellow(st.error ?? "not connected")}`);
      render.line(`  tools loaded   : ${app.registry.list().length}`);
      return { handled: true };
    }

    case "compact": {
      render.info("Compacting conversation…");
      try {
        const msgs = app.session.getMessages().filter((m) => m.role !== "system");
        const transcript = msgs
          .map((m) => `${m.role}: ${m.content ?? "[tool call]"}`)
          .join("\n")
          .slice(0, 12000);
        const res = await app.router.chat("small", {
          messages: [
            { role: "system", content: "Summarize this assistant session into a tight brief: goals, decisions, open watchers/timers, and next steps. <= 200 words." },
            { role: "user", content: transcript || "(empty)" },
          ],
          maxTokens: 400,
        });
        app.session.injectNote(`Compacted summary of earlier conversation:\n${res.content}`);
        render.success("Conversation compacted.");
      } catch (e) {
        render.error(`Compaction failed: ${e instanceof Error ? e.message : String(e)}`);
      }
      return { handled: true };
    }

    case "recipes": {
      const sub = rest[0];
      if (!sub) {
        const all = app.recipes.list();
        render.line(c.bold(`\nRecipes (${all.length})`));
        for (const r of all) {
          render.line(
            `  ${c.cyan(r.id)} ${c.gray(`[${r.risk}]`)} ${r.title} — ${r.summary}`,
          );
        }
        render.line(
          c.gray("\n  /recipes loaded | reload | load <id…> | clear"),
        );
        return { handled: true };
      }
      if (sub === "loaded") {
        render.line(`\n${app.session.describeRecipes()}`);
        return { handled: true };
      }
      if (sub === "clear") {
        app.session.setRecipes([]);
        render.success("Cleared loaded recipes.");
        return { handled: true };
      }
      if (sub === "load") {
        const ids = rest.slice(1);
        if (ids.length === 0) {
          render.warn("Usage: /recipes load <id> [<id>…]");
          return { handled: true };
        }
        const known = ids.filter((id) => app.recipes.has(id));
        const unknown = ids.filter((id) => !app.recipes.has(id));
        if (unknown.length) {
          render.warn(`Unknown recipe id(s): ${unknown.join(", ")}`);
        }
        if (known.length === 0) {
          // Don't clear the loaded set just because every id was a typo.
          render.warn("No known recipe ids given; loaded recipes unchanged.");
          return { handled: true };
        }
        if (new Set(known).size > 3) {
          render.warn("More than 3 recipes given; loading the first 3.");
        }
        app.session.setRecipes(known);
        render.line(app.session.describeRecipes());
        return { handled: true };
      }
      if (sub === "reload") {
        render.info("Re-selecting recipes…");
        try {
          const ok = await app.session.forceRecipeRefresh();
          if (ok) {
            render.success("Recipe selection refreshed.");
          } else {
            render.warn("Selector unavailable; kept existing recipes.");
          }
          render.line(app.session.describeRecipes());
        } catch (e) {
          render.error(
            `Recipe refresh failed: ${e instanceof Error ? e.message : String(e)}`,
          );
        }
        return { handled: true };
      }
      render.warn("Usage: /recipes [loaded|reload|load <id…>|clear]");
      return { handled: true };
    }

    default:
      render.warn(`Unknown command /${cmd}. Try /help.`);
      return { handled: true };
  }
}
