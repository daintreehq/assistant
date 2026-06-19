/**
 * Small-model recipe selector.
 *
 * Uses the cheap Flash model (router.json("small", ...)) to choose 0-3 recipe ids
 * for the next main-model turn. The selector sees only recipe metadata — never the
 * bodies — so selection stays cheap. Output is validated against RecipeSelection.
 */
import type { ModelRouter } from "../models/router.js";
import type { ChatMessage } from "../models/fireworks.js";
import type { RecipeRegistry } from "./registry.js";
import { RecipeSelection } from "./types.js";

const RECIPE_SELECTOR_SYSTEM_PROMPT = `You are the Daintree Assistant recipe selector.
Return only JSON.
Your job is to choose 0-3 assistant prompt recipes for the next main-model turn.
Choose recipes only when they materially help the main model do the user's task.
Rules:
- Prefer 0 recipes for simple explanations.
- Prefer 1 recipe for focused tasks.
- Use 2-3 recipes only when the task spans multiple Daintree workflows.
- Keep existing recipes if the task has not changed.
- Choose edit/delegation recipes when the user asks to implement, fix, refactor, add tests, update docs, or otherwise change files.
- Choose Daintree recipe/worktree recipes when the user asks to run recipes, create startup layouts, initialize worktrees, review PRs, or set up repeatable environments.
- Choose orchestration recipes when the user asks about agents, terminals, queues, watchers, timers, project state, or what needs attention.
- Never invent recipe ids. Choose only from the candidate list.
Return this JSON shape:
{
  "recipeIds": ["string"],
  "confidence": 0.0,
  "reason": "string",
  "taskType": "string",
  "keepExisting": false
}`;

/** Bound the untrusted user input sent to the small model (cost + injection surface). */
const MAX_USER_INPUT_CHARS = 2000;

export interface SelectRecipesArgs {
  router: ModelRouter;
  registry: RecipeRegistry;
  userInput: string;
  recentMessages: ReadonlyArray<ChatMessage>;
  activeRecipeIds: string[];
  /**
   * Abort signal for the in-flight turn. When the user cancels while this pre-turn
   * selection call is running, the request is torn down (the small model rejects
   * with CancelledError) instead of completing in the background.
   */
  signal?: AbortSignal;
}

export async function selectRecipes(
  args: SelectRecipesArgs,
): Promise<RecipeSelection> {
  const candidates = args.registry.metadataForSelection();
  const userInput = args.userInput.slice(0, MAX_USER_INPUT_CHARS);
  const recent = args.recentMessages
    .filter((m) => m.role === "user" || m.role === "assistant")
    .slice(-8)
    .map((m) => `${m.role}: ${(m.content ?? "").slice(0, 800)}`)
    .join("\n");

  return args.router.json(
    "small",
    {
      messages: [
        { role: "system", content: RECIPE_SELECTOR_SYSTEM_PROMPT },
        {
          role: "user",
          content: `JSON selection task.
Current user input:
${userInput}

Recent conversation:
${recent || "(none)"}

Currently loaded recipe ids:
${JSON.stringify(args.activeRecipeIds)}

Candidate recipes (metadata only):
${JSON.stringify(candidates, null, 2)}

Return the JSON object now.`,
        },
      ],
      temperature: 0,
      maxTokens: 500,
      signal: args.signal,
    },
    RecipeSelection,
  );
}
