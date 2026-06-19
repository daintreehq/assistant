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
import {
  type AgentEventSink,
  type RunIdRef,
  multiSink,
  RunEventSink,
} from "../agent/events.js";
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
  /** Holds the id of the run currently streaming; set per-turn by AgentSession. */
  readonly runIdRef: RunIdRef = { current: undefined };
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
      // Compose two sinks: a durable one that records the run to `run_events`, and
      // the live proxy that forwards to whatever UI sink is registered via
      // setHooks(). multiSink isolates each, so a DB write failure can't break the
      // UI stream. The session stamps the current run id onto runIdRef per turn.
      events: multiSink(new RunEventSink(app.db, app.runIdRef), app.agentEventProxy()),
      runIdRef: app.runIdRef,
    });
    return app;
  }

  /** A stable AgentEventSink that delegates to the live hooks.agentEvents. */
  private agentEventProxy(): AgentEventSink {
    return {
      assistantStart: () => this.hooks.agentEvents?.assistantStart(),
      assistantToken: (token) => this.hooks.agentEvents?.assistantToken(token),
      assistantEnd: (content, reasoning) =>
        this.hooks.agentEvents?.assistantEnd(content, reasoning),
      assistantCancelled: (content) =>
        this.hooks.agentEvents?.assistantCancelled(content),
      toolCall: (event) => this.hooks.agentEvents?.toolCall(event),
      toolResult: (event) => this.hooks.agentEvents?.toolResult(event),
      error: (message) => this.hooks.agentEvents?.error(message),
      info: (message) => this.hooks.agentEvents?.info(message),
      usage: (event) => this.hooks.agentEvents?.usage?.(event),
    };
  }

  buildContext(actor: ToolActor, actorId?: string): ToolContext {
    return {
      config: this.config,
      mcp: this.mcp,
      db: this.db,
      queue: this.queue,
      router: this.router,
      projectPath: this.config.projectPath,
      sessionId: this.sessionId,
      actor,
      actorId,
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
      schedulerActive: Boolean(this.scheduler),
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

  /**
   * Surface documented-vs-live MCP tool drift as a single, non-fatal startup line.
   * Drift means a tool we document isn't advertised at the current tier / plugin
   * config — a developer signal, never user-actionable — so we collapse the
   * per-tool list into one rollup instead of flooding the ledger with a line each.
   * `/doctor` keeps the full breakdown.
   */
  private warnOnDrift(st: McpStatus): void {
    const names = st.driftToolNames ?? [];
    if (names.length === 0) return;
    const log = this.hooks.log ?? (() => {});
    const preview = names.slice(0, 3).join(", ");
    const rest = names.length - Math.min(3, names.length);
    const list = rest > 0 ? `${preview}, +${rest} more` : preview;
    const noun = names.length === 1 ? "tool" : "tools";
    log(
      `⚠️  MCP drift: ${names.length} documented ${noun} not advertised by the live server (${list}). Run /doctor for the full list.`,
    );
  }

  startScheduler(onAttention?: (events: QueueEvent[]) => void): Scheduler {
    // Idempotent: a React effect may run more than once (remount / future
    // StrictMode). Returning the existing scheduler avoids leaking a second
    // interval that shutdown() would never stop — but REBIND its attention
    // callback so a remount's fresh closure replaces the (now-disposed) old one,
    // rather than leaving the scheduler calling a dead hook forever.
    if (this.scheduler) {
      this.scheduler.setOnAttention(onAttention);
      return this.scheduler;
    }
    this.scheduler = new Scheduler({
      db: this.db,
      queue: this.queue,
      router: this.router,
      registry: this.registry,
      ctxFor: (actor, actorId) => this.buildContext(actor, actorId),
      onAttention,
    });
    this.scheduler.start();
    // The runtime context advertises scheduler state; now that it is running,
    // refresh message[1] so the dormant-scheduler note disappears.
    this.session.refreshRuntimeContext(this.promptContext());
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
