/**
 * The controller hook owns all UI state and wires the runtime into React. It:
 *   - registers the agent-event / confirm / log hooks on the App,
 *   - connects MCP and starts the scheduler once,
 *   - reduces the bridge event stream into a timeline,
 *   - polls durable operational state (watchers/timers/inbox/audit) for the deck,
 *   - routes slash commands through the structured command layer.
 */
import {
  useCallback,
  useEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
} from "react";
import type { App } from "../../cli/app.js";
import { UiBridge, type UiBridgeEvent } from "../bridge.js";
import { handleUiCommand, type PanelKey } from "../../cli/commandData.js";
import type {
  DashboardState,
  PendingConfirm,
  TimelineItem,
} from "../types.js";
import type { QueueEvent } from "../../schemas.js";

const MAX_TIMELINE = 200;

let idCounter = 0;
function uid(prefix: string): string {
  return `${prefix}_${(idCounter++).toString(36)}`;
}

/**
 * Close out a trailing streaming-assistant row. A tool-call turn (or an error)
 * emits assistant:start but no assistant:end, which would otherwise leave a blank
 * "▌" row. Drop it if it has no visible text; otherwise stop its caret.
 */
function finalizeStream(items: TimelineItem[]): TimelineItem[] {
  if (items.length === 0) return items;
  const last = items[items.length - 1];
  if (last.kind !== "assistant" || !last.streaming) return items;
  if (last.text.trim() === "") return items.slice(0, -1);
  const copy = [...items];
  copy[copy.length - 1] = { ...last, streaming: false };
  return copy;
}

export type ControllerAction =
  | UiBridgeEvent
  | { type: "user:add"; text: string }
  | { type: "command:add"; title: string; text: string };

export function timelineReducer(
  items: TimelineItem[],
  action: ControllerAction,
): TimelineItem[] {
  const now = Date.now();
  switch (action.type) {
    case "user:add":
      return [
        ...items,
        { id: uid("usr"), kind: "user" as const, text: action.text, ts: now },
      ].slice(-MAX_TIMELINE);

    case "command:add":
      return [
        ...items,
        {
          id: uid("cmd"),
          kind: "command" as const,
          title: action.title,
          text: action.text,
          ts: now,
        },
      ].slice(-MAX_TIMELINE);

    case "assistant:start":
      return [
        ...items,
        {
          id: uid("ast"),
          kind: "assistant" as const,
          text: "",
          streaming: true,
          ts: now,
        },
      ].slice(-MAX_TIMELINE);

    case "assistant:token": {
      const copy = [...items];
      const last = copy[copy.length - 1];
      if (last?.kind === "assistant" && last.streaming) {
        copy[copy.length - 1] = { ...last, text: last.text + action.token };
        return copy;
      }
      return [
        ...items,
        {
          id: uid("ast"),
          kind: "assistant" as const,
          text: action.token,
          streaming: true,
          ts: now,
        },
      ].slice(-MAX_TIMELINE);
    }

    case "assistant:end": {
      const copy = [...items];
      const last = copy[copy.length - 1];
      if (last?.kind === "assistant" && last.streaming) {
        copy[copy.length - 1] = {
          ...last,
          text: action.content || last.text,
          streaming: false,
        };
        return copy;
      }
      // No active stream (pure tool turns) — append the final text if any.
      if (action.content) {
        return [
          ...items,
          {
            id: uid("ast"),
            kind: "assistant" as const,
            text: action.content,
            streaming: false,
            ts: now,
          },
        ].slice(-MAX_TIMELINE);
      }
      return copy;
    }

    case "tool:call":
      return [
        ...finalizeStream(items),
        {
          id: uid("tool"),
          kind: "tool" as const,
          name: action.name,
          args: action.args,
          ts: now,
        },
      ].slice(-MAX_TIMELINE);

    case "tool:result": {
      const copy = [...items];
      // Resolve the most recent un-finished tool row with the same name.
      for (let i = copy.length - 1; i >= 0; i--) {
        const item = copy[i];
        if (
          item.kind === "tool" &&
          item.name === action.name &&
          item.ok === undefined
        ) {
          copy[i] = {
            ...item,
            ok: Boolean(action.result.ok),
            summary: action.result.summary ?? "",
          };
          return copy;
        }
      }
      return copy;
    }

    case "log":
      return [
        // An error ends the turn without an assistant:end — clear any stale caret.
        ...(action.level === "error" ? finalizeStream(items) : items),
        {
          id: uid("log"),
          kind: "system" as const,
          level: action.level,
          text: action.message,
          ts: now,
        },
      ].slice(-MAX_TIMELINE);

    case "attention":
      return [
        ...items,
        ...action.events.map((e) => {
          const ev = e as { title?: string; summary?: string };
          return {
            id: uid("att"),
            kind: "system" as const,
            level: "warn" as const,
            text: `${ev.title ?? "event"}: ${ev.summary ?? ""}`,
            ts: now,
          };
        }),
      ].slice(-MAX_TIMELINE);

    default:
      return items;
  }
}

function snapshot(app: App): DashboardState {
  return {
    mcp: app.mcp.status(),
    watchers: app.db.listWatchers("active"),
    timers: app.db.listTimers("scheduled"),
    inbox: app.queue.digest({ severityAtLeast: "attention", maxItems: 30 }),
    audit: app.db.listAudit(20),
  };
}

export interface DaintreeController {
  bridge: UiBridge;
  timeline: TimelineItem[];
  dashboard: DashboardState;
  busy: boolean;
  pendingConfirm: PendingConfirm | null;
  activePanel: PanelKey | null;
  setActivePanel: (panel: PanelKey | null) => void;
  sendUserMessage: (text: string) => Promise<void>;
  resolveConfirm: (approved: boolean) => void;
}

export function useDaintreeController(
  app: App,
  onExit?: () => void,
): DaintreeController {
  const bridge = useMemo(() => new UiBridge(), []);
  const [timeline, dispatch] = useReducer(timelineReducer, []);
  const [pendingConfirm, setPendingConfirm] = useState<PendingConfirm | null>(
    null,
  );
  const [busy, setBusy] = useState(false);
  const [activePanel, setActivePanel] = useState<PanelKey | null>(null);
  const [dashboard, setDashboard] = useState<DashboardState>(() =>
    snapshot(app),
  );
  // Synchronous serialization lock. `busy` is async React state and can't gate
  // back-to-back submits in the same tick; this ref can.
  const inFlight = useRef(false);

  // Subscribe to the bridge: confirms drive modal state, everything else reduces.
  useEffect(() => {
    return bridge.subscribe((event) => {
      if (event.type === "confirm") {
        setPendingConfirm(event.pending);
      } else {
        dispatch(event);
      }
    });
  }, [bridge]);

  // One-time wiring: hooks, MCP connect, scheduler, dashboard polling.
  useEffect(() => {
    let disposed = false;
    app.setHooks({
      agentEvents: bridge.agentEvents(),
      confirm: (req) => bridge.requestConfirm(req),
      log: (msg) => bridge.emit({ type: "log", level: "info", message: msg }),
    });

    void (async () => {
      await app.connectMcp();
      if (disposed) return;
      app.startScheduler((events: QueueEvent[]) => {
        bridge.emit({ type: "attention", events });
      });
      const st = app.mcp.status();
      bridge.emit({
        type: "log",
        level: st.connected ? "info" : "warn",
        message: st.connected
          ? `Connected to Daintree MCP (${st.transport}, ${st.toolCount ?? "?"} tools).`
          : `Daintree MCP not connected — ${st.error ?? "no url/token"}. Running degraded.`,
      });
      setDashboard(snapshot(app));
    })();

    const timer = setInterval(() => {
      if (!disposed) setDashboard(snapshot(app));
    }, 1000);

    return () => {
      disposed = true;
      clearInterval(timer);
      // Unblock anything already awaiting ctx.confirm(), AND route any future
      // confirm (e.g. an in-flight tool call during ^C) to an auto-decline so a
      // dispatch can never block on a modal that no longer has a UI subscriber.
      bridge.settlePendingConfirms(false);
      app.setHooks({ confirm: async () => false });
    };
  }, [app, bridge]);

  const sendUserMessage = useCallback(
    async (text: string) => {
      const trimmed = text.trim();
      // The ref lock gates synchronously; this covers both model turns AND async
      // slash commands (e.g. /compact) so they can't race app.session state.
      if (!trimmed || inFlight.current) return;
      inFlight.current = true;
      setBusy(true);
      try {
        if (trimmed.startsWith("/")) {
          const result = await handleUiCommand(trimmed, app);
          if (result.quit) {
            onExit?.();
            return;
          }
          if (result.switchPanel) setActivePanel(result.switchPanel);
          if (result.title || result.text) {
            dispatch({
              type: "command:add",
              title: result.title ?? "",
              text: result.text ?? "",
            });
          }
          return;
        }

        dispatch({ type: "user:add", text: trimmed });
        await app.session.send(trimmed);
      } catch (err) {
        bridge.emit({
          type: "log",
          level: "error",
          message: err instanceof Error ? err.message : String(err),
        });
      } finally {
        inFlight.current = false;
        setBusy(false);
      }
    },
    [app, bridge, onExit],
  );

  const resolveConfirm = useCallback(
    (approved: boolean) => {
      setPendingConfirm((cur) => {
        cur?.resolve(approved);
        return null;
      });
    },
    [],
  );

  return {
    bridge,
    timeline,
    dashboard,
    busy,
    pendingConfirm,
    activePanel,
    setActivePanel,
    sendUserMessage,
    resolveConfirm,
  };
}
