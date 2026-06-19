/**
 * CLI entry point. With no prompt and a TTY it launches the Ink operations
 * cockpit. A trailing quoted prompt runs a single turn (console output) and
 * exits. `--classic` forces the legacy readline REPL; non-TTY stdin/stdout also
 * falls back to it. `doctor` checks the environment.
 */
import path from "node:path";
import { Command } from "commander";
import { App } from "./app.js";
import { startRepl } from "./repl.js";
import { render, c } from "./render.js";
import { createConsoleSink } from "./consoleSink.js";
import { createJsonSink } from "./jsonSink.js";
import { startInkApp } from "../ui/runInkApp.js";
import { startDebugLog } from "../debugLog.js";
import { loadProjectInstructions } from "../projectInstructions.js";
import type { ConfigOverrides } from "../config.js";
import type { Tier } from "../schemas.js";

interface CliOptions {
  mcpUrl?: string;
  mcpToken?: string;
  project?: string;
  tier?: string;
  offline?: boolean;
  classic?: boolean;
  inline?: boolean;
  json?: boolean;
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

/**
 * Build the config overrides AND load the project-level instruction file
 * (`DAINTREE.md`) before App.create(). The read is async and best-effort, so it
 * happens here in the entry path rather than inside the synchronous loadConfig().
 * A non-fatal warning (oversized/unreadable file) is surfaced once at startup.
 */
async function buildOverrides(opts: CliOptions): Promise<ConfigOverrides> {
  const overrides = overridesFromOptions(opts);
  // Mirror loadConfig()'s project-root resolution so we read the same DAINTREE.md
  // the session will report as its project path.
  const projectPath = path.resolve(opts.project ?? process.cwd());
  const { content, warning } = await loadProjectInstructions(projectPath);
  if (warning) render.warn(warning);
  return { ...overrides, projectInstructions: content };
}

/** Start the debug log for this session and, when active, tell the user where it
 *  is being written so they can tail it. */
function announceDebugLog(app: App): void {
  const logPath = startDebugLog(app.config, app.sessionId);
  if (logPath) render.line(c.gray(`logging to ${logPath}`));
}

async function runOneShot(prompt: string, opts: CliOptions): Promise<void> {
  // In `--json` mode stdout carries ONLY the JSONL stream, so every human-facing
  // line (debug-log notice, confirm-skip warning, loop log, *and any error*) is
  // routed to stderr; otherwise the existing console UX is preserved unchanged.
  const json = opts.json === true;
  const jsonSink = json ? createJsonSink() : undefined;

  // Reports an unexpected failure through the active surface: the JSON sink (so
  // stdout still ends with a `result` envelope) or the console. Centralised so the
  // boot path and the run path can't pollute stdout differently in JSON mode.
  const reportError = (err: unknown): void => {
    const message = err instanceof Error ? (err.stack ?? err.message) : String(err);
    if (jsonSink) jsonSink.sink.error(message);
    else {
      render.error(message);
      process.exitCode = 1;
    }
  };

  // App.create() can throw (bad config, DB/registry init). buildOverrides() also
  // reads the project-level DAINTREE.md before boot. Keep both inside the error
  // funnel so a boot failure in JSON mode still yields a `result` envelope on
  // stdout rather than an ANSI stack trace via main().catch().
  let app: App;
  try {
    app = App.create({ overrides: await buildOverrides(opts) });
  } catch (err) {
    reportError(err);
    if (jsonSink) process.exitCode = jsonSink.finish().exitCode;
    return;
  }

  if (json) {
    const logPath = startDebugLog(app.config, app.sessionId);
    if (logPath) process.stderr.write(`logging to ${logPath}\n`);
  } else {
    announceDebugLog(app);
  }

  app.setHooks({
    agentEvents: jsonSink ? jsonSink.sink : createConsoleSink(),
    // One-shot is non-interactive: auto-decline mutations rather than hang.
    confirm: async (req) => {
      const msg = `Skipping ${req.toolName} (${req.risk}) — confirmation needed; run interactively to approve.`;
      if (json) process.stderr.write(`${msg}\n`);
      else render.warn(msg);
      return false;
    },
    log: (m) => (json ? process.stderr.write(`  · ${m}\n`) : render.line(c.gray(`  · ${m}`))),
  });

  try {
    await app.connectMcp();
    await app.session.send(prompt);
  } catch (err) {
    // send() handles its own model errors (emitting an `error` event and returning
    // a string); this catches genuinely unexpected throws (e.g. MCP connect).
    reportError(err);
  } finally {
    // shutdown() (mcp.close / db.close) can throw, but it must never prevent the
    // terminal `result` envelope from being written or the exit code from being
    // set — swallow its failure as best-effort cleanup, surfaced off stdout.
    try {
      await app.shutdown();
    } catch (err) {
      if (json) {
        const message = err instanceof Error ? (err.stack ?? err.message) : String(err);
        process.stderr.write(`shutdown error: ${message}\n`);
      } else render.error(err instanceof Error ? (err.stack ?? err.message) : String(err));
    }
    // Emit the terminal `result` envelope last (after shutdown, so no further
    // events can interleave) and adopt its exit code as the process exit code.
    if (jsonSink) process.exitCode = jsonSink.finish().exitCode;
  }
}

async function runInteractive(opts: CliOptions): Promise<void> {
  const app = App.create({ overrides: await buildOverrides(opts) });
  const ttyOk = Boolean(process.stdin.isTTY && process.stdout.isTTY);
  if (opts.classic || !ttyOk) {
    // Classic REPL has no header, so the console "logging to …" line is the only
    // place the path surfaces — keep it.
    announceDebugLog(app);
    await startRepl(app);
    return;
  }
  // Ink cockpit: still open the log (so currentDebugLogPath() resolves for the
  // header), but print nothing — the header's "◌ logging · <path>" badge already
  // shows it, and a console line above the cockpit would just duplicate it.
  startDebugLog(app.config, app.sessionId);
  await startInkApp(app);
}

async function runDoctor(opts: CliOptions): Promise<void> {
  const app = App.create({ overrides: await buildOverrides(opts) });
  await app.connectMcp();
  const st = app.mcp.status();
  render.line(c.bold("Daintree Assistant — doctor"));
  render.line(`  fireworks key  : ${app.config.fireworksApiKey ? c.green("present") : c.red("MISSING — set FIREWORKS_API_KEY")}`);
  render.line(`  mcp url        : ${app.config.mcpUrl ?? c.yellow("(unset)")}`);
  render.line(`  mcp connection : ${st.connected ? c.green(`ok (${st.transport}, ${st.toolCount} tools)`) : c.yellow(st.error ?? "not connected")}`);
  render.line(`  project        : ${app.config.projectPath}`);
  render.line(`  instructions   : ${app.config.projectInstructions ? c.green(`DAINTREE.md (${Buffer.byteLength(app.config.projectInstructions, "utf8")} bytes)`) : c.gray("(none)")}`);
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
    .option("--inline", "Deprecated no-op: the cockpit is always inline now (native scrollback)")
    .option("--json", "One-shot only: stream JSONL events to stdout; the final line is a result envelope (see docs). Diagnostics go to stderr.")
    .argument("[prompt]", "Run a single prompt non-interactively, then exit")
    .action(async (prompt: string | undefined, opts: CliOptions) => {
      if (prompt) await runOneShot(prompt, opts);
      else if (opts.json) {
        // `--json` is a one-shot contract; without a prompt it would otherwise
        // launch the interactive TUI and write non-JSONL output to stdout.
        process.stderr.write("--json requires a prompt argument (one-shot mode only).\n");
        process.exitCode = 1;
      } else await runInteractive(opts);
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
