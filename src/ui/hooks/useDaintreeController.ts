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
  useLayoutEffect,
  useMemo,
  useReducer,
  useRef,
  useState,
  type RefObject,
} from "react";
import type { CliRenderer } from "@opentui/core";
import type { App } from "../../cli/app.js";
import type { ComposerHandle } from "../components/Composer.js";
import { UiBridge, type UiBridgeEvent } from "../bridge.js";
import { handleUiCommand, type PanelKey } from "../../cli/commandData.js";
import { clearHostTerminal } from "../../cli/terminalClear.js";
import { presentTool } from "../presentation/tools.js";
import type {
  ActivityItem,
  DashboardState,
  PendingConfirm,
  SessionUsage,
  TranscriptCell,
  TurnCell,
} from "../types.js";
import type { QueueEvent } from "../../schemas.js";
import { logDebug } from "../../debugLog.js";
import {
  isActionableWake,
  buildWakePrompt,
  isWakeFailureReply,
} from "../../agent/wake.js";

let idCounter = 0;
function uid(prefix: string): string {
  return `${prefix}_${(idCounter++).toString(36)}`;
}

/** A fresh session-usage rollup: zeroed counters, no cost or context reading yet. */
const INITIAL_SESSION_USAGE: SessionUsage = {
  promptTokens: 0,
  completionTokens: 0,
  totalTokens: 0,
  costUsd: undefined,
  contextTokens: 0,
  contextThreshold: 0,
};

/** Index of the last turn cell when it is still active, else -1. */
function activeTurnIndex(cells: TranscriptCell[]): number {
  for (let i = cells.length - 1; i >= 0; i--) {
    const c = cells[i];
    if (c.kind === "turn") return c.state === "active" ? i : -1;
  }
  return -1;
}

/**
 * The most recent turn AND its index IF it is a just-submitted, pre-stream turn —
 * the only state from which Escape can pull the message back into the composer
 * (issue #61). A turn qualifies only before any assistant work has landed:
 *   - active, with empty `assistantText`, and
 *   - not streaming, and
 *   - no tool activities.
 * The `activities` check is load-bearing, not redundant with `!streaming`: a
 * `tool:call` runs `stopCaret`, which resets `streaming` to false, and tool calls
 * never set `assistantText` — so a turn that already spawned an agent or scheduled
 * a timer would otherwise look pre-stream and be silently erased.
 *
 * Scans back past trailing non-turn cells (a background attention note can land
 * between the user message and the Escape press) to the most recent turn, then
 * checks only that one: pull-back acts on the message the user just sent, never an
 * earlier turn. Returns null otherwise, so callers fall back to plain cancel.
 */
export function pullbackCandidate(
  cells: TranscriptCell[],
): { turn: TurnCell; index: number } | null {
  for (let i = cells.length - 1; i >= 0; i--) {
    const c = cells[i];
    if (c.kind !== "turn") continue;
    if (
      c.state === "active" &&
      !c.streaming &&
      c.assistantText === "" &&
      c.activities.length === 0
    ) {
      return { turn: c, index: i };
    }
    return null; // the newest turn isn't pre-stream — nothing to pull back
  }
  return null;
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
  | { type: "user:pullback" }
  | { type: "command:add"; title: string; text: string }
  | { type: "transcript:clear" };

export function transcriptReducer(
  cells: TranscriptCell[],
  action: ControllerAction,
): TranscriptCell[] {
  const now = Date.now();
  switch (action.type) {
    case "user:add":
      return [...cells, newTurn(action.text, now)];

    case "transcript:clear":
      // /clear resets the session to its initial controls — drop every in-flight
      // cell so the live region starts empty. Rows already scrolled into the host's
      // native scrollback stay there (same as a shell `clear`); the caller also wipes
      // that scrollback via clearHostTerminal so the cleared conversation can't be
      // wheeled back into.
      return [];

    case "user:pullback": {
      // Escape pressed before any assistant output landed: drop the just-added
      // turn so the transcript shows no trace of it and the text returns to the
      // composer for editing. Remove that turn by index (not just the tail) so a
      // background attention note that landed after it survives. The guard is the
      // source of truth for the race with assistant:start — if the turn already
      // began streaming or ran a tool, this is a no-op and the in-flight turn stays
      // put (the caller then applies plain-cancel instead).
      const c = pullbackCandidate(cells);
      if (!c) return cells;
      return [...cells.slice(0, c.index), ...cells.slice(c.index + 1)];
    }

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
      // Only an ACTIVE turn can be completed. A stray end with no active turn —
      // e.g. arriving after the turn was already cancelled — must not manufacture a
      // phantom turn. Turns are born from user:add, or from the loop's own
      // start/token/tool events via ensureActiveTurn; a terminal event never is.
      if (idx < 0) return cells;
      const turn = cells[idx] as TurnCell;
      return replaceAt(cells, idx, {
        ...turn,
        assistantText: action.content || turn.assistantText,
        streaming: false,
        state: turn.state === "failed" ? "failed" : "complete",
      });
    }

    case "assistant:cancelled": {
      // User aborted the turn. Stop the caret, keep whatever streamed so far, and
      // mark it cancelled (distinct from failed) with a one-line note. Marking the
      // turn non-active also means a stray late event can't reattach to it.
      const idx = activeTurnIndex(cells);
      if (idx < 0) return cells;
      const turn = cells[idx] as TurnCell;
      return replaceAt(cells, idx, {
        ...turn,
        assistantText: action.content || turn.assistantText,
        streaming: false,
        state: "cancelled",
        notes: [
          ...turn.notes,
          { id: uid("note"), level: "info", text: "Turn cancelled", ts: now },
        ],
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
          // Only the active turn is still live; a completed/cancelled turn has
          // already been committed to native scrollback via <Static>, which never
          // repaints. The loop feeds every tool result back before the turn ends,
          // so this only fires on a stray late/duplicate result — drop it rather
          // than mutate an immutable committed cell (would desync Ink's <Static>).
          if (c.state !== "active") return cells;
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

// Opaque ids Daintree may name a bound directory with — long hex (e.g.
// `ad92568c236b3a18`) or a UUID. We never show these as a "project name"; we wait
// for the real name from the MCP instead of flashing a meaningless string.
const ID_LIKE = /^(?:[0-9a-f]{12,}|[0-9a-f-]{32,})$/i;

/**
 * A provisional project name from the bound directory, shown immediately so the
 * header isn't blank before the MCP answers. The leaf of the project path is a good
 * human name for a plainly-named repo ("assistant"), but when Daintree binds us to a
 * directory named by opaque id we return undefined and let the async MCP fetch fill
 * the real name in.
 */
function provisionalProjectName(projectPath: string): string | undefined {
  const leaf = projectPath.split("/").filter(Boolean).pop();
  if (!leaf || ID_LIKE.test(leaf)) return undefined;
  return leaf;
}

/** Pull a project name out of a getContext payload — either the top-level
 *  `projectName` or a nested `project.name`. Returns a trimmed non-empty string.
 *  Exported for unit tests; this parse is the seam the header name depends on. */
export function readProjectName(value: unknown): string | undefined {
  if (!value || typeof value !== "object") return undefined;
  const rec = value as Record<string, unknown>;
  const direct = rec.projectName;
  if (typeof direct === "string" && direct.trim()) return direct.trim();
  const proj = rec.project;
  if (proj && typeof proj === "object") {
    const nested = (proj as Record<string, unknown>).name;
    if (typeof nested === "string" && nested.trim()) return nested.trim();
  }
  return undefined;
}

/**
 * The authoritative project name from Daintree: `actions.getContext` returns a
 * context object whose `projectName` is the bound project's display name. Daintree
 * only emits `structuredContent` when an action declares an output schema (see
 * buildStructuredContent in the daintree repo), and the assistant's MCP SDK doesn't
 * always surface it — but the same object is ALWAYS serialized into the result's
 * text content, so we read structuredContent first and fall back to parsing the
 * text JSON. Best-effort and non-blocking: any failure resolves to undefined so the
 * caller keeps whatever provisional name it already had.
 */
async function fetchProjectName(app: App): Promise<string | undefined> {
  try {
    if (!app.mcp.status().connected) return undefined;
    const res = await app.mcp.callTool("actions.getContext");
    let name = readProjectName(res?.structuredContent);
    if (!name && typeof res?.text === "string" && res.text.trim()) {
      try {
        name = readProjectName(JSON.parse(res.text));
      } catch {
        /* text wasn't JSON — ignore */
      }
    }
    logDebug(app.config, "header.projectName", {
      isError: res?.isError ?? null,
      hasStructured: res?.structuredContent != null,
      textPreview:
        typeof res?.text === "string" ? res.text.slice(0, 400) : null,
      resolved: name ?? null,
    });
    return name;
  } catch (e) {
    logDebug(app.config, "header.projectName.error", { error: String(e) });
    return undefined;
  }
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
  // No active tool verb to name → the model is composing (waiting for the first
  // token, streaming prose, or between tool steps). All of these read as "Thinking":
  // once real output is streaming the transcript itself shows it, so the composer
  // doesn't separately announce "Responding" — it just signals that work is ongoing.
  return "Thinking";
}

export interface DaintreeController {
  bridge: UiBridge;
  transcript: TranscriptCell[];
  dashboard: DashboardState;
  /** Live token/cost/context-pressure rollup for the status line. */
  sessionUsage: SessionUsage;
  busy: boolean;
  /** Live stage label for the composer (Inspecting, Delegating, Watching…). */
  stage: string;
  pendingConfirm: PendingConfirm | null;
  /** Submit user input. Returns false (synchronously) if rejected — empty, or a
   *  turn is already in flight — so the composer can keep the text. */
  sendUserMessage: (text: string) => boolean;
  /** Abort the in-flight user turn (Escape-to-cancel). No-op when idle. */
  cancelTurn: () => void;
  /** Escape handler for the composer: pulls a just-sent message back into the
   *  input when still pre-stream (abort + remove the turn + restore the text),
   *  else falls back to {@link cancelTurn}. No-op when idle (issue #61). */
  pullBackTurn: () => void;
  /** Imperative handle the composer registers so a pulled-back message can be
   *  pushed back into its buffer. Wired to the rendered `<Composer ref>`. */
  composerRef: RefObject<ComposerHandle | null>;
  /** True only while a cancellable user model turn is in flight (drives the hint). */
  canCancel: boolean;
  /** Number of user follow-ups queued behind the in-flight turn (drives the
   *  "· N queued" hint in the busy indicator). Zero when nothing is waiting. */
  queueDepth: number;
  /** The purposeful view a panel command (`/help`, `/watchers`, …) wants open. */
  activePanel: PanelKey | null;
  setActivePanel: (panel: PanelKey | null) => void;
  resolveConfirm: (approved: boolean) => void;
  /** The bound project's display name (from Daintree's MCP, basename fallback). */
  projectName?: string;
  /** True while the boot splash should own the screen (startup loading in background). */
  booting: boolean;
  /** Called by the splash when its draw has finished — one half of the dismiss gate. */
  notifyAnimationDone: () => void;
  /**
   * Monotonic counter bumped on every `/clear`. Signals that the transcript was
   * REPLACED (not appended), so the scrollback layer can reset its commit cursor and
   * re-commit the masthead deterministically. See {@link useScrollbackTranscript}.
   */
  clearNonce: number;
  /**
   * Monotonic counter bumped on every terminal-resize "nuclear redraw". Like
   * {@link clearNonce} it wipes the host scrollback and forces a full repaint, but it
   * does NOT touch the transcript — the same cells are re-committed fresh at the new
   * width. {@link useResizeRedraw} drives it via {@link requestRedraw}.
   */
  redrawNonce: number;
  /**
   * Trigger a nuclear redraw: wipe the host screen + scrollback, reset OpenTUI's
   * split-footer replay record, force a full repaint, and re-commit the masthead + the
   * whole transcript at the current width. Bumps {@link redrawNonce}. Called by
   * {@link useResizeRedraw} once a resize settles (and once on the boot hand-off).
   */
  requestRedraw: () => void;
}

/**
 * Force OpenTUI to fully repaint the cockpit from the top of the viewport.
 *
 * `/clear` wipes the host terminal's screen + scrollback behind OpenTUI's back
 * (clearHostTerminal). OpenTUI renders by DIFFING the new frame against a shadow
 * copy of what it last drew, so after that external wipe it still believes the
 * masthead (and everything that didn't change) is on screen and skips re-emitting
 * it — which is exactly the blank gap at the top. Mirror OpenTUI's own reset recipe
 * (`resetSplitFooterForReplay`): blank both shadow buffers and raise the
 * forced-repaint flag so the next frame redraws the whole tree from line 1 —
 * masthead included. Must run AFTER React has committed the cleared (short) tree,
 * or it would repaint the OLD conversation straight back into scrollback.
 * Fully guarded — a cosmetic repaint must never take down the command.
 */
function resyncCockpitSurface(
  renderer: CliRenderer,
  cfg: App["config"],
): void {
  // Split-footer keeps its OWN record of the lines it has committed to scrollback so it
  // can replay them on resize. After an external host-scrollback wipe that record is
  // stale, so drop it (`clearSavedLines`) and let the footer redraw from a clean slate;
  // the transcript hook then re-commits the masthead on top of the fresh scrollback.
  // Guarded SEPARATELY from the shadow-buffer reset below so a stub/older renderer that
  // lacks the method still gets the forced repaint that fixes the blank-header gap.
  try {
    renderer.resetSplitFooterForReplay({ clearSavedLines: true });
  } catch (err) {
    logDebug(cfg, "ui.clear.split_reset_failed", {
      error: err instanceof Error ? err.message : String(err),
    });
  }
  try {
    renderer.currentRenderBuffer.clear();
    renderer.nextRenderBuffer.clear();
    // `forceFullRepaintRequested` is private in the typings but is the same flag
    // OpenTUI sets on resize / resume / replay to bypass the per-cell diff.
    (
      renderer as unknown as { forceFullRepaintRequested: boolean }
    ).forceFullRepaintRequested = true;
    renderer.requestRender();
  } catch (err) {
    // A repaint nudge must never break /clear — but stay loud in the debug trace.
    // This reaches into a private OpenTUI field; if a version bump moves it, the
    // blank-header bug would otherwise silently return with no breadcrumb.
    logDebug(cfg, "ui.clear.repaint_failed", {
      error: err instanceof Error ? err.message : String(err),
    });
  }
}

export function useDaintreeController(
  app: App,
  onExit?: () => void,
  // The active OpenTUI renderer, passed in by DaintreeApp (the @opentui importer) so
  // `/clear` can force a clean full repaint after wiping the host scrollback. Injected
  // rather than pulled via `useRenderer()` here so this module stays free of any
  // @opentui *value* import — it also exports the pure `transcriptReducer`, which is
  // unit-tested under Node where importing @opentui/react would break ESM resolution.
  renderer?: CliRenderer,
): DaintreeController {
  const bridge = useMemo(() => new UiBridge(), []);
  // Raw stdout for terminal side-channels (e.g. wiping host scrollback on /clear).
  // Under OpenTUI's main-screen (inline) mode the process owns the real TTY, so we
  // write the scrollback-wipe escape straight to `process.stdout` (Ink's managed
  // `useStdout()` no longer exists). Written to only outside the render body.
  const stdout = process.stdout;
  const [transcript, dispatch] = useReducer(transcriptReducer, []);
  // Bumped each time `/clear` runs. The host-wipe + forced repaint can't fire inline
  // with the dispatch (React commits the cleared tree asynchronously), so it rides a
  // layout effect that runs AFTER the now-empty tree is committed.
  const [clearNonce, setClearNonce] = useState(0);
  // Bumped each time a terminal resize settles (the "nuclear redraw" — see
  // useResizeRedraw). It drives the SAME host-wipe + split-footer reset + forced repaint
  // as /clear, but leaves the transcript intact so the scrollback layer re-commits the
  // masthead and every sealed cell fresh at the new width. Separate from clearNonce so a
  // resize never reads as a logical "conversation cleared".
  const [redrawNonce, setRedrawNonce] = useState(0);
  const [pendingConfirm, setPendingConfirm] = useState<PendingConfirm | null>(
    null,
  );
  const [busy, setBusy] = useState(false);
  // True only while a cancellable USER model turn is in flight — drives the
  // "Esc cancel" hint. Distinct from `busy`, which is also set for slash commands
  // and autonomous wake turns, neither of which Escape can abort.
  const [canCancel, setCanCancel] = useState(false);
  const [activePanel, setActivePanel] = useState<PanelKey | null>(null);
  const [dashboard, setDashboard] = useState<DashboardState>(() =>
    snapshot(app),
  );
  // Live token/cost/context-pressure rollup for the status line, accumulated from
  // the agent's `usage` events (see the bridge subscription below).
  const [sessionUsage, setSessionUsage] = useState<SessionUsage>(
    INITIAL_SESSION_USAGE,
  );
  // The bound project's name. Seeded from the directory leaf (when it's human, not
  // an opaque id) so the header isn't blank, then upgraded to Daintree's real
  // project name once the MCP answers (see the connect effect below).
  const [projectName, setProjectName] = useState<string | undefined>(() =>
    provisionalProjectName(app.config.projectPath),
  );
  // Boot splash gate. We leave the splash up until BOTH the draw has finished AND
  // startup has settled (MCP connect resolved — connected or degraded — and the first
  // dashboard snapshot is in), so a fast connect can't cut the animation short and a
  // slow one can't flash a half-built cockpit. A hard max-timeout guarantees we never
  // get stuck on the splash if startup stalls. Disabled entirely via config.splash.
  const [booting, setBooting] = useState(() => app.config.splash);
  const startupSettled = useRef(false);
  const animationDone = useRef(false);
  // The masthead commits to <Static> (prints once, never repaints — see ControlRoom),
  // so the authoritative project name has to be resolved BEFORE the cockpit's first
  // paint; a late upgrade can no longer be patched into a live header. So the name
  // fetch is a third splash-dismiss gate (bounded by the 8s bootCap). It flips true
  // the moment the name resolves, the link is down, or the retries give up — never a
  // hang. (When the header was live chrome this could lag the splash freely; Static
  // changed that.)
  const projectSettled = useRef(false);
  const finishBootIfReady = useCallback(() => {
    if (
      startupSettled.current &&
      animationDone.current &&
      projectSettled.current
    )
      setBooting(false);
  }, []);
  const notifyAnimationDone = useCallback(() => {
    animationDone.current = true;
    finishBootIfReady();
  }, [finishBootIfReady]);
  // Synchronous serialization lock. `busy` is async React state and can't gate
  // back-to-back submits in the same tick; this ref can.
  const inFlight = useRef(false);
  // Cancels the in-flight USER turn (Escape-to-cancel). A fresh controller is
  // created per user turn and cleared in the finally, so a queued follow-up never
  // inherits a stale, already-aborted signal. Autonomous wake turns are not wired
  // to this — they run their own short read-only inspections.
  const abortController = useRef<AbortController | null>(null);
  // The composer's imperative handle (registered via its `ref`) and a live mirror
  // of the transcript. pullBackTurn is an Escape-event callback that must read the
  // newest cells and push text back into the composer without capturing a stale
  // closure — refs give it both. The transcript mirror is synced in an effect below.
  const composerRef = useRef<ComposerHandle | null>(null);
  const transcriptRef = useRef<TranscriptCell[]>([]);

  // `/clear`: now that the cleared (short) tree has committed, drop the host
  // terminal's screen + scrollback and force a clean full repaint from the top so
  // the masthead reappears with no gap (issue: /clear left a blank band on top).
  // A layout effect runs post-commit but pre-paint, so the forced repaint redraws
  // the CLEARED state — never the old conversation. Skip the initial mount (nonce 0).
  useLayoutEffect(() => {
    if (clearNonce === 0) return;
    clearHostTerminal(stdout);
    if (renderer) resyncCockpitSurface(renderer, app.config);
  }, [clearNonce, renderer, stdout, app.config]);

  // Resize "nuclear redraw": once React has committed the post-resize tree (and
  // useScrollbackTranscript has re-armed its commit cursor off the bumped resetKey), wipe
  // the scrollback and force a full repaint so the stale duplicate footer rows OpenTUI
  // freezes on resize are cleared and the masthead + whole transcript re-commit at the new
  // width. Unlike `/clear` we do NOT call clearHostTerminal here: resyncCockpitSurface's
  // `resetSplitFooterForReplay` already erases the viewport AND scrollback (it emits
  // clearScreen + clearSavedLines + home) through OpenTUI's OWN tracked writer, so its
  // cursor-column bookkeeping stays consistent and the masthead re-commits flush, with no
  // spurious leading blank. A raw clearHostTerminal would double-clear and — because
  // capture-stdout intercepts and queues its escapes, then flushes them mid-reset —
  // intermittently inject an extra blank line above the header. clearHostTerminal stays
  // only as the fallback for when no renderer is available. Skip the initial mount.
  useLayoutEffect(() => {
    if (redrawNonce === 0) return;
    if (renderer) resyncCockpitSurface(renderer, app.config);
    else clearHostTerminal(stdout);
  }, [redrawNonce, renderer, stdout, app.config]);

  // Bump the redraw nonce — wired to useResizeRedraw, which calls this once a terminal
  // resize settles (and once on the boot → cockpit hand-off). The layout effect above
  // does the actual scrollback wipe + repaint post-commit; useScrollbackTranscript
  // re-commits the masthead + cells because DaintreeApp folds redrawNonce into its
  // resetKey.
  const requestRedraw = useCallback(() => {
    setRedrawNonce((n) => n + 1);
  }, []);
  // Follow-ups typed while a turn is in flight queue here (FIFO) and drain one at a
  // time once the lock clears — user input is drained before any pending wake. The
  // ref (not state) keeps enqueue/drain synchronous with the inFlight lock so a
  // re-render can't double-submit. sendUserMessageRef lets the drain re-enter the
  // latest callback without threading it through every closure.
  const queuedInput = useRef<string[]>([]);
  // Render-visible mirror of `queuedInput.current.length`. The ref stays the
  // source of truth (synchronous with the inFlight lock); this state only exists
  // to re-render the busy indicator when the queue grows or drains. Always set
  // from the ref's length (never ±1) so it can't drift if a future change pushes
  // or shifts more than one item in a single call.
  const [queueDepth, setQueueDepth] = useState(0);
  const sendUserMessageRef = useRef<(text: string) => boolean>(() => false);

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
  // Terminal IDs the assistant has already summarized this session. A terminal's
  // lifecycle surfaces several events (e.g. waiting_for_input then terminal_exited);
  // without this memory the model re-summarizes the same terminal on each wake. We
  // feed it to buildWakePrompt so follow-up events downgrade to a one-line ack, and
  // only record IDs on the success path (a failed turn delivered no summary).
  const summarizedTerminals = useRef<Set<string>>(new Set());

  // Drain the next pending work item now that the lock is free. User-typed
  // follow-ups take priority over autonomous wakes: a message the human queued
  // while waiting should run before the assistant reacts to a background watcher.
  // Re-enters sendUserMessage (via the ref) so the queued text goes through the
  // normal path — including its transcript user:add at the moment it starts.
  const drainPending = useCallback(() => {
    if (inFlight.current) return;
    const next = queuedInput.current.shift();
    if (next !== undefined) {
      // Update the count BEFORE the drained item re-enters as a new turn:
      // otherwise the indicator would briefly read "1 queued" while that item is
      // already the active turn — the exact stale-count confusion issue #95 fixes.
      setQueueDepth(queuedInput.current.length);
      sendUserMessageRef.current(next);
      return;
    }
    if (pendingWake.current.length > 0) reactToWakeRef.current();
  }, []);

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
      const reply = await app.session.send(
        buildWakePrompt(events, {
          alreadySummarized: summarizedTerminals.current,
        }),
        { readOnly: true },
      );
      wakeRetried.current = false; // success resets the per-burst retry budget
      // Record only when the turn actually delivered a report: send() returns a
      // sentinel string (not a throw) on model failure, so guard on it — otherwise a
      // transient outage would mark these terminals reported and suppress the real
      // summary the user never saw. On a real reply, their later lifecycle events
      // become one-line acks instead of repeat summaries.
      if (!isWakeFailureReply(reply)) {
        for (const e of events) {
          const terminalId = (e as { target?: { terminalId?: string } }).target
            ?.terminalId;
          if (terminalId) summarizedTerminals.current.add(terminalId);
        }
      }
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
      // A user follow-up queued during the reaction drains first, then any further
      // wake events that arrived (or a retry that was re-queued).
      drainPending();
    }
  }, [app, bridge, drainPending]);

  useEffect(() => {
    reactToWakeRef.current = () => void reactToWake();
  }, [reactToWake]);

  // Subscribe to the bridge: confirms drive modal state, usage updates the session
  // rollup, everything else reduces into the transcript.
  useEffect(() => {
    return bridge.subscribe((event) => {
      if (event.type === "confirm") {
        setPendingConfirm(event.pending);
      } else if (event.type === "usage") {
        const u = event.usage;
        setSessionUsage((prev) => ({
          promptTokens: prev.promptTokens + u.promptTokens,
          completionTokens: prev.completionTokens + u.completionTokens,
          totalTokens: prev.totalTokens + u.totalTokens,
          // Keep the prior total when this call's model has no rate, so one
          // unpriced call doesn't blank an already-accumulated cost.
          costUsd:
            u.costUsd === undefined
              ? prev.costUsd
              : (prev.costUsd ?? 0) + u.costUsd,
          // Context pressure is the latest reading, not a sum.
          contextTokens: u.contextTokens,
          contextThreshold: u.contextThreshold,
          lastTier: u.tier,
          lastModel: u.model,
        }));
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
      // Startup has settled (connect resolved — connected or degraded — and the first
      // snapshot is in): the other half of the splash dismiss gate. The project-name
      // fetch below is non-blocking and deliberately does NOT hold the splash.
      startupSettled.current = true;
      finishBootIfReady();
      // Ask Daintree for the authoritative project name and fill it into the header.
      // This now GATES the splash (via projectSettled) because the masthead freezes
      // into <Static> on first paint — a name arriving after that can't be shown.
      // Retry a few times (right after connect the renderer may not have a project
      // bound yet), but always settle the gate in `finally` so a miss, an offline
      // link, or disposal drops cleanly into the cockpit with the provisional name
      // rather than stranding the user on the splash (the 8s bootCap is the backstop).
      void (async () => {
        try {
          for (let attempt = 0; attempt < 4 && !disposed; attempt++) {
            if (!app.mcp.status().connected) return;
            const name = await fetchProjectName(app);
            if (name) {
              if (!disposed) setProjectName(name);
              return;
            }
            await new Promise((resolve) => setTimeout(resolve, 1000));
          }
        } finally {
          projectSettled.current = true;
          if (!disposed) finishBootIfReady();
        }
      })();
    })();

    const timer = setInterval(() => {
      if (!disposed) setDashboard(snapshot(app));
    }, 1000);

    // Safety net: never strand the user on the splash if startup stalls (e.g. a hung
    // MCP). After this cap we drop into the cockpit regardless of readiness.
    const bootCap = app.config.splash
      ? setTimeout(() => {
          if (!disposed) setBooting(false);
        }, 8000)
      : undefined;

    return () => {
      disposed = true;
      clearInterval(timer);
      if (bootCap) clearTimeout(bootCap);
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
      // we reject. The ref lock gates both model turns and async slash commands.
      if (!trimmed) return false;
      // A turn is already in flight: don't block the user. Queue the follow-up
      // (FIFO) and report acceptance so the composer clears — it drains in the
      // finally below once the lock frees. This is the whole point of issue #45:
      // typing stays live while the assistant works.
      if (inFlight.current) {
        queuedInput.current.push(trimmed);
        setQueueDepth(queuedInput.current.length);
        return true;
      }
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
            if (result.switchPanel) {
              // A panel command opens a purposeful VIEW rather than dumping text
              // into the transcript — the multi-layout owns a dedicated screen
              // for operations/help. Re-running the same command re-opens it.
              setActivePanel(result.switchPanel);
            } else if (result.clearTranscript) {
              // /clear restarts the cockpit in place: wipe the live transcript and
              // drop a single confirmation card into the now-empty tree so the user
              // sees it ran. The host-terminal scrollback wipe + forced repaint are
              // deferred to a layout effect (keyed on this nonce) so they fire AFTER
              // React commits the cleared tree — repainting the fresh, header-on-top
              // state instead of reflowing the old conversation back into scrollback.
              dispatch({ type: "transcript:clear" });
              dispatch({
                type: "command:add",
                title: result.title ?? "Clear",
                text: result.text ?? "Conversation cleared — starting fresh.",
              });
              setClearNonce((n) => n + 1);
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
          // Fresh controller per turn so Escape-to-cancel aborts THIS turn only;
          // cleared in the finally so a queued follow-up starts uncancelled. Only
          // now (not for slash commands) is the turn actually cancellable.
          const controller = new AbortController();
          abortController.current = controller;
          setCanCancel(true);
          await app.session.send(trimmed, { signal: controller.signal });
        } catch (err) {
          bridge.emit({
            type: "log",
            level: "error",
            message: err instanceof Error ? err.message : String(err),
          });
        } finally {
          abortController.current = null;
          inFlight.current = false;
          setBusy(false);
          setCanCancel(false);
          // Drain a queued user follow-up first, else react to any watcher that
          // surfaced while this turn ran — neither should be stranded.
          drainPending();
        }
      })();
      return true;
    },
    [app, bridge, onExit, drainPending, stdout],
  );

  // Keep the ref pointing at the latest sendUserMessage so drainPending can
  // re-enter the current closure (refs alone can't capture the live callback).
  useEffect(() => {
    sendUserMessageRef.current = sendUserMessage;
  }, [sendUserMessage]);

  // Mirror the live transcript into a ref so pullBackTurn (an Escape-event
  // callback) can read the newest cells without a stale closure. Assigned during
  // render, not in an effect: an effect runs a tick later, leaving a window where
  // an assistant:start has already reduced but the ref still shows the pre-stream
  // turn — pullBackTurn would then fire its side effects against stale state. Only
  // event handlers read this ref, never the render output, so the write is safe.
  transcriptRef.current = transcript;

  // Abort the in-flight user turn (Escape on an empty composer while busy).
  // Idempotent and a no-op when nothing is running.
  const cancelTurn = useCallback(() => {
    abortController.current?.abort();
  }, []);

  // Escape on an empty composer while busy. When the just-sent turn is still
  // pre-stream (no assistant output yet), pull it back: remove the transcript
  // turn, abort the request, and restore the original text into the composer for
  // editing. Once any output has streamed the window is closed, so fall through to
  // plain cancel (the turn stays in the transcript, marked cancelled). Issue #61.
  const pullBackTurn = useCallback(() => {
    const candidate = pullbackCandidate(transcriptRef.current);
    if (!candidate) {
      cancelTurn();
      return;
    }
    // Drop follow-ups typed while waiting so the drained queue can't fire a new
    // turn while the user is editing the pulled-back text. Dispatch BEFORE the
    // abort: the reducer removes the turn synchronously, so the assistant:cancelled
    // the abort triggers finds no active turn and is a no-op (no phantom left).
    queuedInput.current = [];
    setQueueDepth(0);
    dispatch({ type: "user:pullback" });
    abortController.current?.abort();
    composerRef.current?.restore(candidate.turn.userText);
  }, [cancelTurn]);

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
    sessionUsage,
    busy,
    stage,
    pendingConfirm,
    sendUserMessage,
    cancelTurn,
    pullBackTurn,
    composerRef,
    canCancel,
    queueDepth,
    activePanel,
    setActivePanel,
    resolveConfirm,
    projectName,
    booting,
    notifyAnimationDone,
    clearNonce,
    redrawNonce,
    requestRedraw,
  };
}
