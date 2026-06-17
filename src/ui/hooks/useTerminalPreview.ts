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
            app.mcp.callTool("terminal.getStatus", { terminalId }),
            app.mcp.callTool("terminal.getOutput", { terminalId, lines: 40 }),
          ]);
          const sc = (status.structuredContent ?? {}) as Record<string, unknown>;
          next.push({
            terminalId,
            watcherId: watcher.id,
            title: watcher.title,
            agentState:
              typeof sc.agentState === "string" ? sc.agentState : undefined,
            runtimeStatus:
              typeof sc.runtimeStatus === "string" ? sc.runtimeStatus : undefined,
            tail: output.text.slice(-3000),
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
