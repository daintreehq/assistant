/**
 * Small-model skill selector.
 *
 * The main model calls the `skill.find` tool with a natural-language query ("how
 * do I spawn an agent to edit files?"). This selector hands that query plus every
 * skill's headers (NOT bodies) to the cheap Flash model and gets back the 0-3
 * skill ids whose bodies should be loaded. Metadata-only input keeps the call
 * cheap; the output is validated against SkillSelection.
 */
import type { ModelRouter } from "../models/router.js";
import { SkillSelection, type SkillMetadata } from "./types.js";

const SKILL_SELECTOR_SYSTEM_PROMPT = `You are the Daintree Assistant skill selector.
Return only JSON.
The main assistant has hit a point where it wants a procedural runbook ("skill") and has given you a query describing what it needs to figure out. Choose the 0-3 skills whose full instructions best answer that query.
Rules:
- Match the query against each candidate's "whenToUse" (the detailed signal) and "summary".
- Return 0 skills if none genuinely fit — do not force a match.
- Return 1 skill for a focused need; 2-3 only when the task clearly spans multiple skills.
- Order skillIds best-match first.
- Never invent skill ids. Choose only from the candidate list.
Return this JSON shape:
{
  "skillIds": ["string"],
  "confidence": 0.0,
  "reason": "string",
  "taskType": "string"
}`;

/** Bound the untrusted query sent to the small model (cost + injection surface). */
const MAX_QUERY_CHARS = 2000;

export interface SelectSkillsArgs {
  router: ModelRouter;
  /** Header-only view of every candidate skill (never bodies). */
  candidates: SkillMetadata[];
  /** The main model's natural-language "what I need to figure out" query. */
  query: string;
  /**
   * Abort signal for the in-flight turn. When the user cancels while this
   * selection call is running, the request is torn down (the small model rejects
   * with CancelledError) instead of completing in the background.
   */
  signal?: AbortSignal;
}

export async function selectSkills(
  args: SelectSkillsArgs,
): Promise<SkillSelection> {
  const query = args.query.slice(0, MAX_QUERY_CHARS);

  return args.router.json(
    "small",
    {
      messages: [
        { role: "system", content: SKILL_SELECTOR_SYSTEM_PROMPT },
        {
          role: "user",
          content: `JSON selection task.
Query (what the assistant needs to figure out):
${query}

Candidate skills (headers only):
${JSON.stringify(args.candidates, null, 2)}

Return the JSON object now.`,
        },
      ],
      temperature: 0,
      maxTokens: 500,
      signal: args.signal,
    },
    SkillSelection,
  );
}
