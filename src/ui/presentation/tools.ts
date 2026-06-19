/**
 * Translate raw tool calls into human verbs for the activity tree. Known
 * first-party tools become an operator-readable "verb + target" pair; unknown
 * tools fall back to their internal name — but never to raw `fn({...})` syntax.
 * Raw args stay available for the expanded/detail view (see ActivityTree).
 */
import { truncate } from "../../utils/text.js";

export interface ToolPresentation {
  /** Human verb, e.g. "Read", "Delegated", "Watching". */
  label: string;
  /** The target/object of the verb, e.g. a relative path or a goal. */
  detail?: string;
}

type Args = Record<string, unknown>;

function str(v: unknown): string | undefined {
  if (typeof v === "string" && v.trim()) return v;
  if (typeof v === "number") return String(v);
  return undefined;
}

/** A workspace-relative-ish path: trim the cwd prefix when present. */
function relativePath(p: unknown): string | undefined {
  const s = str(p);
  if (!s) return undefined;
  let cwd = "";
  try {
    cwd = process.cwd();
  } catch {
    /* cwd unavailable (sandbox) — show the path as-is */
  }
  if (cwd && s.startsWith(cwd)) {
    const rel = s.slice(cwd.length).replace(/^[/\\]+/, "");
    return rel || s;
  }
  return s;
}

function ids(v: unknown): string | undefined {
  if (Array.isArray(v)) return v.map(String).join(", ") || undefined;
  return str(v);
}

const MAP: Record<string, (a: Args) => ToolPresentation> = {
  "fs.read": (a) => ({ label: "Read", detail: relativePath(a.path) }),
  "fs.list": (a) => ({ label: "Listed", detail: relativePath(a.path) ?? "." }),
  "fs.search": (a) => ({ label: "Searched", detail: str(a.query) }),
  "tool.search": (a) => ({ label: "Searched tools", detail: str(a.query) }),
  "context.snapshot": () => ({ label: "Snapshotted", detail: "workspace context" }),
  "context.summarize": (a) => ({ label: "Summarized", detail: str(a.terminalId) }),
  "agentTask.spawnForEdits": (a) => ({
    label: "Delegated",
    detail: str(a.title) ?? str(a.goal),
  }),
  "watcher.terminal.create": (a) => ({
    label: "Watching",
    detail: str(a.goal) ?? str(a.title) ?? ids(a.terminalIds),
  }),
  "watcher.list": () => ({ label: "Listed watchers" }),
  "watcher.cancel": (a) => ({ label: "Stopped watcher", detail: str(a.id) }),
  "timer.schedule": (a) => ({ label: "Scheduled", detail: str(a.title) }),
  "timer.list": () => ({ label: "Listed timers" }),
  "timer.cancel": (a) => ({ label: "Cancelled timer", detail: str(a.id) }),
  "terminal.focus": (a) => ({ label: "Focused", detail: str(a.terminalId) }),
  "terminal.extract": (a) => ({ label: "Extracted", detail: str(a.terminalId) }),
  "terminal.extract.async": (a) => ({ label: "Extracting", detail: str(a.terminalId) }),
  "terminal.summarize": (a) => ({ label: "Summarized", detail: str(a.terminalId) }),
  "queue.publish": (a) => ({ label: "Raised", detail: str(a.title) }),
  "queue.digest": () => ({ label: "Read inbox" }),
  "queue.resolve": (a) => ({ label: "Resolved", detail: str(a.id) }),
  "recipe.list": () => ({ label: "Listed recipes" }),
  "recipe.run": (a) => ({ label: "Ran recipe", detail: str(a.recipeId) }),
  "recipe.step.advance": (a) => ({
    label: "Advanced step",
    detail: str(a.completedStep)
      ? `${str(a.recipeId) ?? "recipe"} · step ${str(a.completedStep)}`
      : str(a.recipeId),
  }),
  "recipe.run.get": (a) => ({ label: "Checked recipe progress", detail: str(a.recipeId) }),
  "worktree.createWithRecipe": (a) => ({
    label: "Created worktree",
    detail: str(a.recipeId),
  }),
  "forge.getIssue": (a) => ({ label: "Read issue", detail: str(a.issueNumber) }),
  "forge.listIssues": () => ({ label: "Listed issues" }),
  "forge.listPRs": () => ({ label: "Listed PRs" }),
  "workflow.startWorkOnIssue": (a) => ({
    label: "Started work",
    detail: str(a.issueNumber) ?? str(a.title),
  }),
  "workflow.prepBranchForReview": (a) => ({
    label: "Prepping branch",
    detail: str(a.branch) ?? str(a.worktreeId),
  }),
  "grant.create": () => ({ label: "Granted automation" }),
  "grant.list": () => ({ label: "Listed grants" }),
  "grant.revoke": (a) => ({ label: "Revoked grant", detail: str(a.id) }),
  // The result summary already starts with "Daintree MCP …", so the label must
  // not repeat "Daintree" (it would read "Checked Daintree Daintree MCP …").
  // "Checked status" still stands on its own on active/failed rows that have no
  // summary yet, and the listTools label likewise drops the redundant "Daintree".
  "daintree.status": () => ({ label: "Checked status" }),
  "daintree.listTools": () => ({ label: "Listed tools" }),
  "daintree.call": (a) => ({ label: "Called", detail: str(a.toolName) ?? str(a.name) }),
};

/** Present a tool call as a verb + target. */
export function presentTool(name: string, args: unknown): ToolPresentation {
  const a = (args && typeof args === "object" ? args : {}) as Args;
  const fn = MAP[name];
  if (fn) {
    try {
      const p = fn(a);
      return { label: p.label, detail: p.detail ? truncate(p.detail, 48) : undefined };
    } catch {
      /* fall through to the name fallback */
    }
  }
  // Unknown tool: show the internal name as the verb, never raw fn() syntax.
  return { label: name };
}
