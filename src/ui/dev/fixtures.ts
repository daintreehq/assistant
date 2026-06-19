/**
 * Deterministic UI fixtures. Redesign work happens against THESE, not a live
 * model + scheduler + MCP connection. Every timestamp is frozen relative to
 * {@link FIXED_NOW} so screenshots and golden-frame snapshots don't drift
 * between runs.
 */
import type {
  AuditRecord,
  QueueEvent,
  TimerRecord,
  WatcherRecord,
} from "../../schemas.js";
import type { TerminalPreview } from "../hooks/useTerminalPreview.js";
import type {
  ActivityItem,
  DashboardState,
  PendingConfirm,
  SessionUsage,
  TranscriptCell,
} from "../types.js";
import type { View } from "../ControlRoom.js";

/** A fixed wall clock — fixtures are offsets from here. */
export const FIXED_NOW = 1_700_000_000_000;
const s = (n: number) => n * 1000;

export interface Fixture {
  key: string;
  label: string;
  connected: boolean;
  busy: boolean;
  stage: string;
  /** User follow-ups queued behind the in-flight turn; exercises the "· N queued"
   *  busy-indicator hint in the gallery (#95). Omitted/0 means none waiting. */
  queueDepth?: number;
  view: View;
  transcript: TranscriptCell[];
  dashboard: DashboardState;
  previews: TerminalPreview[];
  pending: PendingConfirm | null;
  sessionUsage: SessionUsage;
}

/** Build a session-usage rollup at a given context-pressure level (0–1). */
function usage(pressure: number, costUsd: number | undefined = 0.012): SessionUsage {
  const contextThreshold = 60_000;
  return {
    promptTokens: 18_400,
    completionTokens: 2_600,
    totalTokens: 21_000,
    costUsd,
    contextTokens: Math.round(contextThreshold * pressure),
    contextThreshold,
    lastTier: "large",
    lastModel: "minimax-m3",
  };
}

function watcher(over: Partial<WatcherRecord>): WatcherRecord {
  return {
    id: "wch_1",
    kind: "terminal",
    title: "repair watcher tests",
    goal: "wait for tests to pass",
    targetsJson: JSON.stringify(["term_8"]),
    cadenceMs: 30000,
    modelTier: "small",
    status: "active",
    lastClassification: "still_working",
    nextCheckAt: FIXED_NOW,
    createdAt: FIXED_NOW - s(18),
    ...over,
  };
}

function timer(over: Partial<TimerRecord>): TimerRecord {
  return {
    id: "tmr_1",
    title: "review branch when ready",
    fireAt: FIXED_NOW + s(600),
    runCount: 0,
    payloadType: "enqueue",
    payloadJson: "{}",
    status: "scheduled",
    createdAt: FIXED_NOW,
    ...over,
  };
}

function event(over: Partial<QueueEvent>): QueueEvent {
  return {
    id: "evt_1",
    source: "terminal_watcher",
    severity: "error",
    title: "Tests failed in term_8",
    summary: "3 failures in WatcherPanel.test.tsx",
    createdAt: FIXED_NOW - s(4),
    count: 1,
    ...over,
  };
}

function audit(id: string, toolName: string, ok: boolean, ms: number): AuditRecord {
  return {
    id,
    ts: FIXED_NOW - s(30),
    actor: "main",
    toolName,
    argsJson: "{}",
    outcome: ok ? "ok" : "error",
    durationMs: ms,
    summary: "",
  };
}

function emptyDash(connected: boolean): DashboardState {
  return {
    mcp: { connected } as DashboardState["mcp"],
    watchers: [],
    timers: [],
    inbox: [],
    audit: [],
  };
}

function activity(over: Partial<ActivityItem> & Pick<ActivityItem, "id" | "name" | "label">): ActivityItem {
  return {
    state: "done",
    startedAt: FIXED_NOW - s(20),
    endedAt: FIXED_NOW - s(20) + 180,
    ...over,
  };
}

/** A long user prompt — exercises the boxed UserMessageCard's vertical cost. */
function longUserRun(): TranscriptCell[] {
  return [
    {
      kind: "turn",
      id: "turn_long",
      userText:
        "Fix the watcher tests, then once they pass open a PR against main with a " +
        "summary of the race condition we found, link the failing run, and schedule " +
        "a follow-up to prune stale terminals tomorrow morning.",
      assistantText: "On it — delegating the repair and attaching a watcher.",
      streaming: false,
      state: "active",
      ts: FIXED_NOW - s(28),
      notes: [],
      activities: [
        activity({ id: "c1", name: "fs.search", label: "Inspected", detail: "tests/ui", summary: "8 matches" }),
        activity({
          id: "c2",
          name: "agentTask.spawnForEdits",
          label: "Delegated",
          detail: "term_8 · repair watcher tests",
          state: "active",
          endedAt: undefined,
        }),
      ],
    },
  ];
}

/** A representative in-flight run: inspect → delegate → watch. */
function activeRun(): TranscriptCell[] {
  return [
    {
      kind: "turn",
      id: "turn_1",
      userText: "Fix the watcher tests and tell me when the branch is ready.",
      assistantText: "I'll delegate the edit and supervise the result.",
      streaming: false,
      state: "active",
      ts: FIXED_NOW - s(28),
      notes: [],
      activities: [
        activity({
          id: "c1",
          name: "fs.search",
          label: "Inspected",
          detail: "tests/ui",
          summary: "8 matches",
        }),
        activity({
          id: "c2",
          name: "agentTask.spawnForEdits",
          label: "Delegated",
          detail: "term_8 · repair watcher tests",
          endedAt: FIXED_NOW - s(19),
        }),
        activity({
          id: "c3",
          name: "watcher.terminal.create",
          label: "Watching",
          detail: "tests running · 42 passed",
          state: "active",
          startedAt: FIXED_NOW - s(18),
          endedAt: undefined,
        }),
      ],
    },
  ];
}

export function buildFixtures(): Fixture[] {
  return [
    {
      key: "1",
      sessionUsage: usage(0.08, undefined),
      label: "idle",
      connected: true,
      busy: false,
      stage: "Thinking",
      view: "home",
      transcript: [],
      dashboard: emptyDash(true),
      previews: [],
      pending: null,
    },
    {
      key: "2",
      sessionUsage: usage(0.42),
      label: "active",
      connected: true,
      busy: true,
      stage: "Watching",
      // Two follow-ups typed while the turn runs — exercises the "· 2 queued" hint.
      queueDepth: 2,
      view: "home",
      transcript: activeRun(),
      dashboard: {
        ...emptyDash(true),
        watchers: [watcher({})],
        timers: [timer({})],
        audit: [
          audit("a1", "fs.search", true, 180),
          audit("a2", "agentTask.spawnForEdits", true, 920),
        ],
      },
      previews: [
        {
          terminalId: "term_8",
          watcherId: "wch_1",
          title: "repair watcher tests",
          agentState: "working",
          tail: "RUN  tests/ui/WatcherPanel.test.tsx\n  42 passed",
          updatedAt: FIXED_NOW,
        },
      ],
      pending: null,
    },
    {
      key: "3",
      sessionUsage: usage(0.8, 0.181),
      label: "attention",
      connected: true,
      busy: false,
      stage: "Waiting",
      view: "home",
      transcript: activeRun(),
      dashboard: {
        ...emptyDash(true),
        watchers: [
          // inferred: a small-model content classification.
          watcher({ lastClassification: "tests_failed", lastEpistemicKind: "inferred" }),
          watcher({
            id: "wch_2",
            title: "repair authentication flow",
            goal: "resolve the permission prompt",
            targetsJson: JSON.stringify(["term_12"]),
            lastClassification: "permission_prompt",
            lastEpistemicKind: "inferred",
            createdAt: FIXED_NOW - s(40),
          }),
          watcher({
            id: "wch_3",
            title: "build the release bundle",
            goal: "wait for the build to exit",
            targetsJson: JSON.stringify(["term_15"]),
            // observed: a deterministic terminal exit (no model consulted).
            lastClassification: "terminal_exited",
            lastEpistemicKind: "observed",
            createdAt: FIXED_NOW - s(70),
          }),
        ],
        inbox: [
          event({
            // inferred: published off a model classification.
            epistemicKind: "inferred",
            recommendedActions: [
              { label: "focus terminal", toolName: "terminal.focus", args: { terminalId: "term_8" } },
              { label: "rerun", toolName: "recipe.run" },
              { label: "dismiss", toolName: "queue.resolve" },
            ],
          }),
          event({
            id: "evt_2",
            severity: "attention",
            title: "term_12 awaiting input",
            summary: "permission prompt: allow git push?",
            // observed: agentState=waiting read straight from Daintree.
            epistemicKind: "observed",
          }),
        ],
      },
      previews: [],
      pending: null,
    },
    {
      key: "4",
      sessionUsage: usage(0.93, 0.244),
      label: "approval",
      connected: true,
      busy: false,
      stage: "Waiting for approval",
      view: "home",
      transcript: activeRun(),
      dashboard: {
        ...emptyDash(true),
        watchers: [watcher({ lastClassification: "tests_passed" })],
      },
      previews: [],
      pending: {
        id: "cfm_1",
        request: {
          toolName: "git.push",
          risk: "external",
          summary: "tests passed and the branch is ready for review",
          args: { branch: "fix/watcher-race", remote: "origin" },
        },
        resolve: () => {},
      },
    },
    {
      key: "5",
      sessionUsage: { ...usage(0), contextThreshold: 0, costUsd: undefined },
      label: "degraded",
      connected: false,
      busy: false,
      stage: "Thinking",
      view: "home",
      transcript: [
        {
          kind: "note",
          id: "note_1",
          level: "warn",
          text: "Daintree MCP not connected — no url/token. Running degraded.",
          ts: FIXED_NOW - s(2),
        },
      ],
      dashboard: emptyDash(false),
      previews: [],
      pending: null,
    },
    {
      key: "6",
      sessionUsage: usage(0.3),
      label: "timers",
      connected: true,
      busy: false,
      stage: "Thinking",
      view: "home",
      transcript: [],
      dashboard: {
        ...emptyDash(true),
        timers: [
          timer({ id: "tmr_1", title: "check CI", fireAt: FIXED_NOW + s(60 * 45) }),
          timer({ id: "tmr_2", title: "review stale terminals", fireAt: FIXED_NOW + s(60 * 120) }),
          timer({ id: "tmr_3", title: "prune old watchers", fireAt: FIXED_NOW + s(60 * 60 * 26) }),
        ],
      },
      previews: [],
      pending: null,
    },
    {
      key: "7",
      sessionUsage: usage(0.66, 0.135),
      label: "fleet",
      connected: true,
      busy: true,
      stage: "Watching",
      view: "home",
      transcript: activeRun(),
      dashboard: {
        ...emptyDash(true),
        watchers: [
          watcher({ id: "wch_1", targetsJson: JSON.stringify(["term_8"]), lastClassification: "still_working", createdAt: FIXED_NOW - s(134) }),
          watcher({ id: "wch_2", title: "ship the branch", goal: "branch ready for review", targetsJson: JSON.stringify(["term_4"]), lastClassification: "tests_passed", createdAt: FIXED_NOW - s(531) }),
          watcher({ id: "wch_3", title: "resolve permission prompt", goal: "waiting for input", targetsJson: JSON.stringify(["term_2"]), lastClassification: "waiting_for_input", createdAt: FIXED_NOW - s(786) }),
        ],
        timers: [timer({ id: "tmr_1", title: "check CI", fireAt: FIXED_NOW + s(60 * 45) })],
      },
      previews: [
        { terminalId: "term_8", watcherId: "wch_1", title: "repair watcher tests", agentState: "working", tail: "RUN  tests/ui/WatcherPanel.test.tsx\n  42 passed", updatedAt: FIXED_NOW },
      ],
      pending: null,
    },
    {
      key: "8",
      sessionUsage: usage(0.88, 0.207),
      label: "long message",
      connected: true,
      busy: true,
      stage: "Delegating",
      view: "home",
      transcript: longUserRun(),
      dashboard: {
        ...emptyDash(true),
        watchers: [watcher({})],
        timers: [timer({ id: "tmr_1", title: "prune stale terminals", fireAt: FIXED_NOW + s(60 * 60 * 18) })],
      },
      previews: [
        { terminalId: "term_8", watcherId: "wch_1", title: "repair watcher tests", agentState: "working", tail: "RUN  tests/ui/WatcherPanel.test.tsx\n  42 passed", updatedAt: FIXED_NOW },
      ],
      pending: null,
    },
  ];
}
