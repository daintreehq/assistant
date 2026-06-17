/**
 * Polls bounded tails of the terminals that active watchers are targeting, so the
 * ops deck can show live preview cards. Strictly UI-only: raw scrollback is shown
 * to the human, never fed into the main model (the watcher engine handles signal
 * extraction with the small model).
 */
import { useEffect, useState } from "react";
import type { App } from "../../cli/app.js";
import type { WatcherRecord } from "../../schemas.js";

export interface TerminalPreview {
  terminalId: string;
  watcherId?: string;
  title?: string;
  agentState?: string;
  runtimeStatus?: string;
  tail: string;
  updatedAt: number;
}

const MAX_TERMINALS = 4;
const POLL_MS = 2500;

function parseTargets(w: WatcherRecord): string[] {
  try {
    const v = JSON.parse(w.targetsJson);
    return Array.isArray(v) ? v.map(String) : [];
  } catch {
    return [];
  }
}

export function useTerminalPreview(
  app: App,
  watchers: WatcherRecord[],
): TerminalPreview[] {
  const [previews, setPreviews] = useState<TerminalPreview[]>([]);
  // Stable dependency so the effect only re-runs when the target set changes.
  const targetKey = watchers
    .filter((w) => w.kind === "terminal")
    .map((w) => `${w.id}:${w.targetsJson}`)
    .join("|");

  useEffect(() => {
    let cancelled = false;

    async function refresh(): Promise<void> {
      if (!app.mcp.isConnected()) {
        if (!cancelled) setPreviews([]);
        return;
      }
      // Dedupe by terminalId so several watchers on the same terminal don't each
      // trigger a separate poll of it. First watcher to reference a terminal owns
      // the preview card's title.
      const seen = new Set<string>();
      const targets = watchers
        .filter((w) => w.kind === "terminal")
        .flatMap((w) =>
          parseTargets(w).map((terminalId) => ({ terminalId, watcher: w })),
        )
        .filter(({ terminalId }) => {
          if (seen.has(terminalId)) return false;
          seen.add(terminalId);
          return true;
        })
        .slice(0, MAX_TERMINALS);

      const next: TerminalPreview[] = [];
      for (const { terminalId, watcher } of targets) {
        try {
          const [status, output] = await Promise.all([
            app.mcp.callTool("terminal.getStatus", { terminalIds: [terminalId] }),
            app.mcp.callTool("terminal.getOutput", { terminalId, maxLines: 40 }),
          ]);
          // terminal.getStatus -> { terminals: [{ terminalId, agentState, ... }] }.
          // Only attribute status when the returned id matches the one we asked
          // for — never guess from terminals[0].
          const statusSc = (status.structuredContent ?? {}) as Record<string, unknown>;
          const terminals = Array.isArray(statusSc.terminals)
            ? (statusSc.terminals as Array<Record<string, unknown>>)
            : [];
          const entry = terminals.find((t) => t?.terminalId === terminalId);
          const agentState =
            entry && typeof entry.agentState === "string" ? entry.agentState : undefined;
          // terminal.getOutput -> { content }; ignore errored reads.
          const outSc = (output.structuredContent ?? {}) as Record<string, unknown>;
          const content =
            !output.isError && typeof outSc.content === "string" ? outSc.content : "";
          next.push({
            terminalId,
            watcherId: watcher.id,
            title: watcher.title,
            agentState,
            runtimeStatus: agentState === "exited" ? "exited" : undefined,
            tail: (content ?? "").slice(-3000),
            updatedAt: Date.now(),
          });
        } catch {
          // Preview reads are best-effort; the watcher engine still queues signals.
        }
      }
      if (!cancelled) setPreviews(next);
    }

    void refresh();
    const timer = setInterval(refresh, POLL_MS);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [app, targetKey]);

  return previews;
}
