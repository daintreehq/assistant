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

/** Run a single watcher check. Reads terminal, classifies, publishes, updates. */
export async function runTerminalWatcherCheck(
  rec: WatcherRecord,
  ctx: ToolContext,
): Promise<CheckOutcome> {
  const now = Date.now();
  const targets: string[] = JSON.parse(rec.targetsJson);
  const terminalId = targets[0];
  const stopWhen: WatchCondition | undefined = rec.stopWhenJson
    ? JSON.parse(rec.stopWhenJson)
    : undefined;
  const alertWhen: WatchCondition | undefined = rec.alertWhenJson
    ? JSON.parse(rec.alertWhenJson)
    : undefined;

  const timedOut = Boolean(
    rec.stopAfterMs && now - rec.createdAt >= rec.stopAfterMs,
  );

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
    // Fast deterministic resolutions.
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
      const verdict = await classifyWithModel(rec, signals, ctx);
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
    previous: rec.lastClassification,
    signals,
    stopWhen,
    alertWhen,
    timedOut,
  });

  if (outcome.shouldPublish) {
    ctx.queue.publish({
      source: "terminal_watcher",
      severity: outcome.severity,
      title: `${rec.title}: ${humanize(outcome.classification)}`,
      summary: outcome.summary,
      target: { terminalId },
      evidence: outcome.evidence,
      dedupeKey: `watcher:${rec.id}:${outcome.classification}`,
      recommendedActions:
        outcome.classification === "waiting_for_input" ||
        outcome.classification === "permission_prompt"
          ? [
              {
                label: "Focus terminal",
                toolName: "daintree.call",
                args: { name: "terminal.focus", arguments: { terminalId } },
                risk: "ui",
                requiresConfirmation: false,
              },
            ]
          : undefined,
    });
  }

  ctx.db.updateWatcher(rec.id, {
    lastClassification: outcome.classification,
    lastCheckedAt: now,
    nextCheckAt: now + rec.cadenceMs,
    status: outcome.stop
      ? outcome.stopReason === "timeout"
        ? "timeout"
        : "condition_met"
      : "active",
  });

  return outcome;
}

async function classifyWithModel(
  rec: WatcherRecord,
  signals: WatcherSignals,
  ctx: ToolContext,
): Promise<WatcherVerdict> {
  try {
    return await ctx.router.json(
      rec.modelTier,
      {
        messages: [
          { role: "system", content: WATCHER_SYSTEM_PROMPT },
          {
            role: "user",
            content: buildWatcherUserPrompt({
              goal: rec.goal,
              agentState: signals.agentState,
              runtimeStatus: signals.runtimeStatus,
              previous: rec.lastClassification,
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
