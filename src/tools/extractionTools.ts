/**
 * On-demand terminal extraction tools.
 *
 * These point the small model at one or more Daintree terminals, read a bounded
 * tail, and pull out caller-specified content — as plain text or structured JSON.
 * Raw scrollback never enters the main agent's context; only the extracted result
 * does. Two shapes:
 *
 *   - terminal.extract        inline. Reads once, or (with a `wait` condition)
 *                             polls until the condition is met, then extracts and
 *                             returns the result to the calling turn. Capped by
 *                             maxAttempts/pollIntervalMs so a wait can never hang.
 *   - terminal.extract.async  fire-and-forget. Runs the same poll+extract in the
 *                             background and publishes the result (optionally with
 *                             a pass/fail verdict) to the attention queue via
 *                             `model_worker`, instead of blocking the turn.
 *
 * Read-only, like the other terminal tools: no terminal input, no file edits. The
 * poll loop reuses the watcher engine's deterministic helpers (evaluateCondition,
 * readOutput, readStatuses, nextOutputState) so the wait vocabulary matches the
 * watcher DSL. `modelJudge` conditions are intentionally unsupported here — they
 * would re-run the classifier every tick; use contains/regex/noOutputForMs/
 * runtimeStatusIs/stateIs, which are deterministic.
 */
import { z } from "zod";
import { randomUUID } from "node:crypto";
import { ok, fail, type ToolDef, type ToolContext } from "./types.js";
import { WatchCondition } from "../schemas.js";
import {
  evaluateCondition,
  readOutput,
  readStatuses,
  nextOutputState,
  collectModelJudges,
  type WatcherSignals,
} from "../daemon/watcherEngine.js";
import {
  EXTRACTOR_SYSTEM_PROMPT,
  buildExtractorUserPrompt,
} from "../models/prompts/index.js";

/* ------------------------------- arg schemas ----------------------------- */

const baseExtractShape = {
  terminalIds: z
    .array(z.string().min(1))
    .min(1)
    .max(16)
    .describe("Daintree terminal id(s) to read and extract from."),
  format: z
    .enum(["text", "json"])
    .default("text")
    .describe("Output shape: plain text, or structured JSON (needs jsonSchema)."),
  jsonSchema: z
    .string()
    .optional()
    .describe(
      "When format=json, a description/JSON-Schema of the value to extract; embedded in the prompt.",
    ),
  wait: WatchCondition.optional().describe(
    "Poll until this condition is met before extracting (contains/regex/noOutputForMs/runtimeStatusIs/stateIs; modelJudge unsupported).",
  ),
  pollIntervalMs: z
    .number()
    .int()
    .min(0)
    .max(60_000)
    .default(2000)
    .describe("Delay between polls in wait mode, in ms."),
  maxAttempts: z
    .number()
    .int()
    .min(1)
    .max(120)
    .default(30)
    .describe("Hard cap on poll attempts so a wait can never hang."),
  tailBytes: z
    .number()
    .int()
    .positive()
    .max(100_000)
    .default(12_000)
    .describe("Max characters of each terminal's tail fed to the model."),
  maxTokens: z
    .number()
    .int()
    .positive()
    .max(2000)
    .default(400)
    .describe("Max tokens the extraction model may produce."),
} as const;

export const ExtractArgs = z
  .object({
    instruction: z
      .string()
      .min(1)
      .optional()
      .describe(
        "What to extract from the output. Omit to run a wait/finished gate only (no model call).",
      ),
    ...baseExtractShape,
  })
  .superRefine((a, ctx) => {
    if (a.format === "json" && a.instruction && !a.jsonSchema) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        path: ["jsonSchema"],
        message: "jsonSchema is required when format is 'json'.",
      });
    }
  });
export type ExtractArgs = z.infer<typeof ExtractArgs>;

export const ExtractAsyncArgs = z
  .object({
    instruction: z
      .string()
      .min(1)
      .describe("What to extract from the output."),
    ...baseExtractShape,
    title: z
      .string()
      .optional()
      .describe("Short label for the queue event the result is published under."),
    verdictInstruction: z
      .string()
      .optional()
      .describe(
        "A pass/fail question evaluated against the extracted result; its verdict drives the event severity.",
      ),
    dedupeKey: z
      .string()
      .optional()
      .describe("Events sharing this key collapse into one in the queue."),
    ttlMs: z
      .number()
      .int()
      .positive()
      .optional()
      .describe("Time-to-live for the published event, in ms."),
  })
  .superRefine((a, ctx) => {
    if (a.format === "json" && !a.jsonSchema) {
      ctx.addIssue({
        code: z.ZodIssueCode.custom,
        path: ["jsonSchema"],
        message: "jsonSchema is required when format is 'json'.",
      });
    }
  });
export type ExtractAsyncArgs = z.infer<typeof ExtractAsyncArgs>;

/* --------------------------------- helpers -------------------------------- */

type TerminalState = ReturnType<typeof nextOutputState>["state"];

const delay = (ms: number, signal?: AbortSignal): Promise<void> =>
  new Promise((resolve) => {
    // Already cancelled — don't even arm the timer; the next poll-loop check exits.
    if (signal?.aborted) return resolve();
    // unref so a pending poll delay never keeps the process alive at shutdown
    // (mirrors the scheduler's interval — supervision pauses when the CLI exits).
    const t = setTimeout(() => {
      signal?.removeEventListener("abort", onAbort);
      resolve();
    }, ms);
    t.unref?.();
    // An Escape-to-cancel mid-wait resolves the delay early so the poll loop can
    // stop on its next iteration instead of sleeping out the full interval.
    const onAbort = () => {
      clearTimeout(t);
      resolve();
    };
    signal?.addEventListener("abort", onAbort, { once: true });
  });

/** Map Daintree's agentState onto the coarse runtimeStatus the DSL exposes. */
function runtimeFromAgentState(agentState?: string): string | undefined {
  if (!agentState) return undefined;
  return agentState === "exited" ? "exited" : "running";
}

interface ReadResult {
  signals: WatcherSignals;
  /** Combined, terminal-labelled tail fed to the extraction model. */
  combinedTail: string;
  /** Every target terminal has exited (or is gone) — no more work anywhere. */
  finished: boolean;
}

/**
 * One read across all target terminals, folded into a single aggregate signal:
 *   - tail        every terminal's tail concatenated UNLABELLED, so contains/regex
 *                 conditions match real output, not the [terminalId] label headers;
 *   - runtimeStatus "exited" only when ALL terminals have exited/are gone;
 *   - msSinceOutput the MIN across terminals (the most recently active one), so a
 *                   noOutputForMs condition fires only once every terminal is idle;
 *   - agentState  the single terminal's state (only meaningful for one target).
 * Mutates `states` with the per-terminal output-tracking memory for noOutputForMs.
 */
async function readSignals(
  ctx: ToolContext,
  terminalIds: string[],
  tailBytes: number,
  states: Map<string, TerminalState>,
  now: number,
): Promise<ReadResult> {
  const statuses = await readStatuses(ctx, terminalIds, true);
  let allExited = statuses.ok;
  let minMsSinceOutput = Number.POSITIVE_INFINITY;
  const parts: {
    terminalId: string;
    tail: string;
    agentState?: string;
    exitCode?: number;
  }[] = [];

  for (const id of terminalIds) {
    const entry = statuses.byId.get(id);
    // A successful status read that omits this id means the terminal is gone.
    const absent = statuses.ok && !entry;
    const agentState = absent ? "exited" : entry?.agentState;
    // The inline recentOutput tail is capped at 50 lines, so it can satisfy
    // extraction only when it already covers the requested tailBytes; otherwise
    // fall back to the deep terminal.getOutput read so contains/regex matching
    // never silently runs against a truncated tail.
    const inline = entry?.recentOutput;
    // Only the deep terminal.getOutput fallback can fail; the inline tail and the
    // absent case are both already-known reads. On a failed deep read fall back to
    // the inline tail (if any) for content matching, but DON'T advance the
    // output-tracking state — a transport hiccup must not read as silence and
    // falsely advance noOutputForMs.
    const prev = states.get(id);
    let tail: string;
    let readFailed = false;
    if (absent) {
      tail = "";
    } else if (inline !== undefined && inline.length >= tailBytes) {
      tail = inline.slice(-tailBytes);
    } else {
      const read = await readOutput(ctx, id, tailBytes);
      readFailed = !read.ok;
      tail = read.ok ? read.value : (inline ?? "");
    }
    if (readFailed) {
      // Preserve prior state and skip this terminal's noOutputForMs contribution.
      if (prev) states.set(id, prev);
    } else {
      const out = nextOutputState(prev, tail, now);
      states.set(id, out.state);
      minMsSinceOutput = Math.min(minMsSinceOutput, out.msSinceOutput);
    }
    if (agentState !== "exited") allExited = false;
    parts.push({ terminalId: id, tail, agentState, exitCode: entry?.exitCode });
  }

  // Labelled tail goes to the model (so it knows which terminal said what); the
  // raw, unlabelled tail drives contains/regex so the [id] headers never match.
  const combinedTail = parts
    .map((p) => (terminalIds.length > 1 ? `[${p.terminalId}]\n${p.tail}` : p.tail))
    .join("\n\n");
  const rawTail = parts.map((p) => p.tail).join("\n\n");

  const signals: WatcherSignals = {
    agentState: terminalIds.length === 1 ? parts[0]?.agentState : undefined,
    runtimeStatus: allExited
      ? "exited"
      : terminalIds.length === 1
        ? runtimeFromAgentState(parts[0]?.agentState)
        : "running",
    // Only meaningful for a single target; aggregating exit codes across many
    // terminals into one signal would be ambiguous.
    exitCode: terminalIds.length === 1 ? parts[0]?.exitCode : undefined,
    tail: rawTail,
    msSinceOutput: Number.isFinite(minMsSinceOutput) ? minMsSinceOutput : 0,
  };

  return { signals, combinedTail, finished: allExited };
}

interface PollResult {
  matched: boolean;
  attempts: number;
  combinedTail: string;
  finished: boolean;
}

/**
 * Read once, or poll until `wait` is met (or attempts are exhausted). Without a
 * `wait` condition it reads a single time and reports matched=true. The loop is
 * hard-capped by maxAttempts so a never-satisfied condition cannot hang.
 */
async function pollUntil(
  ctx: ToolContext,
  args: {
    terminalIds: string[];
    wait?: WatchCondition;
    pollIntervalMs: number;
    maxAttempts: number;
    tailBytes: number;
  },
): Promise<PollResult> {
  const states = new Map<string, TerminalState>();
  let attempts = 0;
  let read: ReadResult | undefined;

  while (attempts < args.maxAttempts) {
    // The user cancelled the turn mid-wait: stop polling now rather than burning
    // the remaining attempts (each a model/MCP read) in the background after the
    // turn is already gone. Reports matched=false; the caller maps it to a clean
    // CANCELLED/timeout result.
    if (ctx.signal?.aborted) break;
    attempts++;
    read = await readSignals(
      ctx,
      args.terminalIds,
      args.tailBytes,
      states,
      Date.now(),
    );
    if (!args.wait || evaluateCondition(args.wait, read.signals)) {
      return {
        matched: true,
        attempts,
        combinedTail: read.combinedTail,
        finished: read.finished,
      };
    }
    if (attempts < args.maxAttempts && args.pollIntervalMs > 0) {
      await delay(args.pollIntervalMs, ctx.signal);
    }
  }

  return {
    matched: false,
    attempts,
    combinedTail: read?.combinedTail ?? "",
    finished: read?.finished ?? false,
  };
}

// `result` defaults to null so a model returning a bare `{}` (no result key)
// doesn't leave it `undefined` — JSON.stringify(undefined) is itself `undefined`.
const ExtractionResult = z.object({ result: z.unknown().nullable().default(null) });

/** Run the extraction model against the gathered tail. */
async function runExtract(
  ctx: ToolContext,
  args: {
    instruction: string;
    format: "text" | "json";
    jsonSchema?: string;
    tailBytes: number;
    maxTokens: number;
    terminalIds: string[];
  },
  tail: string,
): Promise<{ text?: string; json?: unknown }> {
  const userPrompt = buildExtractorUserPrompt({
    instruction: args.instruction,
    format: args.format,
    jsonSchema: args.jsonSchema,
    tail,
    terminalIds: args.terminalIds,
  });
  const messages = [
    { role: "system" as const, content: EXTRACTOR_SYSTEM_PROMPT },
    { role: "user" as const, content: userPrompt },
  ];

  if (args.format === "json") {
    // Always route JSON through router.json: it sets response_format json_object
    // and runs stripThink/extractJson, which absorb the DeepSeek reasoning-leak
    // where the object lands in reasoning_content instead of content.
    const out = await ctx.router.json(
      "small",
      { messages, maxTokens: args.maxTokens },
      ExtractionResult,
    );
    return { json: out.result };
  }
  const res = await ctx.router.chat("small", { messages, maxTokens: args.maxTokens });
  return { text: res.content.trim() };
}

const VERDICT_SYSTEM_PROMPT = `You judge whether an extracted result satisfies a caller's pass/fail condition. Return ONLY a JSON object { "pass": boolean, "reason": "<one short sentence>" }. Be strict and literal; do not invent facts beyond the provided result.`;

async function runVerdict(
  ctx: ToolContext,
  verdictInstruction: string,
  resultText: string,
): Promise<{ pass: boolean; reason: string }> {
  return ctx.router.json(
    "small",
    {
      messages: [
        { role: "system", content: VERDICT_SYSTEM_PROMPT },
        {
          role: "user",
          content: `Pass/fail condition: ${verdictInstruction}\n\nExtracted result:\n"""\n${resultText || "(empty)"}\n"""\n\nReturn the json verdict now.`,
        },
      ],
      maxTokens: 200,
    },
    z.object({ pass: z.boolean(), reason: z.string() }),
  );
}

/** modelJudge would re-run the classifier every poll tick — reject it up front. */
function rejectModelJudge(wait?: WatchCondition) {
  if (wait && collectModelJudges(wait).length > 0) {
    return fail(
      "UNSUPPORTED_CONDITION",
      "modelJudge is not supported in terminal extraction wait conditions; use contains, regex, noOutputForMs, runtimeStatusIs, or stateIs.",
      { recoverable: false },
    );
  }
  return undefined;
}

/**
 * Background extraction for terminal.extract.async. Runs the poll+extract, then
 * (optionally) a pass/fail verdict, and publishes the outcome to the attention
 * queue. Never throws: any failure becomes an `error` event so the result always
 * lands in the inbox rather than vanishing. Exported for direct unit testing.
 */
export async function runAsyncExtraction(
  ctx: ToolContext,
  args: ExtractAsyncArgs,
  requestId: string,
): Promise<void> {
  const label = args.title ?? `Extraction (${args.terminalIds.join(", ")})`;
  const target =
    args.terminalIds.length === 1 ? { terminalId: args.terminalIds[0] } : undefined;
  const dedupeKey = args.dedupeKey ?? `extract:${requestId}`;
  try {
    const poll = await pollUntil(ctx, args);
    if (args.wait && !poll.matched) {
      ctx.queue.publish({
        source: "model_worker",
        severity: "attention",
        title: `${label}: wait timed out`,
        summary: `Wait condition not met after ${poll.attempts} attempt(s); nothing extracted.`,
        target,
        dedupeKey,
        ttlMs: args.ttlMs,
      });
      return;
    }

    const extracted = await runExtract(ctx, args, poll.combinedTail);
    const resultText =
      (args.format === "json"
        ? JSON.stringify(extracted.json)
        : extracted.text) ?? "";

    let verdict: { pass: boolean; reason: string } | undefined;
    if (args.verdictInstruction) {
      verdict = await runVerdict(ctx, args.verdictInstruction, resultText);
    }

    const severity = verdict ? (verdict.pass ? "done" : "attention") : "done";
    ctx.queue.publish({
      source: "model_worker",
      severity,
      title: verdict
        ? `${label}: ${verdict.pass ? "pass" : "fail"}`
        : `${label}: done`,
      summary: verdict
        ? verdict.reason
        : resultText.slice(0, 280) || "(empty result)",
      evidence: [resultText.slice(0, 2000)],
      target,
      dedupeKey,
      ttlMs: args.ttlMs,
    });
  } catch (e) {
    ctx.queue.publish({
      source: "model_worker",
      severity: "error",
      title: `${label}: error`,
      summary: `Extraction failed: ${e instanceof Error ? e.message : String(e)}`,
      target,
      dedupeKey,
      ttlMs: args.ttlMs,
    });
  }
}

/* ---------------------------------- tools --------------------------------- */

const WAIT_PARAM = {
  type: "object",
  description:
    "Poll until this WatchCondition is met before extracting (contains/regex/noOutputForMs/runtimeStatusIs/stateIs; modelJudge unsupported).",
} as const;

const SHARED_EXTRACT_PROPERTIES = {
  terminalIds: {
    type: "array",
    items: { type: "string" },
    description: "Daintree terminal id(s) to read and extract from.",
  },
  format: {
    type: "string",
    enum: ["text", "json"],
    description: "Output shape: plain text, or structured JSON (needs jsonSchema).",
  },
  jsonSchema: {
    type: "string",
    description: "When format=json, a description/JSON-Schema of the value to extract.",
  },
  wait: WAIT_PARAM,
  pollIntervalMs: {
    type: "number",
    description: "Delay between polls in wait mode, in ms (default 2000).",
  },
  maxAttempts: {
    type: "number",
    description: "Hard cap on poll attempts (default 30, max 120).",
  },
  tailBytes: {
    type: "number",
    description: "Max characters of each terminal's tail fed to the model.",
  },
  maxTokens: {
    type: "number",
    description: "Max tokens the extraction model may produce (default 400).",
  },
} as const;

export const extractionTools: ToolDef[] = [
  {
    name: "terminal.extract",
    description:
      "Read a bounded tail of one or more Daintree terminals and extract caller-specified content with the small model — as plain text or structured JSON. Optionally wait (poll) until a condition is met before extracting. Omit `instruction` to use it as a finished/condition gate (returns booleans, no model call). Read-only; requires Daintree MCP.",
    risk: "read",
    readOnly: true,
    schema: ExtractArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        instruction: {
          type: "string",
          description:
            "What to extract. Omit to run a wait/finished gate only (no model call).",
        },
        ...SHARED_EXTRACT_PROPERTIES,
      },
      required: ["terminalIds"],
    },
    async handler(args: ExtractArgs, ctx) {
      if (!ctx.mcp.isConnected()) {
        return fail(
          "MCP_UNAVAILABLE",
          "Daintree MCP is not connected, so terminal output cannot be read.",
          { recoverable: true },
        );
      }
      const rejected = rejectModelJudge(args.wait);
      if (rejected) return rejected;

      const startedAt = Date.now();
      const poll = await pollUntil(ctx, args);
      const elapsedMs = Date.now() - startedAt;

      // Gate-only mode: no instruction ⇒ no model call, just report the booleans.
      // Checked before the timeout path so a gate always answers met/not-met
      // rather than erroring when the condition didn't resolve.
      if (!args.instruction) {
        return ok(
          `finished=${poll.finished}, condition ${poll.matched ? "met" : "not met"} (${poll.attempts} attempt(s)).`,
          {
            finished: poll.finished,
            matched: poll.matched,
            attempts: poll.attempts,
            elapsedMs,
            terminalIds: args.terminalIds,
          },
        );
      }

      if (args.wait && !poll.matched) {
        return fail(
          "WAIT_TIMEOUT",
          `Wait condition not met after ${poll.attempts} attempt(s) (${elapsedMs}ms).`,
          {
            recoverable: true,
            details: { attempts: poll.attempts, finished: poll.finished },
          },
        );
      }

      try {
        const extracted = await runExtract(
          ctx,
          { ...args, instruction: args.instruction },
          poll.combinedTail,
        );
        const result = args.format === "json" ? extracted.json : extracted.text;
        const summary =
          args.format === "json"
            ? "Extracted JSON result."
            : (extracted.text || "(empty result)");
        return ok(summary, {
          terminalIds: args.terminalIds,
          format: args.format,
          attempts: poll.attempts,
          elapsedMs,
          matched: poll.matched,
          finished: poll.finished,
          result,
        });
      } catch (e) {
        return fail(
          "EXTRACT",
          `Extraction failed: ${e instanceof Error ? e.message : String(e)}`,
        );
      }
    },
  },
  {
    name: "terminal.extract.async",
    description:
      "Fire-and-forget terminal extraction. Polls the terminal(s) until the wait condition is met, extracts with the small model, optionally judges the result against a pass/fail condition, and publishes the outcome to the attention queue (instead of blocking the turn). The main thread drains the verdict when next idle. Read-only; requires Daintree MCP.",
    risk: "local",
    schema: ExtractAsyncArgs,
    parameters: {
      type: "object",
      additionalProperties: false,
      properties: {
        instruction: { type: "string", description: "What to extract from the output." },
        ...SHARED_EXTRACT_PROPERTIES,
        title: {
          type: "string",
          description: "Short label for the queue event the result is published under.",
        },
        verdictInstruction: {
          type: "string",
          description:
            "A pass/fail question evaluated against the extracted result; drives event severity.",
        },
        dedupeKey: {
          type: "string",
          description: "Events sharing this key collapse into one in the queue.",
        },
        ttlMs: {
          type: "number",
          description: "Time-to-live for the published event, in ms.",
        },
      },
      required: ["terminalIds", "instruction"],
    },
    async handler(args: ExtractAsyncArgs, ctx) {
      if (!ctx.mcp.isConnected()) {
        return fail(
          "MCP_UNAVAILABLE",
          "Daintree MCP is not connected, so terminal output cannot be read.",
          { recoverable: true },
        );
      }
      const rejected = rejectModelJudge(args.wait);
      if (rejected) return rejected;

      const requestId = randomUUID();
      // Fire in the background; the result lands in the attention queue. This work
      // is deliberately fire-and-forget and OUTLIVES the turn, so it must NOT carry
      // the turn's abort signal — cancelling the spawning turn must not abort an
      // already-detached background extraction. Strip the signal from its context.
      void runAsyncExtraction({ ...ctx, signal: undefined }, args, requestId);
      return ok(
        `Started background extraction ${requestId}; the result will land in the attention queue.`,
        { requestId, terminalIds: args.terminalIds },
      );
    },
  },
];
