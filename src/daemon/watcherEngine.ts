/**
 * Terminal watcher engine.
 *
 * A watcher is a small state machine, NOT a blind log poller. Each check:
 *   1. reads bounded terminal state + tail from Daintree (deterministic signals);
 *   2. resolves what it can deterministically (exited / waiting / conditions);
 *   3. only then asks the SMALL model to classify the tail;
 *   4. dedupes against the previous classification and publishes to the queue
 *      ONLY on meaningful change / condition match / timeout.
 *
 * The pure helpers (evaluateCondition, classificationToEvent, decideStop) are
 * exported for unit testing without any model or MCP.
 */
import {
  WatcherVerdict,
  type Severity,
  type WatchCondition,
  type WatcherClassification,
} from "../schemas.js";
import {
  WATCHER_SYSTEM_PROMPT,
  buildWatcherUserPrompt,
} from "../models/prompts/index.js";
import type { ToolContext } from "../tools/types.js";
import type { WatcherRecord } from "../schemas.js";

export interface WatcherSignals {
  agentState?: string;
  runtimeStatus?: string;
  tail: string;
  msSinceOutput?: number;
  classification?: WatcherClassification;
  confidence?: number;
}

/** Classifications that are worth interrupting the user about. */
const MEANINGFUL: ReadonlySet<WatcherClassification> = new Set([
  "waiting_for_input",
  "permission_prompt",
  "command_failed",
  "tests_failed",
  "tests_passed",
  "merge_conflict",
  "completed_success",
  "completed_unknown",
  "terminal_exited",
]);

/** Classifications that mean the watcher's job is done and it should stop. */
const TERMINAL_CLASS: ReadonlySet<WatcherClassification> = new Set([
  "completed_success",
  "terminal_exited",
]);

const SEVERITY_MAP: Record<WatcherClassification, Severity> = {
  no_change: "debug",
  still_working: "debug",
  waiting_for_input: "attention",
  permission_prompt: "attention",
  command_failed: "error",
  tests_failed: "error",
  tests_passed: "done",
  merge_conflict: "blocked",
  completed_success: "done",
  completed_unknown: "info",
  terminal_exited: "urgent",
  needs_large_model: "attention",
  unknown: "info",
};

/** Pure evaluation of a WatchCondition against deterministic signals. */
export function evaluateCondition(
  cond: WatchCondition,
  s: WatcherSignals,
): boolean {
  if ("stateIs" in cond) return s.agentState === cond.stateIs;
  if ("runtimeStatusIs" in cond) return s.runtimeStatus === cond.runtimeStatusIs;
  if ("contains" in cond) return s.tail.includes(cond.contains);
  if ("regex" in cond) {
    try {
      return new RegExp(cond.regex).test(s.tail);
    } catch {
      return false;
    }
  }
  if ("noOutputForMs" in cond)
    return (s.msSinceOutput ?? 0) >= cond.noOutputForMs;
  if ("modelJudge" in cond) {
    // The classifier ran against the watcher goal; treat a confident, meaningful
    // classification as the judge being satisfied.
    return (
      !!s.classification &&
      MEANINGFUL.has(s.classification) &&
      (s.confidence ?? 0) >= 0.6
    );
  }
  if ("all" in cond) return cond.all.every((c) => evaluateCondition(c, s));
  if ("any" in cond) return cond.any.some((c) => evaluateCondition(c, s));
  if ("not" in cond) return !evaluateCondition(cond.not, s);
  return false;
}

export interface CheckOutcome {
  classification: WatcherClassification;
  confidence: number;
  summary: string;
  evidence: string[];
  /** Whether this check warrants a queue event. */
  shouldPublish: boolean;
  severity: Severity;
  /** Whether the watcher should stop after this check. */
  stop: boolean;
  stopReason?: "condition_met" | "timeout" | "terminal";
}

/** Decide publish/stop/severity from a classification + conditions. Pure. */
export function decideOutcome(args: {
  classification: WatcherClassification;
  confidence: number;
  summary: string;
  evidence: string[];
  previous?: string;
  signals: WatcherSignals;
  stopWhen?: WatchCondition;
  alertWhen?: WatchCondition;
  timedOut?: boolean;
}): CheckOutcome {
  const { classification, confidence, summary, evidence } = args;
  const sig = { ...args.signals, classification, confidence };
  const alertMatched = args.alertWhen
    ? evaluateCondition(args.alertWhen, sig)
    : false;
  const stopMatched = args.stopWhen
    ? evaluateCondition(args.stopWhen, sig)
    : false;
  const changed = classification !== args.previous;
  const isTerminal = TERMINAL_CLASS.has(classification);

  const shouldPublish =
    args.timedOut ||
    alertMatched ||
    stopMatched ||
    (changed && MEANINGFUL.has(classification)) ||
    classification === "needs_large_model";

  const stop = stopMatched || isTerminal || Boolean(args.timedOut);
  const stopReason = stopMatched
    ? "condition_met"
    : args.timedOut
      ? "timeout"
      : isTerminal
        ? "terminal"
        : undefined;

  // An explicitly matched alert/stop condition is always worth at least
  // "attention" — never bury it at debug/info just because the underlying
  // classification was low severity.
  let severity: Severity = args.timedOut ? "attention" : SEVERITY_MAP[classification];
  if ((alertMatched || stopMatched) && (severity === "debug" || severity === "info")) {
    severity = "attention";
  }

  return {
    classification,
    confidence,
    summary,
    evidence,
    severity,
    shouldPublish,
    stop,
    stopReason,
  };
}

/** Read bounded terminal state + tail from Daintree via MCP. */
export async function readTerminal(
  ctx: ToolContext,
  terminalId: string,
  tailBytes = 12000,
): Promise<WatcherSignals> {
  let agentState: string | undefined;
  let runtimeStatus: string | undefined;
  let tail = "";
  try {
    const status = await ctx.mcp.callTool("terminal.getStatus", { terminalId });
    const sc = (status.structuredContent ?? {}) as Record<string, unknown>;
    agentState = (sc.agentState as string) ?? undefined;
    runtimeStatus = (sc.runtimeStatus as string) ?? undefined;
    if (!agentState && status.text) agentState = status.text.trim() || undefined;
  } catch {
    /* status optional */
  }
  try {
    const out = await ctx.mcp.callTool("terminal.getOutput", {
      terminalId,
      lines: 200,
    });
    tail = out.text.slice(-tailBytes);
  } catch {
    /* output optional */
  }
  return { agentState, runtimeStatus, tail };
}

/** Stable 32-bit hash of a terminal tail, used to detect new output cheaply. */
export function hashTail(s: string): string {
  let h = 0;
  for (let i = 0; i < s.length; i++) {
    h = (h << 5) - h + s.charCodeAt(i);
    h |= 0;
  }
  return (h >>> 0).toString(36);
}

/** Per-terminal memory persisted in the watcher's optionsJson. */
interface TerminalState {
  /** Previous classification for change detection. */
  prev?: string;
  /** Hash of the last observed tail. */
  outHash?: string;
  /** Wall-clock ms when the tail last changed (for noOutputForMs). */
  outAt?: number;
}
type WatcherOptions = { perTerminal?: Record<string, TerminalState> };

/**
 * Advance a terminal's output-tracking state. When the tail changed since the
 * last check, `outAt` becomes `now`; otherwise it is preserved so the time since
 * the last *new* output keeps growing. Returns the new state and the elapsed ms.
 * Pure — unit-testable without MCP.
 */
export function nextOutputState(
  prev: TerminalState | undefined,
  tail: string,
  now: number,
): { state: TerminalState; msSinceOutput: number } {
  const outHash = hashTail(tail);
  const changed = !prev || prev.outHash !== outHash;
  const outAt = changed ? now : prev?.outAt ?? now;
  return {
    state: { prev: prev?.prev, outHash, outAt },
    msSinceOutput: Math.max(0, now - outAt),
  };
}

/** Find the first modelJudge question inside a (possibly composite) condition. */
export function findModelJudge(cond?: WatchCondition): string | undefined {
  if (!cond) return undefined;
  if ("modelJudge" in cond) return cond.modelJudge;
  if ("all" in cond) return cond.all.map((c) => findModelJudge(c)).find(Boolean);
  if ("any" in cond) return cond.any.map((c) => findModelJudge(c)).find(Boolean);
  if ("not" in cond) return findModelJudge(cond.not);
  return undefined;
}

/**
 * Run a single watcher check across ALL of its target terminals. Each terminal
 * is read, classified, and published independently with per-terminal change
 * detection and output-staleness tracking; the most urgent per-terminal outcome
 * is returned and drives the watcher's aggregate state. Corrupt watcher state is
 * disabled with a visible error event rather than throwing every tick.
 */
export async function runTerminalWatcherCheck(
  rec: WatcherRecord,
  ctx: ToolContext,
): Promise<CheckOutcome> {
  const now = Date.now();

  let targets: string[];
  let stopWhen: WatchCondition | undefined;
  let alertWhen: WatchCondition | undefined;
  let options: WatcherOptions;
  try {
    targets = JSON.parse(rec.targetsJson);
    if (
      !Array.isArray(targets) ||
      targets.length === 0 ||
      !targets.every((t) => typeof t === "string" && t.length > 0)
    ) {
      throw new Error("watcher has no valid string target terminals");
    }
    stopWhen = rec.stopWhenJson ? JSON.parse(rec.stopWhenJson) : undefined;
    alertWhen = rec.alertWhenJson ? JSON.parse(rec.alertWhenJson) : undefined;
    const parsedOptions = rec.optionsJson ? JSON.parse(rec.optionsJson) : {};
    options =
      parsedOptions && typeof parsedOptions === "object" && !Array.isArray(parsedOptions)
        ? (parsedOptions as WatcherOptions)
        : {};
  } catch (err) {
    // Disable the watcher and tell the user, instead of throwing silently every
    // tick (the scheduler swallows watcher errors).
    ctx.db.updateWatcher(rec.id, { status: "error", lastCheckedAt: now });
    ctx.queue.publish({
      source: "terminal_watcher",
      severity: "error",
      title: `${rec.title}: watcher disabled`,
      summary: `Corrupt watcher state for ${rec.id}: ${err instanceof Error ? err.message : String(err)}`,
    });
    return {
      classification: "unknown",
      confidence: 0,
      summary: "Watcher disabled due to corrupt state.",
      evidence: [],
      severity: "error",
      shouldPublish: false,
      stop: true,
      stopReason: "terminal",
    };
  }

  const judge = findModelJudge(alertWhen) ?? findModelJudge(stopWhen);
  const timedOut = Boolean(
    rec.stopAfterMs && now - rec.createdAt >= rec.stopAfterMs,
  );
  const perTerminal: Record<string, TerminalState> = { ...options.perTerminal };

  const outcomes: CheckOutcome[] = [];

  for (const terminalId of targets) {
    const prevState = perTerminal[terminalId];
    let signals: WatcherSignals = { tail: "" };
    let classification: WatcherClassification = "unknown";
    let confidence = 0.4;
    let summary = "Watcher check.";
    let evidence: string[] = [];

    if (!ctx.mcp.isConnected()) {
      classification = "needs_large_model";
      summary = "Daintree MCP not connected; cannot read terminal.";
    } else {
      signals = await readTerminal(ctx, terminalId);
      const out = nextOutputState(prevState, signals.tail, now);
      perTerminal[terminalId] = out.state;
      signals.msSinceOutput = out.msSinceOutput;

      if (signals.runtimeStatus === "exited" || signals.agentState === "exited") {
        classification = "terminal_exited";
        confidence = 0.95;
        summary = "Terminal exited.";
        evidence = ["runtimeStatus/agentState=exited"];
      } else if (signals.agentState === "waiting") {
        classification = "waiting_for_input";
        confidence = 0.9;
        summary = "Agent is waiting for input.";
        evidence = ["agentState=waiting"];
      } else if (signals.agentState === "completed") {
        classification = "completed_success";
        confidence = 0.85;
        summary = "Agent reports completion.";
        evidence = ["agentState=completed"];
      } else if (signals.tail.trim().length > 0) {
        const verdict = await classifyWithModel(rec, signals, ctx, judge, prevState?.prev);
        classification = verdict.classification;
        confidence = verdict.confidence;
        summary = verdict.summary;
        evidence = verdict.evidence;
      } else {
        classification = "no_change";
        summary = "No new output.";
      }
    }

    const outcome = decideOutcome({
      classification,
      confidence,
      summary,
      evidence,
      previous: prevState?.prev,
      signals,
      stopWhen,
      alertWhen,
      timedOut,
    });
    // Preserve any output-tracking state captured above (absent when MCP is
    // disconnected, since we never read the terminal) and record the new prev.
    perTerminal[terminalId] = {
      ...(perTerminal[terminalId] ?? prevState ?? {}),
      prev: outcome.classification,
    };

    if (outcome.shouldPublish) {
      ctx.queue.publish({
        source: "terminal_watcher",
        severity: outcome.severity,
        title: `${rec.title}: ${humanize(outcome.classification)}`,
        summary:
          targets.length > 1 ? `[${terminalId}] ${outcome.summary}` : outcome.summary,
        target: { terminalId },
        evidence: outcome.evidence,
        // Keyed by terminal so concurrent terminals don't collapse together.
        dedupeKey: `watcher:${rec.id}:${terminalId}:${outcome.classification}`,
        recommendedActions:
          outcome.classification === "waiting_for_input" ||
          outcome.classification === "permission_prompt"
            ? [
                {
                  label: "Focus terminal",
                  toolName: "terminal.focus",
                  args: { terminalId },
                  risk: "ui",
                  requiresConfirmation: false,
                },
              ]
            : undefined,
      });
    }

    outcomes.push(outcome);
  }

  // Display headline = the most urgent per-terminal outcome.
  let headline: CheckOutcome =
    outcomes[0] ?? {
      classification: "no_change" as WatcherClassification,
      confidence: 0.4,
      summary: "No terminals to check.",
      evidence: [],
      severity: "debug" as Severity,
      shouldPublish: false,
      stop: false,
    };
  for (const o of outcomes) if (moreUrgent(o, headline)) headline = o;

  // Stop semantics computed across ALL terminals — NOT from the headline, so a
  // single completed terminal can't stop a watcher whose other terminals are
  // still working (and vice-versa):
  //   - timeout is global (derived from stopAfterMs);
  //   - an explicit stopWhen match on ANY terminal ends the watcher;
  //   - terminal completion stops only when EVERY target has stopped.
  let stopReason: CheckOutcome["stopReason"];
  if (outcomes.some((o) => o.stopReason === "timeout")) stopReason = "timeout";
  else if (outcomes.some((o) => o.stopReason === "condition_met")) stopReason = "condition_met";
  else if (outcomes.length > 0 && outcomes.every((o) => o.stop)) stopReason = "terminal";
  const stop = stopReason !== undefined;

  ctx.db.updateWatcher(rec.id, {
    lastClassification: headline.classification,
    lastCheckedAt: now,
    nextCheckAt: now + rec.cadenceMs,
    optionsJson: JSON.stringify({ ...options, perTerminal }),
    status: stop ? (stopReason === "timeout" ? "timeout" : "condition_met") : "active",
  });

  return { ...headline, stop, stopReason };
}

/** Severity ordering for picking the watcher's headline outcome. */
const SEVERITY_WEIGHT: Record<Severity, number> = {
  debug: 0,
  info: 1,
  done: 2,
  attention: 3,
  blocked: 4,
  urgent: 5,
  error: 6,
};

function moreUrgent(a: CheckOutcome, b: CheckOutcome): boolean {
  return SEVERITY_WEIGHT[a.severity] > SEVERITY_WEIGHT[b.severity];
}

async function classifyWithModel(
  rec: WatcherRecord,
  signals: WatcherSignals,
  ctx: ToolContext,
  modelJudge?: string,
  previous?: string,
): Promise<WatcherVerdict> {
  // Fold the goal and any modelJudge question into one focus line so the
  // classifier actually evaluates the specific condition the watcher asked for.
  const goal = modelJudge ? `${rec.goal}\nSpecifically decide: ${modelJudge}` : rec.goal;
  try {
    return await ctx.router.json(
      rec.modelTier,
      {
        messages: [
          { role: "system", content: WATCHER_SYSTEM_PROMPT },
          {
            role: "user",
            content: buildWatcherUserPrompt({
              goal,
              agentState: signals.agentState,
              runtimeStatus: signals.runtimeStatus,
              lastOutputAt:
                signals.msSinceOutput !== undefined
                  ? `${Math.floor(signals.msSinceOutput / 1000)}s ago`
                  : undefined,
              previous,
              tail: signals.tail,
            }),
          },
        ],
        temperature: 0,
      },
      WatcherVerdict,
    );
  } catch {
    return {
      classification: "unknown",
      confidence: 0.3,
      summary: "Could not classify terminal output.",
      evidence: [],
      recommendedAction: "none",
    };
  }
}

function humanize(c: WatcherClassification): string {
  return c.replace(/_/g, " ");
}
