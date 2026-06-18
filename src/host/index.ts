/**
 * Native-host entry point. Daintree launches this module with
 * `utilityProcess.fork()` when it wants to render the assistant as native React
 * instead of an xterm pane (Daintree issue #10649). It speaks the
 * {@link HostEvent}/{@link HostCommand} protocol over Electron's
 * `process.parentPort`; the CLI/Ink entry (`src/cli/index.ts`) is untouched and
 * stays the default and fallback.
 *
 * Boot sequence:
 *   1. Validate we're in a utility process (`process.parentPort` exists).
 *   2. Install the bootstrap error guard BEFORE any dynamic import (#8833).
 *   3. Receive + validate the session descriptor (first inbound message).
 *   4. Dynamically import and wire `App`, connect MCP best-effort.
 *   5. Announce `host:ready`; then service commands until shut down.
 *
 * Secrets (MCP url/token) arrive via env, never the descriptor — `loadConfig()`
 * reads `DAINTREE_MCP_URL` / `DAINTREE_MCP_TOKEN` / `DAINTREE_WINDOW_ID` itself.
 */
import { HostBridge } from "./bridge.js";
import { installBootstrapErrorGuard } from "./errorGuard.js";
import { isActionableWake, buildWakePrompt } from "../agent/wake.js";
import type { QueueEvent } from "../schemas.js";
import {
  PROTOCOL_VERSION,
  type HostCommand,
  type HostEvent,
  type HostShutdownReason,
  type SessionDescriptor,
  isHostCommand,
  isSessionDescriptor,
} from "./protocol.js";

/** Minimal shape of Electron's `MessagePortMain` exposed as `process.parentPort`. */
interface ParentPort {
  on(event: "message", listener: (messageEvent: { data: unknown }) => void): void;
  postMessage(message: unknown): void;
  start?(): void;
}

const parentPort = (process as unknown as { parentPort?: ParentPort }).parentPort;

function errMessage(err: unknown): string {
  return err instanceof Error ? (err.stack ?? err.message) : String(err);
}

async function main(): Promise<void> {
  if (!parentPort) {
    // Run via `daintree-assistant` (the CLI) for an interactive session; this
    // entry is only meaningful inside an Electron utility process.
    process.stderr.write("daintree-assistant host entry requires an Electron utility process.\n");
    process.exit(1);
  }
  const port = parentPort;

  // sessionId is unknown until the descriptor lands; the bootstrap guard holds a
  // closure over it so a pre-descriptor crash still names the (empty) session
  // rather than vanishing.
  let sessionId = "";
  const post = (event: HostEvent): void => port.postMessage(event);
  const disposeBootstrapGuard = installBootstrapErrorGuard((code, message) => {
    if (sessionId) post({ type: "host:error", sessionId, code, message });
  });

  let state: "await-descriptor" | "running" = "await-descriptor";
  let ready = false;
  let busy = false;
  let bridge: HostBridge | null = null;
  // Set once the App is wired; `unknown` keeps this file free of a cycle into app.ts types.
  let app: {
    session: {
      send(input: string, opts?: { readOnly?: boolean }): Promise<string>;
    };
    shutdown(): Promise<void>;
  } | null = null;

  // Autonomous wake-ups: the scheduler surfaces terminal-watcher events; when the
  // host is idle we run a READ-ONLY turn so the model can read the terminal and
  // report through the normal event stream. Serialized against command-driven
  // turns by `busy`; one retry on failure so a transient error isn't stranded.
  const pendingWake: QueueEvent[] = [];
  let wakeRetried = false;
  const reactWake = async (): Promise<void> => {
    if (busy || !ready || !bridge || !app) return;
    const events = pendingWake.splice(0);
    if (events.length === 0) return;
    busy = true;
    bridge.startExchange();
    try {
      await app.session.send(buildWakePrompt(events), { readOnly: true });
      wakeRetried = false;
    } catch (err) {
      post({ type: "host:error", sessionId, code: "wake-failed", message: errMessage(err) });
      if (!wakeRetried) {
        wakeRetried = true;
        pendingWake.unshift(...events);
      }
    } finally {
      bridge.settleTurn("answered");
      busy = false;
      if (pendingWake.length > 0) void reactWake();
    }
  };

  const teardown = async (reason: HostShutdownReason, resumeSessionId?: string): Promise<void> => {
    bridge?.settlePendingApprovals("rejected");
    post({ type: "host:shutdown", sessionId, reason, ...(resumeSessionId ? { resumeSessionId } : {}) });
    try {
      await app?.shutdown();
    } catch {
      // Best-effort: we're exiting regardless.
    }
    setImmediate(() => process.exit(0));
  };

  const boot = async (descriptor: SessionDescriptor): Promise<void> => {
    sessionId = descriptor.sessionId;
    if (descriptor.protocolVersion !== PROTOCOL_VERSION) {
      post({
        type: "host:error",
        sessionId,
        code: "protocol-mismatch",
        message: `Host speaks protocol v${PROTOCOL_VERSION}; Daintree expects v${descriptor.protocolVersion}.`,
      });
      await teardown("error");
      return;
    }

    // Dynamic import AFTER the bootstrap guard, so a module-load failure (e.g.
    // node:sqlite, a native dep) is reported and exits instead of hanging.
    const { App } = await import("../cli/app.js");
    const { startDebugLog } = await import("../debugLog.js");
    const appSessionId = descriptor.resumeSessionId ?? descriptor.sessionId;
    const instance = App.create({
      sessionId: appSessionId,
      // Project binding comes from the descriptor; MCP url/token/tier and the
      // project id are read from env by loadConfig(), mirroring the CLI path.
      overrides: { projectPath: descriptor.cwd },
    });
    app = instance;
    // Open this session's global debug log (prune old + header); no-op unless enabled.
    startDebugLog(instance.config, appSessionId);

    bridge = new HostBridge({
      sessionId,
      post,
      riskOf: (name) => instance.registry.get(name)?.risk,
    });
    instance.setHooks({
      agentEvents: bridge.sink,
      confirm: (req) => bridge!.confirm({ toolName: req.toolName, summary: req.summary }),
    });

    // Best-effort: a degraded MCP connection surfaces in the assistant's own
    // prompt context and tool results, not as a boot failure.
    await instance.connectMcp();

    // Start the daemon so watchers/timers tick in the host runtime (the Ink path
    // does this in its controller). Terminal-watcher events autonomously wake a
    // read-only turn via reactWake; other sources stay in the attention queue.
    instance.startScheduler((events) => {
      const actionable = events.filter(isActionableWake);
      if (actionable.length === 0) return;
      // A burst starting from empty gets a fresh single-retry budget.
      if (pendingWake.length === 0) wakeRetried = false;
      pendingWake.push(...actionable);
      void reactWake();
    });

    // Hand off from the bootstrap guard to long-lived runtime handlers.
    disposeBootstrapGuard();
    const onFatal = (err: unknown) => {
      post({ type: "host:error", sessionId, code: "uncaught", message: errMessage(err) });
      void teardown("error");
    };
    process.on("uncaughtException", onFatal);
    process.on("unhandledRejection", onFatal);

    ready = true;
    post({
      type: "host:ready",
      sessionId,
      protocolVersion: PROTOCOL_VERSION,
      ...(descriptor.resumeSessionId ? { resumedSessionId: descriptor.resumeSessionId } : {}),
    });
  };

  const handleCommand = async (cmd: HostCommand): Promise<void> => {
    if (!ready || !bridge || !app) {
      post({ type: "host:error", sessionId, code: "not-ready", message: "Host is still starting." });
      return;
    }
    switch (cmd.type) {
      case "prompt": {
        if (busy) {
          post({
            type: "host:error",
            sessionId,
            code: "turn-in-progress",
            message: "A turn is already running; interrupt it before sending another prompt.",
          });
          return;
        }
        busy = true;
        bridge.startExchange();
        try {
          await app.session.send(cmd.text);
        } catch (err) {
          post({ type: "host:error", sessionId, code: "turn-failed", message: errMessage(err) });
        } finally {
          // Close any assistant turn the loop left open (error paths don't emit
          // assistantEnd); a normal completion already closed it, so this no-ops.
          bridge.settleTurn("answered");
          busy = false;
          // A watcher may have surfaced something while this turn ran — react now
          // that we're idle.
          if (pendingWake.length > 0) void reactWake();
        }
        return;
      }
      case "approval:decide":
        bridge.resolveApproval(cmd.approvalId, cmd.decision);
        return;
      case "interrupt":
        // Best-effort: stops forwarding the in-flight turn's output and closes
        // it. The underlying model stream is not yet abortable mid-token — a
        // real cancel needs an AbortSignal threaded through ModelRouter.stream
        // (tracked follow-up). The next prompt is accepted once send() returns.
        bridge.interrupt();
        return;
      case "hibernate":
        // Conversation state persists in SQLite keyed by sessionId; the resume
        // handle IS the sessionId, replayed in a later cold-start descriptor.
        await teardown("hibernate", sessionId);
        return;
      case "shutdown":
        await teardown("exit");
        return;
    }
  };

  port.on("message", (messageEvent) => {
    const data = messageEvent.data;
    if (state === "await-descriptor") {
      if (!isSessionDescriptor(data)) {
        post({
          type: "host:error",
          sessionId,
          code: "bad-descriptor",
          message: "First host message was not a valid session descriptor.",
        });
        void teardown("error");
        return;
      }
      state = "running";
      void boot(data);
      return;
    }
    if (!isHostCommand(data) || data.sessionId !== sessionId) {
      // Foreign or garbled message: drop it (Daintree validates with Zod too).
      return;
    }
    void handleCommand(data);
  });
  port.start?.();
}

void main();
