/**
 * Aggregates every tool module into the flat list the registry consumes.
 * Each module exports a `ToolDef[]`; keep this list in sync as modules are added.
 */
import type { ToolDef } from "./types.js";
import { fsTools } from "./fsTools.js";
import { mcpTools } from "./mcpTools.js";
import { timerTools } from "./timerTools.js";
import { watcherTools } from "./watcherTools.js";
import { queueTools } from "./queueTools.js";
import { contextTools } from "./contextTools.js";
import { agentTaskTools } from "./agentTaskTools.js";

export function buildAllTools(): ToolDef[] {
  return [
    ...fsTools,
    ...mcpTools,
    ...timerTools,
    ...watcherTools,
    ...queueTools,
    ...contextTools,
    ...agentTaskTools,
  ];
}
