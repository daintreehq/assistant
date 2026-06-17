/**
 * The main-thread agentic loop. Streams the large model, executes any tool calls
 * it requests through the registry, and feeds results back until the model
 * produces a final answer. Conversation is persisted to SQLite so a session can
 * be inspected/compacted later.
 */
import type { ChatMessage } from "../models/fireworks.js";
import { FireworksUnavailableError } from "../models/fireworks.js";
import type { ModelRouter } from "../models/router.js";
import type { ToolRegistry } from "../tools/registry.js";
import type { ToolContext } from "../tools/types.js";
import type { ToolResult } from "../schemas.js";
import { BASE_SYSTEM_PROMPT, BASE_SYSTEM_PROMPT_VERSION } from "../models/prompts/base.js";
import { buildRuntimeContextMessage } from "../models/prompts/runtimeContext.js";
import { buildLoadedRecipesMessage } from "../models/prompts/recipes.js";
import type { MainPromptContext } from "../models/prompts/runtimeContext.js";
import type { RecipeRegistry } from "../recipes/registry.js";
import { selectRecipes } from "../recipes/selector.js";
import {
  renderRecipeBundle,
  type RenderedRecipeBundle,
} from "../recipes/render.js";
import type { RecipeSelection } from "../recipes/types.js";
import { type AgentEventSink, noopAgentEvents } from "./events.js";

const MAX_TOOL_ITERATIONS = 12;
/** How many control messages always sit at the front of the conversation. */
const CONTROL_MESSAGE_COUNT = 3;
/** Re-run recipe selection at least this often even without trigger terms. */
const RECIPE_REFRESH_INTERVAL = 4;
/** Stable Fireworks cache key for the main-thread prefix (see docs §9). */
const MAIN_PROMPT_CACHE_KEY = BASE_SYSTEM_PROMPT_VERSION;
/** Strong terms that justify re-selecting recipes mid-conversation. */
const RECIPE_TRIGGER_RE =
  /\b(recipe|worktree|agent|edit|fix|implement|refactor|test|monitor|watch|terminal)\b/i;
const MAX_TOOL_RESULT_CHARS = 8000;
/**
 * Auto-compact the conversation once the estimated prompt size crosses this many
 * tokens. Conservative — well under a typical 128k context window, but high
 * enough that ordinary sessions never trip it. Tune here if the window changes.
 */
const AUTO_COMPACT_TOKEN_THRESHOLD = 60_000;
/** Rough chars-per-token ratio used by the dependency-free token estimator. */
const CHARS_PER_TOKEN = 4;

/**
 * Cheap, dependency-free token estimate: total message size divided by a fixed
 * chars-per-token ratio. Counts message `content` plus tool-call argument JSON
 * (assistant tool-call turns often carry `content: null` but large arguments),
 * so a tool-heavy session still trips the threshold. Approximate by design —
 * good enough for a "should we compact?" decision.
 */
function estimateTokens(messages: ChatMessage[]): number {
  const chars = messages.reduce((n, m) => {
    let c = m.content?.length ?? 0;
    for (const tc of m.tool_calls ?? []) c += tc.function.arguments?.length ?? 0;
    return n + c;
  }, 0);
  return Math.ceil(chars / CHARS_PER_TOKEN);
}

export interface AgentSessionDeps {
  router: ModelRouter;
  registry: ToolRegistry;
  recipeRegistry: RecipeRegistry;
  ctx: ToolContext;
  promptContext: MainPromptContext;
  sessionId: string;
  /** Where streamed tokens, tool calls, and errors go. Defaults to a no-op sink. */
  events?: AgentEventSink;
}

export class AgentSession {
  private messages: ChatMessage[] = [];
  private seq = 0;
  private readonly deps: AgentSessionDeps;
  private readonly events: AgentEventSink;

  // Recipe state. Control messages live at fixed indices:
  //   [0] base system prompt (cached prefix, never changes mid-session)
  //   [1] runtime context (tier/project/MCP/models)
  //   [2] loaded recipes
  private activeRecipeIds: string[] = [];
  private recipeBundle: RenderedRecipeBundle = renderRecipeBundle([]);
  private turnSinceRecipeRefresh = 0;

  constructor(deps: AgentSessionDeps) {
    this.deps = deps;
    this.events = deps.events ?? noopAgentEvents;
    this.messages = [
      { role: "system", content: BASE_SYSTEM_PROMPT },
      { role: "system", content: buildRuntimeContextMessage(deps.promptContext) },
      { role: "system", content: buildLoadedRecipesMessage(this.recipeBundle) },
    ];
    for (const m of this.messages) this.persistMessage(m);
  }

  /**
   * Refresh the runtime-context message (e.g. after MCP connects or the tier
   * changes). Only message[1] is rewritten, so the cached base prefix is intact.
   */
  refreshRuntimeContext(promptContext: MainPromptContext): void {
    this.deps.promptContext = promptContext;
    this.messages[1] = {
      role: "system",
      content: buildRuntimeContextMessage(promptContext),
    };
  }

  getMessages(): ReadonlyArray<ChatMessage> {
    return this.messages;
  }

  /** Inject a queue/system note so the model sees it on the next turn. */
  injectNote(note: string): void {
    this.pushMessage({
      role: "user",
      content: `[system event]\n${note}`,
    });
  }

  /**
   * Compact the conversation: drop the working history and replace it with a
   * single summary note, keeping the three control messages (base prompt,
   * runtime context, loaded recipes) so the prompt-cache prefix and recipe state
   * survive. Unlike injectNote(), this actually shrinks the prompt — the model no
   * longer receives the old turns. A marker is appended to the durable log so the
   * persisted transcript records that earlier context was intentionally dropped.
   */
  compact(summary: string): void {
    const control = this.messages.slice(0, CONTROL_MESSAGE_COUNT);
    const note: ChatMessage = {
      role: "user",
      content: `[compacted summary of earlier conversation]\n${summary}`,
    };
    this.messages = [...control, note];
    this.persistMessage({
      role: "system",
      content: "[conversation compacted — earlier turns dropped from context]",
    });
    this.persistMessage(note);
  }

  /**
   * Before a turn, compact automatically if the accumulated history is large. We
   * summarize the working history (everything past the 3 control messages) with
   * the small model and fold it into a single note via compact(), so the cached
   * base prefix and recipe state survive. Best-effort: any failure (or no real
   * history to compact) leaves the conversation untouched and the turn proceeds.
   */
  private async maybeAutoCompact(): Promise<void> {
    if (estimateTokens(this.messages) <= AUTO_COMPACT_TOKEN_THRESHOLD) return;
    // Need real working history beyond the controls + any prior summary note.
    if (this.messages.length <= CONTROL_MESSAGE_COUNT + 1) return;

    const history = this.messages.slice(CONTROL_MESSAGE_COUNT);
    try {
      const result = await this.deps.router.chat("small", {
        messages: [
          {
            role: "system",
            content:
              "Summarize the conversation below in 2-3 sentences: the current goals, key decisions made, and any pending work. Be concise and factual.",
          },
          ...history,
        ],
      });
      const summary = result.content.trim();
      if (!summary) {
        this.events.info("Auto-compact skipped: empty summary");
        return;
      }
      this.compact(summary);
      this.events.info("Auto-compacted conversation");
    } catch {
      // Summary failed — keep the full history and let the turn continue.
      this.events.info("Auto-compact skipped: summary failed");
    }
  }

  /** Run a full user turn. Returns the final assistant text. */
  async send(userInput: string): Promise<string> {
    await this.maybeAutoCompact();
    await this.maybeRefreshRecipes(userInput);
    this.pushMessage({ role: "user", content: userInput });

    // Projection can throw if a registered tool produces an illegal or
    // colliding wire name (a registration-time programmer error). Surface it
    // through the event sink rather than letting it escape send() and strand
    // the session after the user message was already persisted.
    let tools;
    try {
      tools = this.deps.registry.toOpenAITools();
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      this.events.error(`Tool projection failed: ${msg}`);
      return `Tool projection failed: ${msg}`;
    }

    for (let i = 0; i < MAX_TOOL_ITERATIONS; i++) {
      this.events.assistantStart();
      let result;
      try {
        result = await this.deps.router.stream(
          "large",
          {
            messages: this.messages,
            tools,
            toolChoice: "auto",
            promptCacheKey: MAIN_PROMPT_CACHE_KEY,
          },
          (tok) => this.events.assistantToken(tok),
        );
      } catch (err) {
        if (err instanceof FireworksUnavailableError) {
          const msg = `Model unavailable: ${err.message}`;
          this.events.error(msg);
          return msg;
        }
        const msg = err instanceof Error ? err.message : String(err);
        this.events.error(`Model error: ${msg}`);
        return `Model error: ${msg}`;
      }

      // Record the assistant turn (with any tool calls).
      this.pushMessage({
        role: "assistant",
        content: result.content || null,
        tool_calls: result.toolCalls.length ? result.toolCalls : undefined,
      });

      if (result.toolCalls.length === 0) {
        this.events.assistantEnd(result.content);
        return result.content;
      }

      // Execute each requested tool call.
      for (const call of result.toolCalls) {
        // The model echoes back the OpenAI-legal wire name (e.g. `fs__read`);
        // translate it to the internal dotted name (`fs.read`) for dispatch,
        // events, and audit. Fall back to the raw name if it's unrecognized so
        // an unknown call still surfaces as UNKNOWN_TOOL rather than crashing.
        const internalName =
          this.deps.registry.resolveWireName(call.function.name) ??
          call.function.name;

        let args: unknown;
        let parseFailed = false;
        try {
          args = call.function.arguments ? JSON.parse(call.function.arguments) : {};
        } catch {
          parseFailed = true;
        }

        let res: ToolResult;
        if (parseFailed) {
          this.events.toolCall(internalName, call.function.arguments);
          res = {
            ok: false,
            summary: `Invalid JSON arguments for ${internalName}; not executed.`,
            error: {
              code: "INVALID_TOOL_ARGS_JSON",
              message: "Arguments were not valid JSON.",
              recoverable: true,
            },
          };
        } else {
          this.events.toolCall(internalName, args);
          res = await this.deps.registry.dispatch(
            internalName,
            args,
            this.deps.ctx,
          );
        }
        this.events.toolResult(internalName, res);

        this.pushMessage({
          role: "tool",
          tool_call_id: call.id,
          name: internalName,
          content: serializeToolResult(res),
        });
      }
    }

    const msg = "Reached the tool-iteration limit without a final answer.";
    this.events.error(msg);
    return msg;
  }

  private pushMessage(m: ChatMessage): void {
    this.messages.push(m);
    this.persistMessage(m);
  }

  /**
   * Persist a message to the conversation log (best-effort). Mutable control
   * messages (runtime context, loaded recipes) are persisted once on insert; when
   * they are later rewritten in place we deliberately do NOT re-persist them, so
   * the durable log keeps the initial control snapshot and append seqs stay clean.
   */
  private persistMessage(m: ChatMessage): void {
    try {
      this.deps.ctx.db.insertMessage({
        sessionId: this.deps.sessionId,
        seq: this.seq++,
        role: m.role,
        content: m.content ?? "",
        toolCallsJson: m.tool_calls ? JSON.stringify(m.tool_calls) : undefined,
        toolCallId: m.tool_call_id,
      });
    } catch {
      /* persistence is best-effort */
    }
  }

  /* --------------------------- recipe control ---------------------------- */

  /** Currently loaded recipe ids (for inspection / selection context). */
  getActiveRecipeIds(): ReadonlyArray<string> {
    return this.activeRecipeIds;
  }

  /**
   * Decide whether to re-run recipe selection for this turn, and if so, swap the
   * loaded-recipes control message. Throttled to preserve prompt-cache locality:
   * we only ask the small model on the first turn, every Nth turn, or when the
   * input contains a strong trigger term.
   */
  private async maybeRefreshRecipes(userInput: string): Promise<void> {
    const shouldCheck =
      this.turnSinceRecipeRefresh === 0 ||
      this.turnSinceRecipeRefresh >= RECIPE_REFRESH_INTERVAL ||
      RECIPE_TRIGGER_RE.test(userInput);
    if (!shouldCheck) {
      this.turnSinceRecipeRefresh++;
      return;
    }
    await this.runSelection(userInput);
  }

  /**
   * Force a recipe re-selection now, ignoring the throttle (manual /recipes
   * reload). Returns false if the selector failed and we kept the existing set.
   */
  async forceRecipeRefresh(userInput = ""): Promise<boolean> {
    return this.runSelection(userInput);
  }

  /**
   * Run the small-model selector and apply the result. Returns false if the
   * selector errored and we fell back to keeping the existing recipes.
   */
  private async runSelection(userInput: string): Promise<boolean> {
    let selection: RecipeSelection;
    let ok = true;
    try {
      selection = await selectRecipes({
        router: this.deps.router,
        registry: this.deps.recipeRegistry,
        userInput,
        recentMessages: this.messages.slice(CONTROL_MESSAGE_COUNT),
        activeRecipeIds: [...this.activeRecipeIds],
      });
    } catch {
      ok = false;
      selection = {
        recipeIds: [...this.activeRecipeIds],
        confidence: 0,
        reason: "selector failed; keeping existing recipes",
        taskType: "unknown",
        keepExisting: true,
      };
    }

    // keepExisting (when there is something to keep) means "don't change", so the
    // model's recipeIds are ignored — the selector sets it only when the task is
    // unchanged. Otherwise honour the requested set.
    const requested =
      selection.keepExisting && this.activeRecipeIds.length > 0
        ? this.activeRecipeIds
        : selection.recipeIds;
    const known = this.resolveKnownIds(requested);
    // The model named recipes but none are known — treat that as a hallucination
    // and keep the current set. Only an explicitly empty selection clears recipes.
    const nextIds =
      known.length === 0 && requested.length > 0 ? this.activeRecipeIds : known;
    this.applyRecipeBundle(this.deps.recipeRegistry.getMany(nextIds));
    this.turnSinceRecipeRefresh = 1;
    this.logSelection(userInput, selection);
    return ok;
  }

  /** Manually load these recipe ids (unknown ids are dropped, capped at three). */
  setRecipes(ids: string[]): void {
    this.applyRecipeBundle(
      this.deps.recipeRegistry.getMany(this.resolveKnownIds(ids)),
    );
    // Manual selection should persist; defer the next automatic check.
    this.turnSinceRecipeRefresh = 1;
  }

  /**
   * Dedupe, drop unknown ids, then cap at three. Resolving known ids before the
   * cap means a hallucinated/unknown id can never push a valid recipe out.
   */
  private resolveKnownIds(ids: string[]): string[] {
    return [...new Set(ids)]
      .filter((id) => this.deps.recipeRegistry.has(id))
      .slice(0, 3);
  }

  /** Render the loaded recipes for /recipes loaded. */
  describeRecipes(): string {
    if (this.recipeBundle.items.length === 0) {
      return "No recipes are currently loaded.";
    }
    const lines = this.recipeBundle.items.map(
      (r) => `  ${r.id}  [${r.risk}]  ${r.title} — ${r.summary}`,
    );
    return `Loaded recipes (${this.recipeBundle.items.length}, bundle ${this.recipeBundle.hash}):\n${lines.join("\n")}`;
  }

  /** Swap in a new recipe bundle and rewrite the loaded-recipes control message. */
  private applyRecipeBundle(recipes: ReturnType<RecipeRegistry["getMany"]>): void {
    this.recipeBundle = renderRecipeBundle(recipes);
    this.activeRecipeIds = this.recipeBundle.ids;
    this.messages[2] = {
      role: "system",
      content: buildLoadedRecipesMessage(this.recipeBundle),
    };
  }

  /** Append a selection decision to the dataset (best-effort). */
  private logSelection(userInput: string, selection: RecipeSelection): void {
    try {
      this.deps.ctx.db.insertRecipeSelection({
        sessionId: this.deps.sessionId,
        userInput: userInput.slice(0, 1000),
        selectedRecipeIdsJson: JSON.stringify(this.activeRecipeIds),
        confidence: selection.confidence,
        taskType: selection.taskType,
        reason: selection.reason,
      });
    } catch {
      /* logging is best-effort */
    }
  }
}

export function serializeToolResult(res: {
  ok: boolean;
  summary: string;
  result?: unknown;
  error?: unknown;
}): string {
  const payload = {
    ok: res.ok,
    summary: res.summary,
    result: res.result,
    error: res.error,
  };
  let s: string;
  try {
    s = JSON.stringify(payload);
  } catch {
    s = JSON.stringify({ ok: res.ok, summary: res.summary });
  }
  if (s.length <= MAX_TOOL_RESULT_CHARS) return s;
  const omitted = s.length - MAX_TOOL_RESULT_CHARS;
  // Explicit marker so the model knows output was clipped (vs. a silent ellipsis).
  return `${s.slice(0, MAX_TOOL_RESULT_CHARS)}\n[output truncated: ${omitted} chars omitted]`;
}
