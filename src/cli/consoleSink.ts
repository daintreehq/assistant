/**
 * Legacy console renderer expressed as an AgentEventSink. Used for one-shot
 * prompts, the --classic REPL, and any non-TTY invocation where the Ink UI is
 * inappropriate (pipes, CI).
 */
import { render } from "./render.js";
import type { AgentEventSink } from "../agent/events.js";

export function createConsoleSink(): AgentEventSink {
  return {
    assistantStart() {
      render.assistantStart();
    },
    assistantToken(token) {
      render.streamToken(token);
    },
    assistantEnd() {
      render.assistantEnd();
    },
    toolCall(name, args) {
      render.line();
      render.toolCall(name, args);
    },
    toolResult(_name, result) {
      render.toolResult(result.ok, result.summary);
    },
    error(message) {
      render.line();
      render.error(message);
    },
    info(message) {
      render.info(message);
    },
  };
}
