/**
 * Visual theme for the Ink UI. Restrained palette: crisp borders, subdued labels,
 * strong severity cues. The assistant should read like an operations console.
 */
export const theme = {
  brand: "#6EE7B7",
  dim: "gray",
  ok: "green",
  warn: "yellow",
  error: "red",
  info: "cyan",
  blocked: "magenta",
  border: "gray",
} as const;

/** Map a queue severity to a color. */
export function severityColor(severity: string): string {
  switch (severity) {
    case "done":
      return theme.ok;
    case "info":
      return theme.info;
    case "attention":
      return theme.warn;
    case "urgent":
    case "blocked":
      return theme.blocked;
    case "error":
      return theme.error;
    default:
      return theme.dim;
  }
}

/** Map a watcher classification to a short badge + color for the sidebar. */
export function watcherBadge(classification?: string): {
  label: string;
  color: string;
} {
  switch (classification) {
    case "waiting_for_input":
    case "permission_prompt":
      return { label: "needs input", color: theme.warn };
    case "command_failed":
    case "tests_failed":
      return { label: "failed", color: theme.error };
    case "tests_passed":
    case "completed_success":
      return { label: "done", color: theme.ok };
    case "merge_conflict":
      return { label: "blocked", color: theme.blocked };
    case "still_working":
      return { label: "working", color: theme.info };
    case "terminal_exited":
      return { label: "exited", color: theme.dim };
    default:
      return { label: classification ?? "pending", color: theme.dim };
  }
}
