/**
 * Interactive REPL. Manual readline loop so confirmation prompts and streaming
 * output interleave cleanly. Sub-thread (watcher/timer) events are printed
 * out-of-band when the scheduler raises attention items.
 */
import readline from "node:readline";
import type { App } from "./app.js";
import { handleSlashCommand } from "./commands.js";
import { render, c } from "./render.js";
import { createConsoleSink } from "./consoleSink.js";
import type { ConfirmRequest } from "../tools/types.js";
import type { QueueEvent } from "../schemas.js";
import { compactArgs } from "./render.js";

export async function startRepl(app: App): Promise<void> {
  const rl = readline.createInterface({
    input: process.stdin,
    output: process.stdout,
  });
  const ask = (q: string) =>
    new Promise<string>((resolve) => rl.question(q, resolve));

  // Wire interactive confirmation + out-of-band logging into the app.
  app.setHooks({
    agentEvents: createConsoleSink(),
    confirm: async (req: ConfirmRequest) => {
      render.line();
      render.warn(
        `${c.bold(req.toolName)} (${req.risk}) wants to run:\n     ${req.summary}\n     args: ${compactArgs(req.args)}`,
      );
      const a = await ask(c.yellow("   approve? [y/N] "));
      return /^y(es)?$/i.test(a.trim());
    },
    log: (msg: string) => render.line(c.gray(`  · ${msg}`)),
  });

  // Connect to Daintree (best-effort) and start the daemon.
  await app.connectMcp();
  const st = app.mcp.status();
  app.startScheduler((events: QueueEvent[]) => printAttention(events));

  banner(app);
  if (!st.connected) {
    render.warn(
      `Daintree MCP not connected (${st.error ?? "no url/token"}). Running in degraded local mode — fs/timer/watcher/queue tools work; orchestration tools need a connection.`,
    );
  }

  let quit = false;
  while (!quit) {
    const line = (await ask(c.cyan("\ndaintree ❯ "))).trim();
    if (!line) continue;

    if (line.startsWith("/")) {
      const res = await handleSlashCommand(line, app);
      if (res.quit) quit = true;
      continue;
    }

    try {
      await app.session.send(line);
    } catch (err) {
      render.error(err instanceof Error ? err.message : String(err));
    }
  }

  rl.close();
  await app.shutdown();
  render.line(c.gray("Goodbye."));
}

function printAttention(events: QueueEvent[]): void {
  for (const e of events) {
    render.line();
    render.line(
      `${c.magenta("◆ inbox")} ${c.bold(e.title)} ${c.gray(`(${e.severity})`)}`,
    );
    render.line(`  ${e.summary}`);
    if (e.evidence?.length) render.line(c.gray(`  evidence: ${e.evidence.join(" | ")}`));
  }
  process.stdout.write(c.cyan("\ndaintree ❯ "));
}

function banner(app: App): void {
  const st = app.mcp.status();
  render.banner([
    c.bold(c.green("Daintree Assistant")) + c.gray("  — local operations officer"),
    `${c.gray("project")}   ${app.config.projectPath}`,
    `${c.gray("mcp")}       ${st.connected ? c.green(`connected (${st.transport})`) : c.yellow("degraded local mode")}`,
    `${c.gray("models")}    large=${app.config.largeModel.split("/").pop()} · small=${app.config.smallModel.split("/").pop()}`,
    `${c.gray("tier")}      ${app.config.tier}`,
    c.gray("Type /help for commands. I supervise Daintree and spawn agents — I never edit files directly."),
  ]);
}
