/**
 * The controller hook owns all UI state and wires the runtime into React. It:
 *   - registers the agent-event / confirm / log hooks on the App,
 *   - connects MCP and starts the scheduler once,
 *   - folds the bridge event stream into a RUN-ORIENTED transcript (turns, not a
 *     flat row list), matching tool results to their call by id,
 *   - polls durable operational state (watchers/timers/inbox/audit),
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
import { presentTool } from "../presentation/tools.js";
import type {
  ActivityItem,
  DashboardState,
  PendingConfirm,
  TranscriptCell,
  TurnCell,
} from "../types.js";
import type { QueueEvent } from "../../schemas.js";

const MAX_CELLS = 200;

let idCounter = 0;
function uid(prefix: string): string {
  return `${prefix}_${(idCounter++).toString(36)}`;
}

/** Index of the last turn cell when it is still active, else -1. */
function activeTurnIndex(cells: TranscriptCell[]): number {
  for (let i = cells.length - 1; i >= 0; i--) {
    const c = cells[i];
    if (c.kind === "turn") return c.state === "active" ? i : -1;
  }
  return -1;
}

/** Immutably replace the cell at `index`. */
function replaceAt(
  cells: TranscriptCell[],
  index: number,
  next: TranscriptCell,
): TranscriptCell[] {
  const copy = [...cells];
  copy[index] = next;
  return copy;
}

function newTurn(userText: string, now: number): TurnCell {
  return {
    kind: "turn",
    id: uid("turn"),
    userText,
    assistantText: "",
    streaming: false,
    activities: [],
    notes: [],
    state: "active",
    ts: now,
  };
}

/** Ensure there is an active turn to attach to; create a system-origin one if not. */
function ensureActiveTurn(
  cells: TranscriptCell[],
  now: number,
): { cells: TranscriptCell[]; index: number } {
  const idx = activeTurnIndex(cells);
  if (idx >= 0) return { cells, index: idx };
  const turn = newTurn("", now);
  const next = [...cells, turn].slice(-MAX_CELLS);
  return { cells: next, index: next.length - 1 };
}

/** Stop a trailing streaming caret without dropping committed assistant text. */
function stopCaret(turn: TurnCell): TurnCell {
  return turn.streaming ? { ...turn, streaming: false } : turn;
}

export type ControllerAction =
  | UiBridgeEvent
  | { type: "user:add"; text: string }
  | { type: "command:add"; title: string; text: string };

export function transcriptReducer(
  cells: TranscriptCell[],
  action: ControllerAction,
): TranscriptCell[] {
  const now = Date.now();
  switch (action.type) {
    case "user:add":
      return [...cells, newTurn(action.text, now)].slice(-MAX_CELLS);

    case "command:add":
      return [
        ...cells,
        {
          kind: "command" as const,
          id: uid("cmd"),
          title: action.title,
          text: action.text,
          ts: now,
        },
      ].slice(-MAX_CELLS);

    case "assistant:start": {
      const { cells: c, index } = ensureActiveTurn(cells, now);
      const turn = c[index] as TurnCell;
      return replaceAt(c, index, { ...turn, streaming: true });
    }

    case "assistant:token": {
      const { cells: c, index } = ensureActiveTurn(cells, now);
      const turn = c[index] as TurnCell;
      return replaceAt(c, index, {
        ...turn,
        assistantText: turn.assistantText + action.token,
        streaming: true,
      });
    }

    case "assistant:end": {
      const idx = activeTurnIndex(cells);
      if (idx < 0) {
        if (!action.content) return cells;
        const turn = newTurn("", now);
        return [
          ...cells,
          { ...turn, assistantText: action.content, state: "complete" as const },
        ].slice(-MAX_CELLS);
      }
      const turn = cells[idx] as TurnCell;
      return replaceAt(cells, idx, {
        ...turn,
        assistantText: action.content || turn.assistantText,
        streaming: false,
        state: turn.state === "failed" ? "failed" : "complete",
      });
    }

    case "tool:call": {
      const { cells: c, index } = ensureActiveTurn(cells, now);
      const turn = stopCaret(c[index] as TurnCell);
      const p = presentTool(action.name, action.args);
      const activity: ActivityItem = {
        id: action.id,
        name: action.name,
        label: p.label,
        detail: p.detail,
        args: action.args,
        state: "active",
        startedAt: action.startedAt,
      };
      return replaceAt(c, index, {
        ...turn,
        activities: [...turn.activities, activity],
      });
    }

    case "tool:result": {
      // Match by call id across all turns (results can arrive out of order).
      for (let i = cells.length - 1; i >= 0; i--) {
        const c = cells[i];
        if (c.kind !== "turn") continue;
        const ai = c.activities.findIndex((a) => a.id === action.id);
        if (ai >= 0) {
          const activities = [...c.activities];
          activities[ai] = {
            ...activities[ai],
            state: action.result.ok ? "done" : "failed",
            summary: action.result.summary ?? "",
            endedAt: action.endedAt,
          };
          return replaceAt(cells, i, { ...c, activities });
        }
      }
      return cells;
    }

    case "log": {
      const idx = activeTurnIndex(cells);
      if (idx >= 0) {
        // Attach to the active turn; an error fails it and stops the caret.
        const turn = stopCaret(cells[idx] as TurnCell);
        const level = action.level;
        return replaceAt(cells, idx, {
          ...turn,
          state: level === "error" ? "failed" : turn.state,
          notes: [
            ...turn.notes,
            { id: uid("note"), level, text: action.message, ts: now },
          ],
        });
      }
      return [
        ...cells,
        {
          kind: "note" as const,
          id: uid("note"),
          level: action.level,
          text: action.message,
          ts: now,
        },
      ].slice(-MAX_CELLS);
    }

    case "attention":
      return [
        ...cells,
        ...action.events.map((e) => {
          const ev = e as { title?: string; summary?: string };
          return {
            kind: "note" as const,
            id: uid("att"),
            level: "warn" as const,
            text: `${ev.title ?? "event"}${ev.summary ? `: ${ev.summary}` : ""}`,
            ts: now,
          };
        }),
      ].slice(-MAX_CELLS);

    default:
      return cells;
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

/** The composer's live-stage label — derived from the active run, not random. */
function deriveStage(cells: TranscriptCell[]): string {
  const idx = activeTurnIndex(cells);
  if (idx < 0) return "Thinking";
  const turn = cells[idx] as TurnCell;
  const active = [...turn.activities].reverse().find((a) => a.state === "active");
  if (active) {
    // Map the verb to a control-room stage.
    switch (active.label) {
      case "Delegated":
        return "Delegating";
      case "Watching":
        return "Watching";
      case "Read":
      case "Listed":
      case "Searched":
        return "Inspecting";
      case "Scheduled":
        return "Scheduling";
      default:
        return "Working";
    }
  }
  if (turn.streaming) return "Responding";
  if (turn.activities.length > 0) return "Orienting";
  return "Planning";
}

export interface DaintreeController {
  bridge: UiBridge;
  transcript: TranscriptCell[];
  dashboard: DashboardState;
  busy: boolean;
  /** Live stage label for the composer (Inspecting, Delegating, Watching…). */
  stage: string;
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
  const [transcript, dispatch] = useReducer(transcriptReducer, []);
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
          if (result.switchPanel) {
            // A panel command opens a purposeful view rather than dumping text
            // into the transcript. Bump to a fresh object each time so re-running
            // the same command re-opens the view even after it was closed.
            setActivePanel(result.switchPanel);
          } else if (result.title || result.text) {
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

  const resolveConfirm = useCallback((approved: boolean) => {
    setPendingConfirm((cur) => {
      cur?.resolve(approved);
      return null;
    });
  }, []);

  const stage = useMemo(() => deriveStage(transcript), [transcript]);

  return {
    bridge,
    transcript,
    dashboard,
    busy,
    stage,
    pendingConfirm,
    activePanel,
    setActivePanel,
    sendUserMessage,
    resolveConfirm,
  };
}
