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
import { logDebug } from "../debugLog.js";
import { WATCHER_SPAWN_GRACE_MS } from "../watcherCadence.js";

export interface WatcherSignals {
  agentState?: string;
  /** Coarse liveness derived from agentState ("running" | "exited"). */
  runtimeStatus?: string;
  /** Why the agent is waiting, when agentState === "waiting" ("prompt" | "question"). */
  waitingReason?: string;
  /** Numeric process exit code, present once the terminal has exited. */
  exitCode?: number;
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
  // "attention" — never bury it below the scheduler's surfacing threshold just
  // because the underlying classification was low severity. "done" counts as below
  // (a clean completion that matched an explicit stop/alert must still surface).
  let severity: Severity = args.timedOut ? "attention" : SEVERITY_MAP[classification];
  if (
    (alertMatched || stopMatched) &&
    (severity === "debug" || severity === "info" || severity === "done")
  ) {
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
  /** Recent output tail returned inline when includeOutput is requested. May be
   *  absent even when requested (Daintree can omit it), so callers must fall
   *  back to terminal.getOutput when this is undefined. */
  recentOutput?: string;
  /** Numeric process exit code, present once the terminal has exited. Used as
   *  signal evidence (a nonzero code is failure evidence) — never as a trust gate;
   *  completion trust still requires the deterministic git verification pass. */
  exitCode?: number;
  /** Epoch-ms timestamp the terminal/agent was spawned, when reported. */
  spawnedAt?: number;
  /** Epoch-ms timestamp of the last agentState transition, when reported. */
  lastTransitionAt?: number;
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
 *
 * When `includeOutput` is true, the call also requests a bounded recent-output
 * tail (capped at the Daintree-documented max of 50 lines) inline on each entry,
 * so the common watcher/extraction poll needs only this ONE call instead of an
 * additional per-terminal terminal.getOutput. `recentOutput` may still be absent
 * per entry — callers fall back to readOutput in that case.
 */
export async function readStatuses(
  ctx: ToolContext,
  terminalIds: string[],
  includeOutput = false,
): Promise<StatusBatch> {
  const byId = new Map<string, TerminalStatusEntry>();
  if (terminalIds.length === 0) return { ok: true, byId };
  try {
    const args: Record<string, unknown> = { terminalIds };
    if (includeOutput) {
      args.includeOutput = { lines: 50, stripAnsi: true };
    }
    const res = await ctx.mcp.callTool("terminal.getStatus", args);
    if (res.isError) {
      logDebug(ctx.config, "mcp.getStatus", {
        requested: terminalIds,
        ok: false,
        error: typeof (res as { text?: string }).text === "string" ? (res as { text?: string }).text : undefined,
      });
      return { ok: false, byId };
    }
    const sc = (res.structuredContent ?? {}) as Record<string, unknown>;
    const terminals = Array.isArray(sc.terminals) ? sc.terminals : [];
    // Diagnostic: record exactly which ids Daintree returned vs. which we asked
    // for. A requested id that's missing here is what the watcher reads as
    // "exited" — so this line distinguishes "wrong/foreign id namespace" (other
    // ids present) from "truly nothing tracked" (empty), and "scoped out".
    logDebug(ctx.config, "mcp.getStatus", {
      requested: terminalIds,
      ok: true,
      returnedIds: terminals
        .map((t) => (t && typeof t === "object" ? (t as Record<string, unknown>).terminalId : undefined))
        .filter(Boolean),
      rawTerminals: terminals,
    });
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
        recentOutput:
          typeof e.recentOutput === "string" ? e.recentOutput : undefined,
        // New exit metadata — read defensively. Exit codes and epoch-ms
        // timestamps are integers, so Number.isInteger rejects NaN, Infinity, and
        // stray fractional values; null, strings, and absent values all fall
        // through to undefined for backwards compat.
        exitCode: Number.isInteger(e.exitCode as number)
          ? (e.exitCode as number)
          : undefined,
        spawnedAt: Number.isInteger(e.spawnedAt as number)
          ? (e.spawnedAt as number)
          : undefined,
        lastTransitionAt: Number.isInteger(e.lastTransitionAt as number)
          ? (e.lastTransitionAt as number)
          : undefined,
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
    const sc = (out.structuredContent ?? {}) as Record<string, unknown>;
    const content = typeof sc.content === "string" ? sc.content : "";
    logDebug(ctx.config, "mcp.getOutput", {
      terminalId,
      isError: Boolean(out.isError),
      contentLen: content.length,
      error:
        out.isError && typeof (out as { text?: string }).text === "string"
          ? (out as { text?: string }).text
          : undefined,
    });
    if (out.isError) return "";
    return content.slice(-tailBytes);
  } catch {
    return "";
  }
}

/** A terminal as reported by the authoritative `terminal.list` inventory. */
export interface ListedTerminal {
  agentState?: string;
  waitingReason?: string;
  exitCode?: number;
}

/**
 * Result of reading the terminal.list inventory. `ok` is true ONLY when the call
 * succeeded AND returned a recognizable `terminals` array (even an empty one) — so
 * the caller can tell "inventory says the terminal is gone" (ok, id absent) from
 * "inventory could not be read" (call errored / unparseable). An absent id must be
 * treated as a real exit only when `ok` is true; otherwise the watcher stays alive.
 */
export interface TerminalListResult {
  ok: boolean;
  byId: Map<string, ListedTerminal>;
}

/**
 * Read Daintree's authoritative terminal inventory via terminal.list, keyed by id.
 * Used to cross-check a terminal that terminal.getStatus omitted: a terminal listed
 * here is alive, regardless of getStatus. terminal.list may return its array under
 * `structuredContent.terminals` AND/OR a JSON string in `text`, keyed by `id` (with
 * a `terminalId` fallback) — read both shapes, merge by id. Never throws.
 */
export async function readTerminalList(
  ctx: ToolContext,
): Promise<TerminalListResult> {
  const byId = new Map<string, ListedTerminal>();
  try {
    const res = await ctx.mcp.callTool("terminal.list", {});
    if (res.isError) {
      logDebug(ctx.config, "mcp.terminalList", { ok: false, isError: true });
      return { ok: false, byId };
    }
    // Collect terminals from BOTH the structured payload and a JSON `text` body —
    // either may be the one Daintree populated. A recognized array (even empty)
    // means the inventory was readable.
    const entries: unknown[] = [];
    let foundArray = false;
    const sc = (res.structuredContent ?? {}) as Record<string, unknown>;
    if (Array.isArray(sc.terminals)) {
      entries.push(...sc.terminals);
      foundArray = true;
    }
    const text = (res as { text?: string }).text;
    if (typeof text === "string" && text.trim()) {
      try {
        const parsed = JSON.parse(text) as { terminals?: unknown };
        if (Array.isArray(parsed?.terminals)) {
          entries.push(...parsed.terminals);
          foundArray = true;
        }
      } catch {
        /* not JSON — ignore this source */
      }
    }
    if (!foundArray) {
      logDebug(ctx.config, "mcp.terminalList", { ok: false, reason: "no terminals array" });
      return { ok: false, byId };
    }
    for (const t of entries) {
      if (!t || typeof t !== "object") continue;
      const e = t as Record<string, unknown>;
      const id =
        typeof e.id === "string"
          ? e.id
          : typeof e.terminalId === "string"
            ? e.terminalId
            : undefined;
      if (!id) continue;
      byId.set(id, {
        agentState: typeof e.agentState === "string" ? e.agentState : undefined,
        waitingReason:
          typeof e.waitingReason === "string" ? e.waitingReason : undefined,
        exitCode: Number.isInteger(e.exitCode as number)
          ? (e.exitCode as number)
          : undefined,
      });
    }
    // A non-empty inventory whose entries yielded ZERO parseable ids is schema
    // drift, not "everything is gone" — treat it as unreadable so a target absent
    // from it is not falsely declared exited.
    if (entries.length > 0 && byId.size === 0) {
      logDebug(ctx.config, "mcp.terminalList", { ok: false, reason: "no parseable ids", entries: entries.length });
      return { ok: false, byId };
    }
    logDebug(ctx.config, "mcp.terminalList", { ok: true, ids: [...byId.keys()] });
    return { ok: true, byId };
  } catch {
    logDebug(ctx.config, "mcp.terminalList", { ok: false, threw: true });
    return { ok: false, byId };
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
  /** Whether this terminal has ever been observed in terminal.getStatus. Until it
   *  has (and within the spawn grace), an absent terminal is "still registering",
   *  not exited — see WATCHER_SPAWN_GRACE_MS. */
  seen?: boolean;
}
type WatcherOptions = {
  perTerminal?: Record<string, TerminalState>;
  /** Scopes the post-completion git verification pass to a specific worktree.
   *  Absent for manual watchers — verification then uses the active context. */
  verificationScope?: { worktreeId?: string };
  /** The agentTask spawn mode that created this watcher. For a one-shot
   *  "explore" agent, an `agentState=waiting` (idle at the prompt, not a real
   *  question) is end-of-turn completion — not a human-input block — so it routes
   *  through the completion gate instead of waking the main thread. Absent for
   *  manual watchers and treated as "edit" (genuine waiting_for_input). */
  spawnMode?: "edit" | "explore";
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
 * Whether a (possibly composite) condition matches against terminal output text
 * (`contains`/`regex`). Such conditions need the full scrollback window, so when
 * one is present the watcher reads the deep terminal.getOutput tail instead of
 * trusting the bounded inline recentOutput tail — a 50-line tail could otherwise
 * silently miss a match that lives deeper in the output.
 */
export function hasTextCondition(cond?: WatchCondition): boolean {
  if (!cond) return false;
  if ("contains" in cond || "regex" in cond) return true;
  if ("all" in cond) return cond.all.some((c) => hasTextCondition(c));
  if ("any" in cond) return cond.any.some((c) => hasTextCondition(c));
  if ("not" in cond) return hasTextCondition(cond.not);
  return false;
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
 * Read-only post-completion reconciliation pass. A reported completion (and its
 * exit code, when present) is only signal evidence, so before any irreversible
 * action is suggested we deterministically check the worktree's git cleanliness
 * via git.getProjectPulse
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
    logDebug(ctx.config, "watcher.disabled", {
      watcherId: rec.id,
      title: rec.title,
      reason: "corrupt watcher state",
      error: err instanceof Error ? err.message : String(err),
    });
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
  // A deterministic contains/regex condition must see the full scrollback the
  // user expects, so the bounded inline tail isn't enough — read deep in that
  // case. Pure state/model watchers (the common supervisor) keep the cheap tail.
  const needsDeepTail =
    hasTextCondition(alertWhen) || hasTextCondition(stopWhen);
  const timedOut = Boolean(
    rec.stopAfterMs && now - rec.createdAt >= rec.stopAfterMs,
  );
  const perTerminal: Record<string, TerminalState> = { ...options.perTerminal };

  logDebug(ctx.config, "watcher.check.start", {
    watcherId: rec.id,
    title: rec.title,
    targets,
    isSupervisor: rec.isSupervisor,
    cadenceMs: rec.cadenceMs,
    ageMs: now - rec.createdAt,
    timedOut,
    mcpConnected: ctx.mcp.isConnected(),
  });

  // One batched terminal.getStatus for ALL targets, instead of N per-terminal
  // status calls. includeOutput piggybacks a recent-output tail on the same
  // call so the common case needs zero per-terminal terminal.getOutput reads.
  const statuses: StatusBatch = ctx.mcp.isConnected()
    ? await readStatuses(ctx, targets, true)
    : { ok: false, byId: new Map<string, TerminalStatusEntry>() };

  // terminal.getStatus has been observed to omit live agent terminals that
  // terminal.list still reports (id-namespace / scope gap), so a missing id is NOT
  // reliable proof of exit. When any target is absent from getStatus, cross-check
  // the authoritative inventory ONCE this tick and trust it over getStatus.
  // statuses.ok already implies the MCP call succeeded (i.e. connected), so no
  // separate isConnected() gate — that would open a flap window where `list` is
  // left undefined while we still enter the absent branch below.
  let list: TerminalListResult | undefined;
  if (statuses.ok && targets.some((t) => !statuses.byId.has(t))) {
    list = await readTerminalList(ctx);
  }

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
      // Absent from terminal.getStatus. Don't trust that as "exited" — cross-check
      // the authoritative inventory first (getStatus omits live agent terminals
      // that terminal.list still reports).
      const listed = list?.byId.get(terminalId);
      if (listed) {
        // Alive per terminal.list — getStatus simply didn't include it. Classify
        // from the listed agentState (no scrollback is available via this path).
        const agentState = listed.agentState;
        signals = {
          agentState,
          runtimeStatus: runtimeFromAgentState(agentState),
          waitingReason: listed.waitingReason,
          exitCode: listed.exitCode,
          tail: "",
        };
        if (agentState === "exited") {
          classification = "terminal_exited";
          confidence = 0.95;
          summary = "Terminal exited.";
          evidence = ["agentState=exited (terminal.list)"];
          if (typeof listed.exitCode === "number" && listed.exitCode !== 0) {
            evidence.push(`exitCode=${listed.exitCode} (nonzero)`);
          }
        } else if (agentState === "waiting") {
          if (options.spawnMode === "explore" && listed.waitingReason !== "question") {
            // A one-shot explore agent idle at the prompt (no real question) has
            // finished its turn — that's completion, not a human-input block. Explore
            // is read-only, so there is nothing to git-verify and no irreversible
            // action to gate: classify completed_success directly (terminal). Routing
            // through gateCompletion would loop forever on a pre-existing dirty
            // worktree, since explore can never clean it. We rely on Daintree marking
            // a genuine interactive question as waitingReason="question"; any other or
            // absent reason is treated as idle/end-of-turn.
            classification = "completed_success";
            confidence = 0.85;
            summary = "Explore agent finished its turn (idle at prompt).";
            evidence = [
              `agentState=waiting${listed.waitingReason ? ` (${listed.waitingReason})` : ""} (explore-idle, terminal.list)`,
            ];
          } else {
            classification = "waiting_for_input";
            confidence = 0.9;
            summary =
              listed.waitingReason === "question"
                ? "Agent is asking a question."
                : "Agent is waiting for input.";
            evidence = [
              `agentState=waiting${listed.waitingReason ? ` (${listed.waitingReason})` : ""} (terminal.list)`,
            ];
          }
        } else if (agentState === "completed") {
          ({ classification, confidence, summary, evidence } = await gateCompletion(
            ctx,
            options.verificationScope,
            ["agentState=completed (terminal.list)"],
          ));
        } else {
          classification = "no_change";
          confidence = 0.5;
          summary = `Agent ${agentState ?? "active"} (per terminal.list; getStatus omitted it).`;
        }
      } else if (!list || !list.ok) {
        // getStatus omitted the terminal AND the inventory couldn't be read (errored,
        // malformed, or never fetched) — we CANNOT prove it exited (this is exactly
        // how the original false-exit bug arises). Stay alive and re-check rather
        // than assert death.
        classification = "no_change";
        confidence = 0.4;
        summary =
          "Terminal absent from terminal.getStatus and terminal.list could not be read; will re-check.";
        signals = { tail: "" };
      } else if (!prevState?.seen && now - rec.createdAt < WATCHER_SPAWN_GRACE_MS) {
        // Never observed yet and still inside the spawn grace: right after
        // agent.launch the terminal may not be registered anywhere for a moment.
        // Treat as still-registering so we don't stop before ever seeing it.
        classification = "no_change";
        confidence = 0.5;
        summary = "Terminal not yet registered by Daintree (just spawned); will re-check.";
        signals = { tail: "" };
      } else {
        // Absent from BOTH terminal.getStatus and a SUCCESSFULLY-READ terminal.list
        // (or past the grace) — now it's a real exit.
        classification = "terminal_exited";
        confidence = 0.9;
        summary = "Terminal is no longer reported by Daintree (closed or removed).";
        evidence = ["absent from terminal.getStatus and terminal.list"];
        signals = { agentState: "exited", runtimeStatus: "exited", tail: "" };
      }
    } else {
      const agentState = entry?.agentState;
      const waitingReason = entry?.waitingReason;
      // Prefer the inline tail from terminal.getStatus (includeOutput). It is
      // bounded to 50 lines — enough for the watcher to classify — and saves a
      // per-terminal terminal.getOutput. Fall back to the deep read when Daintree
      // omitted recentOutput, or when a contains/regex condition needs the full
      // scrollback window. An empty-string tail is a valid "no output yet", so we
      // fall back on undefined, not on falsiness.
      const tail =
        !needsDeepTail && entry?.recentOutput !== undefined
          ? entry.recentOutput
          : await readOutput(ctx, terminalId);
      const out = nextOutputState(prevState, tail, now);
      perTerminal[terminalId] = out.state;
      signals = {
        agentState,
        runtimeStatus: runtimeFromAgentState(agentState),
        waitingReason,
        exitCode: entry?.exitCode,
        tail,
        msSinceOutput: out.msSinceOutput,
      };

      if (agentState === "exited") {
        classification = "terminal_exited";
        confidence = 0.95;
        summary = "Terminal exited.";
        evidence = ["agentState=exited"];
        // A nonzero exit code is failure evidence worth surfacing; a clean exit
        // (0) is silent to avoid noise. The classification stays terminal_exited
        // either way — exit code enriches evidence, it does not reroute.
        if (typeof signals.exitCode === "number" && signals.exitCode !== 0) {
          evidence.push(`exitCode=${signals.exitCode} (nonzero)`);
        }
      } else if (agentState === "waiting") {
        if (options.spawnMode === "explore" && waitingReason !== "question") {
          // A one-shot explore agent idle at the prompt (no real question) has
          // finished its turn — that's completion, not a human-input block. Explore
          // is read-only, so there is nothing to git-verify and no irreversible
          // action to gate: classify completed_success directly (terminal). Routing
          // through gateCompletion would loop forever on a pre-existing dirty
          // worktree, since explore can never clean it. We rely on Daintree marking
          // a genuine interactive question as waitingReason="question"; any other or
          // absent reason is treated as idle/end-of-turn.
          classification = "completed_success";
          confidence = 0.85;
          summary = "Explore agent finished its turn (idle at prompt).";
          evidence = [
            `agentState=waiting${waitingReason ? ` (${waitingReason})` : ""} (explore-idle)`,
          ];
        } else {
          classification = "waiting_for_input";
          confidence = 0.9;
          summary =
            waitingReason === "question"
              ? "Agent is asking a question."
              : "Agent is waiting for input.";
          evidence = [
            `agentState=waiting${waitingReason ? ` (${waitingReason})` : ""}`,
          ];
        }
      } else if (agentState === "completed") {
        // The agent claims completion. The exit code (when present) is signal
        // evidence, not a trust gate — completion trust is gated on a
        // deterministic, read-only git check before any irreversible action can
        // be suggested downstream.
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
      // Latch "seen" once Daintree reports the terminal anywhere (getStatus OR the
      // terminal.list inventory), so a later absence from both counts as a real
      // exit rather than re-entering the spawn grace.
      seen: prevState?.seen || Boolean(entry) || Boolean(list?.byId.get(terminalId)),
    };

    logDebug(ctx.config, "watcher.check.terminal", {
      watcherId: rec.id,
      terminalId,
      present: Boolean(entry),
      agentState: signals.agentState,
      waitingReason: signals.waitingReason,
      exitCode: signals.exitCode,
      msSinceOutput: signals.msSinceOutput,
      tailLen: signals.tail.length,
      previous: prevState?.prev,
      classification: outcome.classification,
      confidence: outcome.confidence,
      severity: outcome.severity,
      summary: outcome.summary,
      evidence: outcome.evidence,
      shouldPublish: outcome.shouldPublish,
      stop: outcome.stop,
      stopReason: outcome.stopReason,
    });

    if (outcome.shouldPublish) {
      // A supervisor watcher exists to announce when its spawned agent is done. A
      // clean completion is severity "done", which sits BELOW the scheduler's
      // "attention" surfacing threshold and would never reach the inbox or wake the
      // main loop. Promote a supervisor's terminal-ending outcome to at least
      // "attention" so "the agent finished" actually surfaces.
      const severity: Severity =
        rec.isSupervisor &&
        outcome.stop &&
        SEVERITY_WEIGHT[outcome.severity] < SEVERITY_WEIGHT.attention
          ? "attention"
          : outcome.severity;
      logDebug(ctx.config, "watcher.publish", {
        watcherId: rec.id,
        terminalId,
        severity,
        classification: outcome.classification,
        dedupeKey: `watcher:${rec.id}:${terminalId}:${outcome.classification}`,
      });
      ctx.queue.publish({
        source: "terminal_watcher",
        severity,
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

  logDebug(ctx.config, stop ? "watcher.stop" : "watcher.check.done", {
    watcherId: rec.id,
    title: rec.title,
    headline: headline.classification,
    severity: headline.severity,
    stop,
    stopReason,
    nextCheckAt: stop ? undefined : now + rec.cadenceMs,
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
