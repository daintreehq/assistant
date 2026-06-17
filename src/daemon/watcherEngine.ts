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
  VERIFICATION_EVIDENCE_PREFIX,
  type Severity,
  type WatchCondition,
  type WatcherClassification,
  type VerificationResult,
  type RecommendedAction,
} from "../schemas.js";
import {
  WATCHER_SYSTEM_PROMPT,
  buildWatcherUserPrompt,
} from "../models/prompts/index.js";
import type { ToolContext } from "../tools/types.js";
import type { WatcherRecord } from "../schemas.js";

export interface WatcherSignals {
  agentState?: string;
  /** Coarse liveness derived from agentState ("running" | "exited"). */
  runtimeStatus?: string;
  /** Why the agent is waiting, when agentState === "waiting" ("prompt" | "question"). */
  waitingReason?: string;
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
  "completed_unverified",
  "completed_unknown",
  "terminal_exited",
]);

/** Classifications that mean the watcher's job is done and it should stop.
 *  `completed_unverified` is deliberately NOT here: the agent reported completion
 *  but the worktree is dirty/unverified, so the watcher keeps polling until it
 *  either reaches a clean `completed_success` or the user resolves it.
 *  NOTE: an explicit `stopWhen: { stateIs: "completed" }` fires on the raw FSM
 *  state BEFORE the verification gate runs and will stop the watcher regardless of
 *  the verdict. To keep the watcher alive until the tree is clean, gate on the
 *  classification instead (e.g. a modelJudge / classification-based condition). */
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
  completed_unverified: "attention",
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

/** A single terminal's status from a batched terminal.getStatus call. */
export interface TerminalStatusEntry {
  terminalId: string;
  agentState?: string;
  waitingReason?: string;
  error?: string;
}

/** Result of a batched status read: the per-terminal map plus whether the call
 *  itself succeeded — so an absent id can be told apart from a failed read. */
export interface StatusBatch {
  ok: boolean;
  byId: Map<string, TerminalStatusEntry>;
}

/**
 * Batch-read the status of N terminals in ONE terminal.getStatus call.
 *
 * Daintree's terminal.getStatus takes `terminalIds: string[]` (1–256) and
 * returns `{ terminals: [{ terminalId, agentState, waitingReason, ... }] }` —
 * there is no flat `agentState`/`runtimeStatus` on the result. Reading the wrong
 * shape silently never detects state, so the watcher would fall through to the
 * model every tick. `ok` is false when the call failed (so a missing terminal
 * id is NOT mistaken for "closed"); on success an absent id means it is gone.
 */
export async function readStatuses(
  ctx: ToolContext,
  terminalIds: string[],
): Promise<StatusBatch> {
  const byId = new Map<string, TerminalStatusEntry>();
  if (terminalIds.length === 0) return { ok: true, byId };
  try {
    const res = await ctx.mcp.callTool("terminal.getStatus", { terminalIds });
    if (res.isError) return { ok: false, byId };
    const sc = (res.structuredContent ?? {}) as Record<string, unknown>;
    const terminals = Array.isArray(sc.terminals) ? sc.terminals : [];
    for (const t of terminals) {
      if (!t || typeof t !== "object") continue;
      const e = t as Record<string, unknown>;
      const id = typeof e.terminalId === "string" ? e.terminalId : undefined;
      if (!id) continue;
      byId.set(id, {
        terminalId: id,
        agentState: typeof e.agentState === "string" ? e.agentState : undefined,
        waitingReason:
          typeof e.waitingReason === "string" ? e.waitingReason : undefined,
        error: typeof e.error === "string" ? e.error : undefined,
      });
    }
    return { ok: true, byId };
  } catch {
    return { ok: false, byId };
  }
}

/**
 * Read a bounded tail of one terminal via terminal.getOutput. The scrollback is
 * in `structuredContent.content` (a string), NOT the JSON-serialized `text`. An
 * errored read returns "" rather than leaking the error JSON in as fake output.
 */
export async function readOutput(
  ctx: ToolContext,
  terminalId: string,
  tailBytes = 12000,
): Promise<string> {
  try {
    const out = await ctx.mcp.callTool("terminal.getOutput", {
      terminalId,
      maxLines: 200,
    });
    if (out.isError) return "";
    const sc = (out.structuredContent ?? {}) as Record<string, unknown>;
    return typeof sc.content === "string" ? sc.content.slice(-tailBytes) : "";
  } catch {
    return "";
  }
}

/** Map Daintree's agentState onto the coarse runtimeStatus the DSL exposes. */
function runtimeFromAgentState(agentState?: string): string | undefined {
  if (!agentState) return undefined;
  return agentState === "exited" ? "exited" : "running";
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
type WatcherOptions = {
  perTerminal?: Record<string, TerminalState>;
  /** Scopes the post-completion git verification pass to a specific worktree.
   *  Absent for manual watchers — verification then uses the active context. */
  verificationScope?: { worktreeId?: string };
};

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
 * Count uncommitted file changes from a git.getProjectPulse structuredContent,
 * tolerating several plausible shapes (a flat count, a changed-files array, or
 * grouped staged/unstaged/untracked collections). Returns undefined when none of
 * the recognized shapes are present, so the caller can fall back to text parsing.
 */
function countChangedFiles(sc: Record<string, unknown>): number | undefined {
  for (const k of ["changedFiles", "changed_files", "fileCount", "changeCount"]) {
    const v = sc[k];
    if (typeof v === "number" && Number.isFinite(v)) return Math.max(0, Math.floor(v));
    if (Array.isArray(v)) return v.length;
  }
  let total = 0;
  let found = false;
  for (const k of ["staged", "unstaged", "untracked", "modified", "added", "deleted"]) {
    const v = sc[k];
    if (Array.isArray(v)) {
      total += v.length;
      found = true;
    } else if (typeof v === "number" && Number.isFinite(v)) {
      total += Math.max(0, Math.floor(v));
      found = true;
    }
  }
  return found ? total : undefined;
}

/**
 * Derive a VerificationResult from a git.getProjectPulse result. The pulse shape
 * is not strictly documented, so this is defensive: prefer an explicit dirty/clean
 * flag, then a changed-file count, then text markers from a `git status`-style
 * body. When nothing is conclusive the verdict is "unknown" — never a false
 * "clean" — so the conductor stays on the safe (review-first) path. Pure +
 * exported for unit testing without MCP.
 */
export function deriveVerification(
  sc: Record<string, unknown>,
  text: string,
): VerificationResult {
  const changedFiles = countChangedFiles(sc);

  const dirtyFlag =
    typeof sc.isDirty === "boolean"
      ? sc.isDirty
      : typeof sc.dirty === "boolean"
        ? sc.dirty
        : typeof sc.clean === "boolean"
          ? !sc.clean
          : typeof sc.isClean === "boolean"
            ? !sc.isClean
            : undefined;

  let hasGitChanges: boolean | undefined;
  if (dirtyFlag !== undefined) hasGitChanges = dirtyFlag;
  else if (changedFiles !== undefined) hasGitChanges = changedFiles > 0;
  else if (text) {
    // Check dirty markers before clean ones — a status body listing changes never
    // contains "working tree clean", but file paths could spuriously match /clean/.
    if (
      /Changes not staged|Changes to be committed|Untracked files|modified:|new file:|deleted:|renamed:/i.test(
        text,
      )
    ) {
      hasGitChanges = true;
    } else if (/nothing to commit|working tree clean|no changes/i.test(text)) {
      hasGitChanges = false;
    }
  }
  // Dirty wins: a positive changed-file count overrides a clean flag, so a
  // self-contradictory pulse ({ isDirty:false, changedFiles:3 }) is never read as
  // clean. The safe failure mode is "needs review", never a false "verified".
  if (changedFiles !== undefined && changedFiles > 0) hasGitChanges = true;

  if (hasGitChanges === undefined) {
    return {
      verdict: "unknown",
      hasGitChanges: false,
      changedFiles: 0,
      gitSummary: "git state could not be determined from the project pulse",
    };
  }
  if (hasGitChanges) {
    const count = changedFiles ?? 0;
    return {
      verdict: "dirty",
      hasGitChanges: true,
      changedFiles: count,
      gitSummary:
        count > 0
          ? `${count} uncommitted file change(s) in the worktree`
          : "uncommitted changes present in the worktree",
    };
  }
  return {
    verdict: "clean",
    hasGitChanges: false,
    changedFiles: 0,
    gitSummary: "working tree clean (no uncommitted changes)",
  };
}

/**
 * Read-only post-completion reconciliation pass. When an agent reports completion
 * Daintree gives no exit code, so before any irreversible action is suggested we
 * deterministically check the worktree's git cleanliness via git.getProjectPulse
 * (a workbench-tier read tool — no confirmation, safe from watcher context). Never
 * throws: any failure (MCP down, errored call, unrecognized shape) yields verdict
 * "unknown", which the caller treats as not-yet-verified rather than clean.
 */
export async function runVerificationPass(
  ctx: ToolContext,
  scope?: { worktreeId?: string },
): Promise<VerificationResult> {
  const unverifiable = (gitSummary: string): VerificationResult => ({
    verdict: "unknown",
    hasGitChanges: false,
    changedFiles: 0,
    gitSummary,
  });
  if (!ctx.mcp.isConnected()) {
    return unverifiable("Daintree MCP not connected; could not verify git state");
  }
  try {
    const res = await ctx.mcp.callTool("git.getProjectPulse", {
      ...(scope?.worktreeId ? { worktreeId: scope.worktreeId } : {}),
    });
    if (res.isError) return unverifiable("git.getProjectPulse reported an error");
    const sc = (res.structuredContent ?? {}) as Record<string, unknown>;
    return deriveVerification(sc, typeof res.text === "string" ? res.text : "");
  } catch {
    return unverifiable("git.getProjectPulse call failed");
  }
}

/** A completion classification resolved through the verification gate. */
interface GatedCompletion {
  classification: WatcherClassification;
  confidence: number;
  summary: string;
  evidence: string[];
}

/**
 * Resolve a tentative "the agent is done" signal into a trustworthy outcome by
 * running the read-only verification pass. A clean worktree promotes to
 * `completed_success` (terminal, severity done); a dirty or unverifiable tree
 * demotes to `completed_unverified` (non-terminal, attention) so no irreversible
 * action is suggested off an unverified completion. Called from BOTH the Daintree
 * FSM `completed` path and the small-model `completed_success` path, so the gate
 * cannot be bypassed by the model independently classifying completion from tail
 * text while the FSM is still `working`.
 */
async function gateCompletion(
  ctx: ToolContext,
  scope: { worktreeId?: string } | undefined,
  baseEvidence: string[],
): Promise<GatedCompletion> {
  const verification = await runVerificationPass(ctx, scope);
  const evidence = [
    ...baseEvidence,
    `${VERIFICATION_EVIDENCE_PREFIX}${JSON.stringify(verification)}`,
  ];
  if (verification.verdict === "clean") {
    return {
      classification: "completed_success",
      confidence: 0.85,
      summary: "Agent completed; worktree clean and verified.",
      evidence,
    };
  }
  return {
    classification: "completed_unverified",
    confidence: 0.8,
    summary:
      verification.verdict === "dirty"
        ? `Agent completed but ${verification.gitSummary} — review before commit/push.`
        : `Agent completed but git state is unverified (${verification.gitSummary}).`,
    evidence,
  };
}

/**
 * Build the recommended actions attached to a published watcher event. Both
 * waiting states and an unverified completion point the user at the terminal —
 * the latter so they can review and commit before any irreversible git action is
 * suggested. terminal.focus is the real UI tool (open_review is display-only).
 */
function recommendedActionsFor(
  classification: WatcherClassification,
  terminalId: string,
): RecommendedAction[] | undefined {
  if (
    classification === "waiting_for_input" ||
    classification === "permission_prompt"
  ) {
    return [
      {
        label: "Focus terminal",
        toolName: "terminal.focus",
        args: { terminalId },
        risk: "ui",
        requiresConfirmation: false,
      },
    ];
  }
  if (classification === "completed_unverified") {
    return [
      {
        label: "Review completion",
        toolName: "terminal.focus",
        args: { terminalId },
        risk: "ui",
        requiresConfirmation: false,
      },
    ];
  }
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
    // A disabled watcher will never check again — release any scoped grants.
    ctx.db.revokeGrantsByActor(rec.id, now);
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

  // One batched terminal.getStatus for ALL targets, instead of N per-terminal
  // status calls. The deep scrollback tail is still read per terminal below.
  const statuses: StatusBatch = ctx.mcp.isConnected()
    ? await readStatuses(ctx, targets)
    : { ok: false, byId: new Map<string, TerminalStatusEntry>() };

  const outcomes: CheckOutcome[] = [];

  for (const terminalId of targets) {
    const prevState = perTerminal[terminalId];
    let signals: WatcherSignals = { tail: "" };
    let classification: WatcherClassification = "unknown";
    let confidence = 0.4;
    let summary = "Watcher check.";
    let evidence: string[] = [];

    const entry = statuses.byId.get(terminalId);

    if (!ctx.mcp.isConnected()) {
      classification = "needs_large_model";
      summary = "Daintree MCP not connected; cannot read terminal.";
    } else if (statuses.ok && !entry) {
      // The status call succeeded but this terminal isn't in the response — it
      // has been closed/removed. Treat as exited so the watcher stops polling a
      // terminal that no longer exists, instead of looping on empty no_change.
      classification = "terminal_exited";
      confidence = 0.9;
      summary = "Terminal is no longer reported by Daintree (closed or removed).";
      evidence = ["absent from terminal.getStatus response"];
      signals = { agentState: "exited", runtimeStatus: "exited", tail: "" };
    } else {
      const agentState = entry?.agentState;
      const waitingReason = entry?.waitingReason;
      const tail = await readOutput(ctx, terminalId);
      const out = nextOutputState(prevState, tail, now);
      perTerminal[terminalId] = out.state;
      signals = {
        agentState,
        runtimeStatus: runtimeFromAgentState(agentState),
        waitingReason,
        tail,
        msSinceOutput: out.msSinceOutput,
      };

      if (agentState === "exited") {
        classification = "terminal_exited";
        confidence = 0.95;
        summary = "Terminal exited.";
        evidence = ["agentState=exited"];
      } else if (agentState === "waiting") {
        classification = "waiting_for_input";
        confidence = 0.9;
        summary =
          waitingReason === "question"
            ? "Agent is asking a question."
            : "Agent is waiting for input.";
        evidence = [
          `agentState=waiting${waitingReason ? ` (${waitingReason})` : ""}`,
        ];
      } else if (agentState === "completed") {
        // The agent claims completion, but Daintree exposes no exit code — gate
        // trust on a deterministic, read-only git check before any irreversible
        // action can be suggested downstream.
        ({ classification, confidence, summary, evidence } = await gateCompletion(
          ctx,
          options.verificationScope,
          ["agentState=completed"],
        ));
      } else if (tail.trim().length > 0) {
        const verdict = await classifyWithModel(rec, signals, ctx, judge, prevState?.prev);
        classification = verdict.classification;
        confidence = verdict.confidence;
        summary = verdict.summary;
        evidence = verdict.evidence;
        // The small model can also conclude completion from tail text while the
        // FSM is still "working". Route that through the SAME verification gate so
        // a model-claimed completion can never bypass the git-cleanliness check.
        if (classification === "completed_success") {
          ({ classification, confidence, summary, evidence } = await gateCompletion(
            ctx,
            options.verificationScope,
            evidence,
          ));
        }
      } else {
        classification = "no_change";
        summary = "No new output.";
      }

      // Surface a per-terminal status error as evidence (e.g. a transient
      // read problem reported by Daintree) without overriding the verdict.
      if (entry?.error) evidence = [...evidence, `status error: ${entry.error}`];
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
        recommendedActions: recommendedActionsFor(outcome.classification, terminalId),
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

  // Once the watcher has stopped (timed out or its stop condition met) it will
  // never run again — release any scoped automation grants tied to it.
  if (stop) ctx.db.revokeGrantsByActor(rec.id, now);

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
