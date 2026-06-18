/**
 * CLI entry point. With no prompt and a TTY it launches the Ink operations
 * cockpit. A trailing quoted prompt runs a single turn (console output) and
 * exits. `--classic` forces the legacy readline REPL; non-TTY stdin/stdout also
 * falls back to it. `doctor` checks the environment.
 */
import { Command } from "commander";
import { App } from "./app.js";
import { startRepl } from "./repl.js";
import { render, c } from "./render.js";
import { createConsoleSink } from "./consoleSink.js";
import { startInkApp } from "../ui/runInkApp.js";
import { startDebugLog } from "../debugLog.js";
import type { ConfigOverrides } from "../config.js";
import type { Tier } from "../schemas.js";

interface CliOptions {
  mcpUrl?: string;
  mcpToken?: string;
  project?: string;
  tier?: string;
  offline?: boolean;
  classic?: boolean;
  altScreen?: boolean;
}

function overridesFromOptions(opts: CliOptions): ConfigOverrides {
  return {
    mcpUrl: opts.mcpUrl,
    mcpToken: opts.mcpToken,
    projectPath: opts.project,
    tier: opts.tier as Tier | undefined,
    offline: opts.offline,
  };
}

/** Start the debug log for this session and, when active, tell the user where it
 *  is being written so they can tail it. */
function announceDebugLog(app: App): void {
  const logPath = startDebugLog(app.config, app.sessionId);
  if (logPath) render.line(c.gray(`logging to ${logPath}`));
}

async function runOneShot(prompt: string, opts: CliOptions): Promise<void> {
  const app = App.create({ overrides: overridesFromOptions(opts) });
  announceDebugLog(app);
  app.setHooks({
    agentEvents: createConsoleSink(),
    // One-shot is non-interactive: auto-decline mutations rather than hang.
    confirm: async (req) => {
      render.warn(
        `Skipping ${req.toolName} (${req.risk}) — confirmation needed; run interactively to approve.`,
      );
      return false;
    },
    log: (m) => render.line(c.gray(`  · ${m}`)),
  });
  await app.connectMcp();
  await app.session.send(prompt);
  await app.shutdown();
}

async function runInteractive(opts: CliOptions): Promise<void> {
  const app = App.create({ overrides: overridesFromOptions(opts) });
  announceDebugLog(app);
  const ttyOk = Boolean(process.stdin.isTTY && process.stdout.isTTY);
  if (opts.classic || !ttyOk) {
    await startRepl(app);
    return;
  }
  await startInkApp(app, { alternateScreen: opts.altScreen !== false });
}

async function runDoctor(opts: CliOptions): Promise<void> {
  const app = App.create({ overrides: overridesFromOptions(opts) });
  await app.connectMcp();
  const st = app.mcp.status();
  render.line(c.bold("Daintree Assistant — doctor"));
  render.line(`  fireworks key  : ${app.config.fireworksApiKey ? c.green("present") : c.red("MISSING — set FIREWORKS_API_KEY")}`);
  render.line(`  mcp url        : ${app.config.mcpUrl ?? c.yellow("(unset)")}`);
  render.line(`  mcp connection : ${st.connected ? c.green(`ok (${st.transport}, ${st.toolCount} tools)`) : c.yellow(st.error ?? "not connected")}`);
  render.line(`  project        : ${app.config.projectPath}`);
  render.line(`  tools loaded   : ${app.registry.list().length}`);
  render.line(`  tier           : ${app.config.tier}`);
  await app.shutdown();
}

async function main(): Promise<void> {
  const program = new Command();
  program
    .name("daintree-assistant")
    .description("Daintree's local orchestration assistant (Fireworks-powered).")
    .version("0.1.0")
    .option("--mcp-url <url>", "Daintree MCP url (or DAINTREE_MCP_URL)")
    .option("--mcp-token <token>", "Daintree MCP bearer token (or DAINTREE_MCP_TOKEN)")
    .option("--project <path>", "Project directory (defaults to cwd)")
    .option("--tier <tier>", "supervisor | operator | system")
    .option("--offline", "Do not make network calls")
    .option("--classic", "Use the legacy readline interface")
    .option("--no-alt-screen", "Render Ink without the alternate (full-screen) buffer")
    .argument("[prompt]", "Run a single prompt non-interactively, then exit")
    .action(async (prompt: string | undefined, opts: CliOptions) => {
      if (prompt) await runOneShot(prompt, opts);
      else await runInteractive(opts);
    });

  program
    .command("doctor")
    .description("Check MCP connection, Fireworks key, and project mapping")
    .action(() => runDoctor(program.opts() as CliOptions));

  await program.parseAsync(process.argv);
}

main().catch((err) => {
  render.error(err instanceof Error ? err.stack ?? err.message : String(err));
  process.exit(1);
});
