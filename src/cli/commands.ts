/**
 * Slash command handling for the REPL. Commands are local, fast, and never call
 * the large model except /compact.
 */
import type { App } from "./app.js";
import { render, c } from "./render.js";
import { clearHostTerminal } from "./terminalClear.js";
import { describeConfig } from "../config.js";
import { Tier } from "../schemas.js";
import { runDoctor, formatRunTimeline, formatRunList } from "./commandData.js";
import { parseAuditExportArgs, serializeAudit } from "../tools/auditTools.js";
import { helpLines } from "../commandRegistry.js";
import { contentToText } from "../models/fireworks.js";

export interface CommandResult {
  handled: boolean;
  quit?: boolean;
}

const HELP = [
  c.bold("Commands"),
  ...helpLines().map((l) => `  ${l}`),
  "",
  "Anything else is sent to the assistant.",
].join("\n");

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
      // `/audit export <json|csv> [filters]` prints the serialized export;
      // plain `/audit [n]` keeps the existing recent-calls listing.
      if (rest[0] === "export") {
        const parsed = parseAuditExportArgs(rest.slice(1));
        if ("error" in parsed) {
          render.line(c.yellow(parsed.error));
          return { handled: true };
        }
        const rows = app.db.queryAudit(parsed.filters);
        const content = serializeAudit(rows, parsed.format);
        render.line(
          c.bold(`\nAudit export (${parsed.format}, ${rows.length} row${rows.length === 1 ? "" : "s"})`),
        );
        render.line(content || c.gray("(none)"));
        return { handled: true };
      }
      const n = Number(arg) || 15;
      const rows = app.db.listAudit(n);
      render.line(c.bold(`\nAudit (last ${rows.length})`));
      for (const r of rows) {
        // Tag grant-authorized rows with the grant's provenance, matching the
        // Ink UI's /audit rendering so both surfaces read the same.
        const label =
          r.outcome === "grant_ok" && r.grantSource
            ? `grant_ok[${r.grantSource}]`
            : r.outcome;
        const mark = r.outcome === "ok" ? c.green("ok") : r.outcome === "denied" ? c.yellow("denied") : c.red(label);
        render.line(
          `  ${c.gray(new Date(r.ts).toLocaleTimeString())} ${r.toolName.padEnd(22)} ${mark} ${c.gray(`${r.durationMs}ms`)} — ${r.summary}`,
        );
      }
      return { handled: true };
    }

    case "explain": {
      // Mirror the Ink handler: no id lists recent runs; an id replays one via the
      // shared timeline formatter so both surfaces read identically.
      if (!arg) {
        const runs = app.db.listRuns(10);
        render.line(c.bold(`\nExplain — recent runs (${runs.length})`));
        render.line(formatRunList(runs));
        render.line(c.gray("\n  /explain <runId> to replay one."));
        return { handled: true };
      }
      const runId = arg;
      const events = app.db.listRunEvents(runId);
      if (events.length === 0) {
        render.warn(
          `No events found for run '${runId}'. Use /explain to list recent runs.`,
        );
        return { handled: true };
      }
      const auditRows = app.db.listAuditByRunId(runId);
      render.line(c.bold(`\nExplain ${runId} (${events.length} events)`));
      render.line(formatRunTimeline(events, auditRows));
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
      const checks = await runDoctor(app);
      for (const ch of checks) {
        const mark = ch.ok ? c.green("✓") : c.red("✗");
        const fix = !ch.ok && ch.fix ? c.gray(`  → ${ch.fix}`) : "";
        render.line(`  ${mark} ${ch.label.padEnd(16)}: ${ch.detail}${fix}`);
      }
      return { handled: true };
    }

    case "reconnect": {
      render.info("Reconnecting to Daintree MCP…");
      await app.reconnectMcp();
      const st = app.mcp.status();
      if (st.connected) {
        render.success(`Reconnected (${st.transport}, ${st.toolCount ?? "?"} tools).`);
      } else {
        render.warn(`Still not connected — ${st.error ?? "no url/token"}.`);
      }
      return { handled: true };
    }

    case "compact": {
      render.info("Compacting conversation…");
      try {
        const msgs = app.session.getMessages().filter((m) => m.role !== "system");
        const transcript = msgs
          .map((m) => `${m.role}: ${contentToText(m.content) || "[tool call]"}`)
          .join("\n")
          .slice(0, 12000);
        const res = await app.router.chat("small", {
          messages: [
            { role: "system", content: "Summarize this assistant session into a tight brief: goals, decisions, open watchers/timers, and next steps. <= 200 words." },
            { role: "user", content: transcript || "(empty)" },
          ],
          maxTokens: 400,
        });
        app.session.compact(res.content);
        render.success("Conversation compacted — earlier turns replaced with a summary.");
      } catch (e) {
        render.error(`Compaction failed: ${e instanceof Error ? e.message : String(e)}`);
      }
      return { handled: true };
    }

    case "clear": {
      app.session.clear();
      // Also drop the host terminal's scrollback so the visual reset matches the
      // logical one — otherwise the user can scroll back into the cleared turns.
      clearHostTerminal(process.stdout);
      render.success("Conversation cleared — starting fresh.");
      return { handled: true };
    }

    case "skills": {
      const sub = rest[0];
      if (!sub) {
        const all = app.skills.list();
        render.line(c.bold(`\nSkills (${all.length})`));
        for (const r of all) {
          render.line(
            `  ${c.cyan(r.id)} ${c.gray(`[${r.risk}]`)} ${r.title} — ${r.summary}`,
          );
        }
        render.line(
          c.gray("\n  /skills loaded | find <query> | load <id…> | clear"),
        );
        return { handled: true };
      }
      if (sub === "loaded") {
        render.line(`\n${app.session.describeSkills()}`);
        return { handled: true };
      }
      if (sub === "clear") {
        app.session.setSkills([]);
        render.success("Cleared loaded skills.");
        return { handled: true };
      }
      if (sub === "load") {
        const ids = rest.slice(1);
        if (ids.length === 0) {
          render.warn("Usage: /skills load <id> [<id>…]");
          return { handled: true };
        }
        const known = ids.filter((id) => app.skills.has(id));
        const unknown = ids.filter((id) => !app.skills.has(id));
        if (unknown.length) {
          render.warn(`Unknown skill id(s): ${unknown.join(", ")}`);
        }
        if (known.length === 0) {
          // Don't clear the loaded set just because every id was a typo.
          render.warn("No known skill ids given; loaded skills unchanged.");
          return { handled: true };
        }
        if (new Set(known).size > 3) {
          render.warn("More than 3 skills given; loading the first 3.");
        }
        app.session.setSkills(known);
        render.line(app.session.describeSkills());
        return { handled: true };
      }
      if (sub === "find") {
        const query = rest.slice(1).join(" ").trim();
        if (!query) {
          render.warn("Usage: /skills find <query>");
          return { handled: true };
        }
        render.info(`Finding skills for "${query}"…`);
        try {
          const res = await app.session.findSkills(query);
          if (!res.ok) {
            render.warn("Selector unavailable; loaded skills unchanged.");
          } else if (res.matched) {
            render.success(
              `Loaded: ${res.selected.map((r) => r.id).join(", ")}`,
            );
          } else {
            render.warn(`No skill matched "${query}".`);
          }
          render.line(app.session.describeSkills());
        } catch (e) {
          render.error(
            `Skill find failed: ${e instanceof Error ? e.message : String(e)}`,
          );
        }
        return { handled: true };
      }
      render.warn("Usage: /skills [loaded|find <query>|load <id…>|clear]");
      return { handled: true };
    }

    default:
      render.warn(`Unknown command /${cmd}. Try /help.`);
      return { handled: true };
  }
}
