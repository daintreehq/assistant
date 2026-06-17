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
import { buildMainSystemPrompt, type MainPromptContext } from "../models/prompts.js";
import { truncate } from "../utils/text.js";
import { type AgentEventSink, noopAgentEvents } from "./events.js";

const MAX_TOOL_ITERATIONS = 12;
const MAX_TOOL_RESULT_CHARS = 8000;

export interface AgentSessionDeps {
  router: ModelRouter;
  registry: ToolRegistry;
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

  constructor(deps: AgentSessionDeps) {
    this.deps = deps;
    this.events = deps.events ?? noopAgentEvents;
    const system = buildMainSystemPrompt(deps.promptContext);
    this.pushMessage({ role: "system", content: system });
  }

  /** Refresh the system prompt (e.g. after MCP connects or tier changes). */
  refreshSystemPrompt(promptContext: MainPromptContext): void {
    this.deps.promptContext = promptContext;
    this.messages[0] = {
      role: "system",
      content: buildMainSystemPrompt(promptContext),
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

  /** Run a full user turn. Returns the final assistant text. */
  async send(userInput: string): Promise<string> {
    this.pushMessage({ role: "user", content: userInput });

    const tools = this.deps.registry.toOpenAITools();

    for (let i = 0; i < MAX_TOOL_ITERATIONS; i++) {
      this.events.assistantStart();
      let result;
      try {
        result = await this.deps.router.stream(
          "large",
          { messages: this.messages, tools, toolChoice: "auto" },
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
        let args: unknown;
        let parseFailed = false;
        try {
          args = call.function.arguments ? JSON.parse(call.function.arguments) : {};
        } catch {
          parseFailed = true;
        }

        let res: ToolResult;
        if (parseFailed) {
          this.events.toolCall(call.function.name, call.function.arguments);
          res = {
            ok: false,
            summary: `Invalid JSON arguments for ${call.function.name}; not executed.`,
            error: {
              code: "INVALID_TOOL_ARGS_JSON",
              message: "Arguments were not valid JSON.",
              recoverable: true,
            },
          };
        } else {
          this.events.toolCall(call.function.name, args);
          res = await this.deps.registry.dispatch(
            call.function.name,
            args,
            this.deps.ctx,
          );
        }
        this.events.toolResult(call.function.name, res);

        this.pushMessage({
          role: "tool",
          tool_call_id: call.id,
          name: call.function.name,
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
}

function serializeToolResult(res: {
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
  return truncate(s, MAX_TOOL_RESULT_CHARS);
}
