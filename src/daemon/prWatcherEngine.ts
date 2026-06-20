/**
 * PR-state watcher engine.
 *
 * A "pr_state" watcher polls `forge.getPR` on a fixed cadence and surfaces PR
 * lifecycle transitions to the attention queue — entirely deterministically, with
 * NO model call (unlike the terminal watcher FSM in {@link watcherEngine}). The
 * forge surface exposes no review-thread read action (`forge.getPRReviewThreads`
 * does not exist), so this engine can only observe what `forge.getPR` returns:
 * the PR's state (open/closed/merged), its draft flag, and its `updatedAt`
 * timestamp. It therefore detects:
 *   - state → merged / closed   (a terminal transition; stops the watcher)
 *   - draft true → false        (the PR became ready for review)
 *   - updatedAt advanced         (secondary activity — a comment, push, or review;
 *                                 surfaced at `info`, below the interrupt threshold)
 *
 * Like every watcher it is session-scoped and foreground-only: it polls only while
 * the assistant is open, is cancelled on the next session boundary, and never
 * implies background supervision. Last-seen PR state is persisted in the watcher's
 * `optionsJson` so each tick is a pure compare-and-publish against the prior poll.
 */
import type { WatcherRecord } from "../schemas.js";
import type { McpCallResult } from "../mcp/client.js";
import type { ToolContext } from "../tools/types.js";
import { PR_WATCHER_CADENCE_MS } from "../watcherCadence.js";
import { logDebug } from "../debugLog.js";
import { MCP_READ_RETRY_POLICY, MCP_READ_TIMEOUT_MS } from "../reliability.js";

/** Read-only MCP call opts for PR watcher ticks: bound `forge.getPR` with a
 *  timeout and retry a transient transport hiccup, so a slow or unreachable forge
 *  doesn't burn the MCP SDK's 60s default and stall the scheduler tick (issue
 *  #142). Mirrors {@link watcherEngine}'s MCP_READ_OPTS — read-only call only. */
const MCP_READ_OPTS = {
  timeoutMs: MCP_READ_TIMEOUT_MS,
  retries: MCP_READ_RETRY_POLICY.maxRetries,
} as const;

/** Last-seen PR state persisted in `WatcherRecord.optionsJson` between ticks. */
export interface PrWatcherOptions {
  /** Working directory passed to forge.getPR so Daintree resolves the repo. */
  cwd?: string;
  /** The PR/MR number this watcher follows. */
  prNumber: number;
  /** Normalized state at the previous poll: "open" | "closed" | "merged". */
  lastState?: string;
  /** Draft flag at the previous poll. */
  lastIsDraft?: boolean;
  /** `updatedAt` ISO string at the previous poll (for activity detection). */
  lastUpdatedAt?: string;
}

/** What a single PR check did — returned for testability; the scheduler discards it. */
export interface PrCheckResult {
  status: WatcherRecord["status"];
  /** The transition published this tick, if any. */
  transition?: "state_change" | "draft_ready" | "activity";
  published: boolean;
  /** Normalized state observed this tick (undefined when not read). */
  state?: string;
}

/** The PR fields this engine reads, normalized across GitHub/GitLab shapes. */
interface PrFields {
  state?: "open" | "closed" | "merged";
  isDraft?: boolean;
  updatedAt?: string;
  title?: string;
}

/**
 * Pull the PR object out of an MCP result regardless of where the server put it.
 * Daintree may return the payload in `structuredContent`, in a JSON-encoded text
 * body, and may nest it under a wrapper key (`pr`, `pullRequest`, `result`,
 * `data`). We collect every candidate object and let {@link extractPrFields}
 * pick the first that looks like a PR. Pure; never throws.
 */
function candidateObjects(res: McpCallResult): Record<string, unknown>[] {
  const out: Record<string, unknown>[] = [];
  const consider = (v: unknown): void => {
    if (!v || typeof v !== "object" || Array.isArray(v)) return;
    const obj = v as Record<string, unknown>;
    out.push(obj);
    // Common single-object wrapper keys — unwrap one level so a `{ pr: {...} }`
    // envelope is recognized as readily as a bare PR object.
    for (const key of ["pr", "pullRequest", "mergeRequest", "result", "data"]) {
      const nested = obj[key];
      if (nested && typeof nested === "object" && !Array.isArray(nested)) {
        out.push(nested as Record<string, unknown>);
      }
    }
  };
  consider(res.structuredContent);
  if (typeof res.text === "string" && res.text.trim()) {
    try {
      consider(JSON.parse(res.text));
    } catch {
      /* text wasn't JSON — ignore this source */
    }
  }
  return out;
}

function asString(v: unknown): string | undefined {
  return typeof v === "string" && v.length > 0 ? v : undefined;
}
function asBool(v: unknown): boolean | undefined {
  return typeof v === "boolean" ? v : undefined;
}

/**
 * Normalize a PR object's lifecycle fields across forge dialects. GitHub PRs carry
 * `state` ("open"/"closed") plus a separate `merged` boolean / `merged_at`; GitLab
 * MRs fold "merged" into `state` directly and use `work_in_progress` for drafts.
 * We map both onto a single {open, closed, merged} state and a boolean draft flag.
 * Returns `undefined` fields when a value can't be determined (treated as "no
 * change" downstream) so a partial payload never fabricates a transition.
 */
function extractPrFields(res: McpCallResult): PrFields | undefined {
  for (const obj of candidateObjects(res)) {
    const rawState = asString(obj.state)?.toLowerCase();
    const merged =
      asBool(obj.merged) === true ||
      asString(obj.merged_at) !== undefined ||
      asString(obj.mergedAt) !== undefined ||
      rawState === "merged";
    // An object with neither a state nor a merged signal isn't a PR payload.
    if (!rawState && !merged) continue;

    let state: PrFields["state"];
    if (merged) state = "merged";
    else if (rawState === "closed") state = "closed";
    else if (rawState === "open" || rawState === "opened") state = "open";
    else {
      // This object has a `state` we don't recognize as a PR lifecycle value —
      // it's likely an envelope/metadata field (e.g. `{ state: "ok", pr: {...} }`),
      // not the PR itself. Fall through to the next candidate (the unwrapped PR)
      // rather than returning an unusable `{ state: undefined }` and masking the
      // real merge/close transition nested below.
      continue;
    }

    const isDraft =
      asBool(obj.isDraft) ??
      asBool(obj.draft) ??
      asBool(obj.work_in_progress) ??
      asBool(obj.workInProgress);

    const updatedAt = asString(obj.updatedAt) ?? asString(obj.updated_at);
    const title = asString(obj.title);
    return { state, isDraft, updatedAt, title };
  }
  return undefined;
}

/** True when `next` is a strictly later valid timestamp than `prev`. */
function advanced(prev: string | undefined, next: string | undefined): boolean {
  if (!prev || !next) return false;
  const p = Date.parse(prev);
  const n = Date.parse(next);
  if (Number.isNaN(p) || Number.isNaN(n)) return false;
  return n > p;
}

/**
 * Run one PR-state watcher check: poll forge.getPR, diff against the last-seen
 * state stored in `optionsJson`, publish any transition, and persist the new
 * baseline + next check time. Transient failures (MCP down, forge error) simply
 * reschedule without publishing — the same don't-stop-on-a-hiccup policy the
 * terminal engine uses. Corrupt options disable the watcher.
 */
export async function runPrWatcherCheck(
  rec: WatcherRecord,
  ctx: ToolContext,
): Promise<PrCheckResult> {
  const now = Date.now();

  // --- parse persisted options -------------------------------------------
  let options: PrWatcherOptions;
  try {
    const parsed = rec.optionsJson ? JSON.parse(rec.optionsJson) : undefined;
    if (
      !parsed ||
      typeof parsed !== "object" ||
      Array.isArray(parsed) ||
      typeof (parsed as PrWatcherOptions).prNumber !== "number"
    ) {
      throw new Error("pr watcher options missing a numeric prNumber");
    }
    options = parsed as PrWatcherOptions;
  } catch (err) {
    logDebug(ctx.config, "pr_watcher.disabled", {
      watcherId: rec.id,
      reason: "corrupt pr watcher state",
      error: err instanceof Error ? err.message : String(err),
    });
    ctx.db.updateWatcher(rec.id, { status: "error", lastCheckedAt: now });
    ctx.db.revokeGrantsByActor(rec.id, now);
    ctx.queue.publish({
      source: "pr_watcher",
      severity: "error",
      title: `${rec.title}: watcher disabled`,
      summary: `Corrupt PR watcher state for ${rec.id}: ${err instanceof Error ? err.message : String(err)}`,
      epistemicKind: "unverified",
    });
    return { status: "error", published: false };
  }

  // Time-based timeout wins before any read: a PR watcher with a stopAfterMs
  // budget should retire on schedule even if the forge is unreachable.
  if (rec.stopAfterMs && now - rec.createdAt >= rec.stopAfterMs) {
    ctx.db.updateWatcher(rec.id, { status: "timeout", lastCheckedAt: now });
    ctx.db.revokeGrantsByActor(rec.id, now);
    ctx.queue.publish({
      source: "pr_watcher",
      severity: "info",
      title: `${rec.title}: watch ended`,
      summary: `Stopped watching PR #${options.prNumber} after the configured timeout; no terminal transition observed.`,
      epistemicKind: "observed",
      dedupeKey: `pr_watcher:${rec.id}:timeout`,
    });
    logDebug(ctx.config, "pr_watcher.timeout", {
      watcherId: rec.id,
      prNumber: options.prNumber,
    });
    return { status: "timeout", published: true };
  }

  const reschedule = (): void => {
    ctx.db.updateWatcher(rec.id, {
      lastCheckedAt: now,
      nextCheckAt: now + PR_WATCHER_CADENCE_MS,
      status: "active",
    });
  };

  // --- transient guards: never stop the watcher on a hiccup --------------
  if (!ctx.mcp.isConnected()) {
    logDebug(ctx.config, "pr_watcher.skip", {
      watcherId: rec.id,
      reason: "mcp_disconnected",
    });
    reschedule();
    return { status: "active", published: false };
  }

  let res: McpCallResult;
  try {
    res = await ctx.mcp.callTool(
      "forge.getPR",
      {
        cwd: options.cwd ?? ctx.projectPath,
        prNumber: options.prNumber,
      },
      undefined,
      MCP_READ_OPTS,
    );
  } catch (err) {
    logDebug(ctx.config, "pr_watcher.error", {
      watcherId: rec.id,
      reason: "forge_getpr_threw",
      error: err instanceof Error ? err.message : String(err),
    });
    reschedule();
    return { status: "active", published: false };
  }

  const fields = res.isError ? undefined : extractPrFields(res);
  if (!fields) {
    // forge.getPR errored or returned an unrecognizable payload — treat as a
    // transient read failure and re-check, rather than fabricating a transition.
    logDebug(ctx.config, "pr_watcher.unreadable", {
      watcherId: rec.id,
      isError: res.isError,
    });
    reschedule();
    return { status: "active", published: false };
  }

  // --- diff against the last-seen baseline -------------------------------
  const firstObservation = options.lastState === undefined;
  const stateChanged =
    fields.state !== undefined && fields.state !== options.lastState;
  const becameTerminal =
    (fields.state === "merged" || fields.state === "closed") &&
    (firstObservation || stateChanged);
  const draftReady =
    options.lastIsDraft === true && fields.isDraft === false;
  // Only attribute "activity" when nothing more specific fired — a state change or
  // a merge also bumps updatedAt, and we don't want a duplicate info ping for it.
  const activity =
    !becameTerminal &&
    !draftReady &&
    !stateChanged &&
    advanced(options.lastUpdatedAt, fields.updatedAt);

  const prLabel = `PR #${options.prNumber}`;
  const titleSuffix = fields.title ? ` — ${fields.title}` : "";
  let result: PrCheckResult = {
    status: "active",
    published: false,
    state: fields.state,
  };

  if (becameTerminal) {
    const verb = fields.state === "merged" ? "merged" : "closed";
    ctx.queue.publish({
      source: "pr_watcher",
      severity: "attention",
      title: `${rec.title}: ${prLabel} ${verb}`,
      summary: `${prLabel} is ${verb}${titleSuffix}.`,
      evidence: [prLabel, `state: ${fields.state}`],
      epistemicKind: "observed",
      // Stable across re-publishes of the same transition so a lingering tick
      // updates one inbox row instead of stacking duplicates.
      dedupeKey: `pr_watcher:${rec.id}:state_change`,
    });
    result = { status: "condition_met", transition: "state_change", published: true, state: fields.state };
  } else if (draftReady) {
    ctx.queue.publish({
      source: "pr_watcher",
      severity: "attention",
      title: `${rec.title}: ${prLabel} ready for review`,
      summary: `${prLabel} moved out of draft and is ready for review${titleSuffix}.`,
      evidence: [prLabel, "draft: false"],
      epistemicKind: "observed",
      dedupeKey: `pr_watcher:${rec.id}:draft_ready`,
    });
    result = { status: "active", transition: "draft_ready", published: true, state: fields.state };
  } else if (activity) {
    ctx.queue.publish({
      // Secondary activity (a comment/push/review) sits below the interrupt
      // threshold — it queues quietly at "info" rather than waking the main thread.
      source: "pr_watcher",
      severity: "info",
      title: `${rec.title}: ${prLabel} updated`,
      summary: `${prLabel} has new activity (comment, push, or review)${titleSuffix}.`,
      evidence: [prLabel, fields.updatedAt ? `updatedAt: ${fields.updatedAt}` : "updatedAt advanced"],
      epistemicKind: "observed",
      dedupeKey: `pr_watcher:${rec.id}:activity`,
    });
    result = { status: "active", transition: "activity", published: true, state: fields.state };
  }

  // --- persist the new baseline + schedule -------------------------------
  const nextOptions: PrWatcherOptions = {
    ...options,
    lastState: fields.state ?? options.lastState,
    lastIsDraft: fields.isDraft ?? options.lastIsDraft,
    lastUpdatedAt: fields.updatedAt ?? options.lastUpdatedAt,
  };
  ctx.db.updateWatcher(rec.id, {
    lastClassification: result.transition ?? "no_change",
    lastEpistemicKind: "observed",
    lastCheckedAt: now,
    nextCheckAt: now + PR_WATCHER_CADENCE_MS,
    optionsJson: JSON.stringify(nextOptions),
    status: result.status,
  });
  // A terminal PR will never transition again — release any scoped grants so a
  // recycled watcher id can't inherit a stale authorization.
  if (result.status === "condition_met") {
    ctx.db.revokeGrantsByActor(rec.id, now);
  }

  logDebug(ctx.config, "pr_watcher.check", {
    watcherId: rec.id,
    prNumber: options.prNumber,
    state: fields.state,
    isDraft: fields.isDraft,
    transition: result.transition,
    published: result.published,
    status: result.status,
  });

  return result;
}
