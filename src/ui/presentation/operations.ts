/**
 * Build operations view-models from durable state. Watchers and watched-terminal
 * previews are merged into a single AGENT row — the user thinks about one
 * supervised agent doing one job, not "a watcher" and "a terminal" as separate
 * concepts. Rows are ordered by human urgency (needs-input first), so an urgent
 * approval never gets the same weight as idle work.
 */
import {
  classificationEpistemicKind,
  type EpistemicKind,
  type WatcherRecord,
} from "../../schemas.js";
import type { TerminalPreview } from "../hooks/useTerminalPreview.js";
import { watcherBadge, type WatcherBadge } from "../theme.js";

export interface AgentRow {
  /** Primary id shown to the user — the terminal it supervises, else the watcher. */
  id: string;
  watcherId: string;
  title: string;
  goal: string;
  badge: WatcherBadge;
  classification?: string;
  /** Epistemic provenance of the last verdict (issue #85) — whether this row's
   * state is an observed fact, a model inference, or unverified. Taken from the
   * watcher's persisted `lastEpistemicKind`, with a classification-derived fallback
   * for rows stored before that field existed. */
  epistemicKind: EpistemicKind;
  /** Last non-empty preview line, when a terminal preview is available. */
  preview?: string;
  /** Live agent/runtime state from the preview, if any. */
  agentState?: string;
  startedAt: number;
  needsAttention: boolean;
}

function parseTargets(w: WatcherRecord): string[] {
  try {
    const v = JSON.parse(w.targetsJson);
    return Array.isArray(v) ? v.map(String) : [];
  } catch {
    return [];
  }
}

function lastLine(tail: string | undefined): string | undefined {
  if (!tail) return undefined;
  const lines = tail.split("\n").filter((l) => l.trim().length > 0);
  return lines.length ? lines[lines.length - 1].trim() : undefined;
}

/** Merge watchers + terminal previews into urgency-ordered agent rows. */
export function buildAgentRows(
  watchers: WatcherRecord[],
  previews: TerminalPreview[] = [],
): AgentRow[] {
  const byTerminal = new Map(previews.map((p) => [p.terminalId, p]));
  const rows = watchers.map((w): AgentRow => {
    const targets = parseTargets(w);
    const preview = targets.map((t) => byTerminal.get(t)).find(Boolean);
    const badge = watcherBadge(w.lastClassification);
    return {
      id: preview?.terminalId ?? targets[0] ?? w.id,
      watcherId: w.id,
      title: w.title,
      goal: w.goal,
      badge,
      classification: w.lastClassification,
      epistemicKind:
        w.lastEpistemicKind ?? classificationEpistemicKind(w.lastClassification),
      preview: lastLine(preview?.tail),
      agentState: preview?.agentState ?? preview?.runtimeStatus,
      startedAt: w.createdAt,
      needsAttention: badge.priority <= 1,
    };
  });
  // Urgency first (lower priority value), then most-recent.
  return rows.sort(
    (a, b) => a.badge.priority - b.badge.priority || b.startedAt - a.startedAt,
  );
}

/** A short key for a recommended action, derived from its label's first word. */
export function actionKey(label: string, index: number): string {
  const m = label.match(/[a-z]/i);
  return (m?.[0] ?? String(index + 1)).toUpperCase();
}
