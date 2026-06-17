/**
 * App wiring. Builds every dependency once, exposes a tool ToolContext factory,
 * the main AgentSession, and the scheduler. The CLI entry and REPL drive it.
 */
import { randomUUID } from "node:crypto";
import { loadConfig, type AppConfig, type ConfigOverrides } from "../config.js";
import { Db } from "../storage/db.js";
import { DaintreeMcpClient, type McpClientOptions } from "../mcp/client.js";
import { Queue } from "../queue.js";
import { ModelRouter } from "../models/router.js";
import { ToolRegistry } from "../tools/registry.js";
import { buildAllTools } from "../tools/index.js";
import type { ConfirmRequest, ToolActor, ToolContext } from "../tools/types.js";
import { AgentSession } from "../agent/loop.js";
import type { MainPromptContext } from "../models/prompts.js";
import { Scheduler } from "../daemon/scheduler.js";
import type { QueueEvent } from "../schemas.js";

export interface AppHooks {
  /** Interactive confirmation for mutating actions (main actor only). */
  confirm?: (req: ConfirmRequest) => Promise<boolean>;
  /** Out-of-band line printer for tools/daemon. */
  log?: (msg: string) => void;
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
    this.hooks = opts.hooks ?? {};
    this.sessionId = opts.sessionId ?? `ses_${randomUUID().slice(0, 8)}`;
  }

  static create(opts: AppCreateOptions = {}): App {
    const app = new App(opts);
    app.session = new AgentSession({
      router: app.router,
      registry: app.registry,
      ctx: app.buildContext("main"),
      promptContext: app.promptContext(),
      sessionId: app.sessionId,
    });
    return app;
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
    await this.mcp.connect();
    this.session.refreshSystemPrompt(this.promptContext());
  }

  startScheduler(onAttention?: (events: QueueEvent[]) => void): Scheduler {
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
    // Context closures read this.hooks live, so no session rebuild is needed.
    this.hooks = hooks;
  }

  async shutdown(): Promise<void> {
    this.scheduler?.stop();
    await this.scheduler?.drain();
    await this.mcp.close();
    this.db.close();
  }
}
