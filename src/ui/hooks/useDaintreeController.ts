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
import { handleUiCommand } from "../../cli/commandData.js";
import { buildAgentRows } from "../presentation/operations.js";
import { presentTool } from "../presentation/tools.js";
import type {
  ActivityItem,
  DashboardState,
  PendingConfirm,
  TranscriptCell,
  TurnCell,
} from "../types.js";
import type { QueueEvent } from "../../schemas.js";
import { logDebug } from "../../debugLog.js";
import { isActionableWake, buildWakePrompt } from "../../agent/wake.js";

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
  const next = [...cells, turn];
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
      return [...cells, newTurn(action.text, now)];

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
      ];

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
        ];
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
      ];
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
      ];

    default:
      return cells;
  }
}

function snapshot(app: App): DashboardState {
  return {
    mcp: app.mcp.status(),
    workflowRuns: app.db
      .listWorkflowRuns()
      .filter(
        (r) =>
          r.status === "pending" ||
          r.status === "active" ||
          r.status === "blocked",
      )
      .slice(0, 5),
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
  /** Submit user input. Returns false (synchronously) if rejected — empty, or a
   *  turn is already in flight — so the composer can keep the text. */
  sendUserMessage: (text: string) => boolean;
  /** Print a one-shot operations snapshot inline (the `^O` key). */
  openOps: () => void;
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
  const [dashboard, setDashboard] = useState<DashboardState>(() =>
    snapshot(app),
  );
  // Synchronous serialization lock. `busy` is async React state and can't gate
  // back-to-back submits in the same tick; this ref can.
  const inFlight = useRef(false);

  // Autonomous wake-ups: attention events the scheduler surfaces while idle are
  // queued here, then fed to the model as a turn so it can decide to read a
  // finished/blocked terminal and report back. Drained only when no turn is in
  // flight; `reactToWakeRef` lets the (once-registered) scheduler callback and the
  // turn-finished path invoke the latest reactor without re-registering.
  const pendingWake = useRef<QueueEvent[]>([]);
  const reactToWakeRef = useRef<() => void>(() => {});
  // Whether the current burst has already been retried once after a failure —
  // bounds retries so a persistently-failing model can't spin the wake loop.
  const wakeRetried = useRef(false);

  const reactToWake = useCallback(async () => {
    if (inFlight.current) return; // a turn is running — drain when it finishes
    const events = pendingWake.current;
    if (events.length === 0) return;
    pendingWake.current = [];
    inFlight.current = true;
    setBusy(true);
    logDebug(app.config, "wake.react", {
      count: events.length,
      titles: events.map((e) => (e as { title?: string }).title),
    });
    try {
      // readOnly: an autonomous turn the user didn't initiate must only be able to
      // inspect (read the terminal) and report — never run a mutating tool.
      await app.session.send(buildWakePrompt(events), { readOnly: true });
      wakeRetried.current = false; // success resets the per-burst retry budget
    } catch (err) {
      bridge.emit({
        type: "log",
        level: "error",
        message: err instanceof Error ? err.message : String(err),
      });
      // Best-effort single retry so a transient model failure isn't stranded;
      // capped to avoid a failure loop. The events also remain in the inbox.
      if (!wakeRetried.current) {
        wakeRetried.current = true;
        pendingWake.current.unshift(...events);
      }
    } finally {
      inFlight.current = false;
      setBusy(false);
      // More events may have arrived during the reaction (or a retry was queued) —
      // drain them too.
      if (pendingWake.current.length > 0) reactToWakeRef.current();
    }
  }, [app, bridge]);

  useEffect(() => {
    reactToWakeRef.current = () => void reactToWake();
  }, [reactToWake]);

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
        // The scheduler outlives this hook (startScheduler is idempotent and keeps
        // the first callback). After cleanup, do no UI/turn work against a dead
        // closure.
        if (disposed) return;
        bridge.emit({ type: "attention", events });
        // Terminal-watcher events autonomously wake the model so it can read the
        // terminal and report; other sources stay passive in the inbox.
        const actionable = events.filter(isActionableWake);
        if (actionable.length > 0) {
          // A burst starting from empty gets a fresh single-retry budget.
          if (pendingWake.current.length === 0) wakeRetried.current = false;
          pendingWake.current.push(...actionable);
          logDebug(app.config, "wake.enqueue", { count: actionable.length });
          reactToWakeRef.current();
        }
      });
      const st = app.mcp.status();
      bridge.emit({
        type: "log",
        level: st.connected ? "info" : "warn",
        message: st.connected
          ? `Connected to Daintree MCP.`
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
    (text: string): boolean => {
      const trimmed = text.trim();
      // Decide acceptance SYNCHRONOUSLY so the composer can keep the typed text if
      // we reject (e.g. an autonomous wake turn grabbed the lock in the same tick).
      // The ref lock gates both model turns and async slash commands.
      if (!trimmed) return false;
      if (inFlight.current) return false;
      inFlight.current = true;
      setBusy(true);
      // Run the turn in the background; acceptance is already decided.
      void (async () => {
        try {
          if (trimmed.startsWith("/")) {
            const result = await handleUiCommand(trimmed, app);
            if (result.quit) {
              onExit?.();
              return;
            }
            // Every command prints its result inline into the stream (Claude Code
            // shaped), where it commits to the terminal's scrollback — no
            // full-screen panel takes over. `switchPanel` is now just metadata.
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
          // A watcher may have surfaced something while this turn ran — react now
          // that we're idle, so it isn't stranded in the inbox.
          if (pendingWake.current.length > 0) reactToWakeRef.current();
        }
      })();
      return true;
    },
    [app, bridge, onExit],
  );

  const resolveConfirm = useCallback((approved: boolean) => {
    setPendingConfirm((cur) => {
      cur?.resolve(approved);
      return null;
    });
  }, []);

  // `^O`: snapshot the current operational state into the stream as a command
  // cell. Inline like Claude Code's /status — it scrolls away into history, it
  // doesn't take over the screen. Reads the latest dashboard each press.
  const openOps = useCallback(() => {
    dispatch({
      type: "command:add",
      title: "Operations",
      text: formatOpsSnapshot(snapshot(app)),
    });
  }, [app]);

  const stage = useMemo(() => deriveStage(transcript), [transcript]);

  return {
    bridge,
    transcript,
    dashboard,
    busy,
    stage,
    pendingConfirm,
    sendUserMessage,
    openOps,
    resolveConfirm,
  };
}

/** Render the ops deck (agents · timers · runs · inbox) as a compact text block
 *  for the inline `^O` snapshot. */
function formatOpsSnapshot(dashboard: DashboardState): string {
  const agents = buildAgentRows(dashboard.watchers);
  const runs = dashboard.workflowRuns ?? [];
  const { timers, inbox } = dashboard;
  const out: string[] = [
    `agents ${agents.length} · runs ${runs.length} · timers ${timers.length} · inbox ${inbox.length}`,
  ];
  for (const a of agents) {
    out.push(`  ${a.id}  [${a.classification ?? "pending"}] ${a.goal || a.title}`);
  }
  for (const r of runs) {
    const title = r.issueTitle ?? r.branch ?? r.worktreeId ?? r.id;
    out.push(`  run ${r.status} — ${title}`);
  }
  for (const t of timers) {
    out.push(`  timer ${t.title} — ${new Date(t.fireAt).toLocaleTimeString()}`);
  }
  for (const e of inbox.slice(0, 5)) {
    const ev = e as { title?: string; summary?: string };
    out.push(`  ! ${ev.title ?? "event"}${ev.summary ? ` — ${ev.summary}` : ""}`);
  }
  return out.join("\n");
}
