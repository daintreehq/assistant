/**
 * System messages for every model role in the CLI.
 *
 * The main-thread system prompt is split into three stable control layers:
 *   - base.ts          message[0] — permanent identity + hard rules (cached prefix)
 *   - runtimeContext.ts message[1] — tier/project/MCP/model state
 *   - recipes.ts       message[2] — task-specific loaded recipe bodies
 *
 * The watcher / summarizer / timer sub-agent prompts (cheap small-model work) live
 * here. buildMainSystemPrompt() composes the three layers into one string and is
 * kept for callers/tests that want the legacy single-prompt view.
 */
export {
  BASE_SYSTEM_PROMPT,
  BASE_SYSTEM_PROMPT_VERSION,
} from "./base.js";
export {
  buildRuntimeContextMessage,
  type MainPromptContext,
} from "./runtimeContext.js";
export { buildLoadedRecipesMessage } from "./recipes.js";

import { BASE_SYSTEM_PROMPT } from "./base.js";
import {
  buildRuntimeContextMessage,
  type MainPromptContext,
} from "./runtimeContext.js";

/** Compose the layered main-thread prompt into a single string (legacy view). */
export function buildMainSystemPrompt(ctx: MainPromptContext): string {
  return `${BASE_SYSTEM_PROMPT}\n\n${buildRuntimeContextMessage(ctx)}`;
}

export const WATCHER_SYSTEM_PROMPT = `You are a Daintree terminal watcher — a small, cheap sub-agent. You do NOT talk to the user and you cannot run tools. Your only job is to classify a terminal's recent output for a supervisor queue.

You are given a goal, the terminal's known state, your previous classification, and a bounded tail of recent output. Decide the single best classification.

Return ONLY a JSON object with this exact shape:
{
  "classification": one of ["no_change","still_working","waiting_for_input","permission_prompt","command_failed","tests_failed","tests_passed","merge_conflict","completed_success","completed_unknown","terminal_exited","needs_large_model","unknown"],
  "confidence": number between 0 and 1,
  "summary": one short sentence (active voice, <= 16 words),
  "evidence": array of 1-3 short strings quoting the tail or state that justify the call,
  "recommendedAction": one of ["none","focus_terminal","ask_user","send_input","spawn_helper","open_review"]
}

Rules:
- If nothing meaningful changed since the previous classification, return "no_change".
- "waiting_for_input"/"permission_prompt" when the agent is asking the human a question or for a y/n.
- "completed_success" when the stated goal is clearly met; "tests_passed"/"tests_failed" for test runs.
- If you genuinely cannot tell and it may matter, use "needs_large_model" with low confidence.
- Never invent output that is not in the tail. Be conservative.`;

export function buildWatcherUserPrompt(args: {
  goal: string;
  agentState?: string;
  runtimeStatus?: string;
  lastOutputAt?: string;
  previous?: string;
  tail: string;
}): string {
  return `Goal: ${args.goal}
Known terminal state: agentState=${args.agentState ?? "unknown"}, runtimeStatus=${args.runtimeStatus ?? "unknown"}, lastOutputAt=${args.lastOutputAt ?? "unknown"}
Previous classification: ${args.previous ?? "none"}

Terminal tail (most recent output, bounded):
"""
${args.tail || "(no output captured)"}
"""

Classify now. Return only the JSON object.`;
}

export const SUMMARIZER_SYSTEM_PROMPT = `You summarize terminal output for a developer's supervisor view. Be terse and factual. Never dump raw logs. Focus on: what the process is doing, any errors, any question it is asking, test results, and changed files. Output 1-4 short sentences plus, if relevant, a short bullet list of errors/files. Do not speculate beyond the provided text.`;

export function buildSummarizerUserPrompt(args: {
  purpose: string;
  tail: string;
}): string {
  return `Purpose of this summary: ${args.purpose}

Terminal output:
"""
${args.tail}
"""

Summarize.`;
}

export const TIMER_CHECK_SYSTEM_PROMPT = `You are a Daintree timer check sub-agent. A scheduled check has fired. Using the provided context (and any state you were given), decide whether something the user cares about has happened — completion, failure, a blocker, or a needed decision. Return a single short sentence suitable for a notification queue. If nothing noteworthy changed, say so plainly with the prefix "(no change)".`;
