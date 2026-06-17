/**
 * App wiring. Builds every dependency once, exposes a tool ToolContext factory,
 * the main AgentSession, and the scheduler. The CLI entry and REPL drive it.
 */
import { randomUUID } from "node:crypto";
import { loadConfig, type AppConfig, type ConfigOverrides } from "../config.js";
import { Db } from "../storage/db.js";
import {
  DaintreeMcpClient,
  type McpClientOptions,
  type McpStatus,
} from "../mcp/client.js";
import { Queue } from "../queue.js";
import { ModelRouter } from "../models/router.js";
import { ToolRegistry } from "../tools/registry.js";
import { buildAllTools } from "../tools/index.js";
import type { ConfirmRequest, ToolActor, ToolContext } from "../tools/types.js";
import { RecipeRegistry } from "../recipes/registry.js";
import { AgentSession } from "../agent/loop.js";
import type { AgentEventSink } from "../agent/events.js";
import type { MainPromptContext } from "../models/prompts/index.js";
import { Scheduler } from "../daemon/scheduler.js";
import type { QueueEvent } from "../schemas.js";

export interface AppHooks {
  /** Interactive confirmation for mutating actions (main actor only). */
  confirm?: (req: ConfirmRequest) => Promise<boolean>;
  /** Out-of-band line printer for tools/daemon. */
  log?: (msg: string) => void;
  /** Where the main agent loop streams tokens/tool-calls/errors. */
  agentEvents?: AgentEventSink;
}

export interface AppCreateOptions {
  overrides?: ConfigOverrides;
  hooks?: AppHooks;
  /** Inject a fake MCP client (tests). */
  mcpOptions?: McpClientOptions;
  sessionId?: string;
}

export class App {
  readonly config: AppConfig;
  readonly db: Db;
  readonly mcp: DaintreeMcpClient;
  readonly queue: Queue;
  readonly router: ModelRouter;
  readonly registry: ToolRegistry;
  readonly recipes: RecipeRegistry;
  readonly sessionId: string;
  session!: AgentSession;
  scheduler?: Scheduler;

  private hooks: AppHooks;

  private constructor(opts: AppCreateOptions) {
    this.config = loadConfig(opts.overrides);
    this.db = new Db(this.config.dbPath);
    this.mcp = new DaintreeMcpClient(this.config, opts.mcpOptions);
    this.queue = new Queue(this.db);
    this.router = new ModelRouter(this.config);
    this.registry = new ToolRegistry();
    this.registry.registerAll(buildAllTools());
    this.registry.assertSafe();
    this.recipes = new RecipeRegistry();
    this.hooks = opts.hooks ?? {};
    this.sessionId = opts.sessionId ?? `ses_${randomUUID().slice(0, 8)}`;
  }

  static create(opts: AppCreateOptions = {}): App {
    const app = new App(opts);
    app.session = new AgentSession({
      router: app.router,
      registry: app.registry,
      recipeRegistry: app.recipes,
      ctx: app.buildContext("main"),
      promptContext: app.promptContext(),
      sessionId: app.sessionId,
      // Forward to whatever sink is currently registered via setHooks(); reading
      // this.hooks live means the UI can attach after the session is built.
      events: app.agentEventProxy(),
    });
    return app;
  }

  /** A stable AgentEventSink that delegates to the live hooks.agentEvents. */
  private agentEventProxy(): AgentEventSink {
    return {
      assistantStart: () => this.hooks.agentEvents?.assistantStart(),
      assistantToken: (token) => this.hooks.agentEvents?.assistantToken(token),
      assistantEnd: (content) => this.hooks.agentEvents?.assistantEnd(content),
      toolCall: (name, args) => this.hooks.agentEvents?.toolCall(name, args),
      toolResult: (name, result) =>
        this.hooks.agentEvents?.toolResult(name, result),
      error: (message) => this.hooks.agentEvents?.error(message),
      info: (message) => this.hooks.agentEvents?.info(message),
    };
  }

  buildContext(actor: ToolActor): ToolContext {
    return {
      config: this.config,
      mcp: this.mcp,
      db: this.db,
      queue: this.queue,
      router: this.router,
      projectPath: this.config.projectPath,
      actor,
      // Read hooks live so setHooks() updates take effect without rebuilding the
      // session (which would drop conversation history).
      confirm:
        actor === "main"
          ? (req) => (this.hooks.confirm ?? (async () => false))(req)
          : async () => false,
      log: (msg) => (this.hooks.log ?? (() => {}))(msg),
      // Read live: the scheduler is started later (interactive paths) and never
      // in a one-shot run, so timers/watchers can warn honestly.
      daemonActive: () => Boolean(this.scheduler),
    };
  }

  promptContext(): MainPromptContext {
    const st = this.mcp.status();
    const statusLine = st.connected
      ? `connected (${st.transport}, ${st.toolCount ?? "?"} tools)`
      : `not connected — ${st.error ?? "no url/token"}`;
    return {
      tier: this.config.tier,
      projectPath: this.config.projectPath,
      projectId: this.config.projectId,
      mcpConnected: st.connected,
      mcpStatusLine: statusLine,
      largeModel: this.config.largeModel,
      smallModel: this.config.smallModel,
    };
  }

  /** Connect to Daintree MCP (best-effort) and refresh the system prompt. */
  async connectMcp(): Promise<void> {
    const st = await this.mcp.connect();
    this.warnOnDrift(st);
    this.session.refreshRuntimeContext(this.promptContext());
  }

  /** Force a fresh MCP connection (e.g. /reconnect, /doctor) and refresh prompt. */
  async reconnectMcp(): Promise<void> {
    const st = await this.mcp.reconnect();
    this.warnOnDrift(st);
    this.session.refreshRuntimeContext(this.promptContext());
  }

  /** Surface documented-vs-live MCP tool drift as a startup warning (non-fatal). */
  private warnOnDrift(st: McpStatus): void {
    if (!st.driftWarnings?.length) return;
    const log = this.hooks.log ?? (() => {});
    for (const w of st.driftWarnings) log(`⚠️  ${w}`);
  }

  startScheduler(onAttention?: (events: QueueEvent[]) => void): Scheduler {
    // Idempotent: a React effect may run more than once (remount / future
    // StrictMode). Returning the existing scheduler avoids leaking a second
    // interval that shutdown() would never stop.
    if (this.scheduler) return this.scheduler;
    this.scheduler = new Scheduler({
      db: this.db,
      queue: this.queue,
      router: this.router,
      registry: this.registry,
      ctxFor: (actor) => this.buildContext(actor),
      onAttention,
    });
    this.scheduler.start();
    return this.scheduler;
  }

  setHooks(hooks: AppHooks): void {
    // Merge so partial updates (e.g. attaching only agentEvents) don't drop the
    // existing confirm/log hooks. Context + event closures read this.hooks live,
    // so no session rebuild is needed and conversation state is preserved.
    this.hooks = { ...this.hooks, ...hooks };
  }

  async shutdown(): Promise<void> {
    this.scheduler?.stop();
    await this.scheduler?.drain();
    await this.mcp.close();
    this.db.close();
  }
}
